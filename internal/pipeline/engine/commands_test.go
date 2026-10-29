package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
)

func TestControlCommand(t *testing.T) {
	for _, c := range []struct{ in, cmd, rest string }{
		{"/clear", cmdClear, ""},
		{"  /Compact  please\n", cmdCompact, "please"},
		{"/cancel not needed any more", cmdCancel, "not needed any more"},
		{"/approve", "", "/approve"},
		{"please /clear", "", "please /clear"},
		{"", "", ""},
	} {
		if cmd, rest := controlCommand(c.in); cmd != c.cmd || (cmd != "" && rest != c.rest) {
			t.Errorf("controlCommand(%q) = %q, %q", c.in, cmd, rest)
		}
	}
}

// reviewedRun brings a run to review with a pull request, a comment from
// before the run on its issue and a human comment on the pull request.
func reviewedRun(t *testing.T, mod func(p *spec.Pipeline), turns ...func(*fakeTurn) agent.Result) (*harness, *fakeIssue, store.Run, vcs.Comment) {
	t.Helper()
	h := vcsHarness(t, vcs.KindGitHubPR, func(p *spec.Pipeline) {
		p.Feedback.YouTrack = []string{"alice"}
		if mod != nil {
			mod(p)
		}
	})
	is := h.yt.addIssue("Fix the login redirect", "Open")
	h.yt.addComment(is.Key, "carol", "Context from before the run")
	h.ag.script(append([]func(*fakeTurn) agent.Result{fixTurn("Fixed the redirect.")}, turns...)...)
	h.start()
	run := h.waitReview(1, is.Key)
	c := h.vcs.comment(vcs.Comment{Author: "bob", Body: "Looks fine"})
	return h, is, run, c
}

// TestClearCommand: /clear hides the run's comments on the issue and the
// pull request, the command itself included, without an agent turn; what
// was there before the run stays.
func TestClearCommand(t *testing.T) {
	h, is, run, prComment := reviewedRun(t, nil)
	h.yt.addComment(is.Key, "alice", "/clear")
	h.waitFor("comments cleared", func() bool { return len(h.events(run.ID, "comments.cleared")) == 1 })
	if got := h.yt.issueComments(is.Key); !slices.Equal(got, []string{"Context from before the run"}) {
		t.Errorf("visible comments = %q", got)
	}
	hidden := h.yt.hiddenComments(is.Key)
	if !slices.Contains(hidden, "/clear") || !slices.ContainsFunc(hidden, func(s string) bool { return strings.Contains(s, "Fixed the redirect.") }) {
		t.Errorf("hidden comments = %q", hidden)
	}
	if !slices.Contains(h.vcs.hiddenIDs(), prComment.ID) {
		t.Errorf("hidden on the change = %v, want %s", h.vcs.hiddenIDs(), prComment.ID)
	}
	for _, inv := range h.waitInvocations(run.ID, 2) {
		if inv.Kind != kindStart && inv.Kind != "command" {
			t.Errorf("invocation %s: /clear must not start a turn", inv.Kind)
		}
	}
	if r := h.waitStatus(1, store.StatusReview); r.Status != store.StatusReview {
		t.Errorf("status = %s", r.Status)
	}
}

// TestRestoredCommandRunsOnce: a command comment restored after /clear hid
// it is no new message: the command does not run again. An edit is one.
func TestRestoredCommandRunsOnce(t *testing.T) {
	h, is, run, _ := reviewedRun(t, nil)
	id := h.yt.addComment(is.Key, "alice", "/clear")
	h.waitFor("comments cleared", func() bool { return len(h.events(run.ID, "comments.cleared")) == 1 })
	h.yt.restoreComment(id)
	h.waitFor("the comment seen again", func() bool { return slices.Contains(h.yt.issueComments(is.Key), "/clear") })
	time.Sleep(300 * time.Millisecond) // a dozen polls
	if n := len(h.events(run.ID, "command")); n != 1 {
		t.Fatalf("the command ran %d times", n)
	}
	h.yt.editText(id, "/clear again")
	h.waitFor("the edit runs", func() bool { return len(h.events(run.ID, "command")) == 2 })
}

