// Package youtracktest provides a fake YouTrack + Hub server for tests.
package youtracktest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const (
	ServiceID = "svc-1"
	Login     = "alice"
	Password  = "s3cret"
)

// Server emulates the parts of YouTrack and Hub the client relies on: the
// public config, the interactive login form and the implicit OAuth flow.
// Authenticated /api/ requests are passed to the API handler.
type Server struct {
	*httptest.Server
	api http.Handler

	mu       sync.Mutex
	sessions map[string]bool
	tokens   map[string]bool
	logins   int // password logins
	issued   int // access tokens
}

// New starts a fake instance; it is closed when the test ends.
func New(t testing.TB, api http.Handler) *Server {
	t.Helper()
	f := &Server{api: api, sessions: map[string]bool{}, tokens: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ring":{"url":"/hub","serviceId":%q}}`, ServiceID)
	})
	mux.HandleFunc("POST /hub/api/rest/oauth2/interactive/login", f.handleLogin)
	mux.HandleFunc("GET /hub/api/rest/oauth2/auth", f.handleAuth)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if !f.validToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"Unauthorized","error_description":"Invalid token"}`))
			return
		}
		f.api.ServeHTTP(w, r)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	q := r.URL.Query()
	if r.PostForm.Get("username") != Login || r.PostForm.Get("password") != Password {
		http.Redirect(w, r, "/hub/auth/login?message=hub-auth-credentials-bad&developer_message=Bad+credentials", http.StatusSeeOther)
		return
	}
	f.mu.Lock()
	f.logins++
	sid := fmt.Sprintf("session-%d", f.logins)
	f.sessions[sid] = true
	f.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: sid, Path: "/hub"})
	q.Set("request_credentials", "skip")
	http.Redirect(w, r, "/hub/api/rest/oauth2/auth?"+q.Encode(), http.StatusSeeOther)
}

func (f *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != ServiceID || q.Get("response_type") != "token" {
		http.Error(w, "bad client", http.StatusBadRequest)
		return
	}
	frag := url.Values{"state": {q.Get("state")}}
	c, err := r.Cookie("JSESSIONID")
	f.mu.Lock()
	if err == nil && f.sessions[c.Value] {
		f.issued++
		tok := fmt.Sprintf("tok-%d", f.issued)
		f.tokens[tok] = true
		frag.Set("access_token", tok)
		frag.Set("token_type", "Bearer")
		frag.Set("expires_in", "3600")
	} else {
		frag.Set("error", "access_denied")
		frag.Set("error_description", "Logged in user is banned")
	}
	f.mu.Unlock()
	http.Redirect(w, r, q.Get("redirect_uri")+"#"+frag.Encode(), http.StatusFound)
}

func (f *Server) validToken(tok string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[tok]
}

// RevokeTokens invalidates every issued access token, keeping sessions.
func (f *Server) RevokeTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = map[string]bool{}
}

// Counts reports how many password logins and access tokens happened.
func (f *Server) Counts() (logins, issued int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins, f.issued
}
