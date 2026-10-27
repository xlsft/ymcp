package vcs

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newGitHub(t *testing.T, f *fakeAPI, kind Kind) *github {
	t.Helper()
	s, err := New(kind, Repo{Host: "github.com", Owner: "o", Name: "r", Path: "o/r", Kind: KindGitHubPR, API: f.srv.URL}, Auth{Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	return fast(s).(*github)
}

// ghComments is a mutable list served with GitHub's since filter.
type ghComments struct {
	mu   sync.Mutex
	list []map[string]any
}

func (c *ghComments) add(m map[string]any) {
	c.mu.Lock()
	c.list = append(c.list, m)
	c.mu.Unlock()
}

func (c *ghComments) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []map[string]any{}
	since, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	for _, m := range c.list {
		u, _ := time.Parse(time.RFC3339, m["updated_at"].(string))
		if u.Before(since) {
			continue
		}
		out = append(out, m)
	}
	writeJSON(w, r, http.StatusOK, out, false)
}

func ghCommentJSON(id int, login, body, created, updated string) map[string]any {
	return map[string]any{
		"id": id, "user": map[string]any{"login": login, "type": "User"}, "body": body,
		"html_url":   fmt.Sprintf("https://github.com/o/r/pull/7#issuecomment-%d", id),
		"created_at": created, "updated_at": updated, "minimized": nil,
	}
}

func TestGitHubComments(t *testing.T) {
	f := newFakeAPI(t)
	conv := &ghComments{}
	conv.add(ghCommentJSON(1, "alice", "please rename", "2026-10-27T10:00:00Z", "2026-10-27T10:00:00Z"))
	conv.add(ghCommentJSON(2, "xlsft", "<!-- ymcp:agent pipeline=p run=1 msg=a -->\nagent", "2026-10-27T10:01:00Z", "2026-10-27T10:05:00Z"))
	hidden := ghCommentJSON(3, "spam", "buy", "2026-10-27T10:02:00Z", "2026-10-27T10:02:00Z")
	hidden["minimized"] = map[string]any{"is_minimized": true}
	conv.add(hidden)
	inline := &ghComments{}
	inline.add(map[string]any{
		"id": 100, "user": map[string]any{"login": "bob"}, "body": "nit", "html_url": "u100",
		"created_at": "2026-10-27T13:03:00+03:00", "updated_at": "2026-10-27T13:03:00+03:00",
		"path": "main.go", "line": 10, "original_line": 10, "diff_hunk": "@@ -1 +1 @@", "subject_type": "line",
		"pull_request_review_id": 9,
	})
	inline.add(map[string]any{
		"id": 101, "user": map[string]any{"login": "alice"}, "body": "+1", "html_url": "u101",
		"created_at": "2026-10-27T10:04:00Z", "updated_at": "2026-10-27T10:04:00Z",
		"path": "main.go", "line": 10, "in_reply_to_id": 100, "subject_type": "line",
	})
	inline.add(map[string]any{
		"id": 102, "user": map[string]any{"login": "bob"}, "body": "old", "html_url": "u102",
		"created_at": "2026-10-27T09:00:00Z", "updated_at": "2026-10-27T09:00:00Z",
		"path": "a.go", "line": nil, "original_line": 5, "position": nil, "subject_type": "line",
	})
	reviews := []map[string]any{
		{"id": 9, "user": map[string]any{"login": "bob"}, "body": "", "state": "COMMENTED", "submitted_at": "2026-10-27T10:03:00Z"},
		{"id": 10, "user": map[string]any{"login": "bob"}, "body": "needs tests", "state": "CHANGES_REQUESTED", "html_url": "rv10", "submitted_at": "2026-10-27T10:02:30Z"},
		{"id": 11, "user": map[string]any{"login": "xlsft"}, "body": "draft", "state": "PENDING"},
	}
	f.handle("GET /repos/o/r/issues/7/comments", conv.serve)
	f.handle("GET /repos/o/r/pulls/7/comments", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("sort") != "updated" || q.Get("direction") != "asc" || q.Get("per_page") != "100" {
			t.Errorf("review comments query = %s", r.URL.RawQuery)
		}
		inline.serve(w, r)
	})
	f.handle("GET /repos/o/r/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("since") {
			t.Error("reviews have no since")
		}
		writeJSON(w, r, http.StatusOK, reviews, false)
	})
	g := newGitHub(t, f, KindGitHubPR)
	ctx := ctxT(t)

	got, cur, err := g.Comments(ctx, 7, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "discussion_r102,issuecomment-1,issuecomment-2,pullrequestreview-10,discussion_r100,discussion_r101"; ids(got) != want {
		t.Fatalf("ids = %s, want %s", ids(got), want)
	}
	byID := map[string]Comment{}
	for _, c := range got {
		byID[c.ID] = c
	}
	if c := byID["discussion_r100"]; c.Created.Location() != time.UTC || c.Created.Hour() != 10 || c.File != "main.go" || c.Line != 10 ||
		c.Hunk != "@@ -1 +1 @@" || c.ThreadID != "discussion_r100" || c.Kind != CommentReviewComment || c.Outdated {
		t.Errorf("inline root = %+v", c)
	}
	if c := byID["discussion_r101"]; c.ThreadID != "discussion_r100" {
		t.Errorf("reply thread = %q", c.ThreadID)
	}
	if c := byID["discussion_r102"]; !c.Outdated || c.Line != 5 {
		t.Errorf("outdated = %+v", c)
	}
	if c := byID["pullrequestreview-10"]; c.Kind != CommentReview || c.Body != "needs tests" || c.Author != "bob" {
		t.Errorf("review = %+v", c)
	}
	if c := byID["issuecomment-1"]; c.Kind != CommentConversation || c.Author != "alice" || c.URL == "" {
		t.Errorf("conversation = %+v", c)
	}
	if cur["since:conversation"] != "2026-10-27T10:05:00Z" || cur["since:review_comments"] != "2026-10-27T10:04:00Z" {
		t.Errorf("cursor = %v", cur)
	}

	// The next poll asks since the newest update minus the overlap.
	f.reset()
	got, cur, err = g.Comments(ctx, 7, cur)
	if err != nil {
		t.Fatal(err)
	}
	if ids(got) != "issuecomment-2,discussion_r101" {
		t.Errorf("overlap poll = %s", ids(got))
	}
	if q := f.find("GET", "/repos/o/r/issues/7/comments")[0].Query; !strings.Contains(q, "since=2026-10-27T10%3A04%3A58Z") {
		t.Errorf("since query = %s", q)
	}

	// Nothing changed: every list answers 304 and nothing comes back.
	f.reset()
	got, cur, err = g.Comments(ctx, 7, cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("unchanged poll = %s", ids(got))
	}
	for _, r := range f.requests() {
		if r.IfNoneMatch == "" {
			t.Errorf("%s %s without If-None-Match", r.Method, r.Path)
		}
		if r.Auth != "Bearer tok" {
			t.Errorf("auth header = %q", r.Auth)
		}
	}
	if len(f.requests()) != 3 {
		t.Errorf("%d requests, want 3", len(f.requests()))
	}

	// An edit shows up again.
	conv.mu.Lock()
	conv.list[0]["body"], conv.list[0]["updated_at"] = "please rename it", "2026-10-27T11:00:00Z"
	conv.mu.Unlock()
	got, _, err = g.Comments(ctx, 7, cur)
	if err != nil {
		t.Fatal(err)
	}
	if ids(got) != "issuecomment-1,issuecomment-2" || got[0].Body != "please rename it" || !got[0].Updated.Equal(time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("edit poll = %+v", got)
	}
}

