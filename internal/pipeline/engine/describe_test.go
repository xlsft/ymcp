package engine

import (
	"strings"
	"testing"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
)

// describeHarness is a GitLab pipeline whose agent writes the change's
// title and description in a turn of their own.
func describeHarness(t *testing.T) *harness {
	return vcsHarness(t, vcs.KindGitLabMR, func(p *spec.Pipeline) { p.Strategy.Describe = nil })
}

// setChange is a describe turn that calls set_change.
func setChange(title, description string) func(*fakeTurn) agent.Result {
	return func(t *fakeTurn) agent.Result {
		if out, err := t.Tool("set_change", map[string]any{"title": title, "description": description}); err != nil {
			t.Say("set_change failed: " + err.Error() + out)
		}
		t.Say("Done.")
		return agent.Result{}
	}
}

// TestDescribeOpensWithTheAgentsTitle: the change opens with the title and
// description the agent passed to set_change in the describe turn, not
// with the work turn's answer and its narration.
func TestDescribeOpensWithTheAgentsTitle(t *testing.T) {
	h := describeHarness(t)
	is := h.yt.addIssue("Мелкие баги модуля", "Open")
	h.ag.script(
		fixTurn("Изучаю канон обработки ошибок.\n\nЛинтер прошёл.\n\n- Fixes the errors."),
		setChange("fix(registry): show the causes of errors", "- Shows the causes of errors ([K8S-1](https://yt/K8S-1))"),
	)
	h.start()
	run := h.waitReview(1, is.Key)
	opens := h.vcs.openLog()
	if len(opens) != 1 {
		t.Fatalf("opens = %+v", opens)
	}
	o := opens[0]
	body := text.StripHeader(o.Body)
	if o.Title != "fix(registry): show the causes of errors" || strings.TrimSpace(body) != "- Shows the causes of errors ([K8S-1](https://yt/K8S-1))" {
		t.Errorf("opened %q:\n%s", o.Title, body)
	}
	prompts := h.ag.promptLog()
	if len(prompts) != 2 || !strings.Contains(prompts[1], "set_change") || !strings.Contains(prompts[1], "merge request") || !strings.Contains(prompts[1], "opens") {
		t.Errorf("describe prompt = %q", prompts[len(prompts)-1])
	}
	if starts := h.ag.startLog(); len(starts) != 2 || starts[1].SessionID != "session-1" || starts[1].SystemPrompt != "" {
		t.Errorf("the describe turn did not resume the session: %+v", starts)
	}
	if len(h.events(run.ID, "describe.start")) != 1 || len(h.events(run.ID, "agent.set_change")) != 1 {
		t.Error("the describe turn left no events")
	}
}

// TestDescribeFallsBack: an agent that doesn't call set_change is asked
// once more; then the answer of that turn, which ran no tools, is the
// description.
func TestDescribeFallsBack(t *testing.T) {
	h := describeHarness(t)
	is := h.yt.addIssue("Fix it", "Open")
	h.ag.script(
		fixTurn("Narration.\n\nFixed."),
		func(t *fakeTurn) agent.Result { t.Say("Here is a description."); return agent.Result{} },
		func(t *fakeTurn) agent.Result { t.Say("- Fixes it."); return agent.Result{} },
	)
	h.start()
	h.waitReview(1, is.Key)
	prompts := h.ag.promptLog()
	if len(prompts) != 3 || prompts[2] != describeAgain {
		t.Fatalf("prompts = %q", prompts)
	}
	o := h.vcs.openLog()[0]
	if !strings.HasSuffix(o.Title, "Fix it") || strings.TrimSpace(text.StripHeader(o.Body)) != "- Fixes it." {
		t.Errorf("opened %q:\n%s", o.Title, o.Body)
	}
}

// TestDescribeUpdatesUntilEdited: a later push updates the change with the
// new title and description, unless a person edited it on the host.
func TestDescribeUpdatesUntilEdited(t *testing.T) {
	h := describeHarness(t)
	is := h.yt.addIssue("Fix it", "Open")
	h.ag.script(
		fixTurn("Fixed."),
		setChange("fix: one", "- One."),
		func(t *fakeTurn) agent.Result {
			writeFile(t, "fix.go", "package fix // v2\n")
			t.Say("Renamed.")
			return agent.Result{}
		},
		setChange("fix: two", "- Two."),
		func(t *fakeTurn) agent.Result {
			writeFile(t, "fix.go", "package fix // v3\n")
			t.Say("Again.")
			return agent.Result{}
		},
		setChange("fix: three", "- Three."),
	)
	h.start()
	run := h.waitReview(1, is.Key)

	h.yt.addComment(is.Key, "alice", "Rename it")
	h.waitInvocations(run.ID, 2)
	h.waitFor("the update", func() bool { return len(h.vcs.updateLog()) == 1 })
	if u := h.vcs.updateLog()[0]; u.Title != "fix: two" || strings.TrimSpace(text.StripHeader(u.Body)) != "- Two." {
		t.Errorf("update = %+v", u)
	}
	run = h.waitReview(1, is.Key)

	h.vcs.editChange("fix: mine", "My own words.")
	h.yt.addComment(is.Key, "alice", "Once more")
	h.waitInvocations(run.ID, 3)
	h.waitFor("the skipped update", func() bool { return len(h.events(run.ID, "vcs.update_skipped")) == 1 })
	if n := len(h.vcs.updateLog()); n != 1 {
		t.Errorf("%d updates after a person's edit", n)
	}
}

// TestDescribeOff: with strategy.describe false the change opens as
// before, from strategy.title and the answer.
func TestDescribeOff(t *testing.T) {
	h := vcsHarness(t, vcs.KindGitLabMR, nil)
	is := h.yt.addIssue("Fix it", "Open")
	h.ag.script(fixTurn("Fixed it."))
	h.start()
	h.waitReview(1, is.Key)
	if n := len(h.ag.promptLog()); n != 1 {
		t.Errorf("%d turns", n)
	}
	if o := h.vcs.openLog()[0]; !strings.Contains(o.Body, "Fixed it.") {
		t.Errorf("body = %q", o.Body)
	}
}
