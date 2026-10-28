package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
)

// fakeCI is a code host's CI: what ran for each commit, and the logs.
type fakeCI struct {
	mu   sync.Mutex
	runs map[string]vcs.CIRun
	logs map[int64]string
}

func (f *fakeCI) set(sha string, r vcs.CIRun) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runs == nil {
		f.runs = map[string]vcs.CIRun{}
	}
	f.runs[sha] = r
}

func (f *fakeCI) Checks(_ context.Context, sha string) (vcs.CIRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.runs[sha]; ok {
		return r, nil
	}
	return vcs.CIRun{State: vcs.CINone}, nil
}

func (f *fakeCI) Log(_ context.Context, job vcs.CIJob, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs[job.ID], nil
}

func (f *fakeVCS) Checks(ctx context.Context, sha string) (vcs.CIRun, error) {
	return f.ci.Checks(ctx, sha)
}

func (f *fakeVCS) Log(ctx context.Context, job vcs.CIJob, limit int) (string, error) {
	return f.ci.Log(ctx, job, limit)
}

// pushed is the commit the runner pushed last for a run.
func (h *harness) pushed(runID int64) string {
	h.t.Helper()
	v, _, err := h.st.RunState(context.Background(), runID, statePushedHead)
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

func failedCI(job string, id int64) vcs.CIRun {
	return vcs.CIRun{State: vcs.CIFailure, URL: "https://ci.test/p/1", Jobs: []vcs.CIJob{{ID: id, Name: job, State: vcs.CIFailure, URL: "https://ci.test/j/1"}}}
}

// TestCIFailureGoesToTheAgent: on the branch strategy, a failed CI of the
// pushed commit gives the agent a turn with the failed job and the end of
// its log; the fix is watched in turn and passes.
func TestCIFailureGoesToTheAgent(t *testing.T) {
	h := newHarness(t, nil)
	h.ci = &fakeCI{logs: map[int64]string{1: "section_start:1:test\r\x1b[0K\x1b[31m--- FAIL: TestLogin\x1b[0m\nprogress 10%\rprogress 100%\nFAIL\n"}}
	is := h.yt.addIssue("Fix the login page", "Open")
	h.ag.script(
		fixTurn("Fixed the login page."),
		func(t *fakeTurn) agent.Result {
			writeFile(t, "fix.go", "package fix // v2\n")
			t.Say("The test expected the old message; updated it.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitReview(1, is.Key)
	first := h.pushed(run.ID)
	h.ci.set(first, failedCI("test", 1))

	h.waitInvocations(run.ID, 2)
	run = h.waitReview(1, is.Key)
	prompt := h.ag.promptLog()[1]
	for _, want := range []string{"ci · CI · https://ci.test/p/1", "The CI failed on " + first[:10], "### test (https://ci.test/j/1)", "--- FAIL: TestLogin", "progress 100%"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "\x1b") || strings.Contains(prompt, "section_start") || strings.Contains(prompt, "progress 10%") {
		t.Errorf("the log is not cleaned:\n%q", prompt)
	}
	second := h.pushed(run.ID)
	if second == first {
		t.Fatal("the fix was not pushed")
	}
	h.ci.set(second, vcs.CIRun{State: vcs.CISuccess, URL: "https://ci.test/p/2"})
	h.waitFor("the CI to pass", func() bool { return len(h.events(run.ID, "ci.passed")) == 1 })
	if n := len(h.events(run.ID, "ci.failed")); n != 1 {
		t.Errorf("ci.failed events = %d", n)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(h.ag.promptLog()); n != 2 {
		t.Errorf("%d turns after the CI passed", n)
	}
}

// TestCIFixesRunOut: once the agent has had ci.fixes turns in a row, the
// next failure waits for a human.
func TestCIFixesRunOut(t *testing.T) {
	h := vcsHarness(t, vcs.KindGitLabMR, func(p *spec.Pipeline) { p.CI.Fixes = 1 })
	is := h.yt.addIssue("Fix the login page", "Open")
	h.ag.script(
		fixTurn("Fixed the login page."),
		func(t *fakeTurn) agent.Result {
			writeFile(t, "fix.go", "package fix // v2\n")
			t.Say("Fixed the lint.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitReview(1, is.Key)
	h.vcs.ci.set(h.pushed(run.ID), failedCI("lint", 0))
	h.waitInvocations(run.ID, 2)
	run = h.waitReview(1, is.Key)
	h.vcs.ci.set(h.pushed(run.ID), failedCI("lint", 0))
	run = h.waitStatus(1, store.StatusNeedsInput)
	if !strings.Contains(run.NeedsInputReason, "The CI failed 2 times in a row.") {
		t.Errorf("reason = %q", run.NeedsInputReason)
	}
	if n := len(h.ag.promptLog()); n != 2 {
		t.Errorf("%d turns", n)
	}
}

// TestCIOff: with ci.watch off nothing is read.
func TestCIOff(t *testing.T) {
	off := false
	h := newHarness(t, func(p *spec.Pipeline) { p.CI.Watch = &off })
	h.ci = &fakeCI{}
	is := h.yt.addIssue("Fix the login page", "Open")
	h.ag.script(fixTurn("Fixed."))
	h.start()
	run := h.waitReview(1, is.Key)
	h.ci.set(h.pushed(run.ID), failedCI("test", 0))
	time.Sleep(200 * time.Millisecond)
	if n := len(h.ag.promptLog()); n != 1 || len(h.events(run.ID, "ci.failed")) != 0 {
		t.Errorf("%d turns with the CI off", n)
	}
}

func TestCleanLog(t *testing.T) {
	in := "section_start:17:build\r\x1b[0K\x1b[32;1m$ make\x1b[0m\r\n10%\r55%\r100%\nok\nsection_end:17:build\r\x1b[0K"
	if got := cleanLog(in, 1000); got != "$ make\n100%\nok" {
		t.Errorf("cleanLog = %q", got)
	}
	if got := cleanLog(strings.Repeat("line\n", 100)+"last", 30); !strings.HasSuffix(got, "last") || len(got) > 30 || strings.HasPrefix(got, "ine") {
		t.Errorf("cut = %q", got)
	}
}

// FuzzCleanLog: a cleaned log has no escape sequences or carriage returns
// and fits its limit.
func FuzzCleanLog(f *testing.F) {
	for _, s := range []string{"", "ok\n", "section_start:1:x\r\x1b[0K\x1b[31mred\x1b[0m\r\n10%\r100%\n", strings.Repeat("line\n", 300)} {
		f.Add(s, 100)
	}
	f.Fuzz(func(t *testing.T, log string, limit int) {
		limit = 1 + abs(limit)%5000
		out := cleanLog(log, limit)
		if len(out) > limit || strings.ContainsAny(out, "\x1b\r") {
			t.Errorf("cleanLog(%q, %d) = %q", log, limit, out)
		}
	})
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
