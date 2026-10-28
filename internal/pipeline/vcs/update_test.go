package vcs

import (
	"net/http"
	"testing"
)

func TestGitHubUpdate(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("GET /repos/o/r/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 200, map[string]any{"number": 9, "state": "open", "body": "old\n\nCloses #4"}, false)
	})
	f.handle("PATCH /repos/o/r/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 200, map[string]any{"number": 9, "state": "open", "title": "fix: x", "body": "new\n\nCloses #4"}, false)
	})
	g := newGitHub(t, f, KindGitHubIssuePR)
	ch, err := g.Update(ctxT(t), 9, "fix: x", "new")
	if err != nil || ch.Title != "fix: x" || ch.Body != "new\n\nCloses #4" || ch.Issue != 4 {
		t.Fatalf("update = %+v, %v", ch, err)
	}
	in := f.find("PATCH", "/repos/o/r/pulls/9")[0].Body
	if in["title"] != "fix: x" || in["body"] != "new\n\nCloses #4" {
		t.Errorf("sent %v", in)
	}
}

func TestGitLabUpdateKeepsDraft(t *testing.T) {
	f := newFakeAPI(t)
	f.handle("GET "+glProject+"/merge_requests/5", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 200, map[string]any{"iid": 5, "state": "opened", "draft": true, "title": "Draft: old"}, false)
	})
	f.handle("PUT "+glProject+"/merge_requests/5", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 200, map[string]any{"iid": 5, "state": "opened", "draft": true, "title": "Draft: fix: x", "description": "new"}, false)
	})
	l := newGitLab(t, f)
	ch, err := l.Update(ctxT(t), 5, "fix: x", "new")
	if err != nil || ch.Title != "Draft: fix: x" || ch.Body != "new" || !ch.Draft {
		t.Fatalf("update = %+v, %v", ch, err)
	}
	in := f.find("PUT", "/api/v4/projects/team/sub/proj/merge_requests/5")[0].Body
	if in["title"] != "Draft: fix: x" || in["description"] != "new" {
		t.Errorf("sent %v", in)
	}
}
