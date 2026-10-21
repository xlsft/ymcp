package youtrack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Options{BaseURL: srv.URL + "/api/", Token: "perm:test", Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDoSendsAuthAndDecodes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer perm:test" {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Path != "/api/users/me" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("fields"); got != "login" {
			t.Errorf("fields = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"login":"root"}`))
	})

	var me struct{ Login string }
	if err := c.Get(context.Background(), "users/me", Fields("login"), &me); err != nil {
		t.Fatal(err)
	}
	if me.Login != "root" {
		t.Errorf("login = %q", me.Login)
	}

	var raw json.RawMessage
	if err := c.Get(context.Background(), "/users/me", Fields("login"), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"login":"root"}` {
		t.Errorf("raw = %s", raw)
	}
}

func TestDoParsesError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
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
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html>lander</html>`))
	})
	if err := c.Get(context.Background(), "users/me", nil, &struct{}{}); err == nil {
		t.Fatal("expected error for HTML response")
	}
}
