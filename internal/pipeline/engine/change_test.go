package engine

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
)

const prURL = "https://github.test/acme/api/pull/42"

// vcsHarness is a harness whose pipeline opens pull requests on the fake
// code host.
func vcsHarness(t *testing.T, kind vcs.Kind, mod func(p *spec.Pipeline)) *harness {
	t.Helper()
	h := newHarness(t, func(p *spec.Pipeline) {
		p.Strategy.Kind = string(kind)
		if mod != nil {
			mod(p)
		}
	})
	h.vcs = newFakeVCS(kind)
	return h
}

// fixTurn commits a file and answers: the turn that opens the change.
func fixTurn(answer string) func(*fakeTurn) agent.Result {
	return func(t *fakeTurn) agent.Result {
		writeFile(t, "fix.go", "package fix\n")
		t.Say(answer)
		return agent.Result{}
	}
}

// itemNumber finds the number of the feedback item whose text contains
// body in a rendered feedback prompt.
func itemNumber(prompt, body string) int {
	n := 0
	for _, l := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(l, "[") {
			if i := strings.Index(l, "]"); i > 0 {
				if v, err := strconv.Atoi(l[1:i]); err == nil {
					n = v
				}
			}
		}
		if strings.Contains(l, body) {
			return n
		}
	}
	return 0
}

// replyTo returns the agent's reply to the comment id.
func replyTo(rs []fakeAgentReply, id string) (fakeAgentReply, bool) {
	for _, r := range rs {
		if r.To == id {
			return r, true
		}
	}
	return fakeAgentReply{}, false
}