func TestGitHubIssuePR(t *testing.T) {
	f := newFakeAPI(t)
	var mu sync.Mutex
	issueState := "open"
	f.handle("GET /repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 200, map[string]any{"default_branch": "main"}, false)
	})
	f.handle("GET /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("head") != "o:BE-1-fix" || r.URL.Query().Get("state") != "open" {
			t.Errorf("pulls query = %s", r.URL.RawQuery)
		}
		writeJSON(w, r, 200, []any{}, false)
	})
	f.handle("POST /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 201, map[string]any{"number": 41, "html_url": "https://github.com/o/r/issues/41", "state": "open"}, false)
	})
	var prBody string
	f.handle("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		reqs := f.find("POST", "/repos/o/r/pulls")
		b := reqs[len(reqs)-1].Body
		prBody = b["body"].(string)
		writeJSON(w, r, 201, map[string]any{"number": 42, "html_url": "https://github.com/o/r/pull/42", "state": "open", "draft": true, "body": prBody}, false)
	})
	f.handle("GET /repos/o/r/pulls/42", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 200, map[string]any{"number": 42, "state": "closed", "merged": true, "merged_at": "2026-10-27T12:00:00Z",
			"body": prBody, "base": map[string]any{"ref": "develop"}}, false)
	})
	f.handle("GET /repos/o/r/issues/41", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeJSON(w, r, 200, map[string]any{"number": 41, "state": issueState}, false)
	})
	f.handle("PATCH /repos/o/r/issues/41", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		issueState = "closed"
		mu.Unlock()
		writeJSON(w, r, 200, map[string]any{"number": 41, "state": "closed"}, false)
	})
	issueComments := &ghComments{}
	issueComments.add(ghCommentJSON(5, "carol", "on the issue", "2026-10-27T10:00:00Z", "2026-10-27T10:00:00Z"))
	issueComments.list[0]["html_url"] = "https://github.com/o/r/issues/41#issuecomment-5"
	f.handle("GET /repos/o/r/issues/41/comments", issueComments.serve)
	f.handle("GET /repos/o/r/issues/42/comments", (&ghComments{}).serve)
	f.handle("GET /repos/o/r/pulls/42/comments", (&ghComments{}).serve)
	f.handle("GET /repos/o/r/pulls/42/reviews", (&ghComments{}).serve)
	f.handle("POST /repos/o/r/issues/41/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 201, ghCommentJSON(6, "xlsft", "x", "2026-10-27T10:00:00Z", "2026-10-27T10:00:00Z"), false)
	})

	g := newGitHub(t, f, KindGitHubIssuePR)
	ctx := ctxT(t)
	ch, err := g.Open(ctx, OpenOptions{Branch: "BE-1-fix", Title: "BE-1 Fix", Body: "<!-- ymcp:agent pipeline=p run=1 msg=a -->\nbody", Draft: true,
		IssueTitle: "BE-1 Fix login", IssueBody: "mirror"})
	if err != nil {
		t.Fatal(err)
	}
	if ch.Number != 42 || ch.Issue != 41 || ch.State != StateOpen || !ch.Draft || !strings.HasSuffix(ch.IssueURL, "/o/r/issues/41") {
		t.Errorf("change = %+v", ch)
	}
	is := f.find("POST", "/repos/o/r/issues")[0].Body
	if is["title"] != "BE-1 Fix login" || is["body"] != "mirror" {
		t.Errorf("issue = %v", is)
	}
	pr := f.find("POST", "/repos/o/r/pulls")[0].Body
	if !strings.HasSuffix(pr["body"].(string), "\n\nCloses #41") || !strings.HasPrefix(pr["body"].(string), "<!-- ymcp:agent") ||
		pr["base"] != "main" || pr["head"] != "BE-1-fix" || pr["draft"] != true {
		t.Errorf("pull = %v", pr)
	}

	// A fresh strategy (a restarted runner) finds the issue from the PR body.
	g = newGitHub(t, f, KindGitHubIssuePR)
	got, cur, err := g.Comments(ctx, 42, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ids(got) != "issuecomment-5" || got[0].Kind != CommentIssue || cur["issue"] != "41" {
		t.Fatalf("comments = %+v, cursor %v", got, cur)
	}
	// A reply to an issue comment goes to the issue.
	if _, err := g.Reply(ctx, 42, got[0], "answer"); err != nil {
		t.Fatal(err)
	}
	body := f.find("POST", "/repos/o/r/issues/41/comments")[0].Body["body"].(string)
	if !strings.Contains(body, "> In reply to [@carol's comment](https://github.com/o/r/issues/41#issuecomment-5):\n> on the issue") {
		t.Errorf("reply body = %q", body)
	}

	// Merged into a non-default branch: Change closes the issue itself.
	ch, err = g.Change(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if ch.State != StateMerged || !ch.Merged || ch.Issue != 41 {
		t.Errorf("change = %+v", ch)
	}
	if p := f.find("PATCH", "/repos/o/r/issues/41"); len(p) != 1 || p[0].Body["state"] != "closed" {
		t.Errorf("issue close requests = %+v", p)
	}
	if _, err := g.Change(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if p := f.find("PATCH", "/repos/o/r/issues/41"); len(p) != 1 {
		t.Errorf("closed the issue %d times", len(p))
	}
}

func TestGitHubOpenExistingAndDraftFallback(t *testing.T) {
	f := newFakeAPI(t)
	existing := true
	f.handle("GET /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		if existing {
			writeJSON(w, r, 200, []any{map[string]any{"number": 3, "html_url": "h3", "state": "open"}}, false)
			return
		}
		writeJSON(w, r, 200, []any{}, false)
	})
	f.handle("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		reqs := f.find("POST", "/repos/o/r/pulls")
		if reqs[len(reqs)-1].Body["draft"] == true {
			writeJSON(w, r, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"message": "Draft pull requests are not supported in this repository."}}}, false)
			return
		}
		writeJSON(w, r, 201, map[string]any{"number": 4, "html_url": "h4", "state": "open"}, false)
	})
	g := newGitHub(t, f, KindGitHubPR)
	ctx := ctxT(t)
	ch, err := g.Open(ctx, OpenOptions{Branch: "b", Base: "main", Title: "t", Draft: true})
	if err != nil || ch.Number != 3 {
		t.Fatalf("existing: %+v %v", ch, err)
	}
	if n := len(f.find("POST", "/repos/o/r/pulls")); n != 0 {
		t.Errorf("created %d PRs next to an open one", n)
	}
	existing = false
	ch, err = g.Open(ctx, OpenOptions{Branch: "b", Base: "main", Title: "t", Draft: true})
	if err != nil || ch.Number != 4 || ch.Draft {
		t.Fatalf("draft fallback: %+v %v", ch, err)
	}
	if posts := f.find("POST", "/repos/o/r/pulls"); len(posts) != 2 || posts[1].Body["draft"] != false {
		t.Errorf("posts = %+v", posts)
	}
}

