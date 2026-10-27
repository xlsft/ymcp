package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
)

// The commands a reply can be, besides /approve and /reject: the runner
// carries them out itself, the agent never sees them.
const (
	cmdClear   = "/clear"   // hide the run's comments; they stay in the history
	cmdCompact = "/compact" // /clear, then one short summary comment
	cmdCancel  = "/cancel"  // cancel the run and close the issue
)

// msgSummary is the kind of the comment /compact leaves.
const msgSummary = "summary"

// stateHidden is the state of a message /clear hid; publishing into it
// posts a new comment instead.
const stateHidden = "hidden"

// summaryTimeout bounds the agent session that writes a summary.
const summaryTimeout = 5 * time.Minute

// controlCommand splits a leading /clear, /compact or /cancel off a reply.
func controlCommand(body string) (cmd, rest string) {
	f := strings.Fields(body)
	if len(f) == 0 {
		return "", body
	}
	c := strings.ToLower(f[0])
	if c != cmdClear && c != cmdCompact && c != cmdCancel {
		return "", body
	}
	return c, strings.TrimSpace(strings.TrimSpace(body)[len(f[0]):])
}

// isControl reports whether a reply is a command the runner carries out.
func isControl(body string) bool {
	cmd, _ := controlCommand(body)
	return cmd != ""
}

// withoutControls drops the commands from a batch of replies.
func withoutControls(items []store.InboxItem) []store.InboxItem {
	out := items[:0:0]
	for _, it := range items {
		if !isControl(it.Body) {
			out = append(out, it)
		}
	}
	return out
}

// controls carries out the commands in the inbox of each active run:
// /cancel and /clear at once, /compact once no turn is running, in the
// background.
func (r *Runner) controls(ctx context.Context) {
	for _, w := range r.liveWorkers() {
		if ctx.Err() != nil {
			return
		}
		run, err := r.st.Run(ctx, w.id)
		if err != nil || run.Status.Final() {
			continue
		}
		items, err := r.st.PendingInbox(ctx, w.id)
		if err != nil {
			continue
		}
		for _, it := range items {
			cmd, rest := controlCommand(it.Body)
			if cmd == "" || (cmd == cmdCompact && run.Status == store.StatusWorking) {
				continue
			}
			if !r.consumeCommand(ctx, run, it, cmd) {
				continue
			}
			switch cmd {
			case cmdCancel:
				r.cancelByCommand(ctx, w, run, it.Author, rest)
			case cmdClear:
				r.clearRun(ctx, run, it.Author)
			case cmdCompact:
				// The agent writes the summary for a while; polling goes on.
				r.wg.Add(1)
				go func(run store.Run, author string) {
					defer r.wg.Done()
					r.compactRun(ctx, run, author)
				}(run, it.Author)
			}
			if run, err = r.st.Run(ctx, w.id); err != nil || run.Status.Final() {
				break
			}
		}
	}
}

// consumeCommand marks a command as carried out, under an invocation of
// its own so the run's history shows it.
func (r *Runner) consumeCommand(ctx context.Context, run store.Run, it store.InboxItem, cmd string) bool {
	ctx = context.WithoutCancel(ctx)
	inv, err := r.st.StartInvocation(ctx, run.ID, "command")
	if err != nil {
		r.log.Warn("starting an invocation failed", "run", run.Number, "err", err)
		return false
	}
	if err := r.st.ConsumeInbox(ctx, []int64{it.ID}, inv.ID); err != nil {
		r.log.Warn("consuming the inbox failed", "run", run.Number, "err", err)
		return false
	}
	inv.EndReason = cmd + " by " + it.Author
	if err := r.st.EndInvocation(ctx, inv); err != nil {
		r.log.Warn("ending an invocation failed", "run", run.Number, "err", err)
	}
	r.log.Info("reply command", "run", run.Number, "command", cmd, "author", it.Author)
	r.event(ctx, run.ID, "command", map[string]string{"command": cmd, "author": it.Author, "url": it.URL})
	return true
}

