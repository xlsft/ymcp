package tui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
	"github.com/xlsft/ymcp/internal/youtrack/youtracktest"
)

// fakeYouTrackAPI serves one issue with its comments.
type fakeYouTrackAPI struct {
	mu       sync.Mutex
	comments []map[string]any
	posted   []string
	queries  []string
}

func (a *fakeYouTrackAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/issues/2-1":
		io.WriteString(w, `{"id":"2-1","idReadable":"BE-1","summary":"Fix login","description":"The cookie expires.","customFields":[]}`)
	case r.Method == http.MethodGet && r.URL.Path == "/api/issues/2-1/comments":
		a.queries = append(a.queries, r.URL.RawQuery)
		json.NewEncoder(w).Encode(a.comments)
	case r.Method == http.MethodPost && r.URL.Path == "/api/issues/2-1/comments":
		var body struct{ Text string }
		json.NewDecoder(r.Body).Decode(&body)
		a.posted = append(a.posted, body.Text)
		c := map[string]any{"id": "4-9", "text": body.Text, "created": int64(1_800_000_000_000), "author": map[string]string{"login": youtracktest.Login, "fullName": "Alice"}}
		a.comments = append(a.comments, c)
		json.NewEncoder(w).Encode(c)
	default:
		http.NotFound(w, r)
	}
}

func newYouTrack(t *testing.T) (*YouTrack, *fakeYouTrackAPI) {
	t.Helper()
	api := &fakeYouTrackAPI{comments: []map[string]any{
		{"id": "4-2", "text": "second", "created": int64(1_700_000_002_000), "updated": int64(1_700_000_009_000), "author": map[string]string{"login": "anna", "fullName": "Anna"}},
		{"id": "4-1", "text": "first", "created": int64(1_700_000_001_000), "author": map[string]string{"login": "bob"}},
		{"id": "4-3", "deleted": true, "created": int64(1_700_000_003_000)},
	}}
	srv := youtracktest.New(t, api)
	loads := 0
	yt := &YouTrack{Load: func(p string) (config.Config, error) {
		loads++
		if loads > 1 {
			t.Errorf("profile %s loaded %d times", p, loads)
		}
		return config.Config{Name: p, URL: srv.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"}, nil
	}}
	return yt, api
}

func TestYouTrackIssueAndComments(t *testing.T) {
	yt, api := newYouTrack(t)
	ctx := context.Background()
	is, err := yt.Issue(ctx, "p", "2-1")
	if err != nil || is.Key != "BE-1" || is.Description != "The cookie expires." {
		t.Fatalf("issue %+v (%v)", is, err)
	}
	cs, err := yt.Comments(ctx, "p", "2-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Text != "first" || cs[1].Text != "second" {
		t.Fatalf("comments %+v", cs)
	}
	if cs[1].Author != "anna" || cs[1].AuthorName != "Anna" || cs[1].Updated.IsZero() || !cs[0].Updated.IsZero() {
		t.Errorf("second comment %+v", cs[1])
	}
	if q := api.queries[0]; !strings.Contains(q, "author%28login%2CfullName%29") || !strings.Contains(q, "%24top=200") {
		t.Errorf("query %s", q)
	}

	c, err := yt.AddComment(ctx, "p", "2-1", "/approve")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "4-9" || c.Author != youtracktest.Login || c.Text != "/approve" || !c.Created.Equal(time.UnixMilli(1_800_000_000_000)) {
		t.Errorf("posted %+v", c)
	}
	if len(api.posted) != 1 || api.posted[0] != "/approve" {
		t.Errorf("server got %q", api.posted)
	}
}

func TestYouTrackUnconfiguredProfile(t *testing.T) {
	yt := &YouTrack{Load: func(p string) (config.Config, error) { return config.Config{Name: p}, nil }}
	if _, err := yt.Comments(context.Background(), "p", "2-1"); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("err %v", err)
	}
}

type fakeStrategy struct {
	vcs.Strategy
	comments []vcs.Comment
	numbers  []int
}

func (s *fakeStrategy) Comments(_ context.Context, n int, _ vcs.Cursor) ([]vcs.Comment, vcs.Cursor, error) {
	s.numbers = append(s.numbers, n)
	return s.comments, nil, nil
}

func TestCodeHostComments(t *testing.T) {
	t1 := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s := &fakeStrategy{comments: []vcs.Comment{{ID: "b", Created: t1.Add(time.Minute)}, {ID: "a", Created: t1}}}
	made := 0
	h := &CodeHost{New: func(_ context.Context, kind vcs.Kind, repo string) (vcs.Strategy, error) {
		made++
		if kind != vcs.KindGitHubPR || repo != "github.com/acme/backend" {
			t.Errorf("strategy %s %s", kind, repo)
		}
		return s, nil
	}}
	run := store.Run{Number: 3, VCSKind: "github-pr", VCSRepo: "github.com/acme/backend", PRNumber: 40}
	for range 2 {
		cs, err := h.Comments(context.Background(), run)
		if err != nil || len(cs) != 2 || cs[0].ID != "a" {
			t.Fatalf("comments %+v (%v)", cs, err)
		}
	}
	if made != 1 || len(s.numbers) != 2 || s.numbers[0] != 40 {
		t.Errorf("made %d strategies, read %v", made, s.numbers)
	}
	if _, err := h.Comments(context.Background(), store.Run{Number: 4, VCSKind: "branch"}); err == nil {
		t.Error("a branch run has comments")
	}
	h.New = func(context.Context, vcs.Kind, string) (vcs.Strategy, error) { return nil, errors.New("no token") }
	if _, err := h.Comments(context.Background(), store.Run{Number: 5, VCSKind: "gitlab-mr", VCSRepo: "gitlab.com/a/b", PRNumber: 1}); err == nil {
		t.Error("strategy error lost")
	}
}