func TestGitHubChangeStates(t *testing.T) {
	f := newFakeAPI(t)
	prs := map[string]map[string]any{
		"1": {"number": 1, "state": "open", "draft": true, "merged": false, "merged_at": nil},
		"2": {"number": 2, "state": "closed", "merged": false, "merged_at": nil},
		"3": {"number": 3, "state": "closed", "merged_at": "2026-10-27T12:00:00Z"},
	}
	f.handle("GET /repos/o/r/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := prs[r.PathValue("n")]
		if !ok {
			writeJSON(w, r, 404, map[string]any{"message": "Not Found"}, false)
			return
		}
		writeJSON(w, r, 200, pr, false)
	})
	g := newGitHub(t, f, KindGitHubPR)
	ctx := ctxT(t)
	for n, want := range map[int]Change{
		1: {Number: 1, State: StateOpen, Draft: true},
		2: {Number: 2, State: StateClosed},
		3: {Number: 3, State: StateMerged, Merged: true},
	} {
		got, err := g.Change(ctx, n)
		if err != nil || got != want {
			t.Errorf("Change(%d) = %+v, %v; want %+v", n, got, err, want)
		}
	}
	if _, err := g.Change(ctx, 9); err == nil {
		t.Error("Change(9) of a missing PR: no error")
	}
}

