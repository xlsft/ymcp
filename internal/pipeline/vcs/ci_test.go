package vcs

import (
	"net/http"
	"strings"
	"testing"
)

func TestGitHubChecks(t *testing.T) {
	f := newFakeAPI(t)
	runs := []any{
		map[string]any{"id": 11, "name": "test", "status": "completed", "conclusion": "failure", "html_url": "h11", "app": map[string]any{"slug": "github-actions"}},
		map[string]any{"id": 12, "name": "lint", "status": "completed", "conclusion": "success", "html_url": "h12", "app": map[string]any{"slug": "github-actions"}},
		map[string]any{"id": 13, "name": "codecov", "status": "completed", "conclusion": "skipped", "html_url": "h13", "app": map[string]any{"slug": "codecov"}},
	}
	statuses := []any{map[string]any{"context": "ci/jenkins", "state": "success", "target_url": "j"}}
	f.handle("GET /repos/o/r/commits/{sha}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("sha") != "abc" {
			t.Errorf("sha = %s", r.PathValue("sha"))
		}
		writeJSON(w, r, 200, map[string]any{"total_count": len(runs), "check_runs": runs}, false)
	})
	f.handle("GET /repos/o/r/commits/{sha}/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, 200, map[string]any{"state": "success", "statuses": statuses}, false)
	})
	f.handle("GET /repos/o/r/actions/jobs/11/logs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/blob/11.txt", http.StatusFound)
	})
	f.handle("GET /blob/11.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("setup\n" + strings.Repeat("noise\n", 50) + "--- FAIL: TestLogin\n"))
	})
	g := newGitHub(t, f, KindGitHubPR)
	ctx := ctxT(t)

	run, err := g.Checks(ctx, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != CIFailure || !strings.HasSuffix(run.URL, "/o/r/commit/abc/checks") {
		t.Errorf("run = %+v", run)
	}
	failed := run.Failed()
	if len(failed) != 1 || failed[0].Name != "test" || failed[0].ID != 11 || failed[0].URL != "h11" {
		t.Fatalf("failed = %+v", failed)
	}
	log, err := g.Log(ctx, failed[0], 40)
	if err != nil || !strings.HasSuffix(log, "--- FAIL: TestLogin\n") || len(log) > 40 || strings.HasPrefix(log, "setup") {
		t.Errorf("log = %q, %v", log, err)
	}
	if log, err := g.Log(ctx, CIJob{Name: "codecov"}, 40); log != "" || err != nil {
		t.Errorf("a job without a log = %q, %v", log, err)
	}

	// Still running, then nothing at all.
	runs[1] = map[string]any{"id": 12, "name": "lint", "status": "in_progress"}
	if run, _ := g.Checks(ctx, "abc"); run.State != CIPending {
		t.Errorf("running: %s", run.State)
	}
	runs, statuses = nil, nil
	if run, _ := g.Checks(ctx, "abc"); run.State != CINone {
		t.Errorf("nothing ran: %s", run.State)
	}
}

func TestGitLabChecks(t *testing.T) {
	f := newFakeAPI(t)
	pipelines := []any{map[string]any{"id": 7, "status": "failed", "web_url": "p7"}}
	f.handle("GET "+glProject+"/pipelines", func(w http.ResponseWriter, r *http.Request) {
		checkProject(t, r)
		if q := r.URL.Query(); q.Get("sha") != "abc" || q.Get("order_by") != "id" || q.Get("sort") != "desc" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		writeJSON(w, r, 200, pipelines, false)
	})
	f.handle("GET "+glProject+"/pipelines/7/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope[]") != "failed" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		writeJSON(w, r, 200, []any{
			map[string]any{"id": 70, "name": "commitlint", "status": "failed", "web_url": "j70"},
			map[string]any{"id": 71, "name": "audit", "status": "failed", "web_url": "j71", "allow_failure": true},
		}, false)
	})
	f.handle("GET "+glProject+"/jobs/70/trace", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("✖   subject may not be empty\n"))
	})
	l := newGitLab(t, f)
	ctx := ctxT(t)

	run, err := l.Checks(ctx, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != CIFailure || run.URL != "p7" || len(run.Jobs) != 1 || run.Jobs[0].Name != "commitlint" || run.Jobs[0].ID != 70 {
		t.Fatalf("run = %+v", run)
	}
	if log, err := l.Log(ctx, run.Jobs[0], 1000); err != nil || log != "✖   subject may not be empty\n" {
		t.Errorf("log = %q, %v", log, err)
	}
	for status, want := range map[string]string{"running": CIPending, "pending": CIPending, "success": CISuccess, "manual": CISuccess, "canceled": CINone} {
		pipelines = []any{map[string]any{"id": 8, "status": status, "web_url": "p8"}}
		if run, err := l.Checks(ctx, "abc"); err != nil || run.State != want {
			t.Errorf("%s: %+v, %v", status, run, err)
		}
	}
	pipelines = []any{}
	if run, _ := l.Checks(ctx, "abc"); run.State != CINone {
		t.Errorf("no pipeline: %s", run.State)
	}
}

func TestNewCI(t *testing.T) {
	if _, err := NewCI(Repo{Host: "git.example.com", Kind: KindBranch}, Auth{}); err == nil {
		t.Error("an unknown host has a CI")
	}
	ci, err := NewCI(Repo{Host: "github.com", Owner: "o", Name: "r", Path: "o/r", Kind: KindGitHubPR}, Auth{})
	if _, ok := ci.(*github); err != nil || !ok {
		t.Errorf("github: %T, %v", ci, err)
	}
	ci, err = NewCI(Repo{Host: "gitlab.example.ru", Path: "team/proj", Kind: KindGitLabMR}, Auth{})
	if _, ok := ci.(*gitlab); err != nil || !ok {
		t.Errorf("gitlab: %T, %v", ci, err)
	}
}
