package youtrack

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
	fakeServiceID = "svc-1"
	fakeLogin     = "alice"
	fakePassword  = "s3cret"
)

// fakeYouTrack emulates the parts of YouTrack and Hub the client relies on:
// the public config, the interactive login form and the implicit OAuth flow.
type fakeYouTrack struct {
	*httptest.Server
	api http.HandlerFunc

	mu       sync.Mutex
	sessions map[string]bool
	tokens   map[string]bool
	logins   int // password logins
	issued   int // access tokens
}

func newFakeYouTrack(t *testing.T, api http.HandlerFunc) *fakeYouTrack {
	t.Helper()
	f := &fakeYouTrack{api: api, sessions: map[string]bool{}, tokens: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ring":{"url":"/hub","serviceId":%q}}`, fakeServiceID)
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
		f.api(w, r)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeYouTrack) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	q := r.URL.Query()
	if r.PostForm.Get("username") != fakeLogin || r.PostForm.Get("password") != fakePassword {
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

func (f *fakeYouTrack) handleAuth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != fakeServiceID || q.Get("response_type") != "token" {
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

func (f *fakeYouTrack) validToken(tok string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[tok]
}

func (f *fakeYouTrack) revokeTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = map[string]bool{}
}

func (f *fakeYouTrack) counts() (logins, issued int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins, f.issued
}