func TestGitHubClose(t *testing.T) {
	f := newFakeAPI(t)
	prs := map[string]map[string]any{
		"1": {"number": 1, "state": "open", "merged": false, "body": "x\n\nCloses #7"},
		"2": {"number": 2, "state": "closed", "merged": false},
		"3": {"number": 3, "state": "closed", "merged_at": "2026-10-27T12:00:00Z", "body": "Closes #8"},
	}
	f.handle("GET /repos/o/r/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 200, prs[r.PathValue("n")], false)
	})
	f.handle("PATCH /repos/o/r/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 200, map[string]any{}, false)
	})
	f.handle("GET /repos/o/r/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 200, map[string]any{"number": 7, "state": "open"}, false)
	})
	f.handle("PATCH /repos/o/r/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 200, map[string]any{}, false)
	})
	g := newGitHub(t, f, KindGitHubIssuePR)
	ctx := ctxT(t)
	for _, n := range []int{1, 2, 3} {
		if err := g.Close(ctx, n); err != nil {
			t.Fatalf("Close(%d): %v", n, err)
		}
	}
	closes := f.find("PATCH", "/repos/o/r/pulls/1")
	if len(closes) != 1 || closes[0].Body["state"] != "closed" {
		t.Errorf("PR closes = %+v", closes)
	}
	if n := len(f.find("PATCH", "/repos/o/r/pulls/2")) + len(f.find("PATCH", "/repos/o/r/pulls/3")); n != 0 {
		t.Errorf("%d writes to closed or merged PRs", n)
	}
	issue := f.find("PATCH", "/repos/o/r/issues/7")
	if len(issue) != 1 || issue[0].Body["state"] != "closed" || issue[0].Body["state_reason"] != "not_planned" {
		t.Errorf("issue closes = %+v", issue)
	}
	if n := len(f.find("PATCH", "/repos/o/r/issues/8")); n != 0 {
		t.Error("the issue of a merged PR was closed as not planned")
	}
}

