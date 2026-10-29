package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/youtrack/youtracktest"
)

// postedComment is a comment of the fake YouTrack as AddComment returns it
// to the TUI.
func (f *fakeYT) postedComment(id string) Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.comments {
		if c.ID == id {
			out := Comment{ID: c.ID, Author: c.Author, Text: c.Text, URL: "https://yt.test/issue#" + c.ID, Created: time.UnixMilli(c.Created)}
			if c.Updated > 0 {
				out.Updated = time.UnixMilli(c.Updated)
			}
			return out
		}
	}
	panic("no comment " + id)
}

// What the TUI asks of a run is carried out at once, not on the next poll
// of YouTrack, which here never comes.
func TestLocalRequestsActAtOnce(t *testing.T) {
	fastStopPoll(t)
	h := newHarness(t, func(p *spec.Pipeline) { p.Source.PollInterval = dur(time.Hour) })
	is := h.yt.addIssue("Fix the login page", "Open")
	var got []string
	h.ag.script(
		func(t *fakeTurn) agent.Result {
			writeFile(t, "login.go", "package login\n")
			t.Say("Fixed the login page.")
			return agent.Result{}
		},
		func(t *fakeTurn) agent.Result {
			got = append(got, t.prompt)
			writeFile(t, "login.go", "package login // renamed\n")
			t.Say("Renamed it.")
			return agent.Result{}
		},
		func(t *fakeTurn) agent.Result {
			got = append(got, t.prompt)
			writeFile(t, "login_test.go", "package login\n")
			t.Say("Added a test.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitReview(1, is.Key)
	ctx := context.Background()

	// An answer given in the TUI or the CLI.
	if err := Answer(ctx, h.st, h.p.Name, 1, "rename it"); err != nil {
		t.Fatal(err)
	}
	h.waitInvocations(run.ID, 2)

	// A comment the TUI posted on the issue.
	id := h.yt.addComment(is.Key, youtracktest.Login, "add a test")
	if err := Commented(ctx, h.st, h.p.Name, 1, h.yt.postedComment(id)); err != nil {
		t.Fatal(err)
	}
	h.waitInvocations(run.ID, 3)
	if len(got) != 2 || !strings.Contains(got[0], "rename it") || !strings.Contains(got[1], "add a test") {
		t.Errorf("feedback prompts = %q", got)
	}

	// A cancel.
	h.waitStatus(1, store.StatusReview)
	if err := Cancel(ctx, h.st, h.p.Name, 1); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the worker to stop", func() bool { return len(h.logs.matching("run ended", "status=canceled")) > 0 })
}

// A comment the TUI hands the run and the runner's poll of YouTrack both
// see is queued once and answered by one turn.
func TestCommentedAndPolledOnce(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Fix the login page", "Open")
	h.ag.script(
		func(t *fakeTurn) agent.Result {
			writeFile(t, "login.go", "package login\n")
			t.Say("Fixed the login page.")
			return agent.Result{}
		},
		func(t *fakeTurn) agent.Result {
			writeFile(t, "login_test.go", "package login\n")
			t.Say("Added a test.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitReview(1, is.Key)

	id := h.yt.addComment(is.Key, youtracktest.Login, "add a test")
	c := h.yt.postedComment(id)
	// Handed over before and after the poll may have seen it.
	if err := Commented(context.Background(), h.st, h.p.Name, 1, c); err != nil {
		t.Fatal(err)
	}
	h.waitInvocations(run.ID, 2)
	time.Sleep(20 * h.p.Source.PollInterval.Duration) // polls that see the comment
	if err := Commented(context.Background(), h.st, h.p.Name, 1, c); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * h.p.Source.PollInterval.Duration)

	if inbox := h.events(run.ID, "inbox"); len(inbox) != 1 {
		t.Errorf("queued %d times: %+v", len(inbox), inbox)
	}
	invs, err := h.st.Invocations(context.Background(), run.ID)
	if err != nil || len(invs) != 2 {
		t.Errorf("invocations = %d, %v; want 2", len(invs), err)
	}
}

// A comment on a run that has ended goes nowhere, and says so; a /clear on
// a run that no runner follows any more is refused too.
func TestCommentedOnEndedRun(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	r, err := h.st.CreateRun(ctx, h.p.Name, h.p.Profile, "2-1", "BE-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.ModifyRun(ctx, r.ID, func(run *store.Run) error { run.Status = store.StatusDone; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := Commented(ctx, h.st, h.p.Name, r.Number, Comment{ID: "4-1", Author: "alice", Text: "late", Created: time.Now()}); !errors.Is(err, ErrRunEnded) {
		t.Errorf("a comment on a done run: %v", err)
	}
	// Superseded by a later run of the issue: no runner follows it.
	if _, err := h.st.CreateRun(ctx, h.p.Name, h.p.Profile, "2-1", "BE-1"); err != nil {
		t.Fatal(err)
	}
	if err := Commented(ctx, h.st, h.p.Name, r.Number, Comment{ID: "4-2", Author: "alice", Text: "/clear", Created: time.Now()}); !errors.Is(err, ErrRunEnded) {
		t.Errorf("a /clear on a superseded run: %v", err)
	}
	if err := Answer(ctx, h.st, h.p.Name, r.Number, "/compact"); !errors.Is(err, ErrRunEnded) {
		t.Errorf("a /compact on a superseded run: %v", err)
	}
	// Canceled: never followed.
	c, err := h.st.CreateRun(ctx, h.p.Name, h.p.Profile, "2-2", "BE-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.ModifyRun(ctx, c.ID, func(run *store.Run) error { run.Status = store.StatusCanceled; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := Answer(ctx, h.st, h.p.Name, c.Number, "/clear"); !errors.Is(err, ErrRunEnded) {
		t.Errorf("a /clear on a canceled run: %v", err)
	}
	if items, _ := h.st.PendingInbox(ctx, r.ID); len(items) != 0 {
		t.Errorf("inbox = %+v", items)
	}
}