// cancelByCommand cancels a run on /cancel: its change is closed as on any
// cancel, and the closed stage's command closes the issue.
func (r *Runner) cancelByCommand(ctx context.Context, w *worker, run store.Run, author, why string) {
	reason := "Canceled by " + author + " with /cancel."
	if why != "" {
		reason += " " + why
	}
	r.cancelRun(ctx, w, reason, "Canceled by "+author+".")
	if _, err := r.publish(ctx, run, nil, msgNotice, stateDone, "**Canceled** by "+author+"."+suffix(why)); err != nil {
		r.warnUnlessGone("publishing the cancel failed", run, err)
	}
	if st := r.p.Stages.Closed; st.Enter != "" {
		r.applyStage(ctx, run, stageClosed, st.Enter)
	}
}

// suffix is " text" for a non-empty text.
func suffix(s string) string {
	if s == "" {
		return ""
	}
	return " " + s
}

// clearRun hides every comment of the run, people's and the agent's, on
// the issue and on its change: YouTrack marks them deleted (they can be
// restored), the code host folds them away. Comments from before the run
// and the placeholders of a running turn stay.
func (r *Runner) clearRun(ctx context.Context, run store.Run, author string) {
	ctx = context.WithoutCancel(ctx)
	live := map[string]bool{}
	hidden := map[string]bool{}
	msgs, err := r.st.Messages(ctx, run.ID)
	if err == nil {
		for _, m := range msgs {
			if m.State == stateThinking {
				live[m.ExternalID] = true
			}
		}
	}
	var yt, ch, kept int
	if cs, err := r.comments(ctx, run.IssueID); err != nil {
		r.warnUnlessGone("reading comments to clear failed", run, err)
	} else {
		for _, c := range cs {
			if live[c.ID] || c.Created.Before(run.CreatedAt) {
				continue
			}
			if err := r.hideComment(ctx, run.IssueID, c.ID); err != nil {
				r.warnUnlessGone("hiding a comment failed", run, err)
				kept++
				continue
			}
			hidden[c.ID] = true
			yt++
		}
	}
	if r.hasChange(run) {
		cs, _, err := r.vcs.Comments(ctx, run.PRNumber, nil)
		if err != nil {
			r.log.Warn("reading the change's comments to clear failed", "run", run.Number, "err", err)
		}
		for _, c := range cs {
			if live[c.ID] {
				continue
			}
			switch err := r.vcs.Hide(ctx, c); {
			case err == nil:
				hidden[c.ID] = true
				ch++
			case errors.Is(err, vcs.ErrGone):
			case errors.Is(err, vcs.ErrCantHide):
				kept++
			default:
				r.log.Warn("hiding a comment on the change failed", "run", run.Number, "id", c.ID, "err", err)
				kept++
			}
		}
	}
	for _, m := range msgs {
		if hidden[m.ExternalID] {
			m.State = stateHidden
			if err := r.st.UpdateMessage(ctx, m); err != nil {
				r.log.Warn("saving a message failed", "run", run.Number, "err", err)
			}
		}
	}
	r.log.Info("comments cleared", "run", run.Number, "youtrack", yt, "change", ch, "kept", kept)
	r.event(ctx, run.ID, "comments.cleared", map[string]any{"by": author, "youtrack": yt, "change": ch, "kept": kept})
}

// compactRun is /compact (and the pipeline's autoCompact when a run ends):
// the agent sums the run up in the pipeline's
// language, the run's comments are hidden, and the summary is left as the
// one comment, on the issue and on the change. When no summary can be
// written, nothing is hidden and a note says why.
func (r *Runner) compactRun(ctx context.Context, run store.Run, author string) {
	summary, err := r.summarize(ctx, run)
	if ctx.Err() != nil {
		return // the runner stops; a stop is no failure to report
	}
	ctx = context.WithoutCancel(ctx)
	if err != nil {
		r.log.Warn("writing a summary failed", "run", run.Number, "err", err)
		r.event(ctx, run.ID, "compact.failed", map[string]string{"error": err.Error()})
		if _, perr := r.publish(ctx, run, nil, msgNotice, stateDone, "**Compact failed.** "+capitalize(err.Error())+" Nothing was hidden."); perr != nil {
			r.warnUnlessGone("publishing the compact failure failed", run, perr)
		}
		return
	}
	r.clearRun(ctx, run, author)
	body := "**Summary**\n\n" + summary + "\n\n" + r.summaryLinks(run)
	if _, err := r.publish(ctx, run, nil, msgSummary, stateDone, body); err != nil {
		r.warnUnlessGone("publishing the summary failed", run, err)
	}
	r.mirror(ctx, run, msgSummary, body+"\nYouTrack: "+r.issueURL(run))
	r.event(ctx, run.ID, "run.compacted", map[string]string{"by": author, "summary": trimText(summary, 4000)})
}

