package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
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

// youTrackOn is a YouTrack of one profile on the API of mux.
func youTrackOn(t *testing.T, mux *http.ServeMux) *YouTrack {
	t.Helper()
	srv := youtracktest.New(t, mux)
	return &YouTrack{Load: func(p string) (config.Config, error) {
		return config.Config{Name: p, URL: srv.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"}, nil
	}}
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

// TestYouTrackFields: without admin rights the fields come from recent
// issues, with their types, multiple values and allowed values.
func TestYouTrackFields(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/admin/projects/BE/customFields", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 403, `{"error":"Forbidden"}`)
	})
	mux.HandleFunc("GET /api/issues", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("query"); q != "project: {BE} sort by: updated desc" {
			t.Errorf("query = %q", q)
		}
		if f := r.URL.Query().Get("fields"); !strings.Contains(f, "fieldType(id)") {
			t.Errorf("fields = %q", f)
		}
		reply(w, 200, `[
			{"customFields":[
				{"projectCustomField":{"$type":"EnumProjectCustomField","field":{"name":"flow","fieldType":{"id":"enum[*]"}},"bundle":{"values":[{"name":"frontend"},{"name":"old","archived":true}]}}},
				{"projectCustomField":{"$type":"StateProjectCustomField","field":{"name":"Stage","fieldType":{"id":"state[1]"}},"bundle":{"values":[{"name":"В работе"}]}}},
				{"projectCustomField":{"$type":"UserProjectCustomField","field":{"name":"Assignee","fieldType":{"id":"user[1]"}},"bundle":{"values":[{"name":"Team"}]}}},
				{"projectCustomField":{"$type":"SimpleProjectCustomField","field":{"name":"Story points","fieldType":{"id":"integer"}}}},
				{"projectCustomField":{"$type":"PeriodProjectCustomField","field":{"name":"Estimation","fieldType":{"id":"period"}}}}
			]},
			{"customFields":[
				{"projectCustomField":{"$type":"EnumProjectCustomField","field":{"name":"flow","fieldType":{"id":"enum[*]"}},"bundle":{"values":[{"name":"frontend"}]}}}
			]}
		]`)
	})
	got, err := youTrackOn(t, mux).Fields(t.Context(), "p", "BE")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got {
		names = append(names, fmt.Sprintf("%s:%s:%v:%s", f.Name, f.Type, f.Multi, strings.Join(f.Values, "/")))
	}
	want := []string{"flow:enum:true:frontend", "Stage:state:false:В работе", "Assignee:user:false:",
		"Story points:integer:false:", "Estimation:period:false:"}
	if !slices.Equal(names, want) {
		t.Errorf("fields %q, want %q", names, want)
	}
}

// TestYouTrackApplyAndMatches: a change is a command on the issue; the
// source query narrowed to the issue tells whether it still matches.
func TestYouTrackApplyAndMatches(t *testing.T) {
	var mu sync.Mutex
	var commands, queries []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/commands", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query  string              `json:"query"`
			Issues []map[string]string `json:"issues"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		commands = append(commands, fmt.Sprintf("%s %v", body.Query, body.Issues))
		reply(w, 200, `{}`)
	})
	mux.HandleFunc("GET /api/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		if strings.Contains(q, "tag: ai") {
			reply(w, 200, `[{"id":"2-1","idReadable":"BE-1"}]`)
			return
		}
		reply(w, 200, `[]`)
	})
	yt := youTrackOn(t, mux)
	ctx := t.Context()
	if err := yt.Apply(ctx, "p", "2-1", "Stage {В работе}"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(commands) != 1 || commands[0] != "Stage {В работе} [map[id:2-1]]" {
		t.Errorf("commands %q", commands)
	}
	mu.Unlock()
	if ok, err := yt.Matches(ctx, "p", "BE-1", "project: BE tag: ai"); err != nil || !ok {
		t.Errorf("matches %v (%v), want true", ok, err)
	}
	if ok, err := yt.Matches(ctx, "p", "BE-1", "project: BE #Unresolved"); err != nil || ok {
		t.Errorf("matches %v (%v), want false", ok, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if queries[0] != "(project: BE tag: ai) and issue id: BE-1" {
		t.Errorf("query %q", queries[0])
	}
}
