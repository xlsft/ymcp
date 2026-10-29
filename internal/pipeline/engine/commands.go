package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/mcpserver"
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

// EndedCommand reports whether a reply is a command a run that ended
// takes: /clear or /compact.
func EndedCommand(body string) bool {
	cmd, _ := controlCommand(body)
	return cmd == cmdClear || cmd == cmdCompact
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

// seenMark is what the seen items keep of a comment: its update stamp and
// a hash of its text. A comment that comes back with the same text, such
// as one restored after a /clear, is then not taken as a new message, and
// a command in it does not run twice.
func seenMark(stamp, body string) string {
	sum := sha256.Sum256([]byte(body))
	return stamp + " " + hex.EncodeToString(sum[:8])
}

// sameText reports whether a seen mark was made for a comment with body.
// A mark from before the hash was kept can't tell; for a command it
// counts as the same text, so that no command runs twice.
func sameText(mark, body string) bool {
	_, h, ok := strings.Cut(mark, " ")
	if !ok {
		return isControl(body) || isCommand(body)
	}
	_, want, _ := strings.Cut(seenMark("", body), " ")
	return h == want
}

// controls carries out the commands in the inbox of each active run:
// /cancel and /clear at once, /compact once no turn is running, in the
// background; and /clear and /compact for the runs that ended (see
// endedRuns).
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
	// A run that ended takes /clear and /compact.
	for _, run := range r.endedRuns(ctx) {
		if ctx.Err() != nil {
			return
		}
		items, err := r.st.PendingInbox(ctx, run.ID)
		if err != nil {
			continue
		}
		for _, it := range items {
			cmd, _ := controlCommand(it.Body)
			if !EndedCommand(it.Body) || !r.consumeCommand(ctx, run, it, cmd) {
				continue
			}
			if cmd == cmdClear {
				r.clearRun(ctx, run, it.Author)
				continue
			}
			r.wg.Add(1)
			go func(run store.Run, author string) {
				defer r.wg.Done()
				r.compactRun(ctx, run, author)
			}(run, it.Author)
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
	claimed, err := r.st.ClaimInbox(ctx, it.ID, inv.ID)
	if err != nil || !claimed {
		if err != nil {
			r.log.Warn("consuming the inbox failed", "run", run.Number, "err", err)
		}
		inv.EndReason = cmd + " by " + it.Author + ": already carried out"
		if err := r.st.EndInvocation(ctx, inv); err != nil {
			r.log.Warn("ending an invocation failed", "run", run.Number, "err", err)
		}
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
	reason := "Canceled by " + author + " with /cancel" + because(why)
	r.cancelRun(ctx, w, reason, "Canceled by "+author+because(why))
	if _, err := r.publish(ctx, run, nil, msgNotice, stateDone, "**Canceled** by "+author+because(why)); err != nil {
		r.warnUnlessGone("publishing the cancel failed", run, err)
	}
	if st := r.p.Stages.Closed; st.Enter != "" {
		r.applyStage(ctx, run, stageClosed, st.Enter)
	}
}

// because ends a sentence with the reason given, if any: ": why." or ".".
func because(why string) string {
	why = strings.TrimRight(strings.TrimSpace(why), ".")
	if why == "" {
		return "."
	}
	return ": " + why + "."
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
//
// A /compact while one is still writing the summary of the same run is
// the same request: it is not carried out twice.
func (r *Runner) compactRun(ctx context.Context, run store.Run, author string) {
	r.mu.Lock()
	busy := r.compacting[run.ID]
	r.compacting[run.ID] = true
	r.mu.Unlock()
	if busy {
		r.log.Info("a compact is already running", "run", run.Number, "author", author)
		r.event(ctx, run.ID, "compact.skipped", map[string]string{"by": author})
		return
	}
	defer func() {
		r.mu.Lock()
		delete(r.compacting, run.ID)
		r.mu.Unlock()
	}()
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
	opts := agent.StartOptions{Dir: r.p.Workspace.Dir, Env: r.p.Agent.Env, Model: r.p.Agent.Model}
	if r.p.Agent.UsesEndpoint() {
		token := r.mcp.Grant(mcpserver.RunInfo{ID: run.ID, Number: run.Number, Pipeline: r.p.Name, Mention: r.mention()})
		defer r.mcp.Revoke(token)
		if opts.Endpoint, err = r.agentEndpoint(ctx, token); err != nil {
			return "", fmt.Errorf("the model API: %w", err)
		}
	}
	sess, err := r.agent.Start(ctx, opts)
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
