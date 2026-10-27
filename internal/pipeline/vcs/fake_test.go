package vcs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI is an httptest server whose routes answer JSON with ETags, and
// which records every request.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server
	mux *http.ServeMux

	mu   sync.Mutex
	reqs []recorded
}

type recorded struct {
	Method, Path, RawPath, Query, IfNoneMatch, Auth string
	Body                                            map[string]any
	At                                              time.Time
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, mux: http.NewServeMux()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{
			Method: r.Method, Path: r.URL.Path, RawPath: r.URL.EscapedPath(), Query: r.URL.RawQuery,
			IfNoneMatch: r.Header.Get("If-None-Match"), Auth: r.Header.Get("Authorization"), At: time.Now(),
		}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &rec.Body)
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, rec)
		f.mu.Unlock()
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) handle(pattern string, h http.HandlerFunc) { f.mux.HandleFunc(pattern, h) }

func (f *fakeAPI) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

// find returns the recorded requests with method and path.
func (f *fakeAPI) find(method, path string) []recorded {
	var out []recorded
	for _, r := range f.requests() {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeAPI) reset() {
	f.mu.Lock()
	f.reqs = nil
	f.mu.Unlock()
}

// writeJSON answers v with a strong ETag (weak when weak is set), or 304
// when the request already has it.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any, weak bool) {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	if weak {
		etag = "W/" + etag
	}
	if r.Method == http.MethodGet && status == http.StatusOK {
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// fast removes the write spacing so tests don't wait.
func fast(s Strategy) Strategy {
	switch v := s.(type) {
	case *github:
		v.c.gap = 0
	case *gitlab:
		v.c.gap = 0
	}
	return s
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func ids(cs []Comment) string {
	var s []string
	for _, c := range cs {
		s = append(s, c.ID)
	}
	return strings.Join(s, ",")
}
