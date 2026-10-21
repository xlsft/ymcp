// Package youtrack is a thin client for the YouTrack REST API.
package youtrack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a single YouTrack instance on behalf of one user.
type Client struct {
	baseURL *url.URL
	auth    *hubAuth
	http    *http.Client
}

// Options configures a Client.
type Options struct {
	// BaseURL is the instance root, e.g. https://youtrack.example.com.
	// A trailing /api is tolerated.
	BaseURL string
	// Login and Password of the YouTrack (Hub) account, LDAP included.
	Login    string
	Password string
	// Proxy selects the outbound proxy: "" honours HTTP(S)_PROXY,
	// "none" connects directly, anything else is used as the proxy URL.
	Proxy string
	// Timeout per request; zero means 30s.
	Timeout time.Duration
}

// New validates opts and builds a Client.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("youtrack: base URL is required")
	}
	if opts.Login == "" || opts.Password == "" {
		return nil, fmt.Errorf("youtrack: login and password are required")
	}
	u, err := url.Parse(strings.TrimRight(opts.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("youtrack: parse base URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("youtrack: base URL must be absolute, got %q", opts.BaseURL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/api")

	transport := http.DefaultTransport.(*http.Transport).Clone()
	switch opts.Proxy {
	case "":
		transport.Proxy = http.ProxyFromEnvironment
	case "none", "direct":
		transport.Proxy = nil
	default:
		p, err := url.Parse(opts.Proxy)
		if err != nil {
			return nil, fmt.Errorf("youtrack: parse proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(p)
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL: u,
		auth:    newHubAuth(u, opts.Login, opts.Password, transport, timeout),
		http:    &http.Client{Transport: transport, Timeout: timeout},
	}, nil
}

// BaseURL returns the instance root without a trailing slash.
func (c *Client) BaseURL() string { return c.baseURL.String() }

// Error is a non-2xx response from YouTrack.
type Error struct {
	Status      int
	Code        string `json:"error"`
	Description string `json:"error_description"`
	Body        string `json:"-"`
}

func (e *Error) Error() string {
	switch {
	case e.Description != "":
		return fmt.Sprintf("youtrack: %d %s: %s", e.Status, e.Code, e.Description)
	case e.Code != "":
		return fmt.Sprintf("youtrack: %d %s", e.Status, e.Code)
	default:
		return fmt.Sprintf("youtrack: %d %s", e.Status, strings.TrimSpace(e.Body))
	}
}

// Do performs a request against /api/<path>. body is JSON-encoded when
// non-nil; the response is decoded into out when out is non-nil.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := *c.baseURL
	u.Path = u.Path + "/api/" + strings.TrimLeft(path, "/")
	u.RawQuery = query.Encode()
	return c.do(ctx, "", method, &u, body, out)
}

// HubGet performs a GET against the Hub REST API, <hub>/api/rest/<path>,
// with a Hub-scoped token.
func (c *Client) HubGet(ctx context.Context, path string, query url.Values, out any) error {
	u, err := c.auth.HubURL(ctx)
	if err != nil {
		return err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/rest/" + strings.TrimLeft(path, "/")
	u.RawQuery = query.Encode()
	return c.do(ctx, HubScope, http.MethodGet, u, nil, out)
}

func (c *Client) do(ctx context.Context, scope, method string, u *url.URL, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("youtrack: encode body: %w", err)
		}
	}

	resp, data, err := c.send(ctx, scope, method, u, payload)
	if resp != nil && resp.StatusCode == http.StatusUnauthorized {
		// The token may have been revoked before its expiry; log in again once.
		resp, data, err = c.send(ctx, scope, method, u, payload)
	}
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode, Body: string(data)}
		_ = json.Unmarshal(data, e)
		return e
	}
	if ct := resp.Header.Get("Content-Type"); len(data) > 0 && !strings.Contains(ct, "json") {
		// A proxy or captive portal answered instead of YouTrack.
		return fmt.Errorf("youtrack: unexpected %q response from %s (check URL, VPN and proxy settings)", ct, u.Host)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = append((*raw)[:0], data...)
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("youtrack: decode response: %w", err)
	}
	return nil
}

// send makes one authenticated attempt and returns the full response body.
func (c *Client) send(ctx context.Context, scope, method string, u *url.URL, payload []byte) (*http.Response, []byte, error) {
	token, err := c.auth.Token(ctx, scope)
	if err != nil {
		return nil, nil, err
	}
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("youtrack: %s %s: %w", method, u.Path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("youtrack: read response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.auth.Invalidate(scope, token)
	}
	return resp, data, nil
}

// Get is Do with GET.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	return c.Do(ctx, http.MethodGet, path, query, nil, out)
}

// Post is Do with POST.
func (c *Client) Post(ctx context.Context, path string, query url.Values, body, out any) error {
	return c.Do(ctx, http.MethodPost, path, query, body, out)
}

// Delete is Do with DELETE.
func (c *Client) Delete(ctx context.Context, path string, query url.Values) error {
	return c.Do(ctx, http.MethodDelete, path, query, nil, nil)
}

// Fields builds the common ?fields=... query.
func Fields(fields string) url.Values {
	q := url.Values{}
	if fields != "" {
		q.Set("fields", fields)
	}
	return q
}