func TestGitHubReplyEdit(t *testing.T) {
	f := newFakeAPI(t)
	var mu sync.Mutex
	bodies := map[string]string{} // issue comment id → body
	next := 500
	f.handle("POST /repos/o/r/pulls/7/comments/{id}/replies", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "404" {
			writeJSON(w, r, 404, map[string]any{"message": "Not Found"}, false)
			return
		}
		root, _ := strconv.Atoi(r.PathValue("id"))
		writeJSON(w, r, 201, map[string]any{"id": 900, "in_reply_to_id": root, "body": "x", "path": "a.go", "line": 3,
			"created_at": "2026-10-27T10:00:00Z", "updated_at": "2026-10-27T10:00:00Z"}, false)
	})
	f.handle("POST /repos/o/r/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		reqs := f.find("POST", "/repos/o/r/issues/7/comments")
		mu.Lock()
		next++
		id := strconv.Itoa(next)
		bodies[id] = reqs[len(reqs)-1].Body["body"].(string)
		mu.Unlock()
		writeJSON(w, r, 201, ghCommentJSON(next, "xlsft", bodies[id], "2026-10-27T10:00:00Z", "2026-10-27T10:00:00Z"), false)
	})
	f.handle("GET /repos/o/r/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		b, ok := bodies[r.PathValue("id")]
		mu.Unlock()
		if !ok {
			writeJSON(w, r, 404, map[string]any{"message": "Not Found"}, false)
			return
		}
		id, _ := strconv.Atoi(r.PathValue("id"))
		writeJSON(w, r, 200, ghCommentJSON(id, "xlsft", b, "2026-10-27T10:00:00Z", "2026-10-27T10:00:00Z"), false)
	})
	f.handle("PATCH /repos/o/r/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		reqs := f.find("PATCH", "/repos/o/r/issues/comments/"+r.PathValue("id"))
		mu.Lock()
		bodies[r.PathValue("id")] = reqs[len(reqs)-1].Body["body"].(string)
		mu.Unlock()
		id, _ := strconv.Atoi(r.PathValue("id"))
		writeJSON(w, r, 200, ghCommentJSON(id, "xlsft", bodies[r.PathValue("id")], "2026-10-27T10:00:00Z", "2026-10-27T10:09:00Z"), false)
	})
	f.handle("PATCH /repos/o/r/pulls/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "404" {
			writeJSON(w, r, 404, map[string]any{"message": "Not Found"}, false)
			return
		}
		writeJSON(w, r, 200, map[string]any{"id": 900, "in_reply_to_id": 100, "body": "y",
			"created_at": "2026-10-27T10:00:00Z", "updated_at": "2026-10-27T10:00:00Z"}, false)
	})
	g := newGitHub(t, f, KindGitHubPR)
	ctx := ctxT(t)
	header := "<!-- ymcp:agent pipeline=p run=1 msg=a -->\n@xlsft's agent #1 (p)\n\n"

	// A line comment reply goes to the thread root.
	c, err := g.Reply(ctx, 7, Comment{ID: "discussion_r101", ThreadID: "discussion_r100", Kind: CommentReviewComment}, header+"🧠 Thinking...")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "discussion_r900" || c.ThreadID != "discussion_r100" || c.Kind != CommentReviewComment {
		t.Errorf("thread reply = %+v", c)
	}
	if len(f.find("POST", "/repos/o/r/pulls/7/comments/100/replies")) != 1 {
		t.Error("reply didn't go to the root comment")
	}
	if _, err := g.Edit(ctx, c, header+"done"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Edit(ctx, Comment{ID: "discussion_r404"}, "x"); !errors.Is(err, ErrGone) {
		t.Errorf("edit of a deleted line comment: %v", err)
	}

	// A review can't be answered in place: the reply quotes and links it.
	review := Comment{ID: "pullrequestreview-10", Kind: CommentReview, Author: "bob", URL: "https://github.com/o/r/pull/7#pullrequestreview-10",
		Body: "needs tests\nand docs\nthird\nfourth"}
	c, err = g.Reply(ctx, 7, review, header+"🧠 Thinking...")
	if err != nil {
		t.Fatal(err)
	}
	wantRef := "> In reply to [@bob's review](https://github.com/o/r/pull/7#pullrequestreview-10):\n> needs tests\n> and docs\n> third\n> …"
	if c.Kind != CommentConversation || bodies["501"] != header+wantRef+"\n\n🧠 Thinking..." {
		t.Errorf("fallback reply = %+v, body %q", c, bodies["501"])
	}
	// The edit sends the whole body and keeps the reference.
	if _, err := g.Edit(ctx, Comment{ID: c.ID}, header+"answer"); err != nil {
		t.Fatal(err)
	}
	if bodies["501"] != header+wantRef+"\n\nanswer" {
		t.Errorf("edited body = %q", bodies["501"])
	}

	// A deleted thread root falls back to the conversation.
	c, err = g.Reply(ctx, 7, Comment{ID: "discussion_r404", ThreadID: "discussion_r404", Kind: CommentReviewComment, Author: "bob"}, "x")
	if err != nil || c.Kind != CommentConversation {
		t.Errorf("fallback after 404 = %+v, %v", c, err)
	}

	// Editing a deleted placeholder: ErrGone.
	if _, err := g.Edit(ctx, Comment{ID: "issuecomment-77"}, header+"x"); !errors.Is(err, ErrGone) {
		t.Errorf("edit of a deleted comment: %v", err)
	}
	if _, err := g.Edit(ctx, Comment{ID: "pullrequestreview-1"}, "x"); err == nil {
		t.Error("editing a review: no error")
	}
	if _, err := g.Edit(ctx, Comment{ID: "!1#note_2"}, "x"); err == nil {
		t.Error("editing a GitLab ID on GitHub: no error")
	}

	// Writes are spaced out.
	gw := newGitHub(t, f, KindGitHubPR)
	gw.c.gap = 150 * time.Millisecond
	f.reset()
	for range 3 {
		if _, err := gw.Post(ctx, 7, "x"); err != nil {
			t.Fatal(err)
		}
	}
	posts := f.find("POST", "/repos/o/r/issues/7/comments")
	for i := 1; i < len(posts); i++ {
		if d := posts[i].At.Sub(posts[i-1].At); d < 140*time.Millisecond {
			t.Errorf("writes %d and %d only %v apart", i-1, i, d)
		}
	}
}

