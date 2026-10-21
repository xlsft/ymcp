// Package youtracktest provides a fake YouTrack + Hub server for tests.
package youtracktest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	ServiceID = "svc-1"
	HubScope  = "0-0-0-0-0"
	Login     = "alice"
	Password  = "s3cret"
)

// Server emulates the parts of YouTrack and Hub the client relies on: the
// public config, the interactive login form and the implicit OAuth flow.
// Authenticated /api/ requests are passed to the API handler, and Hub REST
// requests (/hub/api/rest/) with a Hub-scoped token to HubAPI.
type Server struct {
	*httptest.Server
	api http.Handler

	// HubAPI serves /hub/api/rest/ apart from the OAuth endpoints; set it
	// before making requests. Nil answers 404.
	HubAPI http.Handler

	mu       sync.Mutex
	sessions map[string]bool
	tokens   map[string]string // token -> scope
	logins   int               // password logins
	issued   int               // access tokens
}

// New starts a fake instance; it is closed when the test ends.
func New(t testing.TB, api http.Handler) *Server {
	t.Helper()
	f := &Server{api: api, sessions: map[string]bool{}, tokens: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ring":{"url":"/hub","serviceId":%q}}`, ServiceID)
	})
	mux.HandleFunc("POST /hub/api/rest/oauth2/interactive/login", f.handleLogin)
	mux.HandleFunc("GET /hub/api/rest/oauth2/auth", f.handleAuth)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if !f.authorized(w, r, ServiceID) {
			return
		}
		f.api.ServeHTTP(w, r)
	})
	mux.HandleFunc("/hub/api/rest/", func(w http.ResponseWriter, r *http.Request) {
		if !f.authorized(w, r, HubScope) {
			return
		}
		if f.HubAPI == nil {
			http.NotFound(w, r)
			return
		}
		f.HubAPI.ServeHTTP(w, r)
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
		f.tokens[tok] = q.Get("scope")
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

// HasScope reports whether the request's token was issued for scope; a
// token may cover several, space-separated, as Hub allows.
func (f *Server) HasScope(r *http.Request, scope string) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	granted, ok := f.tokens[tok]
	return ok && slices.Contains(strings.Fields(granted), scope)
}

// authorized checks the bearer token is valid for scope, answering 401 otherwise.
func (f *Server) authorized(w http.ResponseWriter, r *http.Request, scope string) bool {
	ok := f.HasScope(r, scope)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Unauthorized","error_description":"Invalid token"}`))
	}
	return ok
}

// RevokeTokens invalidates every issued access token, keeping sessions.
func (f *Server) RevokeTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = map[string]string{}
}

// Counts reports how many password logins and access tokens happened.
func (f *Server) Counts() (logins, issued int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins, f.issued
}