// summaryLinks are the links under a summary: the change and the branch.
func (r *Runner) summaryLinks(run store.Run) string {
	var lines []string
	if run.PRURL != "" {
		lines = append(lines, r.changeNoun()+": "+run.PRURL)
	}
	if run.Branch != "" {
		lines = append(lines, "Branch: `"+run.Branch+"`")
	}
	return strings.Join(lines, "\n")
}

// summaryPrompt asks for the summary; the runner adds the links itself.
const summaryPrompt = "Sum up the work on YouTrack issue %s (%s) for the people who follow it, in %s.\n" +
	"At most five short lines or bullets: what was asked, what was done, where it stands, and what is left or waits for a decision.\n" +
	"Plain Markdown: no heading, no greeting, no signature, no links (they are added for you). Do not use any tools; answer with the summary alone.\n\n" +
	"What happened, oldest first:\n---\n%s\n---"

// summaryInput bounds what the summary prompt quotes of the run.
const summaryInput = 24000

// summarize has the pipeline's agent write a short summary of the run in a
// session of its own, from the run's comments and commits. The session
// gets no tools: every permission it asks for is refused.
func (r *Runner) summarize(ctx context.Context, run store.Run) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, summaryTimeout)
	defer cancel()
	var parts []string
	if cs, err := r.comments(ctx, run.IssueID); err == nil {
		for _, c := range cs {
			if c.Created.Before(run.CreatedAt) || text.IsWorking(c.Text) {
				continue
			}
			who := c.Author
			if _, marked := text.ParseMarker(c.Text); marked {
				who = "the agent"
			}
			parts = append(parts, who+": "+strings.TrimSpace(text.StripHeader(c.Text)))
		}
	}
	if r.hasChange(run) {
		if cs, _, err := r.vcs.Comments(ctx, run.PRNumber, nil); err == nil {
			for _, c := range cs {
				who := c.Author + " on the " + strings.ToLower(r.changeNoun())
				if _, marked := text.ParseMarker(c.Body); marked {
					who = "the agent on the " + strings.ToLower(r.changeNoun())
				}
				if text.IsWorking(c.Body) {
					continue
				}
				parts = append(parts, who+": "+strings.TrimSpace(text.StripHeader(c.Body)))
			}
		}
	}
	if run.Branch != "" && run.Base != "" {
		out, _, err := r.host.Run(ctx, r.p.Workspace.Dir, []string{"git", "log", "--format=- %s", "refs/remotes/origin/" + run.Base + "..refs/remotes/origin/" + run.Branch}, nil)
		if commits := strings.TrimSpace(string(out)); err == nil && commits != "" {
			parts = append(parts, "Commits on "+run.Branch+":\n"+commits)
		}
	}
	thread := strings.Join(parts, "\n\n")
	if len(thread) > summaryInput {
		thread = "…" + thread[len(thread)-summaryInput:]
	}
	issue, err := r.watch.Issue(ctx, run.IssueID)
	if err != nil {
		return "", fmt.Errorf("reading the issue: %w", err)
	}
	lang := r.p.Language
	if lang == "" {
		lang = "English"
	}
	sess, err := r.agent.Start(ctx, agent.StartOptions{Dir: r.p.Workspace.Dir, Env: r.p.Agent.Env, Model: r.p.Agent.Model})
	if err != nil {
		return "", fmt.Errorf("starting the agent: %w", err)
	}
	defer sess.Close()
	go func() {
		for ev := range sess.Events() {
			if ev.Permission != nil {
				_ = sess.Answer(ev.Permission.RequestID, option(*ev.Permission, "reject", false))
			}
		}
	}()
	res, err := sess.Prompt(ctx, fmt.Sprintf(summaryPrompt, issue.Key, issue.Summary, lang, thread))
	switch {
	case err != nil:
		return "", fmt.Errorf("the agent failed: %w", err)
	case res.IsError:
		return "", fmt.Errorf("the agent reported an error: %s", res.Err)
	}
	summary := strings.TrimSpace(res.Text)
	if summary == "" {
		return "", errors.New("the agent wrote no summary")
	}
	return summary, nil
}
