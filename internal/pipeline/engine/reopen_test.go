package engine

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
)

// A run whose pull request was merged ends as done; its issue, still in
// review, doesn't reopen it. Moved out of the working stages and back, it
// does: the agent's session carries on, the comments written since are its
// feedback, and the next push opens a new pull request.
func TestReopenDoneRun(t *testing.T) {
	var prompt string
	h, is, run, _ := reviewedRun(t, nil, func(t *fakeTurn) agent.Result {
		prompt = t.prompt
		writeFile(t, "redirect_test.go", "package redirect\n")
		t.Say("Added the test.")
		return agent.Result{}
	})
	h.vcs.setState(vcs.StateMerged)
	h.waitStatus(1, store.StatusDone)
	time.Sleep(10 * h.p.Source.PollInterval.Duration)
	if r := h.run(1); r.Status != store.StatusDone {
		t.Fatalf("reopened while still in review after the merge: %s", r.Status)
	}

	h.yt.setState(is.Key, "Verified") // out of the working stages
	h.waitFor("the issue seen out of the working stages", func() bool {
		_, left, _ := h.st.RunState(context.Background(), run.ID, stateLeft)
		return left
	})
	h.yt.addComment(is.Key, "alice", "The redirect test is missing")
	h.yt.setState(is.Key, "Review")

	r := h.waitStatus(1, store.StatusReview)
	if r.FinishedAt != nil || r.PRNumber == run.PRNumber || r.AgentSession != run.AgentSession {
		t.Errorf("reopened run = %+v", r)
	}
	h.waitFor("run.reopened", func() bool { return len(h.events(run.ID, "run.reopened")) > 0 })
	if ev := h.events(run.ID, "run.reopened"); len(ev) != 1 || !strings.Contains(string(ev[0].Data), prURL) {
		t.Errorf("reopened events = %+v", ev)
	}
	h.waitInvocations(run.ID, 2)
	if !strings.Contains(prompt, "The redirect test is missing") {
		t.Errorf("feedback prompt = %q", prompt)
	}
	h.waitFor("a new pull request", func() bool { return len(h.vcs.openLog()) == 2 })
	if r := h.waitStatus(1, store.StatusReview); r.PRNumber == 0 || r.PRNumber == run.PRNumber {
		t.Errorf("pull request = #%d, was #%d", r.PRNumber, run.PRNumber)
	}
	if !slicesHasPrefix(h.yt.agentComments(is.Key), "**Reopened**") {
		t.Errorf("agent comments = %q", h.yt.agentComments(is.Key))
	}
}

// slicesHasPrefix reports whether one of the comments, header aside,
// starts with prefix.
func slicesHasPrefix(comments []string, prefix string) bool {
	for _, c := range comments {
		if strings.Contains(c, prefix) {
			return true
		}
	}
	return false
}

// A run that ended still takes /clear and /compact, from YouTrack and from
// the TUI, and stays ended; anything else is refused.
func TestCommandsOnEndedRun(t *testing.T) {
	h, is, run, _ := reviewedRun(t, nil, func(t *fakeTurn) agent.Result {
		t.Say("The redirect is fixed.")
		return agent.Result{}
	})
	h.vcs.setState(vcs.StateMerged)
	h.waitStatus(1, store.StatusDone)

	h.yt.addComment(is.Key, "alice", "/compact")
	h.waitFor("run compacted", func() bool { return len(h.events(run.ID, "run.compacted")) == 1 })
	if r := h.run(1); r.Status != store.StatusDone {
		t.Errorf("status = %s", r.Status)
	}
	if !slicesHasPrefix(h.yt.issueComments(is.Key), "**Summary**") {
		t.Errorf("comments = %q", h.yt.issueComments(is.Key))
	}

	ctx := context.Background()
	if err := Answer(ctx, h.st, h.p.Name, 1, "go on"); err == nil {
		t.Error("an ended run took an answer")
	}
	if err := Answer(ctx, h.st, h.p.Name, 1, "/clear"); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the /clear", func() bool { return len(h.events(run.ID, "command")) == 2 })
	if r := h.run(1); r.Status != store.StatusDone {
		t.Errorf("status = %s", r.Status)
	}
}

// An issue moved back into the needs-input stage reopens the run into
// review and leaves that stage.
func TestReopenFromNeedsInput(t *testing.T) {
	h, is, run, _ := reviewedRun(t, func(p *spec.Pipeline) { p.Stages.NeedsInput.Exit = "State Unblocked" }, func(t *fakeTurn) agent.Result {
		t.Say("Done.")
		return agent.Result{}
	})
	h.vcs.setState(vcs.StateMerged)
	h.waitStatus(1, store.StatusDone)
	h.yt.setState(is.Key, "Verified")
	h.waitFor("the issue seen out of the working stages", func() bool {
		_, left, _ := h.st.RunState(context.Background(), run.ID, stateLeft)
		return left
	})
	h.yt.setState(is.Key, "Blocked")
	h.waitStatus(1, store.StatusReview)
	h.waitFor("the needs-input exit command", func() bool {
		return slices.Contains(h.yt.commandLog(), is.Key+": State Unblocked")
	})
}