// TestCompactCommand: /compact has the agent sum the run up in a session
// of its own, hides the run's comments and leaves the summary with the
// links, on the issue and on the pull request.
func TestCompactCommand(t *testing.T) {
	var prompt string
	h, is, run, _ := reviewedRun(t, func(p *spec.Pipeline) { p.Language = "Russian" }, func(t *fakeTurn) agent.Result {
		prompt = t.prompt
		t.Say("Редирект после входа исправлен.")
		return agent.Result{}
	})
	h.yt.addComment(is.Key, "alice", "/compact")
	h.waitFor("run compacted", func() bool { return len(h.events(run.ID, "run.compacted")) == 1 })
	if !strings.Contains(prompt, "in Russian") || !strings.Contains(prompt, "Fixed the redirect.") || !strings.Contains(prompt, "bob on the pull request: Looks fine") {
		t.Errorf("summary prompt = %q", prompt)
	}
	visible := h.yt.issueComments(is.Key)
	if len(visible) != 2 || visible[0] != "Context from before the run" {
		t.Fatalf("visible comments = %q", visible)
	}
	for _, want := range []string{"**Summary**", "Редирект после входа исправлен.", "Pull request: " + prURL, "Branch: `" + run.Branch + "`"} {
		if !strings.Contains(visible[1], want) {
			t.Errorf("summary %q lacks %q", visible[1], want)
		}
	}
	replies := h.vcs.agentReplies()
	if len(replies) == 0 || !strings.Contains(replies[len(replies)-1].Body, "Редирект после входа исправлен.") {
		t.Errorf("no summary on the change: %+v", replies)
	}
	if r := h.waitStatus(1, store.StatusReview); r.AgentSession == "" {
		t.Error("the run's own session was dropped")
	}
}

// TestCompactTwiceAtOnce: a /compact while the summary of the first is
// still being written is not carried out again.
func TestCompactTwiceAtOnce(t *testing.T) {
	release := make(chan struct{})
	h, is, run, _ := reviewedRun(t, nil, func(t *fakeTurn) agent.Result {
		<-release
		t.Say("The redirect is fixed.")
		return agent.Result{}
	})
	h.yt.addComment(is.Key, "alice", "/compact")
	h.waitFor("the summary started", func() bool { return len(h.events(run.ID, "command")) == 1 })
	h.yt.addComment(is.Key, "alice", "/compact")
	h.waitFor("the second compact skipped", func() bool { return len(h.events(run.ID, "compact.skipped")) == 1 })
	close(release)
	h.waitFor("run compacted", func() bool { return len(h.events(run.ID, "run.compacted")) == 1 })
	time.Sleep(10 * h.p.Source.PollInterval.Duration)
	if n := len(h.events(run.ID, "run.compacted")); n != 1 {
		t.Errorf("compacted %d times", n)
	}
	summaries := 0
	for _, c := range h.yt.issueComments(is.Key) {
		if strings.Contains(c, "**Summary**") {
			summaries++
		}
	}
	if summaries != 1 {
		t.Errorf("%d summaries: %q", summaries, h.yt.issueComments(is.Key))
	}
}

// TestCancelCommand: /cancel cancels the run, closes its pull request and
// closes the issue with the closed stage's command.
func TestCancelCommand(t *testing.T) {
	h, is, run, _ := reviewedRun(t, nil)
	h.yt.addComment(is.Key, "alice", "/cancel not needed any more")
	r := h.waitStatus(1, store.StatusCanceled)
	if r.NeedsInputReason != "Canceled by alice with /cancel: not needed any more." {
		t.Errorf("reason = %q", r.NeedsInputReason)
	}
	h.waitFor("pull request closed", func() bool { return slices.Contains(h.vcs.closeLog(), run.PRNumber) })
	h.waitFor("closing command", func() bool { return slices.Contains(h.yt.commandLog(), is.Key+": State Closed") })
	if !slices.ContainsFunc(h.yt.agentComments(is.Key), func(s string) bool { return strings.Contains(s, "**Canceled** by alice: not needed any more.") }) {
		t.Errorf("agent comments = %q", h.yt.agentComments(is.Key))
	}
}

// TestAutoCompactOnMerge: with autoCompact, a run whose pull request is
// merged is compacted.
func TestAutoCompactOnMerge(t *testing.T) {
	h, is, run, _ := reviewedRun(t, func(p *spec.Pipeline) { p.AutoCompact = true }, func(t *fakeTurn) agent.Result {
		t.Say("Merged: the redirect is fixed.")
		return agent.Result{}
	})
	h.vcs.setState(vcs.StateMerged)
	h.waitStatus(1, store.StatusDone)
	h.waitFor("run compacted", func() bool { return len(h.events(run.ID, "run.compacted")) == 1 })
	visible := h.yt.issueComments(is.Key)
	if len(visible) != 2 || !strings.Contains(visible[1], "Merged: the redirect is fixed.") {
		t.Errorf("visible comments = %q", visible)
	}
}

func TestSameText(t *testing.T) {
	mark := seenMark("1700000000000", "/clear")
	for _, c := range []struct {
		mark, body string
		want       bool
	}{
		{mark, "/clear", true},
		{mark, "/clear again", false},
		{"1700000000000", "/clear", true},      // an old mark: a command never runs twice
		{"1700000000000", "/approve", true},    // the same
		{"1700000000000", "Please fix", false}, // an old mark: the edit counts
	} {
		if got := sameText(c.mark, c.body); got != c.want {
			t.Errorf("sameText(%q, %q) = %v", c.mark, c.body, got)
		}
	}
}