func TestNewChecks(t *testing.T) {
	if _, err := New("svn", Repo{}, Auth{}); err == nil {
		t.Error("unknown kind accepted")
	}
	if _, err := New(KindGitHubPR, Repo{Host: "gitlab.com", Owner: "a", Name: "b", Path: "a/b", Kind: KindGitLabMR}, Auth{}); err == nil {
		t.Error("github-pr on a GitLab host accepted")
	}
	if _, err := New(KindGitHubPR, Repo{Host: "github.com", Owner: "a/b", Name: "c", Path: "a/b/c"}, Auth{}); err == nil {
		t.Error("github-pr with a nested path accepted")
	}
	if _, err := New(KindGitLabMR, Repo{Host: "github.com", Owner: "a", Name: "b", Path: "a/b", Kind: KindGitHubPR}, Auth{}); err == nil {
		t.Error("gitlab-mr on a GitHub host accepted")
	}
	// A self-hosted GitLab that ParseRemote couldn't recognise.
	s, err := New(KindGitLabMR, Repo{Host: "code.example.com", Owner: "a", Name: "b", Path: "a/b", Kind: KindBranch}, Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if api := s.(*gitlab).c.base.String(); api != "https://code.example.com/api/v4" {
		t.Errorf("api = %s", api)
	}
}

func TestBranch(t *testing.T) {
	s, err := New(KindBranch, Repo{}, Auth{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := ctxT(t)
	if ch, err := s.Open(ctx, OpenOptions{Branch: "b"}); err != nil || ch != (Change{}) {
		t.Errorf("Open = %+v, %v", ch, err)
	}
	if cs, cur, err := s.Comments(ctx, 0, Cursor{"a": "b"}); err != nil || len(cs) != 0 || cur["a"] != "b" {
		t.Errorf("Comments = %v, %v, %v", cs, cur, err)
	}
	if me, err := s.Me(ctx); err != nil || me != "" {
		t.Errorf("Me = %q, %v", me, err)
	}
	if _, err := s.Post(ctx, 1, "x"); !errors.Is(err, ErrNoChange) {
		t.Errorf("Post = %v", err)
	}
	if err := s.Close(ctx, 1); !errors.Is(err, ErrNoChange) {
		t.Errorf("Close = %v", err)
	}
	if s.Kind() != KindBranch {
		t.Error("kind")
	}
}
