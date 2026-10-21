package youtrack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/youtrack/youtracktest"
)

func newTestClient(t *testing.T, api http.HandlerFunc) (*Client, *youtracktest.Server) {
	t.Helper()
	f := youtracktest.New(t, api)
	c, err := New(Options{BaseURL: f.URL + "/api/", Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func meHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"login":"alice"}`))
}

func TestDoLogsInAndDecodes(t *testing.T) {
	c, f := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/users/me" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("fields"); got != "login" {
			t.Errorf("fields = %q", got)
		}
		meHandler(w, r)
	})

	var me struct{ Login string }
	if err := c.Get(context.Background(), "users/me", Fields("login"), &me); err != nil {
		t.Fatal(err)
	}
	if me.Login != "alice" {
		t.Errorf("login = %q", me.Login)
	}

	var raw json.RawMessage
	if err := c.Get(context.Background(), "/users/me", Fields("login"), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"login":"alice"}` {
		t.Errorf("raw = %s", raw)
	}
	if logins, issued := f.Counts(); logins != 1 || issued != 1 {
		t.Errorf("logins=%d issued=%d, want one of each (token reused)", logins, issued)
	}
}

func TestBadCredentials(t *testing.T) {
	f := youtracktest.New(t, http.HandlerFunc(meHandler))
	c, err := New(Options{BaseURL: f.URL, Login: youtracktest.Login, Password: "wrong", Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Get(context.Background(), "users/me", nil, nil)
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("err = %v, want ErrNotAuthenticated", err)
	}
}

func TestExpiredTokenRefreshedFromSession(t *testing.T) {
	c, f := newTestClient(t, meHandler)
	now := time.Now()
	c.auth.now = func() time.Time { return now }

	for range 3 {
		if err := c.Get(context.Background(), "users/me", nil, nil); err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Hour)
	}
	if logins, issued := f.Counts(); logins != 1 || issued != 3 {
		t.Errorf("logins=%d issued=%d, want 1 login and a fresh token per call", logins, issued)
	}
}

func TestRevokedTokenRetried(t *testing.T) {
	c, f := newTestClient(t, meHandler)
	if err := c.Get(context.Background(), "users/me", nil, nil); err != nil {
		t.Fatal(err)
	}
	f.RevokeTokens()
	if err := c.Get(context.Background(), "users/me", nil, nil); err != nil {
		t.Fatalf("after revoke: %v", err)
	}
	if _, issued := f.Counts(); issued != 2 {
		t.Errorf("issued=%d, want 2", issued)
	}
}

func TestDoParsesError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"Not Found","error_description":"Entity with id X-1 not found"}`))
	})

	err := c.Get(context.Background(), "issues/X-1", nil, nil)
	var ytErr *Error
	if !errors.As(err, &ytErr) {
		t.Fatalf("err = %v, want *Error", err)
	}
	if ytErr.Status != 404 || ytErr.Description != "Entity with id X-1 not found" {
		t.Errorf("err = %+v", ytErr)
	}
	if want := "youtrack: GET /api/issues/X-1: 404 Not Found: Entity with id X-1 not found"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}

func TestDoRejectsNonJSON(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html>lander</html>`))
	})
	if err := c.Get(context.Background(), "users/me", nil, &struct{}{}); err == nil {
		t.Fatal("expected error for HTML response")
	}
}

func TestHubGetUsesHubScopedToken(t *testing.T) {
	c, f := newTestClient(t, meHandler)
	f.HubAPI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hub/api/rest/usergroups/g-1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"name":"devs"}`))
	})

	if err := c.Get(context.Background(), "users/me", nil, nil); err != nil {
		t.Fatal(err)
	}
	var g struct{ Name string }
	if err := c.HubGet(context.Background(), "usergroups/g-1", nil, &g); err != nil {
		t.Fatal(err)
	}
	if g.Name != "devs" {
		t.Errorf("group = %+v", g)
	}
	// Both scopes come from one login; each is cached and reused.
	if err := c.HubGet(context.Background(), "usergroups/g-1", nil, &g); err != nil {
		t.Fatal(err)
	}
	if logins, issued := f.Counts(); logins != 1 || issued != 2 {
		t.Errorf("logins=%d issued=%d, want 1 login and one token per scope", logins, issued)
	}
}

func TestErrorWithoutCode(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error_description":"Value is not allowed"}`))
	})
	err := c.Post(context.Background(), "issues/X-1", nil, map[string]any{}, nil)
	if want := "youtrack: POST /api/issues/X-1: 400: Value is not allowed"; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestDownloadAndUpload(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/files/12-1":
			if r.URL.Query().Get("sign") != "abc" {
				t.Errorf("sign = %q", r.URL.Query().Get("sign"))
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("hello"))
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues/X-1/attachments":
			f, h, err := r.FormFile("file")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(f)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"name":%q,"size":%d}]`, h.Filename, len(body))
		default:
			http.NotFound(w, r)
		}
	})

	data, ct, err := c.Download(context.Background(), "/api/files/12-1?sign=abc")
	if err != nil || string(data) != "hello" || ct != "text/plain" {
		t.Errorf("download = %q %q %v", data, ct, err)
	}

	var out []struct {
		Name string
		Size int
	}
	if err := c.Upload(context.Background(), "issues/X-1/attachments", nil, []File{{Name: "a.txt", Content: []byte("12345")}}, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "a.txt" || out[0].Size != 5 {
		t.Errorf("upload = %+v", out)
	}
}

// parkingProxy answers every proxied request like a domain parking page.
func parkingProxy(t *testing.T) (*url.URL, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("<html>lander</html>"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u, &hits
}

func TestEnvProxyThatMissesYouTrackIsBypassed(t *testing.T) {
	proxy, hits := parkingProxy(t)
	orig := envProxy
	envProxy = func(*http.Request) (*url.URL, error) { return proxy, nil }
	t.Cleanup(func() { envProxy = orig })

	f := youtracktest.New(t, http.HandlerFunc(meHandler))
	var logs bytes.Buffer
	c, err := New(Options{BaseURL: f.URL, Login: youtracktest.Login, Password: youtracktest.Password,
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	var me struct{ Login string }
	if err := c.Get(context.Background(), "users/me", nil, &me); err != nil {
		t.Fatal(err)
	}
	if me.Login != "alice" || hits.Load() != 1 {
		t.Errorf("login=%q proxy hits=%d, want alice and one probe through the proxy", me.Login, hits.Load())
	}
	if !strings.Contains(logs.String(), "connecting directly") {
		t.Errorf("fallback not logged: %s", logs.String())
	}
}

func TestExplicitProxyIsNotBypassed(t *testing.T) {
	proxy, _ := parkingProxy(t)
	f := youtracktest.New(t, http.HandlerFunc(meHandler))
	c, err := New(Options{BaseURL: f.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: proxy.String()})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), "users/me", nil, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want the proxy's 404", err)
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"yt.example.com":         "https://yt.example.com",
		" yt.example.com/ ":      "https://yt.example.com",
		"http://yt.local:8080/":  "http://yt.local:8080",
		"https://yt.example.com": "https://yt.example.com",
		"":                       "",
	} {
		if got := NormalizeURL(in); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := New(Options{BaseURL: "yt.example.com", Login: "a", Password: "b"}); err != nil {
		t.Errorf("host-only URL rejected: %v", err)
	}
}