// TestChangeOpenedAfterFirstPush: no change while nothing is pushed; the
// first turn that pushes opens it (draft, title, body with the marker, the
// issue link and the summary), YouTrack learns its link and later pushes
// go to the same change.
func TestChangeOpenedAfterFirstPush(t *testing.T) {
	h := vcsHarness(t, vcs.KindGitHubPR, func(p *spec.Pipeline) { p.Strategy.Draft = true })
	is := h.yt.addIssue("Fix the login page", "Open")
	h.ag.script(
		func(t *fakeTurn) agent.Result {
			t.Say("Looked around; nothing to change yet.")
			return agent.Result{}
		},
		fixTurn("Fixed the login page."),
		func(t *fakeTurn) agent.Result {
			writeFile(t, "fix.go", "package fix // v2\n")
			t.Say("Renamed.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitReview(1, is.Key)
	if run.PRNumber != 0 || len(h.vcs.openLog()) != 0 {
		t.Fatalf("a change was opened without a push: %+v", run)
	}
	if n := h.notes.of(NotifyReviewReady); len(n) != 1 || !strings.Contains(n[0].URL, "/issue/"+is.Key) {
		t.Errorf("review_ready without a change: %+v", n)
	}

	h.yt.addComment(is.Key, "alice", "Please fix it for real")
	h.waitInvocations(run.ID, 2)
	run = h.waitReview(1, is.Key)
	opens := h.vcs.openLog()
	if len(opens) != 1 {
		t.Fatalf("opens = %+v", opens)
	}
	o := opens[0]
	if o.Title != is.Key+" Fix the login page" || o.Branch != run.Branch || o.Base != "main" || !o.Draft || o.IssueTitle != "" {
		t.Errorf("open options = %+v", o)
	}
	if _, ok := text.ParseMarker(o.Body); !ok || !strings.Contains(o.Body, "@octo's agent #1 (backend)") {
		t.Errorf("the body is not marked: %q", o.Body)
	}
	body := text.StripHeader(o.Body)
	if !strings.HasPrefix(body, "YouTrack issue: ["+is.Key+"](") || !strings.Contains(body, "/issue/"+is.Key+")") || !strings.Contains(body, "Fixed the login page.") {
		t.Errorf("body = %q", body)
	}
	if run.VCSKind != "github-pr" || run.VCSRepo != "github.test/acme/api" || run.PRNumber != 42 || run.PRURL != prURL {
		t.Errorf("run = %+v", run)
	}
	if said := h.yt.agentComments(is.Key); !contains(said, "Fixed the login page.\n\nPull request: "+prURL) {
		t.Errorf("YouTrack lacks the link: %q", said)
	}
	if n := h.notes.of(NotifyReviewReady); len(n) != 2 || n[1].URL != prURL || n[1].Title != is.Key+" · backend #1: ready for review" {
		t.Errorf("review_ready = %+v", n)
	}
	if len(h.events(run.ID, "vcs.opened")) != 1 {
		t.Errorf("events = %+v", h.events(run.ID, ""))
	}

	// A later push goes to the same change; prompts know its URL.
	h.yt.addComment(is.Key, "alice", "Rename it")
	h.waitInvocations(run.ID, 3)
	h.waitReview(1, is.Key)
	if p := h.ag.promptLog()[2]; !strings.Contains(p, "PR/MR: "+prURL) {
		t.Errorf("prompt = %q", p)
	}
	if !strings.Contains(h.ag.promptLog()[1], "PR/MR: none") {
		t.Errorf("prompt before the change = %q", h.ag.promptLog()[1])
	}
	if n := len(h.vcs.openLog()); n != 1 {
		t.Errorf("%d opens", n)
	}
	if said := h.yt.agentComments(is.Key); contains(said, "Renamed.\n\nPull request") {
		t.Errorf("the link was repeated: %q", said)
	}
	if n := strings.Count(git(t, h.origin, "log", "--format=%s", run.Branch), "\n") + 1; n != 3 {
		t.Errorf("origin has %d commits on the branch, want 3", n)
	}
}

// TestIssuePROpensMirrorIssue: github-issue-pr passes the mirror issue's
// title and body and stores its number.
func TestIssuePROpensMirrorIssue(t *testing.T) {
	h := vcsHarness(t, vcs.KindGitHubIssuePR, nil)
	is := h.yt.addIssue("Mirror me", "Open")
	h.yt.issue(is.Key).Description = "The whole story."
	h.ag.script(fixTurn("Done."))
	h.start()
	run := h.waitReview(1, is.Key)
	opens := h.vcs.openLog()
	if len(opens) != 1 {
		t.Fatalf("opens = %+v", opens)
	}
	o := opens[0]
	ib := text.StripHeader(o.IssueBody)
	if o.IssueTitle != is.Key+" Mirror me" || !strings.Contains(ib, "Mirrors the YouTrack issue ["+is.Key+"](") || !strings.Contains(ib, "The whole story.") {
		t.Errorf("issue = %q / %q", o.IssueTitle, o.IssueBody)
	}
	if _, ok := text.ParseMarker(o.IssueBody); !ok {
		t.Errorf("the issue body is not marked: %q", o.IssueBody)
	}
	if run.GHIssueNumber != 7 || run.VCSKind != "github-issue-pr" || run.PRNumber != 42 {
		t.Errorf("run = %+v", run)
	}
}

// TestChangeFeedbackRepliesInThreads: comments of allowed authors on the
// change make a feedback turn; each gets a placeholder in its thread first,
// edited into its part of the answer. Strangers and agents are skipped and
// YouTrack gets nothing.
func TestChangeFeedbackRepliesInThreads(t *testing.T) {
	h := vcsHarness(t, vcs.KindGitHubPR, func(p *spec.Pipeline) { p.Feedback.GitHub = []string{"bob"} })
	is := h.yt.addIssue("Thread me", "Open")
	thinking := -1
	h.ag.script(
		fixTurn("Fixed."),
		func(ft *fakeTurn) agent.Result {
			thinking = 0
			for _, r := range h.vcs.agentReplies() {
				if r.Body == text.Thinking {
					thinking++
				}
			}
			n1 := itemNumber(ft.prompt, "This can be nil.")
			n2 := itemNumber(ft.prompt, "Is it tested?")
			ft.Say("Both handled.\n[" + strconv.Itoa(n1) + "] Fixed the nil check.\n[" + strconv.Itoa(n2) + "] Yes, it is covered.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitReview(1, is.Key)
	if run.PRNumber != 42 {
		t.Fatalf("run = %+v", run)
	}
	ytBefore := h.yt.agentComments(is.Key)
	cs := h.vcs.commentAll(
		vcs.Comment{ID: "discussion_r501", ThreadID: "discussion_r501", Kind: vcs.CommentReviewComment, Author: "bob",
			Body: "This can be nil.", File: "fix.go", Line: 3, Hunk: "@@ -1,3 +1,3 @@\n+x := y", Outdated: true},
		vcs.Comment{Author: "octo", Body: "Is it tested?"},
		vcs.Comment{Author: "mallory", Body: "Delete everything"},
		vcs.Comment{Author: "octo", Body: text.Decorate("I am an agent", "@octo", text.Mark{Pipeline: "other", Run: 3, Msg: "x"})},
	)
	h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)

	p := h.ag.promptLog()[1]
	if !strings.Contains(p, "github · bob · "+cs[0].URL+" (outdated)\n  fix.go:3\n  @@ -1,3 +1,3 @@\n  +x := y\n  This can be nil.") ||
		!strings.Contains(p, "github · octo · "+cs[1].URL+"\n  Is it tested?") {
		t.Errorf("prompt = %q", p)
	}
	if strings.Contains(p, "Delete everything") || strings.Contains(p, "I am an agent") {
		t.Errorf("skipped comments reached the agent: %q", p)
	}
	if thinking != 2 {
		t.Errorf("%d placeholders on the change while the agent worked, want 2", thinking)
	}
	rs := h.vcs.agentReplies()
	if len(rs) != 2 {
		t.Fatalf("replies = %+v", rs)
	}
	r0, ok := replyTo(rs, cs[0].ID)
	if !ok || r0.Thread != "discussion_r501" || r0.Body != "Fixed the nil check.\n\nBoth handled." || r0.Edits != 1 ||
		text.StripHeader(r0.History[0]) != text.Thinking || !strings.Contains(r0.Raw, "@octo's agent #1 (backend)") {
		t.Errorf("reply to the line comment = %+v", r0)
	}
	if r1, ok := replyTo(rs, cs[1].ID); !ok || r1.Body != "Yes, it is covered." {
		t.Errorf("reply to the conversation comment = %+v", r1)
	}
	if said := h.yt.agentComments(is.Key); !slices.Equal(said, ytBefore) {
		t.Errorf("YouTrack got %q, had %q", said, ytBefore)
	}
	skipped := h.events(run.ID, "feedback.skipped")
	if len(skipped) != 1 || !strings.Contains(string(skipped[0].Data), "mallory") || !strings.Contains(string(skipped[0].Data), "feedback.github") {
		t.Errorf("skipped = %+v", skipped)
	}
	// An agent's own replies coming back from the host are not feedback.
	time.Sleep(100 * time.Millisecond)
	if items, _ := h.st.PendingInbox(context.Background(), run.ID); len(items) != 0 {
		t.Errorf("pending = %+v", items)
	}
}

// TestMixedYouTrackAndChangeBatch: one batch with a YouTrack comment and
// two comments of the change; YouTrack gets its part and the rest, the
// change's comments their parts, or a link to YouTrack.
func TestMixedYouTrackAndChangeBatch(t *testing.T) {
	h := vcsHarness(t, vcs.KindGitHubPR, func(p *spec.Pipeline) { p.Feedback.Debounce = dur(400 * time.Millisecond) })
	is := h.yt.addIssue("Mix", "Open")
	h.ag.script(
		fixTurn("Fixed."),
		func(ft *fakeTurn) agent.Result {
			yt := itemNumber(ft.prompt, "From YouTrack")
			gh := itemNumber(ft.prompt, "From GitHub")
			ft.Say("Overall note.\n[" + strconv.Itoa(yt) + "] YT part.\n[" + strconv.Itoa(gh) + "] VCS part.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitReview(1, is.Key)
	yc := h.yt.addComment(is.Key, "alice", "From YouTrack")
	cs := h.vcs.commentAll(vcs.Comment{Author: "octo", Body: "From GitHub"}, vcs.Comment{Author: "octo", Body: "Also from GitHub"})
	h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)
	if invs, _ := h.st.Invocations(context.Background(), run.ID); len(invs) != 2 {
		t.Fatalf("invocations = %+v", invs)
	}
	said := h.yt.agentComments(is.Key)
	last := said[len(said)-1]
	if !strings.HasPrefix(last, "Re: ") || !strings.HasSuffix(last, "#focus=Comments-"+yc+".0-0\n\nYT part.\n\nOverall note.") {
		t.Errorf("YouTrack answer = %q", last)
	}
	rs := h.vcs.agentReplies()
	if r, ok := replyTo(rs, cs[0].ID); !ok || r.Body != "VCS part." {
		t.Errorf("reply 1 = %+v", r)
	}
	if r, ok := replyTo(rs, cs[1].ID); !ok || !strings.HasPrefix(r.Body, "Answered in YouTrack: ") || !strings.Contains(r.Body, "#focus=Comments-") {
		t.Errorf("reply 2 = %+v", r)
	}
}

// TestMergedChangeEndsRunDone: a merged change ends the run as done, with
// the done command when the stage is enabled, and removes the worktree.
func TestMergedChangeEndsRunDone(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run("done enabled "+strconv.FormatBool(enabled), func(t *testing.T) {
			h := vcsHarness(t, vcs.KindGitHubPR, func(p *spec.Pipeline) {
				on := enabled
				p.Stages.Done = spec.Stage{Enter: "State Done", Match: "State: Done", Enabled: &on}
			})
			is := h.yt.addIssue("Merge me", "Open")
			h.ag.script(fixTurn("Fixed."))
			h.start()
			run := h.waitReview(1, is.Key)
			h.vcs.setState(vcs.StateMerged)
			run = h.waitStatus(1, store.StatusDone)
			if run.Stage != stageDone {
				t.Errorf("stage = %q", run.Stage)
			}
			h.waitFor("the worktree removal", func() bool {
				_, err := os.Stat(run.Worktree)
				return os.IsNotExist(err)
			})
			if enabled {
				h.waitFor("State Done", func() bool { return h.yt.state(is.Key) == "Done" })
			} else {
				time.Sleep(100 * time.Millisecond)
				if cmds := h.yt.commandLog(); contains(cmds, "State Done") || h.yt.state(is.Key) != "Review" {
					t.Errorf("commands = %q, state %q", cmds, h.yt.state(is.Key))
				}
			}
			if n := h.notes.of(NotifyRunDone); len(n) != 1 || n[0].URL != prURL {
				t.Errorf("run_done = %+v", n)
			}
			if len(h.events(run.ID, "vcs.merged")) != 1 {
				t.Errorf("events = %+v", h.events(run.ID, ""))
			}
		})
	}
}

// TestClosedChangeEndsRunClosed: a change closed without a merge ends the
// run as closed with the closed command; keepWorktree keeps the worktree.
func TestClosedChangeEndsRunClosed(t *testing.T) {
	h := vcsHarness(t, vcs.KindGitHubPR, func(p *spec.Pipeline) { p.Workspace.KeepWorktree = true })
	is := h.yt.addIssue("Close me", "Open")
	h.ag.script(fixTurn("Fixed."))
	h.start()
	h.waitReview(1, is.Key)
	h.vcs.setState(vcs.StateClosed)
	run := h.waitStatus(1, store.StatusClosed)
	h.waitFor("State Closed", func() bool { return h.yt.state(is.Key) == "Closed" })
	h.stop()
	if _, err := os.Stat(run.Worktree); err != nil {
		t.Errorf("keepWorktree: %v", err)
	}
	if len(h.events(run.ID, "vcs.closed")) != 1 {
		t.Errorf("events = %+v", h.events(run.ID, ""))
	}
}

// TestDeletedChangePlaceholderIsPostedAgain: a placeholder on the change
// that a human deleted is written again with the answer.
func TestDeletedChangePlaceholderIsPostedAgain(t *testing.T) {
	h := vcsHarness(t, vcs.KindGitHubPR, nil)
	is := h.yt.addIssue("Delete my reply", "Open")
	started, release := make(chan struct{}), make(chan struct{})
	h.ag.script(
		fixTurn("Fixed."),
		func(ft *fakeTurn) agent.Result {
			close(started)
			<-release
			ft.Say("[1] Done.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitReview(1, is.Key)
	c := h.vcs.comment(vcs.Comment{Author: "octo", Body: "Please rename"})
	<-started
	rs := h.vcs.agentReplies()
	if len(rs) != 1 || rs[0].Body != text.Thinking {
		t.Fatalf("replies = %+v", rs)
	}
	h.vcs.deleteComment(rs[0].ID)
	close(release)
	h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)
	rs2 := h.vcs.agentReplies()
	if len(rs2) != 1 || rs2[0].To != c.ID || rs2[0].Body != "Done." || rs2[0].ID == rs[0].ID {
		t.Errorf("replies = %+v", rs2)
	}
	if len(h.events(run.ID, "vcs.reposted")) != 1 {
		t.Errorf("events = %+v", h.events(run.ID, ""))
	}
	msgs, _ := h.st.Messages(context.Background(), run.ID)
	if !slices.ContainsFunc(msgs, func(m store.Message) bool {
		return m.Source == sourceGitHub && m.ExternalID == rs2[0].ID && m.State == stateDone
	}) {
		t.Errorf("messages = %+v", msgs)
	}
}

// TestChangeRateLimitBackoff: a rate-limited run's change is not polled
// until the host allows it again; its feedback is read after that.
func TestChangeRateLimitBackoff(t *testing.T) {
	h := vcsHarness(t, vcs.KindGitHubPR, nil)
	is := h.yt.addIssue("Slow down", "Open")
	h.ag.script(fixTurn("Fixed."), func(ft *fakeTurn) agent.Result {
		ft.Say("[1] ok")
		return agent.Result{}
	})
	h.start()
	run := h.waitReview(1, is.Key)
	until := time.Now().Add(600 * time.Millisecond)
	h.vcs.limitUntil(until)
	h.vcs.comment(vcs.Comment{Author: "octo", Body: "After the limit"})
	invs := h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)

	var first time.Time
	limited := 0
	for _, c := range h.vcs.callLog() {
		if c.limited {
			limited++
			if first.IsZero() {
				first = c.at
			}
		}
	}
	if limited != 1 {
		t.Errorf("%d rate-limited calls, want 1", limited)
	}
	for _, c := range h.vcs.callLog() {
		if c.at.After(first) && c.at.Before(until) {
			t.Errorf("polled during the backoff: %+v", c)
		}
	}
	if invs[1].StartedAt.Before(until.Add(-time.Millisecond)) {
		t.Errorf("the feedback turn started at %v, before the limit ended at %v", invs[1].StartedAt, until)
	}
	if !strings.Contains(h.ag.promptLog()[1], "After the limit") {
		t.Errorf("prompt = %q", h.ag.promptLog()[1])
	}
	if n := len(h.events(run.ID, "vcs.rate_limited")); n != 1 {
		t.Errorf("%d rate-limit events", n)
	}
}

// TestRestartWithOpenChange: a new runner follows the change of a run in
// review from the saved cursor, without opening it again or reading old
// comments again.
func TestRestartWithOpenChange(t *testing.T) {
	h := vcsHarness(t, vcs.KindGitHubPR, nil)
	is := h.yt.addIssue("Restart me", "Open")
	h.ag.script(
		fixTurn("Fixed."),
		func(ft *fakeTurn) agent.Result {
			ft.Say("[1] First.")
			return agent.Result{}
		},
		func(ft *fakeTurn) agent.Result {
			ft.Say("[1] Second.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitReview(1, is.Key)
	c1 := h.vcs.comment(vcs.Comment{Author: "octo", Body: "One"})
	h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)
	h.stop()

	seen := len(h.vcs.cursorLog())
	c2 := h.vcs.comment(vcs.Comment{Author: "octo", Body: "Two"})
	h.start()
	h.waitInvocations(run.ID, 3)
	h.waitReview(1, is.Key)
	if n := len(h.vcs.openLog()); n != 1 {
		t.Errorf("%d opens", n)
	}
	if p := h.ag.promptLog()[2]; !strings.Contains(p, "Two") || strings.Contains(p, "One") {
		t.Errorf("prompt = %q", p)
	}
	if cur := h.vcs.cursorLog()[seen]; cur["rev"] == "" {
		t.Errorf("the new runner started without the cursor: %v", cur)
	}
	rs := h.vcs.agentReplies()
	if r, ok := replyTo(rs, c1.ID); !ok || r.Body != "First." {
		t.Errorf("reply 1 = %+v", r)
	}
	if r, ok := replyTo(rs, c2.ID); !ok || r.Body != "Second." {
		t.Errorf("reply 2 = %+v", r)
	}
}

// TestChangeQuestionsAndAnswers: after feedback from the change, a question
// and a permission request are posted there too, and comments on the
// change answer them.
func TestChangeQuestionsAndAnswers(t *testing.T) {
	h := vcsHarness(t, vcs.KindGitHubPR, func(p *spec.Pipeline) { p.Feedback.GitHub = []string{"bob"} })
	is := h.yt.addIssue("Ask me", "Open")
	h.ag.script(
		fixTurn("Fixed."),
		func(ft *fakeTurn) agent.Result {
			if _, err := ft.Tool("request_input", map[string]any{"question": "Rename to foo or bar?"}); err != nil {
				ft.Say(err.Error())
			}
			ft.Say("Asked.")
			return agent.Result{}
		},
		func(ft *fakeTurn) agent.Result {
			if strings.Contains(ft.prompt, "Use foo") {
				ft.Say("Renamed to foo.")
			} else {
				ft.Say("No answer in " + ft.prompt)
			}
			return agent.Result{}
		},
		func(ft *fakeTurn) agent.Result {
			ft.Say("[1] answer: " + ft.Ask("make test"))
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitReview(1, is.Key)

	c1 := h.vcs.comment(vcs.Comment{Author: "bob", Body: "Please rename it"})
	run = h.waitStatus(1, store.StatusNeedsInput)
	h.waitFor("the question on the change", func() bool {
		r, ok := replyTo(h.vcs.agentReplies(), c1.ID)
		return ok && strings.Contains(r.Body, "Rename to foo or bar?")
	})
	h.waitFor("the question in YouTrack", func() bool { return contains(h.yt.agentComments(is.Key), "Rename to foo or bar?") })
	if n := h.notes.of(NotifyNeedsInput); len(n) != 1 || !strings.Contains(n[0].URL, "#focus=Comments-") || !strings.Contains(n[0].Body, "Rename to foo or bar?") {
		t.Errorf("needs_input = %+v", n)
	}

	c2 := h.vcs.comment(vcs.Comment{Author: "bob", Body: "Use foo"})
	h.waitInvocations(run.ID, 3)
	h.waitReview(1, is.Key)
	if invs, _ := h.st.Invocations(context.Background(), run.ID); invs[2].Kind != kindAnswer {
		t.Errorf("kind = %q", invs[2].Kind)
	}
	if r, ok := replyTo(h.vcs.agentReplies(), c2.ID); !ok || r.Body != "Renamed to foo." {
		t.Errorf("answer reply = %+v", r)
	}

	c3 := h.vcs.comment(vcs.Comment{Author: "bob", Body: "Run the tests"})
	h.waitStatus(1, store.StatusNeedsInput)
	h.waitFor("the permission request on the change", func() bool {
		return slices.ContainsFunc(h.vcs.agentReplies(), func(r fakeAgentReply) bool {
			return r.To == "" && strings.Contains(r.Body, "Reply `/approve`")
		})
	})
	h.vcs.comment(vcs.Comment{Author: "bob", Body: "/approve"})
	h.waitInvocations(run.ID, 4)
	h.waitReview(1, is.Key)
	if r, ok := replyTo(h.vcs.agentReplies(), c3.ID); !ok || r.Body != "answer: yes" {
		t.Errorf("permission reply = %+v", r)
	}
	if n := len(h.ag.startLog()); n != 4 {
		t.Errorf("%d turns, want 4", n)
	}
}

// TestCheckStrategy: Check names the remote, the token source and the
// account of a pull request strategy, and fails on a missing token.
func TestCheckStrategy(t *testing.T) {
	h := vcsHarness(t, vcs.KindGitHubPR, nil)
	h.yt.addIssue("Sample", "Open")
	o := h.options()
	res, err := CheckWith(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]CheckResult{}
	for _, r := range res {
		byName[r.Name] = r
	}
	for name, want := range map[string]string{
		"strategy: remote":  "github.test/acme/api (github-pr)",
		"strategy: token":   "from gh",
		"strategy: account": "signed in to github.test as octo",
	} {
		if r := byName[name]; !r.OK || r.Detail != want {
			t.Errorf("%s = %+v, want %q", name, r, want)
		}
	}

	h.vcs.newErr = vcs.ErrNoToken
	res, _ = CheckWith(context.Background(), o)
	if !slices.ContainsFunc(res, func(r CheckResult) bool { return r.Name == "strategy: token" && !r.OK }) {
		t.Errorf("no token failure: %+v", res)
	}

	// The real strategy builder refuses a remote that is not on a code host.
	o.NewStrategy = nil
	res, _ = CheckWith(context.Background(), o)
	if !slices.ContainsFunc(res, func(r CheckResult) bool {
		return r.Name == "strategy: remote" && !r.OK && strings.Contains(r.Detail, "not on a code host")
	}) {
		t.Errorf("no remote failure: %+v", res)
	}

	// The branch strategy has nothing to sign in to.
	h.p.Strategy.Kind = "branch"
	o.Pipeline = h.p
	res, _ = CheckWith(context.Background(), o)
	if !slices.ContainsFunc(res, func(r CheckResult) bool { return r.Name == "strategy" && r.OK }) {
		t.Errorf("branch: %+v", res)
	}
}

// TestNewFailsWithoutStrategy: New refuses a pipeline whose code host
// can't be reached, naming the strategy.
func TestNewFailsWithoutStrategy(t *testing.T) {
	h := vcsHarness(t, vcs.KindGitLabMR, nil)
	h.vcs.newErr = errors.New("no token for gitlab.test")
	if _, err := New(context.Background(), h.options()); err == nil || !strings.Contains(err.Error(), "strategy gitlab-mr") || !strings.Contains(err.Error(), "no token for gitlab.test") {
		t.Errorf("err = %v", err)
	}
	o := h.options()
	o.NewStrategy = nil
	if _, err := New(context.Background(), o); err == nil || !strings.Contains(err.Error(), "not on a code host") {
		t.Errorf("err = %v", err)
	}
}

// TestNotifyRunFailed: a failed turn notifies run_failed with the reason.
func TestNotifyRunFailed(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Fail me", "Open")
	h.ag.script(func(ft *fakeTurn) agent.Result {
		ft.err = errors.New("boom")
		return agent.Result{}
	})
	h.start()
	h.waitStatus(1, store.StatusNeedsInput)
	h.waitFor("the notification", func() bool { return len(h.notes.of(NotifyRunFailed)) == 1 })
	n := h.notes.of(NotifyRunFailed)[0]
	if n.Title != is.Key+" · backend #1: failed" || !strings.Contains(n.Body, "boom") || !strings.Contains(n.URL, "#focus=Comments-") {
		t.Errorf("note = %+v", n)
	}
	if len(h.notes.of(NotifyNeedsInput)) != 0 {
		t.Errorf("needs_input too: %+v", h.notes.of(NotifyNeedsInput))
	}
}
