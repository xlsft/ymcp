package vcs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	perPage     = 100
	maxPages    = 50
	maxBody     = 32 << 20
	writeGap    = time.Second     // GitHub asks for at least a second between writes
	maxWait     = 2 * time.Minute // longer rate-limit waits are returned as *RateLimitError
	maxTries    = 3
	sinceSlack  = 2 * time.Second // since windows overlap by this much
	httpTimeout = 30 * time.Second
)

// HTTPError is an unexpected answer of the host's API.
type HTTPError struct {
	Method  string
	URL     string // without the query
	Status  int
	Message string // the API's message, if any
}

func (e *HTTPError) Error() string {
	s := fmt.Sprintf("vcs: %s %s: %d %s", e.Method, e.URL, e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// RateLimitError means the host asked to wait longer than a call should block.
type RateLimitError struct {
	Until   time.Time
	Message string
}

func (e *RateLimitError) Error() string {
	s := "vcs: rate limited until " + e.Until.UTC().Format(time.RFC3339)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// client is the REST plumbing shared by GitHub and GitLab.
type client struct {
	fam   string
	base  *url.URL // API root
	web   string   // web root, e.g. https://github.com
	token string
	hc    *http.Client

	gap     time.Duration
	maxWait time.Duration
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
}

func newClient(fam string, repo Repo, token string) (*client, error) {
	api := repo.API
	if api == "" {
		api = apiRoot(fam, "https", repo.Host)
	}
	base, err := url.Parse(strings.TrimSuffix(api, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, fmt.Errorf("vcs: bad API root %q", api)
	}
	return &client{
		fam:     fam,
		base:    base,
		web:     webRoot(base.String()),
		token:   strings.TrimSpace(token),
		hc:      &http.Client{Timeout: httpTimeout},
		gap:     writeGap,
		maxWait: maxWait,
		now:     time.Now,
		sleep:   sleepCtx,
	}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// url joins the API root, an already escaped path and a query.
func (c *client) url(path string, q url.Values) string {
	s := c.base.String() + "/" + strings.TrimPrefix(path, "/")
	if len(q) > 0 {
		s += "?" + q.Encode()
	}
	return s
}

type response struct {
	Status int
	Header http.Header
	Body   []byte
}

// send makes one API call. in, when not nil, is sent as JSON. etag makes the
// call conditional: a 304 is returned as a response, not an error. Writes to
// one host go one at a time, at least c.gap apart. Rate-limit answers are
// waited out and retried when the wait is short.
func (c *client) send(ctx context.Context, method, rawURL string, in any, etag string) (response, error) {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return response{}, err
		}
	}
	write := method != http.MethodGet && method != http.MethodHead
	for attempt := 1; ; attempt++ {
		resp, err := c.once(ctx, method, rawURL, payload, etag, write)
		if err != nil {
			return response{}, err
		}
		if resp.Status == http.StatusNotModified && etag != "" {
			return resp, nil
		}
		if resp.Status >= 200 && resp.Status < 300 {
			return resp, nil
		}
		if rateLimited(resp) {
			d := c.waitFor(resp.Header)
			if attempt < maxTries && d <= c.maxWait {
				if err := c.sleep(ctx, d); err != nil {
					return response{}, err
				}
				continue
			}
			return response{}, &RateLimitError{Until: c.now().Add(d), Message: apiMessage(resp.Body)}
		}
		return response{}, &HTTPError{Method: method, URL: stripQuery(rawURL), Status: resp.Status, Message: apiMessage(resp.Body)}
	}
}

func (c *client) once(ctx context.Context, method, rawURL string, payload []byte, etag string, write bool) (response, error) {
	if write {
		g := gateFor(c.base.Host)
		if err := g.acquire(ctx, c.gap, c.now, c.sleep); err != nil {
			return response{}, err
		}
		defer g.release(c.now)
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return response{}, err
	}
	req.Header.Set("User-Agent", "ymcp")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if c.fam == familyGitHub {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// net/http's error repeats the URL, which never holds a token here.
		return response{}, fmt.Errorf("vcs: %s %s: %w", method, stripQuery(rawURL), unwrapURLError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return response{}, fmt.Errorf("vcs: %s %s: reading the answer: %w", method, stripQuery(rawURL), err)
	}
	return response{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

func unwrapURLError(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

func stripQuery(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}

// rateLimited reports a rate-limit answer: 429, or GitHub's 403 with no
// quota left or a "rate limit" message (the secondary limits).
func rateLimited(r response) bool {
	switch r.Status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		if r.Header.Get("X-RateLimit-Remaining") == "0" || r.Header.Get("Retry-After") != "" {
			return true
		}
		return strings.Contains(strings.ToLower(apiMessage(r.Body)), "rate limit")
	}
	return false
}

// waitFor is how long to wait after a rate-limit answer: Retry-After, else
// until the quota resets, else a minute.
func (c *client) waitFor(h http.Header) time.Duration {
	if ra := strings.TrimSpace(h.Get("Retry-After")); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
		if t, err := http.ParseTime(ra); err == nil {
			return max(t.Sub(c.now()), 0)
		}
	}
	for _, p := range []string{"X-RateLimit-", "RateLimit-"} {
		if h.Get(p+"Remaining") == "0" {
			if n, err := strconv.ParseInt(h.Get(p+"Reset"), 10, 64); err == nil {
				return max(time.Unix(n, 0).Sub(c.now())+time.Second, 0)
			}
		}
	}
	return time.Minute
}

// apiMessage pulls the human message out of an error body.
func apiMessage(body []byte) string {
	var m struct {
		Message json.RawMessage `json:"message"`
		Error   string          `json:"error"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	msg := m.Error
	if len(m.Message) > 0 {
		var s string
		if json.Unmarshal(m.Message, &s) == nil {
			msg = s
		} else {
			msg = string(m.Message)
		}
	}
	// GitHub's 422 says "Validation Failed" and puts the reason in errors.
	for _, e := range m.Errors {
		detail := e.Message
		if detail == "" && e.Code != "" {
			detail = strings.TrimSpace(e.Field + " " + e.Code)
		}
		if detail != "" {
			msg += "; " + detail
		}
	}
	msg = strings.TrimPrefix(msg, "; ")
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300]) + "…"
	}
	return msg
}

// gate serialises the writes to one host and spaces them out.
type gate struct {
	ch   chan struct{}
	last time.Time // guarded by ch
}

var gates = struct {
	sync.Mutex
	m map[string]*gate
}{m: map[string]*gate{}}

func gateFor(host string) *gate {
	gates.Lock()
	defer gates.Unlock()
	g := gates.m[host]
	if g == nil {
		g = &gate{ch: make(chan struct{}, 1)}
		gates.m[host] = g
	}
	return g
}

func (g *gate) acquire(ctx context.Context, gap time.Duration, now func() time.Time, sleep func(context.Context, time.Duration) error) error {
	select {
	case g.ch <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if d := g.last.Add(gap).Sub(now()); !g.last.IsZero() && d > 0 {
		if err := sleep(ctx, d); err != nil {
			<-g.ch
			return err
		}
	}
	return nil
}

func (g *gate) release(now func() time.Time) {
	g.last = now()
	<-g.ch
}

// pages polls one paged list. first is the URL of page 1. Every page that
// answers 200 is handed to fn, which returns how many items it held; pages
// that answer 304 to the ETag kept in cur are skipped. The ETag entries for
// the next poll go into next.
func (c *client) pages(ctx context.Context, stream, first string, cur, next Cursor, fn func(body []byte) (int, error)) error {
	u := first
	for p := 1; u != ""; p++ {
		if p > maxPages {
			return fmt.Errorf("vcs: %s: more than %d pages", stream, maxPages)
		}
		key := "etag:" + stream + ":" + strconv.Itoa(p)
		prevURL, etag, prevNext := splitEntry(cur[key])
		if prevURL != u {
			etag, prevNext = "", ""
		}
		resp, err := c.send(ctx, http.MethodGet, u, nil, etag)
		if err != nil {
			return err
		}
		if resp.Status == http.StatusNotModified {
			next[key] = cur[key]
			u = prevNext
			continue
		}
		n, err := fn(resp.Body)
		if err != nil {
			return fmt.Errorf("vcs: %s: %w", stream, err)
		}
		nu, err := c.nextPage(resp.Header, u, n)
		if err != nil {
			return err
		}
		next[key] = joinEntry(u, resp.Header.Get("ETag"), nu)
		u = nu
	}
	return nil
}

// nextPage finds the next page: the Link header, GitLab's X-Next-Page, or,
// when the page was full and neither says, the following page number (an
// empty page then ends the list).
func (c *client) nextPage(h http.Header, cur string, n int) (string, error) {
	if link := linkNext(h.Values("Link")); link != "" {
		u, err := url.Parse(link)
		if err != nil {
			return "", fmt.Errorf("vcs: bad page link %q", link)
		}
		if u.Scheme != c.base.Scheme || u.Host != c.base.Host {
			return "", fmt.Errorf("vcs: refusing to follow a page link to %s", u.Host)
		}
		return u.String(), nil
	}
	page := 0
	if xn := strings.TrimSpace(h.Get("X-Next-Page")); xn != "" {
		page, _ = strconv.Atoi(xn)
	} else if h.Get("X-Total-Pages") == "" && n >= perPage {
		u, _ := url.Parse(cur)
		cp, _ := strconv.Atoi(u.Query().Get("page"))
		page = max(cp, 1) + 1
	}
	if page <= 0 {
		return "", nil
	}
	u, err := url.Parse(cur)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("page", strconv.Itoa(page))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// linkNext returns the rel="next" target of Link headers.
func linkNext(values []string) string {
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			segs := strings.Split(part, ";")
			if len(segs) < 2 {
				continue
			}
			target := strings.TrimSpace(segs[0])
			if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
				continue
			}
			for _, s := range segs[1:] {
				s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
				if s == `rel="next"` || s == "rel=next" {
					return target[1 : len(target)-1]
				}
			}
		}
	}
	return ""
}

const sep = "\x1f"

func joinEntry(u, etag, next string) string { return u + sep + etag + sep + next }

func splitEntry(v string) (u, etag, next string) {
	parts := strings.SplitN(v, sep, 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return parts[0], parts[1], parts[2]
}

// stamp is a JSON timestamp read as UTC.
type stamp struct{ time.Time }

func (s *stamp) UnmarshalJSON(b []byte) error {
	var v *string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if v == nil || *v == "" {
		s.Time = time.Time{}
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *v)
	if err != nil {
		return err
	}
	s.Time = t.UTC()
	return nil
}

// sinceMark is the since value a cursor keeps: the newest update seen.
func sinceMark(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// sinceParam is the since query value for a mark, moved back by sinceSlack
// and cut to whole seconds.
func sinceParam(mark string) string {
	t, err := time.Parse(time.RFC3339Nano, mark)
	if err != nil {
		return ""
	}
	return t.Add(-sinceSlack).UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
}

// advance raises the since mark of key in next to t.
func advance(next Cursor, key string, t time.Time) {
	if t.IsZero() {
		return
	}
	if old, err := time.Parse(time.RFC3339Nano, next[key]); err == nil && !t.After(old) {
		return
	}
	next[key] = sinceMark(t)
}
