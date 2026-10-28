package vcs

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const glProject = "/api/v4/projects/{id}"

func newGitLab(t *testing.T, f *fakeAPI) *gitlab {
	t.Helper()
	s, err := New(KindGitLabMR, Repo{Host: "gitlab.example.ru", Owner: "team/sub", Name: "proj", Path: "team/sub/proj", Kind: KindGitLabMR,
		API: f.srv.URL + "/api/v4"}, Auth{Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	return fast(s).(*gitlab)
}

// checkProject fails the test unless the project id is the URL-encoded path.
func checkProject(t *testing.T, r *http.Request) {
	t.Helper()
	if r.PathValue("id") != "team/sub/proj" || !strings.Contains(r.URL.EscapedPath(), "/projects/team%2Fsub%2Fproj") {
		t.Errorf("project in %s", r.URL.EscapedPath())
	}
}

func glNoteJSON(id int, user, body, created, updated string) map[string]any {
	return map[string]any{"id": id, "body": body, "author": map[string]any{"username": user}, "created_at": created, "updated_at": updated,
		"system": false, "internal": false, "type": nil, "position": nil}
}

func TestGitLabComments(t *testing.T) {
	f := newFakeAPI(t)
	system := glNoteJSON(1, "alice", "added 1 commit", "2026-10-27T12:59:00.000+03:00", "2026-10-27T12:59:00.000+03:00")
	system["system"] = true
	diff := glNoteJSON(3, "bob", "rename this", "2026-10-27T13:01:00.000+03:00", "2026-10-27T13:01:00.000+03:00")
	diff["type"] = "DiffNote"
	diff["position"] = map[string]any{"new_path": "main.go", "old_path": "main.go", "new_line": 12, "old_line": nil}
	reply := glNoteJSON(4, "alice", "agreed", "2026-10-27T13:02:00.000+03:00", "2026-10-27T13:02:00.000+03:00")
	reply["type"] = "DiscussionNote"
	internal := glNoteJSON(6, "bob", "internal", "2026-10-27T13:04:00.000+03:00", "2026-10-27T13:04:00.000+03:00")
	internal["internal"] = true
	var mu sync.Mutex
	page1 := []map[string]any{
		{"id": "d1", "individual_note": true, "notes": []any{system}},
		{"id": "d2", "individual_note": true, "notes": []any{glNoteJSON(2, "alice", "top comment", "2026-10-27T13:00:00.000+03:00", "2026-10-27T13:00:00.000+03:00")}},
		{"id": "d3", "individual_note": false, "resolvable": true, "resolved": false, "notes": []any{diff, reply}},
	}
	agent := glNoteJSON(7, "itorshin", "<!-- ymcp:agent pipeline=p run=1 msg=a -->\nhi", "2026-10-27T13:05:00.000+03:00", "2026-10-27T13:05:00.000+03:00")
	page2 := []map[string]any{
		{"id": "d4", "individual_note": false, "resolvable": true, "resolved": true, "notes": []any{glNoteJSON(5, "bob", "done", "2026-10-27T13:03:00.000+03:00", "2026-10-27T13:03:00.000+03:00")}},
		{"id": "d5", "individual_note": true, "notes": []any{internal}},
		{"id": "d6", "individual_note": true, "notes": []any{agent}},
	}
	f.handle("GET "+glProject+"/merge_requests/5/discussions", func(w http.ResponseWriter, r *http.Request) {
		checkProject(t, r)
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
		}
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Query().Get("page") == "2" {
			w.Header().Set("X-Next-Page", "")
			w.Header().Set("X-Total-Pages", "2")
			writeJSON(w, r, 200, page2, true)
			return
		}
		w.Header().Set("X-Next-Page", "2")
		w.Header().Set("X-Total-Pages", "2")
		writeJSON(w, r, 200, page1, true)
	})
	l := newGitLab(t, f)
	ctx := ctxT(t)

	got, cur, err := l.Comments(ctx, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "!5#note_2,!5#note_3,!5#note_4,!5#note_7"; ids(got) != want {
		t.Fatalf("ids = %s, want %s", ids(got), want)
	}
	top, d3, d3reply := got[0], got[1], got[2]
	if top.Kind != CommentConversation || top.ThreadID != "d2" || top.Author != "alice" || top.Created.Location().String() != "UTC" ||
		top.Created.Hour() != 10 || top.URL != f.srv.URL+"/team/sub/proj/-/merge_requests/5#note_2" {
		t.Errorf("top = %+v", top)
	}
	if d3.Kind != CommentReviewComment || d3.File != "main.go" || d3.Line != 12 || d3.ThreadID != "d3" {
		t.Errorf("diff note = %+v", d3)
	}
	if d3reply.Kind != CommentReviewComment || d3reply.File != "main.go" || d3reply.Line != 12 {
		t.Errorf("diff reply = %+v", d3reply)
	}
	if cur["since:discussions"] != "2026-10-27T10:05:00Z" {
		t.Errorf("cursor = %v", cur)
	}

	// Unchanged: both pages answer 304 to their weak ETags.
	f.reset()
	got, cur, err = l.Comments(ctx, 5, cur)
	if err != nil || len(got) != 0 {
		t.Fatalf("unchanged poll = %v, %v", ids(got), err)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	for _, r := range reqs {
		if !strings.HasPrefix(r.IfNoneMatch, `W/"`) {
			t.Errorf("If-None-Match = %q", r.IfNoneMatch)
		}
	}

	// A new system note changes page 1, but the old notes there stay below
	// the since mark; an edit on page 2 comes back.
	mu.Lock()
	page1 = append(page1, map[string]any{"id": "d7", "individual_note": true, "notes": []any{system}})
	agent["body"], agent["updated_at"] = "edited", "2026-10-27T14:00:00.000+03:00"
	mu.Unlock()
	got, _, err = l.Comments(ctx, 5, cur)
	if err != nil {
		t.Fatal(err)
	}
	if ids(got) != "!5#note_7" || got[0].Body != "edited" {
		t.Errorf("poll after edit = %+v", got)
	}
}

func TestGitLabWrites(t *testing.T) {
	f := newFakeAPI(t)
	var mu sync.Mutex
	notes := map[string]string{"7": "<!-- ymcp:agent pipeline=p run=1 msg=a -->\nAgent #1 (p)\n\n> In reply to [@bob's comment](u):\n> hm\n\n🧠 Thinking..."}
	next := 100
	add := func(body string) map[string]any {
		mu.Lock()
		defer mu.Unlock()
		next++
		notes[strconv.Itoa(next)] = body
		return glNoteJSON(next, "itorshin", body, "2026-10-27T13:00:00Z", "2026-10-27T13:00:00Z")
	}
	lastBody := func(method, path string) string {
		reqs := f.find(method, path)
		return reqs[len(reqs)-1].Body["body"].(string)
	}
	f.handle("POST "+glProject+"/merge_requests/5/discussions/{d}/notes", func(w http.ResponseWriter, r *http.Request) {
		checkProject(t, r)
		if r.PathValue("d") == "gone" {
			writeJSON(w, r, 404, map[string]any{"message": "404 Discussion Not Found"}, false)
			return
		}
		writeJSON(w, r, 201, add(lastBody("POST", r.URL.Path)), false)
	})
	f.handle("POST "+glProject+"/merge_requests/5/discussions", func(w http.ResponseWriter, r *http.Request) {
		n := add(lastBody("POST", r.URL.Path))
		writeJSON(w, r, 201, map[string]any{"id": "newd", "individual_note": true, "notes": []any{n}}, false)
	})
	f.handle("GET "+glProject+"/merge_requests/5/notes/{n}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		b, ok := notes[r.PathValue("n")]
		mu.Unlock()
		if !ok {
			writeJSON(w, r, 404, map[string]any{"message": "404 Not found"}, false)
			return
		}
		id, _ := strconv.Atoi(r.PathValue("n"))
		writeJSON(w, r, 200, glNoteJSON(id, "itorshin", b, "2026-10-27T13:00:00Z", "2026-10-27T13:00:00Z"), false)
	})
	f.handle("PUT "+glProject+"/merge_requests/5/notes/{n}", func(w http.ResponseWriter, r *http.Request) {
		b := lastBody("PUT", r.URL.Path)
		mu.Lock()
		notes[r.PathValue("n")] = b
		mu.Unlock()
		id, _ := strconv.Atoi(r.PathValue("n"))
		writeJSON(w, r, 200, glNoteJSON(id, "itorshin", b, "2026-10-27T13:00:00Z", "2026-10-27T13:10:00+03:00"), false)
	})
	l := newGitLab(t, f)
	ctx := ctxT(t)

	c, err := l.Reply(ctx, 5, Comment{ID: "!5#note_3", ThreadID: "d3", Kind: CommentReviewComment}, "thinking")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "!5#note_101" || c.ThreadID != "d3" || c.Kind != CommentReviewComment {
		t.Errorf("reply = %+v", c)
	}
	// A thread that vanished: a new thread that links to the comment.
	c, err = l.Reply(ctx, 5, Comment{ID: "!5#note_9", ThreadID: "gone", Author: "bob", URL: "u9", Body: "why?"}, "answer")
	if err != nil {
		t.Fatal(err)
	}
	if c.ThreadID != "newd" || notes["102"] != "> In reply to [@bob's comment](u9):\n> why?\n\nanswer" {
		t.Errorf("fallback = %+v, body %q", c, notes["102"])
	}
	c, err = l.Post(ctx, 5, "hello")
	if err != nil || c.ID != "!5#note_103" || c.ThreadID != "newd" {
		t.Errorf("post = %+v, %v", c, err)
	}

	// Edit keeps the reference block and sends the whole body.
	c, err = l.Edit(ctx, Comment{ID: "!5#note_7", ThreadID: "d9"}, "<!-- ymcp:agent pipeline=p run=1 msg=a -->\nAgent #1 (p)\n\nanswer")
	if err != nil {
		t.Fatal(err)
	}
	if want := "<!-- ymcp:agent pipeline=p run=1 msg=a -->\nAgent #1 (p)\n\n> In reply to [@bob's comment](u):\n> hm\n\nanswer"; notes["7"] != want {
		t.Errorf("edited = %q", notes["7"])
	}
	if c.ThreadID != "d9" || c.Updated.Hour() != 10 || c.Updated.Location().String() != "UTC" {
		t.Errorf("edit result = %+v", c)
	}
	if _, err := l.Edit(ctx, Comment{ID: "!5#note_404"}, "x"); !errors.Is(err, ErrGone) {
		t.Errorf("edit of a deleted note: %v", err)
	}
	if _, err := l.Edit(ctx, Comment{ID: "issuecomment-1"}, "x"); err == nil {
		t.Error("edit with a GitHub ID: no error")
	}
}

func TestGitLabOpenChange(t *testing.T) {
	f := newFakeAPI(t)
	existing := false
	f.handle("GET "+glProject, func(w http.ResponseWriter, r *http.Request) {
		checkProject(t, r)
		writeJSON(w, r, 200, map[string]any{"default_branch": "master"}, false)
	})
	f.handle("GET "+glProject+"/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("source_branch") != "BE-1-x" || r.URL.Query().Get("state") != "opened" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		if existing {
			writeJSON(w, r, 200, []any{map[string]any{"iid": 3, "web_url": "w3", "state": "opened", "draft": true}}, false)
			return
		}
		writeJSON(w, r, 200, []any{}, false)
	})
	f.handle("POST "+glProject+"/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 201, map[string]any{"iid": 5, "web_url": "w5", "state": "opened", "draft": true}, false)
	})
	states := map[string]string{"1": "opened", "2": "closed", "3": "merged", "4": "locked"}
	f.handle("GET "+glProject+"/merge_requests/{iid}", func(w http.ResponseWriter, r *http.Request) {
		iid, _ := strconv.Atoi(r.PathValue("iid"))
		writeJSON(w, r, 200, map[string]any{"iid": iid, "state": states[r.PathValue("iid")], "merged_at": "2026-10-27T15:43:49.292+03:00"}, false)
	})
	f.handle("GET /api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 200, map[string]any{"id": 1, "username": "itorshin"}, false)
	})
	l := newGitLab(t, f)
	ctx := ctxT(t)

	ch, err := l.Open(ctx, OpenOptions{Branch: "BE-1-x", Title: "BE-1 Fix", Body: "b", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if ch != (Change{Number: 5, URL: "w5", State: StateOpen, Draft: true}) {
		t.Errorf("change = %+v", ch)
	}
	in := f.find("POST", "/api/v4/projects/team/sub/proj/merge_requests")[0].Body
	if in["title"] != "Draft: BE-1 Fix" || in["target_branch"] != "master" || in["source_branch"] != "BE-1-x" || in["description"] != "b" {
		t.Errorf("create = %v", in)
	}
	if _, squash := in["squash"]; in["remove_source_branch"] != false || squash {
		t.Errorf("merge options without asking: %v", in)
	}
	existing = true
	if ch, err := l.Open(ctx, OpenOptions{Branch: "BE-1-x", Title: "Draft: x", Draft: true}); err != nil || ch.Number != 3 {
		t.Errorf("existing = %+v, %v", ch, err)
	}
	if n := len(f.find("POST", "/api/v4/projects/team/sub/proj/merge_requests")); n != 1 {
		t.Errorf("%d creates", n)
	}
	// The merge options, asked for.
	existing = false
	if _, err := l.Open(ctx, OpenOptions{Branch: "BE-1-x", Title: "BE-1 Fix", Squash: true, DeleteBranch: true}); err != nil {
		t.Fatal(err)
	}
	creates := f.find("POST", "/api/v4/projects/team/sub/proj/merge_requests")
	if in := creates[len(creates)-1].Body; in["remove_source_branch"] != true || in["squash"] != true {
		t.Errorf("merge options = %v", in)
	}
	for iid, want := range map[int]string{1: StateOpen, 2: StateClosed, 3: StateMerged, 4: StateOpen} {
		ch, err := l.Change(ctx, iid)
		if err != nil || ch.State != want || ch.Merged != (want == StateMerged) {
			t.Errorf("Change(%d) = %+v, %v", iid, ch, err)
		}
	}
	if me, err := l.Me(ctx); err != nil || me != "itorshin" {
		t.Errorf("Me = %q, %v", me, err)
	}
	if me, _ := l.Me(ctx); me != "itorshin" || len(f.find("GET", "/api/v4/user")) != 1 {
		t.Error("Me isn't cached")
	}
}

func TestGitLabClose(t *testing.T) {
	f := newFakeAPI(t)
	states := map[string]string{"1": "opened", "2": "closed", "3": "merged"}
	f.handle("GET "+glProject+"/merge_requests/{iid}", func(w http.ResponseWriter, r *http.Request) {
		iid, _ := strconv.Atoi(r.PathValue("iid"))
		writeJSON(w, r, 200, map[string]any{"iid": iid, "state": states[r.PathValue("iid")]}, false)
	})
	f.handle("PUT "+glProject+"/merge_requests/{iid}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 200, map[string]any{"iid": 1, "state": "closed"}, false)
	})
	l := newGitLab(t, f)
	ctx := ctxT(t)
	for _, iid := range []int{1, 2, 3} {
		if err := l.Close(ctx, iid); err != nil {
			t.Fatalf("Close(%d): %v", iid, err)
		}
	}
	puts := f.find("PUT", "/api/v4/projects/team/sub/proj/merge_requests/1")
	if len(puts) != 1 || puts[0].Body["state_event"] != "close" {
		t.Errorf("closes = %+v", puts)
	}
	if n := len(f.find("PUT", "/api/v4/projects/team/sub/proj/merge_requests/2")) + len(f.find("PUT", "/api/v4/projects/team/sub/proj/merge_requests/3")); n != 0 {
		t.Errorf("%d writes to closed or merged MRs", n)
	}
}

