package vcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, f *fakeAPI) *client {
	t.Helper()
	c, err := newClient(familyGitHub, Repo{Host: "github.com", API: f.srv.URL}, "tok")
	if err != nil {
		t.Fatal(err)
	}
	c.gap = 0
	return c
}

// countItems decodes a JSON array page and records the items.
func countItems(seen *[]int) func([]byte) (int, error) {
	return func(b []byte) (int, error) {
		var list []int
		if err := json.Unmarshal(b, &list); err != nil {
			return 0, err
		}
		*seen = append(*seen, list...)
		return len(list), nil
	}
}

func TestPagesLinkAndETag(t *testing.T) {
	f := newFakeAPI(t)
	total := 150
	f.handle("GET /items", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		var items []int
		for i := (page-1)*100 + 1; i <= min(page*100, total); i++ {
			items = append(items, i)
		}
		if items == nil {
			items = []int{}
		}
		if page*100 < total {
			w.Header().Set("Link", fmt.Sprintf(`<%s/items?per_page=100&page=%d>; rel="next", <%s/items?page=9>; rel="last"`, f.srv.URL, page+1, f.srv.URL))
		}
		writeJSON(w, r, 200, items, false)
	})
	c := testClient(t, f)
	ctx := ctxT(t)
	first := c.url("items", map[string][]string{"per_page": {"100"}})

	var seen []int
	next := Cursor{}
	if err := c.pages(ctx, "items", first, nil, next, countItems(&seen)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 150 || seen[149] != 150 {
		t.Fatalf("seen %d items", len(seen))
	}
	if len(next) != 2 {
		t.Errorf("cursor = %v", next)
	}

	// Unchanged: two 304s, nothing decoded.
	f.reset()
	seen = nil
	cur, next := next, Cursor{}
	if err := c.pages(ctx, "items", first, cur, next, countItems(&seen)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 || len(f.requests()) != 2 {
		t.Errorf("unchanged: %d items, %d requests", len(seen), len(f.requests()))
	}

	// A new item lands on page 2 only: page 1 is 304, page 2 is read.
	total = 151
	f.reset()
	seen = nil
	cur, next = next, Cursor{}
	if err := c.pages(ctx, "items", first, cur, next, countItems(&seen)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 51 || seen[50] != 151 {
		t.Errorf("after append: %v", seen)
	}
}

func TestPagesFullLastPageProbe(t *testing.T) {
	f := newFakeAPI(t)
	total := 100
	f.handle("GET /items", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		items := []int{}
		for i := (page-1)*100 + 1; i <= min(page*100, total); i++ {
			items = append(items, i)
		}
		writeJSON(w, r, 200, items, false) // no Link header at all
	})
	c := testClient(t, f)
	ctx := ctxT(t)
	first := c.url("items", map[string][]string{"per_page": {"100"}})
	var seen []int
	cur := Cursor{}
	if err := c.pages(ctx, "items", first, nil, cur, countItems(&seen)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 100 || len(f.requests()) != 2 {
		t.Fatalf("%d items in %d requests", len(seen), len(f.requests()))
	}
	// Page 1 stays the same when item 101 arrives; the probe finds it.
	total = 101
	seen = nil
	if err := c.pages(ctx, "items", first, cur, Cursor{}, countItems(&seen)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != 101 {
		t.Errorf("probe found %v", seen)
	}
}

func TestPagesRefuseForeignLink(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("GET /items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://evil.example.com/items?page=2>; rel="next"`)
		writeJSON(w, r, 200, []int{1}, false)
	})
	c := testClient(t, f)
	err := c.pages(ctxT(t), "items", c.url("items", nil), nil, Cursor{}, countItems(new([]int)))
	if err == nil || !strings.Contains(err.Error(), "evil.example.com") {
		t.Errorf("err = %v", err)
	}
}

func TestRateLimits(t *testing.T) {
	f := newFakeAPI(t)
	var calls atomic.Int32
	f.handle("GET /secondary", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeJSON(w, r, 403, map[string]any{"message": "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}, false)
			return
		}
		writeJSON(w, r, 200, []int{}, false)
	})
	f.handle("POST /retry-after", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1)%2 == 1 {
			w.Header().Set("Retry-After", "7")
			writeJSON(w, r, 429, map[string]any{"message": "slow down"}, false)
			return
		}
		writeJSON(w, r, 201, map[string]any{}, false)
	})
	f.handle("GET /exhausted", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		writeJSON(w, r, 403, map[string]any{"message": "API rate limit exceeded for user ID 1."}, false)
	})
	f.handle("GET /forbidden", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 403, map[string]any{"message": "Resource not accessible by integration"}, false)
	})
	c := testClient(t, f)
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	ctx := ctxT(t)

	if _, err := c.send(ctx, "GET", c.url("secondary", nil), nil, ""); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != time.Minute {
		t.Errorf("secondary limit waits %v, want [1m]", slept)
	}
	slept = nil
	calls.Store(0)
	if _, err := c.send(ctx, "POST", c.url("retry-after", nil), map[string]string{"body": "x"}, ""); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("Retry-After waits %v, want [7s]", slept)
	}
	slept = nil
	_, err := c.send(ctx, "GET", c.url("exhausted", nil), nil, "")
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Until.Before(time.Now().Add(50*time.Minute)) || len(slept) != 0 {
		t.Errorf("exhausted quota: %v (slept %v)", err, slept)
	}
	_, err = c.send(ctx, "GET", c.url("forbidden", nil), nil, "")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 403 || he.Message != "Resource not accessible by integration" || len(slept) != 0 {
		t.Errorf("plain 403: %v", err)
	}
	if strings.Contains(err.Error(), "tok") {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestSinceParam(t *testing.T) {
	if got := sinceParam("2026-10-27T10:05:00.5Z"); got != "2026-10-27T10:04:58Z" {
		t.Errorf("sinceParam = %q", got)
	}
	if sinceParam("") != "" {
		t.Error("empty mark gives a since")
	}
	next := Cursor{}
	advance(next, "k", time.Date(2026, 1, 1, 10, 0, 0, 0, time.FixedZone("x", 3*3600)))
	advance(next, "k", time.Date(2026, 1, 1, 6, 0, 0, 0, time.UTC))
	if next["k"] != "2026-01-01T07:00:00Z" {
		t.Errorf("mark = %q", next["k"])
	}
}

func TestLinkNext(t *testing.T) {
	h := []string{`<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=5>; rel="last"`}
	if got := linkNext(h); got != "https://api.github.com/x?page=2" {
		t.Errorf("linkNext = %q", got)
	}
	if got := linkNext([]string{`<https://a/x?page=1>; rel="prev"`}); got != "" {
		t.Errorf("linkNext = %q", got)
	}
}

func TestRefBlockAndClip(t *testing.T) {
	to := Comment{Kind: CommentConversation, Author: "a", Body: "<!-- ymcp:agent pipeline=p run=1 msg=x -->\nAgent #1 (p)\n\nquoted"}
	if got := refBlock(to); got != "> In reply to @a's comment:\n> quoted" {
		t.Errorf("refBlock = %q", got)
	}
	if got := withRef("plain", "> In reply to x:"); got != "> In reply to x:\n\nplain" {
		t.Errorf("withRef = %q", got)
	}
	long := strings.Repeat("я", 40000) // 80000 bytes
	got := clip(long, githubMaxBody)
	if len(got) > githubMaxBody || !strings.HasSuffix(got, "… [trimmed]") || !strings.HasPrefix(got, "яя") {
		t.Errorf("clip: %d bytes", len(got))
	}
	if clip("short", 10) != "short" {
		t.Error("clip changed a short body")
	}
}
