package youtrack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func newTestClient(t *testing.T, api http.HandlerFunc) (*Client, *fakeYouTrack) {
	t.Helper()
	f := newFakeYouTrack(t, api)
	c, err := New(Options{BaseURL: f.URL + "/api/", Login: fakeLogin, Password: fakePassword, Proxy: "none"})
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
	if logins, issued := f.counts(); logins != 1 || issued != 1 {
		t.Errorf("logins=%d issued=%d, want one of each (token reused)", logins, issued)
	}
}

func TestBadCredentials(t *testing.T) {
	f := newFakeYouTrack(t, meHandler)
	c, err := New(Options{BaseURL: f.URL, Login: fakeLogin, Password: "wrong", Proxy: "none"})
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
	if logins, issued := f.counts(); logins != 1 || issued != 3 {
		t.Errorf("logins=%d issued=%d, want 1 login and a fresh token per call", logins, issued)
	}
}

func TestRevokedTokenRetried(t *testing.T) {
	c, f := newTestClient(t, meHandler)
	if err := c.Get(context.Background(), "users/me", nil, nil); err != nil {
		t.Fatal(err)
	}
	f.revokeTokens()
	if err := c.Get(context.Background(), "users/me", nil, nil); err != nil {
		t.Fatalf("after revoke: %v", err)
	}
	if _, issued := f.counts(); issued != 2 {
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