func TestParseNoteID(t *testing.T) {
	if iid, n, ok := parseNoteID("!12#note_345"); !ok || iid != 12 || n != 345 {
		t.Errorf("parse = %d %d %v", iid, n, ok)
	}
	for _, s := range []string{"", "12#note_3", "!x#note_3", "!1#note_", "!0#note_1", "issuecomment-1"} {
		if _, _, ok := parseNoteID(s); ok {
			t.Errorf("parseNoteID(%q) ok", s)
		}
	}
}

// TestGitLabHide: a note is hidden by resolving its thread; a note outside
// a resolvable thread can't be hidden.
func TestGitLabHide(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("PUT "+glProject+"/merge_requests/{iid}/discussions/{d}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("d") == "flat" {
			writeJSON(w, r, 400, map[string]any{"message": "Discussion is not resolvable"}, false)
			return
		}
		writeJSON(w, r, 200, map[string]any{"id": r.PathValue("d")}, false)
	})
	l := newGitLab(t, f)
	ctx := ctxT(t)
	if err := l.Hide(ctx, Comment{ID: "!3#note_9", ThreadID: "abc"}); err != nil {
		t.Fatal(err)
	}
	puts := f.find("PUT", "/api/v4/projects/team/sub/proj/merge_requests/3/discussions/abc")
	if len(puts) != 1 || puts[0].Body["resolved"] != true {
		t.Errorf("resolves = %+v", puts)
	}
	if err := l.Hide(ctx, Comment{ID: "!3#note_10", ThreadID: "flat"}); !errors.Is(err, ErrCantHide) {
		t.Errorf("unresolvable: %v", err)
	}
	if err := l.Hide(ctx, Comment{ID: "!3#note_11"}); !errors.Is(err, ErrCantHide) {
		t.Errorf("no thread: %v", err)
	}
}
