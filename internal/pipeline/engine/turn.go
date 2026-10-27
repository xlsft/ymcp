package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// Why a turn was stopped by the runner.
const (
	causeSilence  = "silence"
	causeTotal    = "total"
	causePaused   = "paused"
	causeCanceled = "canceled"
	causeShutdown = "shutdown"
)

// cancelGrace is how long a cancelled turn may take to end before its
// session is closed.
const cancelGrace = 10 * time.Second

// drainTimeout bounds waiting for the events of a closed session.
const drainTimeout = 5 * time.Second

// The tools every agent may call: the whole ymcp MCP server, so the
// YouTrack tools the prompts point to (get_issue, get_issue_comments, …)
// work next to request_input and report. Only claude reads this list (as
// --allowedTools); ACP agents ask through permission requests.
var pipelineTools = []string{"mcp__ymcp"}

// turn is one agent turn of a run.
type turn struct {
	r     *Runner
	w     *worker
	run   store.Run
	kind  string
	items []store.InboxItem
	inv   store.Invocation
	ctx   context.Context // the worker's; ends on cancel or shutdown
	bg    context.Context // for bookkeeping that must outlive ctx

	placeholder *store.Message
	ytAnswer    bool        // the turn answers in YouTrack (not only on the change)
	vcsReplies  []*vcsReply // placeholders answering the change's comments
	opened      string      // URL of the change the turn opened
	vars        text.Vars
	out         tail
	lastTool    string

	buf     strings.Builder // coalesced text chunks not yet recorded
	bufKind agent.Kind

	perms       []agent.Permission // open permission requests, oldest first
	permTimer   *time.Timer
	rejected    []string // tools refused during the turn
	late        []string // tools refused because nobody answered in time
	pauseReason string
}

// turn runs one agent turn and publishes its outcome. It returns the
// prompt of a follow-up turn to run at once, or "".
func (r *Runner) turn(w *worker, run store.Run, kind string, items []store.InboxItem, answer string) (follow string) {
	t := &turn{r: r, w: w, run: run, kind: kind, items: items, ctx: w.ctx, bg: context.WithoutCancel(w.ctx)}
	inv, err := r.st.StartInvocation(t.bg, run.ID, kind)
	if err != nil {
		r.log.Warn("starting an invocation failed", "run", run.Number, "err", err)
		return ""
	}
	t.inv = inv
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	if err := r.st.ConsumeInbox(t.bg, ids, inv.ID); err != nil {
		r.log.Warn("consuming the inbox failed", "run", run.Number, "err", err)
	}
	w.beginTurn()
	defer w.endTurn()
	return t.run1(answer)
}

// end records the end of the invocation.
func (t *turn) end(reason string) {
	t.inv.EndReason = reason
	t.inv.LastOutput = trimText(t.out.String(), 4000)
	if err := t.r.st.EndInvocation(t.bg, t.inv); err != nil {
		t.r.log.Warn("ending an invocation failed", "run", t.run.Number, "err", err)
	}
	t.r.event(t.bg, t.run.ID, "turn.end", map[string]string{"kind": t.kind, "reason": reason})
}

// fail ends the turn with the run waiting for a human.
func (t *turn) fail(endReason, reason string) string {
	t.end(endReason)
	t.needsInput(NotifyRunFailed, reason, needsInputBody(reason), true)
	return ""
}

// needsInput puts the run into needs_input from the turn: body goes into
// the turn's YouTrack placeholder (or a new comment) and into its
// placeholders on the change; a turn without those mirrors body on the
// change when the run's last feedback came from there.
func (t *turn) needsInput(kind, reason, body string, applyEnter bool) {
	t.r.needsInput(t.bg, kind, t.run, reason, body, t.placeholder, applyEnter, len(t.vcsReplies) == 0)
	t.finishVCS(stateDone, body)
}

// gone ends a turn whose issue was deleted.
func (t *turn) gone() string {
	t.end("issue deleted")
	if w := t.w; w != nil {
		t.r.cancelRun(t.bg, w, "The issue was deleted.")
	}
	return ""
}

func (t *turn) run1(answer string) string {
	r, run := t.r, t.run

	// Leave needs_input / review and (re)enter the working stage.
	if run.Status == store.StatusNeedsInput {
		r.leaveNeedsInput(t.bg, run)
	}
	if run.Status != store.StatusWorking {
		if st := r.p.Stages.Working; st.On() && st.Enter != "" {
			if err := r.watch.Apply(t.bg, st.Enter, run.IssueKey); err != nil {
				return t.fail("working command failed", "The working command `"+st.Enter+"` failed: "+describeErr(err))
			}
			r.event(t.bg, run.ID, "stage.command", map[string]string{"stage": stageWorking, "command": st.Enter})
		}
	}
	saved, err := r.st.ModifyRun(t.bg, run.ID, func(run *store.Run) error {
		run.Status, run.Stage, run.NeedsInputReason = store.StatusWorking, stageWorking, ""
		return nil
	})
	if err != nil {
		t.end("run ended")
		return ""
	}
	t.run = saved
	run = saved

	// The placeholders, at once: in YouTrack unless every item of the
	// turn came from the change, and on the change for each of its
	// comments.
	fromChange := 0
	for _, it := range t.items {
		if isVCSSource(it.Source) {
			fromChange++
		}
	}
	t.ytAnswer = !r.hasChange(run) || len(t.items) == 0 || fromChange < len(t.items)
	leftover := r.finalizeThinking(t.bg, run)
	if t.ytAnswer {
		t.placeholder = leftover
		if t.placeholder != nil {
			t.placeholder.Kind = msgAnswer
		}
		m, err := r.publish(t.bg, run, t.placeholder, msgAnswer, stateThinking, text.Working)
		switch {
		case err == nil:
			t.placeholder = &m
		case isNotFound(err):
			if ok, xerr := r.watch.Exists(t.bg, run.IssueID); xerr == nil && !ok {
				return t.gone()
			}
			r.log.Warn("posting the placeholder failed", "run", run.Number, "err", err)
		default:
			r.log.Warn("posting the placeholder failed", "run", run.Number, "err", err)
		}
	} else if leftover != nil {
		if _, err := r.publish(t.bg, run, leftover, msgAnswer, stateFailed, "This turn did not finish."); err != nil {
			r.log.Warn("editing a placeholder failed", "run", run.Number, "err", err)
		}
	}
	t.openVCSPlaceholders()

	issue, err := r.watch.Issue(t.ctx, run.IssueID)
	if err != nil {
		if t.ctx.Err() != nil {
			return t.stopped(causeOf(t.ctx))
		}
		if isNotFound(err) {
			return t.gone()
		}
		return t.fail("issue unreadable", "Reading the issue failed: "+err.Error())
	}
	comments, err := r.comments(t.ctx, run.IssueID)
	if err != nil {
		r.log.Warn("reading comments failed", "run", run.Number, "err", err)
	}
	own := map[string]bool{}
	if msgs, err := r.st.Messages(t.bg, run.ID); err == nil {
		for _, m := range msgs {
			own[m.ExternalID] = true
		}
	}
	t.vars = issueVars(issue)
	t.vars["comments"] = humanComments(comments, own)
	t.vars["run.number"] = fmt.Sprint(run.Number)
	t.vars["pipeline.name"] = r.p.Name
	t.vars["language"] = r.p.Language
	t.vars["pr.url"] = "none"
	if run.PRURL != "" {
		t.vars["pr.url"] = run.PRURL
	}

	if err := t.prepareWorkspace(); err != nil {
		if t.ctx.Err() != nil {
			return t.stopped(causeOf(t.ctx))
		}
		return t.fail("workspace failed", "Preparing the git worktree failed: "+err.Error())
	}

	prompt, system, err := t.prompts(answer)
	if err != nil {
		return t.fail("bad template", "A prompt template of the pipeline is invalid: "+err.Error())
	}

	mcpURL, err := r.agentMCPURL(t.ctx)
	if err != nil {
		if t.ctx.Err() != nil {
			return t.stopped(causeOf(t.ctx))
		}
		return t.fail("agent failed", "The agent could not be given the MCP server: "+err.Error())
	}
	opts := agent.StartOptions{
		Dir:          t.w.wt.Path,
		RunDir:       t.w.runDir,
		Env:          r.p.Agent.Env,
		Model:        r.p.Agent.Model,
		Mode:         r.p.Agent.Mode,
		MCP:          []agent.MCPServer{{Name: "ymcp", URL: mcpURL, Headers: map[string]string{"Authorization": "Bearer " + t.w.token}}},
		SessionID:    run.AgentSession,
		AllowedTools: append(slices.Clone(pipelineTools), t.w.approvedTools()...),
		SystemPrompt: system,
		OnGroup:      func(pgid int) { r.recordGroup(t.bg, run, pgid) },
	}
	res, cause, perr := t.session(opts, prompt, comments, issue)
	return t.outcome(res, cause, perr)
}

// prepareWorkspace creates (or reuses) the run's worktree and fills in the
// workspace variables.
func (t *turn) prepareWorkspace() error {
	r, run, w := t.r, t.run, t.w
	branch := run.Branch
	if branch == "" {
		b, err := text.Render(r.p.Workspace.Branch, t.vars)
		if err != nil {
			return fmt.Errorf("workspace.branch: %w", err)
		}
		branch = b
	}
	prefix, err := text.Render(r.p.Workspace.CommitPrefix, t.vars)
	if err != nil {
		return fmt.Errorf("workspace.commitPrefix: %w", err)
	}
	if w.wt.Path != "" {
		// Someone may have deleted the worktree since the last turn.
		ok, err := workspace.Exists(t.ctx, r.host, w.wt)
		if err != nil {
			return err
		}
		if !ok {
			r.log.Warn("the worktree is gone; creating it again", "run", run.Number, "path", w.wt.Path)
			r.event(t.bg, run.ID, "worktree.missing", map[string]string{"path": w.wt.Path})
			w.wt = workspace.Worktree{}
		}
	}
	if w.wt.Path == "" {
		email, err := workspace.UserEmail(t.ctx, r.host, r.p.Workspace.Dir)
		if err != nil {
			return err
		}
		id := workspace.AgentIdentity(email, r.p.Name, run.Number, r.p.Agent.Name)
		base := run.Base
		if base == "" {
			base = r.p.Workspace.Base
		}
		wt, err := workspace.Create(t.ctx, r.host, r.p.Workspace.Dir, r.p.Workspace.Worktrees, branch, base, id)
		if err != nil {
			return err
		}
		w.wt = wt
		saved, err := r.st.ModifyRun(t.bg, run.ID, func(run *store.Run) error {
			run.Worktree, run.Branch, run.Base, run.Host = wt.Path, wt.Branch, wt.Base, r.host.Name()
			return nil
		})
		if err == nil {
			t.run = saved
		}
		r.event(t.bg, run.ID, "worktree", map[string]string{"path": wt.Path, "branch": wt.Branch, "base": wt.Base})
	}
	if w.runDir == "" {
		dir, err := workspace.RunDir(t.ctx, r.host, r.p.Workspace.Dir, run.Number)
		if err != nil {
			r.log.Warn("creating the run directory failed", "run", run.Number, "err", err)
		}
		w.runDir = dir
	}
	t.vars["branch"] = w.wt.Branch
	t.vars["base"] = w.wt.Base
	t.vars["commit.prefix"] = prefix
	return nil
}

// prompts renders the turn's prompt and the system prompt.
func (t *turn) prompts(answer string) (prompt, system string, err error) {
	r := t.r
	switch t.kind {
	case kindFeedback:
		items := make([]text.FeedbackItem, len(t.items))
		for i, it := range t.items {
			items[i] = text.FeedbackItem{N: i + 1, Source: it.Source, Author: it.Author, URL: it.URL, Body: it.Body}
		}
		for _, rp := range t.vcsReplies {
			it := &items[rp.n-1]
			it.File, it.Line, it.Hunk, it.Outdated = rp.to.File, rp.to.Line, rp.to.Hunk, rp.to.Outdated
		}
		t.vars["feedback"] = text.RenderFeedback(items)
		prompt, err = text.Render(r.prompts.Feedback, t.vars)
	case kindAnswer:
		if answer == "" && len(t.items) > 0 {
			answer = t.answerText(t.items[0].Body)
		}
		t.vars["answer"] = answer
		prompt, err = text.Render(r.prompts.Answer, t.vars)
	default:
		prompt, err = text.Render(r.prompts.Start, t.vars)
	}
	if err != nil {
		return "", "", err
	}
	tmpl := spec.LanguageRule
	if r.p.ServiceEnabled() {
		tmpl = spec.ServiceInstruction
	}
	system, err = text.Render(tmpl, t.vars)
	return prompt, system, err
}

// answerText turns a reply into the answer the agent gets: /approve and
// /reject decide about tools denied earlier.
func (t *turn) answerText(body string) string {
	cmd, rest := command(body)
	if cmd == "" {
		return body // the denied calls wait for a /approve or /reject
	}
	retry := t.w.takeRetry()
	switch {
	case len(retry) == 0:
		return body
	case cmd == "/approve":
		t.w.approve(retry)
		t.r.event(t.bg, t.run.ID, "permission.approved", map[string]any{"tools": retry})
		s := "Approved: " + strings.Join(retry, ", ") + ". Retry what was denied."
		if rest != "" {
			s += "\n\n" + rest
		}
		return s
	default:
		t.r.event(t.bg, t.run.ID, "permission.rejected", map[string]any{"tools": retry})
		s := "Rejected: " + strings.Join(retry, ", ") + ". Continue without them."
		if rest != "" {
			s += "\n\n" + rest
		}
		return s
	}
}

// command splits a leading /approve or /reject off a reply.
func command(body string) (cmd, rest string) {
	f := strings.Fields(body)
	if len(f) == 0 {
		return "", body
	}
	c := strings.ToLower(f[0])
	if c != "/approve" && c != "/reject" {
		return "", body
	}
	return c, strings.TrimSpace(strings.TrimSpace(body)[len(f[0]):])
}

// session starts (or resumes) the agent session and runs the prompt. A
// session that can't be resumed is replaced by a new one that gets a recap.
func (t *turn) session(opts agent.StartOptions, prompt string, comments []comment, issue ytwatch.Issue) (agent.Result, string, error) {
	r := t.r
	for attempt := 0; ; attempt++ {
		sess, err := r.agent.Start(t.ctx, opts)
		if err != nil {
			if errors.Is(err, agent.ErrNoResume) && opts.SessionID != "" && attempt == 0 {
				r.event(t.bg, t.run.ID, "agent.no_resume", map[string]string{"error": err.Error()})
				opts.SessionID = ""
				prompt = t.recap(comments, issue) + prompt
				continue
			}
			if t.ctx.Err() != nil {
				return agent.Result{}, causeOf(t.ctx), nil
			}
			return agent.Result{}, "", fmt.Errorf("starting the agent: %w", err)
		}
		t.saveSession(sess.ID())
		t.started(opts, prompt)
		res, cause, perr := t.drive(sess, prompt)
		sess.Close() // this ends the agent's process group too
		t.drain(sess)
		r.forgetGroup(t.bg, t.run)
		if perr != nil && errors.Is(perr, agent.ErrNoResume) && opts.SessionID != "" && attempt == 0 && cause == "" {
			r.event(t.bg, t.run.ID, "agent.no_resume", map[string]string{"error": perr.Error()})
			opts.SessionID = ""
			prompt = t.recap(comments, issue) + prompt
			continue
		}
		return res, cause, perr
	}
}

// saveSession stores the agent's session id for later turns.
func (t *turn) saveSession(id string) {
	if id == "" || id == t.run.AgentSession {
		return
	}
	saved, err := t.r.st.ModifyRun(t.bg, t.run.ID, func(run *store.Run) error {
		run.AgentSession = id
		return nil
	})
	if err == nil {
		t.run = saved
	}
}

// promptLimit bounds the prompt recorded in a turn.start event.
const promptLimit = 16000

// started records the prompt sent to the agent as it was sent: with the
// recap of a replaced session, and with the service instruction a new
// session gets.
func (t *turn) started(opts agent.StartOptions, prompt string) {
	data := map[string]string{"kind": t.kind, "prompt": trimText(prompt, promptLimit)}
	if opts.SessionID == "" && opts.SystemPrompt != "" {
		data["system"] = trimText(opts.SystemPrompt, promptLimit)
	}
	t.r.event(t.bg, t.run.ID, "turn.start", data)
}

// recap tells a new session what an earlier one did. It repeats the task
// the way the start prompt gave it (the issue, its description and
// comments), then the branch's changes and the earlier answers.
func (t *turn) recap(comments []comment, issue ytwatch.Issue) string {
	r, w := t.r, t.w
	var b strings.Builder
	fmt.Fprintf(&b, "You are continuing earlier work on issue %s (%s) in a new session: the previous session could not be resumed.\n", issue.Key, issue.Summary)
	if t.kind != kindStart {
		if task, err := text.Render(r.prompts.Start, t.vars); err == nil && strings.TrimSpace(task) != "" {
			b.WriteString("\nThe task as it was first given to you:\n---\n" + strings.TrimSpace(task) + "\n---\n\n")
		}
	}
	fmt.Fprintf(&b, "Your branch is %s, created from %s.\n", w.wt.Branch, w.wt.Base)
	out, _, err := r.host.Run(t.ctx, w.wt.Path, []string{"git", "diff", "--stat", "origin/" + w.wt.Base + "...HEAD"}, nil)
	if stat := strings.TrimSpace(string(out)); err == nil && stat != "" {
		b.WriteString("\nChanges on the branch so far:\n" + stat + "\n")
	}
	if prev := r.previousAnswers(comments, t.run.Number); len(prev) > 0 {
		b.WriteString("\nYour previous answers on the issue:\n")
		for _, p := range prev {
			b.WriteString("---\n" + trimText(p, 3000) + "\n")
		}
	}
	b.WriteString("\n---\n\n")
	return b.String()
}

type promptResult struct {
	res agent.Result
	err error
}

// drive runs the prompt and handles the session's events until it ends:
// it records events, enforces the timeouts, answers permission requests
// and stops the turn when the run is canceled, paused or the runner stops.
func (t *turn) drive(sess agent.Session, prompt string) (res agent.Result, cause string, err error) {
	r, w := t.r, t.w
	done := make(chan promptResult, 1)
	go func() {
		res, err := sess.Prompt(context.Background(), prompt)
		done <- promptResult{res, err}
	}()
	silenceAfter := r.p.Timeouts.Silence.Duration
	silence := time.NewTimer(silenceAfter)
	defer silence.Stop()
	total := time.NewTimer(r.p.Timeouts.Total.Duration)
	defer total.Stop()
	defer t.stopPermTimer()
	var grace <-chan time.Time
	stopTurn := func(c string) {
		if cause == "" {
			cause = c
		}
		_ = sess.Cancel()
		if grace == nil {
			grace = time.After(cancelGrace)
		}
	}
	events := sess.Events()
	ctxDone := t.ctx.Done()
	for {
		var permC <-chan time.Time
		if t.permTimer != nil {
			permC = t.permTimer.C
		}
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			t.onEvent(sess, ev)
			if len(t.perms) == 0 {
				resetTimer(silence, silenceAfter)
			}
		case pr := <-done:
			return pr.res, cause, pr.err
		case <-silence.C:
			if len(t.perms) > 0 {
				silence.Reset(silenceAfter) // waiting for a human, not stuck
				continue
			}
			stopTurn(causeSilence)
		case <-total.C:
			stopTurn(causeTotal)
		case <-permC:
			t.permTimer = nil
			t.permissionTimeout(sess)
			if len(t.perms) == 0 {
				resetTimer(silence, silenceAfter)
			}
		case <-w.wake:
			if len(t.perms) > 0 {
				t.permissionAnswers(sess)
				if len(t.perms) == 0 {
					resetTimer(silence, silenceAfter)
				}
			}
		case reason := <-w.intr:
			t.pauseReason = reason
			stopTurn(causePaused)
		case <-ctxDone:
			ctxDone = nil
			stopTurn(causeOf(t.ctx))
		case <-grace:
			grace = make(chan time.Time) // once
			_ = sess.Close()             // Prompt must return now
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	t.Stop()
	select {
	case <-t.C:
	default:
	}
	t.Reset(d)
}

// causeOf maps the worker's stop cause to a turn stop cause.
func causeOf(ctx context.Context) string {
	if errors.Is(context.Cause(ctx), errShutdown) {
		return causeShutdown
	}
	return causeCanceled
}

// drain records the events left after the session closed.
func (t *turn) drain(sess agent.Session) {
	timeout := time.After(drainTimeout)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.flush()
				return
			}
			if ev.Kind != agent.EventPermission {
				t.record(ev)
			}
		case <-timeout:
			t.flush()
			return
		}
	}
}

// onEvent records an event and reacts to it.
func (t *turn) onEvent(sess agent.Session, ev agent.Event) {
	t.record(ev)
	switch ev.Kind {
	case agent.EventSession:
		t.saveSession(ev.SessionID)
	case agent.EventPermission:
		if ev.Permission != nil {
			t.permission(sess, *ev.Permission)
		}
	}
}

// record stores an event in the run's timeline; text chunks are
// coalesced.
func (t *turn) record(ev agent.Event) {
	switch ev.Kind {
	case agent.EventText, agent.EventThought:
		if ev.Kind == agent.EventText {
			t.out.add(ev.Text)
		}
		if t.buf.Len() > 0 && t.bufKind != ev.Kind {
			t.flush()
		}
		t.bufKind = ev.Kind
		t.buf.WriteString(ev.Text)
		if t.buf.Len() >= 4000 {
			t.flush()
		}
		return
	}
	t.flush()
	data := map[string]any{}
	if ev.Text != "" {
		data["text"] = trimText(ev.Text, 2000)
	}
	if tc := ev.Tool; tc != nil {
		data["tool"] = map[string]string{"id": tc.ID, "title": tc.Title, "kind": tc.Kind, "status": tc.Status}
		if ev.Kind == agent.EventToolCall && tc.Title != "" {
			t.lastTool = tc.Title
		}
	}
	if ev.SessionID != "" {
		data["session"] = ev.SessionID
	}
	t.r.event(t.bg, t.run.ID, "agent."+ev.Kind.String(), data)
}

func (t *turn) flush() {
	if t.buf.Len() == 0 {
		return
	}
	t.r.event(t.bg, t.run.ID, "agent."+t.bufKind.String(), map[string]string{"text": trimText(t.buf.String(), 4000)})
	t.buf.Reset()
}

// option picks the option of a permission request of the given kind
// family ("allow" or "reject"), preferring the one-time variant, or with
// always the lasting one.
func option(p agent.Permission, family string, always bool) string {
	kinds := []string{family + "_once", family + "_always"}
	if always {
		kinds[0], kinds[1] = kinds[1], kinds[0]
	}
	for _, k := range kinds {
		for _, o := range p.Options {
			if o.Kind == k {
				return o.ID
			}
		}
	}
	for _, o := range p.Options {
		if strings.HasPrefix(o.Kind, family) {
			return o.ID
		}
	}
	return ""
}

// answerPermission replies to a request; a missing reject option cancels
// the turn's request instead. An allowing answer with always picks the
// option that keeps the tool allowed for the rest of the agent's session,
// when the request offers one.
func (t *turn) answerPermission(sess agent.Session, p agent.Permission, allow, always bool, why string) {
	family := "reject"
	if allow {
		family = "allow"
	}
	id := option(p, family, always)
	var err error
	if id == "" {
		err = fmt.Errorf("the request has no %s option", family)
	} else {
		err = sess.Answer(p.RequestID, id)
	}
	if err != nil {
		t.r.log.Warn("answering a permission request failed", "run", t.run.Number, "err", err)
	}
	if !allow {
		t.rejected = append(t.rejected, p.Tool.Title)
	}
	t.r.event(t.bg, t.run.ID, "permission.answer", map[string]any{"tool": p.Tool.Title, "allow": allow, "why": why})
}

// permission handles a permission request of the agent according to the
// pipeline's permissions setting.
func (t *turn) permission(sess agent.Session, p agent.Permission) {
	switch {
	case ownTool(p.Tool):
		t.answerPermission(sess, p, true, true, "ymcp's own tool")
	case t.w.isApproved(p.Tool.Title):
		t.answerPermission(sess, p, true, false, "approved earlier")
	case t.r.p.Permissions == "allow":
		t.answerPermission(sess, p, true, false, "permissions: allow")
	case t.r.p.Permissions == "deny":
		t.answerPermission(sess, p, false, false, "permissions: deny")
	default:
		t.perms = append(t.perms, p)
		if len(t.perms) == 1 {
			t.openPermission()
		}
	}
}

// ownTool reports whether a permission request is about a tool of ymcp's
// own MCP server (the YouTrack tools and the pipeline's request_input and
// report), which every agent may use like claude does by --allowedTools.
// ACP agents title an MCP call by server and tool, kimi as
// mcp__ymcp__get_issue, opencode as ymcp_get_issue. A request of a kind
// that changes or runs something is never one, whatever its title.
func ownTool(tc agent.ToolCall) bool {
	switch tc.Kind {
	case "", "other", "read", "search", "think":
	default:
		return false
	}
	title := strings.TrimSpace(tc.Title)
	for _, prefix := range []string{"mcp__ymcp__", "ymcp_", "ymcp/", "ymcp:", "ymcp."} {
		if name, ok := strings.CutPrefix(title, prefix); ok {
			name = strings.TrimSpace(name)
			return name != "" && strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_") == ""
		}
	}
	return false
}

// openPermission asks a human about the oldest open request: the run
// waits in needs_input until /approve, /reject or Timeouts.Permission.
func (t *turn) openPermission() {
	r := t.r
	p := t.perms[0]
	wait := r.p.Timeouts.Permission.Duration
	t.permTimer = time.NewTimer(wait)
	what := p.Tool.Title
	if what == "" {
		what = "a tool"
	}
	body := "The agent asks for permission to run **" + what + "**."
	if in := strings.TrimSpace(string(p.Tool.Input)); in != "" {
		body += "\n\n" + codeBlock(trimText(in, 3000))
	}
	body += "\n\nReply `/approve` to allow it or `/reject` to refuse. Without a reply within " + fmtDuration(wait) + " it is refused."
	reason := "The agent asks for permission to run " + what + "."
	saved, err := r.setNeedsInput(t.bg, t.run.ID, reason, true)
	if err != nil {
		return
	}
	t.run = saved
	url := r.issueURL(saved)
	if m, err := r.publish(t.bg, saved, nil, msgPermission, stateDone, body); err != nil {
		r.log.Warn("publishing a permission request failed", "run", t.run.Number, "err", err)
	} else if m.ExternalID != "" {
		url = ytwatch.CommentURL(r.yt.BaseURL(), saved.IssueKey, m.ExternalID)
	}
	r.mirror(t.bg, saved, msgPermission, body)
	r.notify(t.bg, NotifyNeedsInput, saved, "needs permission", reason, url)
}

func (t *turn) stopPermTimer() {
	if t.permTimer != nil {
		t.permTimer.Stop()
		t.permTimer = nil
	}
}

// closePermission drops the oldest request and asks about the next one, or
// returns the run to working when none is left.
func (t *turn) closePermission() {
	t.stopPermTimer()
	t.perms = t.perms[1:]
	if len(t.perms) > 0 {
		t.openPermission()
		return
	}
	r := t.r
	r.leaveNeedsInput(t.bg, t.run)
	if st := r.p.Stages.Working; st.On() && st.Enter != "" {
		r.applyStage(t.bg, t.run, stageWorking, st.Enter)
	}
	saved, err := r.st.ModifyRun(t.bg, t.run.ID, func(run *store.Run) error {
		run.Status, run.NeedsInputReason = store.StatusWorking, ""
		return nil
	})
	if err == nil {
		t.run = saved
	}
}

// permissionAnswers answers open requests from /approve and /reject
// replies in the inbox; other replies wait for the end of the turn.
func (t *turn) permissionAnswers(sess agent.Session) {
	r := t.r
	items, err := r.st.PendingInbox(t.bg, t.run.ID)
	if err != nil {
		return
	}
	for _, it := range items {
		if len(t.perms) == 0 {
			return
		}
		cmd, _ := command(it.Body)
		if cmd == "" {
			continue
		}
		if err := r.st.ConsumeInbox(t.bg, []int64{it.ID}, t.inv.ID); err != nil {
			r.log.Warn("consuming the inbox failed", "run", t.run.Number, "err", err)
		}
		if isVCSSource(it.Source) {
			r.vcsComment(t.bg, it) // forget it
		}
		// An approved tool stays allowed in the agent's session, so it
		// does not ask about it again; a rejection is for this call only.
		approved := cmd == "/approve"
		t.answerPermission(sess, t.perms[0], approved, approved, "answered by "+it.Author)
		t.closePermission()
	}
}

// permissionTimeout refuses a request nobody answered; a later /approve
// remembers the tool for the run and retries.
func (t *turn) permissionTimeout(sess agent.Session) {
	p := t.perms[0]
	t.answerPermission(sess, p, false, false, "no answer within "+fmtDuration(t.r.p.Timeouts.Permission.Duration))
	t.late = append(t.late, p.Tool.Title)
	t.closePermission()
}

// stopped ends a turn the runner stopped for cancel or shutdown.
func (t *turn) stopped(cause string) string {
	switch cause {
	case causeShutdown:
		// The placeholder stays: the next runner says the turn did not finish.
		t.end("runner stopped")
	default:
		t.end("canceled")
		body := "Stopped: the run was canceled."
		if errors.Is(context.Cause(t.ctx), errFinished) {
			body = "Stopped: the run finished."
		}
		if t.placeholder != nil {
			if _, err := t.r.publish(t.bg, t.run, t.placeholder, msgAnswer, stateFailed, body); err != nil {
				t.r.log.Warn("editing the placeholder failed", "run", t.run.Number, "err", err)
			}
		}
		t.finishVCS(stateFailed, body)
	}
	return ""
}

// outcome publishes what the turn did and moves the run on.
func (t *turn) outcome(res agent.Result, cause string, perr error) string {
	r := t.r
	questions := t.w.endTurn()
	answer := strings.TrimSpace(res.Text)
	switch cause {
	case causeShutdown, causeCanceled:
		return t.stopped(cause)
	case causePaused:
		t.end("paused")
		t.needsInput(NotifyNeedsInput, t.pauseReason, needsInputBody(t.pauseReason), false)
		return ""
	case causeSilence, causeTotal:
		var reason string
		if cause == causeSilence {
			reason = "The agent produced no output for " + fmtDuration(r.p.Timeouts.Silence.Duration) + " and was stopped."
			if t.lastTool != "" {
				reason += " Its last tool call: " + t.lastTool + "."
			}
		} else {
			reason = "The agent's turn ran longer than " + fmtDuration(r.p.Timeouts.Total.Duration) + " and was stopped."
		}
		reason = withTail(reason, &t.out)
		if _, err := t.commitAndPush(answer); err != nil {
			reason += "\n\n" + capitalize(err.Error())
		}
		return t.fail("timeout: "+cause, reason)
	}
	if perr != nil {
		msg := "The agent failed: " + perr.Error()
		if dir := t.w.wt.Path; dir != "" {
			if ok, err := r.host.IsDir(t.bg, dir); err == nil && !ok {
				msg = "The agent could not run: its working directory " + dir + " does not exist. The next turn creates it again.\n\n" + perr.Error()
			}
		}
		return t.fail("agent failed", withTail(msg, &t.out))
	}
	switch res.StopReason {
	case agent.StopError, agent.StopRefusal, agent.StopMaxTokens:
		msg := res.Err
		if msg == "" {
			msg = "stop reason " + res.StopReason
		}
		reason := withTail("The agent stopped with an error: "+msg, &t.out)
		if _, err := t.commitAndPush(answer); err != nil {
			reason += "\n\n" + capitalize(err.Error())
		}
		return t.fail(res.StopReason, reason)
	}
	if res.IsError {
		return t.fail("agent error", withTail("The agent reported an error: "+res.Err, &t.out))
	}

	pushed, gitErr := t.commitAndPush(answer)
	if gitErr == nil && pushed {
		gitErr = t.openChange(answer)
	}

	// Tools the agent was not allowed to use: claude reports them after the
	// turn, ACP requests nobody answered in time were refused by us.
	d := t.denials(res)
	denied := len(d.tools) > 0 && r.p.Permissions != "deny"

	if len(questions) > 0 {
		q := strings.Join(questions, "\n\n")
		reason := "The agent asked a question."
		q += t.changeLine()
		if denied {
			// The question goes first; the denied calls wait for a later
			// /approve or /reject, which may come with the answer.
			q += "\n\n---\n\n" + t.keepDenials(d)
		}
		if gitErr != nil {
			q += "\n\n" + capitalize(gitErr.Error())
		}
		t.end("question")
		t.needsInput(NotifyNeedsInput, reason+"\n\n"+q, q, true)
		return ""
	}

	if denied {
		if r.p.Permissions == "allow" && len(t.late) == 0 && len(d.rules) > 0 && !t.allApproved(d.rules) {
			t.w.approve(d.rules)
			r.event(t.bg, t.run.ID, "permission.approved", map[string]any{"tools": d.rules, "why": "permissions: allow"})
			t.publishAnswer(answer, gitErr)
			t.end("denied, retrying")
			return "Approved: " + strings.Join(d.rules, ", ") + ". Retry what was denied."
		}
		t.w.setRetry(d.rules)
		reason := d.text()
		body := reason
		if answer != "" {
			body = answer + t.changeLine() + "\n\n---\n\n**Needs input.** " + reason
		}
		if gitErr != nil {
			body += "\n\n" + capitalize(gitErr.Error())
		}
		t.end("denied")
		t.needsInput(NotifyNeedsInput, reason, body, true)
		return ""
	}
	if res.StopReason == agent.StopCancelled && r.p.Permissions != "deny" {
		return t.fail(agent.StopCancelled, withTail("The agent cancelled its turn.", &t.out))
	}

	t.publishAnswer(answer, nil)
	if gitErr != nil {
		t.end("git failed")
		reason := capitalize(gitErr.Error())
		r.needsInput(t.bg, NotifyRunFailed, t.run, reason, needsInputBody(reason), nil, true, true)
		return ""
	}
	t.end(agent.StopEndTurn)
	t.toReview()
	return ""
}

// capitalize starts s with a capital letter, for an error shown as a
// sentence.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (t *turn) allApproved(tools []string) bool {
	for _, tl := range tools {
		if !t.w.isApproved(tl) {
			return false
		}
	}
	return true
}

// publishAnswer edits the placeholders into the answer. Feedback answers
// are split: each YouTrack item gets its part as a separate reply, each
// comment of the change its part in its placeholder there. A batch with a
// single YouTrack item gets no separate reply: the placeholder becomes the
// reply to it. When every item came from the change, the first comment's
// reply also carries the rest of the answer.
func (t *turn) publishAnswer(answer string, extra error) {
	r := t.r
	if answer == "" {
		answer = "The agent finished without an answer."
	}
	var ns, all []int
	for i, it := range t.items {
		if it.Source == sourceYouTrack {
			ns = append(ns, i+1)
			all = append(all, i+1)
		}
	}
	for _, rp := range t.vcsReplies {
		all = append(all, rp.n)
	}
	slices.Sort(all)
	parts, other := map[int]string{}, answer
	if t.kind == kindFeedback {
		parts, other = text.SplitAnswer(answer, all)
	}
	tail := ""
	if extra != nil {
		tail = "\n\n" + capitalize(extra.Error())
	}

	ytURL := ""
	if t.ytAnswer {
		rest := other
		switch {
		case len(ns) == 1:
			body := other
			if part, ok := parts[ns[0]]; ok {
				body = strings.TrimSpace(part + "\n\n" + other)
			}
			rest = "Re: " + t.items[ns[0]-1].URL + "\n\n" + body
		case len(parts) > 0:
			for _, n := range ns {
				part, ok := parts[n]
				if !ok {
					continue
				}
				it := t.items[n-1]
				body := "Re: " + it.URL + "\n\n" + part
				if _, err := r.publish(t.bg, t.run, nil, msgReply, stateDone, body); err != nil {
					r.log.Warn("publishing a reply failed", "run", t.run.Number, "err", err)
				}
			}
		}
		if strings.TrimSpace(rest) == "" {
			rest = "Answered each comment separately."
		}
		m, err := r.publish(t.bg, t.run, t.placeholder, msgAnswer, stateDone, rest+t.changeLine()+tail)
		if err != nil {
			r.log.Warn("publishing the answer failed", "run", t.run.Number, "err", err)
		} else if m.ExternalID != "" {
			ytURL = ytwatch.CommentURL(r.yt.BaseURL(), t.run.IssueKey, m.ExternalID)
		}
	}

	first := ""
	for i, rp := range t.vcsReplies {
		body, ok := parts[rp.n]
		switch {
		case !t.ytAnswer && i == 0:
			body = strings.TrimSpace(body + "\n\n" + other)
			if body == "" {
				body = answer
			}
			body += tail
		case ok:
		case !t.ytAnswer && first != "":
			body = "See " + first
		case !t.ytAnswer:
			body = answer
		case ytURL != "":
			body = "Answered in YouTrack: " + ytURL
		default:
			body = "Answered in YouTrack: " + r.issueURL(t.run)
		}
		t.writeVCS(rp, stateDone, body)
		if i == 0 {
			first = rp.url
		}
	}
}

// toReview moves the run into review and applies the review command; a
// refused command puts it into needs_input.
func (t *turn) toReview() {
	r := t.r
	saved, err := r.st.ModifyRun(t.bg, t.run.ID, func(run *store.Run) error {
		run.Status, run.Stage, run.NeedsInputReason = store.StatusReview, stageReview, ""
		return nil
	})
	if err != nil {
		return
	}
	t.run = saved
	r.log.Info("run in review", "run", saved.Number, "issue", saved.IssueKey, "branch", saved.Branch)
	r.event(t.bg, saved.ID, "run.review", map[string]string{"branch": saved.Branch})
	url, what := r.issueURL(saved), "on branch "+saved.Branch
	if saved.PRURL != "" {
		url, what = saved.PRURL, "in "+saved.PRURL
	}
	r.notify(t.bg, NotifyReviewReady, saved, "ready for review", "The agent's work is ready for review "+what+".", url)
	if st := r.p.Stages.Review; st.On() && st.Enter != "" {
		if err := r.watch.Apply(t.bg, st.Enter, saved.IssueKey); err != nil {
			reason := "The review command `" + st.Enter + "` failed: " + describeErr(err)
			r.toNeedsInput(t.bg, saved, reason, needsInputBody(reason), nil, true)
			return
		}
		r.event(t.bg, saved.ID, "stage.command", map[string]string{"stage": stageReview, "command": st.Enter})
	}
}

// commitAndPush commits what the agent left uncommitted and pushes the
// branch; pushed says whether the branch is on origin now, the error which
// step failed and how.
func (t *turn) commitAndPush(answer string) (pushed bool, err error) {
	r, w := t.r, t.w
	if w.wt.Path == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(t.bg, gitTimeout)
	defer cancel()
	msg := commitMessage(t.vars["commit.prefix"], answer)
	committed, err := workspace.CommitLeftovers(ctx, r.host, w.wt, msg)
	if err != nil {
		r.event(t.bg, t.run.ID, "git.failed", map[string]string{"step": "commit", "error": err.Error()})
		return false, fmt.Errorf("committing the agent's leftovers failed:\n%s", codeBlock(err.Error()))
	}
	if committed {
		r.event(t.bg, t.run.ID, "git.commit", map[string]string{"message": msg})
	}
	// A branch with nothing beyond its base is not pushed; when counting
	// fails, push anyway.
	ahead, err := workspace.Ahead(ctx, r.host, w.wt)
	if err == nil && ahead == 0 {
		r.event(t.bg, t.run.ID, "git.push_skipped", map[string]string{"branch": w.wt.Branch, "reason": "no commits beyond origin/" + w.wt.Base})
		return false, nil
	}
	if err != nil {
		r.log.Warn("counting the branch's commits failed", "run", t.run.Number, "err", err)
	}
	err = workspace.Push(ctx, r.host, w.wt)
	forced := false
	if err != nil && workspace.NonFastForward(err) {
		// The agent rewrote the branch, e.g. rebased it: push over what the
		// runner pushed last, and only over that.
		if lease, ok, serr := r.st.RunState(t.bg, t.run.ID, statePushedHead); serr == nil && ok && lease != "" {
			r.event(t.bg, t.run.ID, "git.push_rejected", map[string]string{"branch": w.wt.Branch, "error": err.Error(), "lease": lease})
			if err = workspace.ForcePush(ctx, r.host, w.wt, lease); err == nil {
				forced = true
			}
		}
	}
	if err != nil {
		r.event(t.bg, t.run.ID, "git.failed", map[string]string{"step": "push", "error": err.Error()})
		return false, fmt.Errorf("pushing the branch failed:\n%s", codeBlock(err.Error()))
	}
	head, _ := workspace.Head(ctx, r.host, w.wt)
	if head != "" {
		if err := r.st.SetRunState(t.bg, t.run.ID, statePushedHead, head); err != nil {
			r.log.Warn("saving the pushed commit failed", "run", t.run.Number, "err", err)
		}
	}
	data := map[string]string{"branch": w.wt.Branch, "head": head}
	if forced {
		data["forced"] = "with lease"
	}
	r.event(t.bg, t.run.ID, "git.push", data)
	return true, nil
}

// statePushedHead is the run state key of the commit the runner last pushed
// to the run's branch: the lease of a push over a rewritten branch.
const statePushedHead = "pushed_head"

// subjectLimit bounds the subject of a leftover commit, prefix included.
const subjectLimit = 72

// commitMessage is the message of a leftover commit: the prefix and the
// first sentence of the answer, cut at a word boundary to fit
// subjectLimit.
func commitMessage(prefix, answer string) string {
	limit := subjectLimit
	if prefix = strings.TrimSpace(prefix); prefix != "" {
		limit -= utf8.RuneCountInString(prefix) + 1
	}
	subject := commitSubject(answer, max(limit, 24))
	if subject == "" {
		subject = "agent changes"
	}
	return strings.TrimSpace(prefix + " " + subject)
}

// commitSubject is the first sentence of the answer's first line, without
// markdown decoration, cut to at most limit runes at a word boundary. A
// first word longer than limit gives "".
func commitSubject(answer string, limit int) string {
	line := strings.TrimLeft(firstLine(answer), "#*->` ")
	line = strings.TrimSpace(strings.ReplaceAll(line, "**", ""))
	line = firstSentence(line)
	r := []rune(line)
	if len(r) <= limit {
		return line
	}
	cut := string(r[:limit+1]) // the rune after the limit tells whether a word ends there
	i := strings.LastIndexAny(cut, " \t")
	if i <= 0 {
		return ""
	}
	return strings.TrimRight(cut[:i], " \t,;:-–—(")
}

// firstSentence returns s up to the end of its first sentence: '.', '!' or
// '?' followed by a space and an upper-case letter, or by the end of s.
func firstSentence(s string) string {
	r := []rune(s)
	for i, c := range r {
		if c != '.' && c != '!' && c != '?' {
			continue
		}
		if i+1 == len(r) {
			return s
		}
		if i+2 < len(r) && unicode.IsSpace(r[i+1]) && unicode.IsUpper(r[i+2]) {
			return string(r[:i+1])
		}
	}
	return s
}

// denials are the calls of a turn the agent was not allowed to make.
type denials struct {
	tools []string // the tools, for people
	// rules is what /approve allows: for claude the rule of each call
	// (agent.ClaudeRule), so that only that call is allowed, not the tool;
	// for ACP agents the tool titles their permission requests carry.
	rules   []string
	calls   []agent.ToolCall // claude's denied calls, for deniedCalls
	inexact []agent.ToolCall // claude's calls no rule allows exactly
}

// denials collects the calls of the turn that were denied: claude reports
// them after the turn, ACP requests nobody answered in time were refused by
// the runner, and opencode ends a turn as cancelled when one is refused.
func (t *turn) denials(res agent.Result) denials {
	var d denials
	add := func(list *[]string, s string) {
		if s != "" && !slices.Contains(*list, s) {
			*list = append(*list, s)
		}
	}
	if t.r.p.Agent.Provider == "claude" {
		for _, c := range res.Denied {
			if c.Title == "" {
				continue
			}
			add(&d.tools, c.Title)
			d.calls = append(d.calls, c)
			if rule := agent.ClaudeRule(c); rule != "" {
				add(&d.rules, rule)
			} else {
				d.inexact = append(d.inexact, c)
			}
		}
	}
	for _, l := range t.late {
		add(&d.tools, l)
		add(&d.rules, l)
	}
	if res.StopReason == agent.StopCancelled && t.r.p.Permissions != "deny" {
		for _, l := range t.rejected {
			add(&d.tools, l)
			add(&d.rules, l)
		}
	}
	return d
}

// keepDenials records the denied calls of a turn that ends some other way
// than asking about them, so that a later /approve or /reject acts on them,
// and returns the text that tells about them. With permissions: allow they
// are approved at once.
func (t *turn) keepDenials(d denials) string {
	r := t.r
	if r.p.Permissions == "allow" && len(t.late) == 0 && len(d.rules) > 0 {
		t.w.approve(d.rules)
		r.event(t.bg, t.run.ID, "permission.approved", map[string]any{"tools": d.rules, "why": "permissions: allow"})
		return "The agent was not allowed to use: " + strings.Join(d.tools, ", ") + "." + deniedCalls(d.calls) +
			"\n\nWith `permissions: allow` the next turn may use " + codeList(d.rules) + "."
	}
	t.w.setRetry(d.rules)
	return d.text()
}

// text says what was denied and what /approve and /reject do about it.
func (d denials) text() string {
	s := "The agent was not allowed to use: " + strings.Join(d.tools, ", ") + "." + deniedCalls(d.calls)
	if len(d.rules) == 0 {
		return s + "\n\n" + inexactNote(d.inexact) + " Reply to continue."
	}
	s += "\n\nReply `/approve` to allow them for this run and retry, or `/reject` to continue without them."
	if !slices.Equal(d.rules, d.tools) {
		s += " `/approve` allows exactly " + codeList(d.rules) + "."
	}
	if len(d.inexact) > 0 {
		s += " " + inexactNote(d.inexact)
	}
	return s
}

// inexactNote says that some denied calls can't be approved one by one.
func inexactNote(calls []agent.ToolCall) string {
	var tools []string
	for _, c := range calls {
		if !slices.Contains(tools, c.Title) {
			tools = append(tools, c.Title)
		}
	}
	return "The denied " + strings.Join(tools, ", ") + " calls with wildcards, parentheses or without a command or an absolute path can't be allowed one by one, so they stay denied; allow them in the pipeline's agent settings if the agent needs them."
}

// codeList formats items as `a`, `b`.
func codeList(items []string) string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = "`" + oneLineTrim(s, 200) + "`"
	}
	return strings.Join(out, ", ")
}

// deniedCalls lists what the agent tried to do with the tools it was
// denied, so whoever answers sees what /approve allows: the command of a
// shell call, the path of a file tool, else the input itself.
func deniedCalls(calls []agent.ToolCall) string {
	var lines []string
	for _, c := range calls {
		var in map[string]any
		_ = json.Unmarshal(c.Input, &in)
		what := ""
		for _, k := range []string{"command", "file_path", "path", "url", "pattern"} {
			if s, ok := in[k].(string); ok && s != "" {
				what = s
				break
			}
		}
		if what == "" {
			what = strings.TrimSpace(string(c.Input))
		}
		if what == "" {
			continue
		}
		line := "- " + c.Title + ": `" + oneLineTrim(what, 200) + "`"
		if !slices.Contains(lines, line) {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(lines, "\n")
}

// oneLineTrim puts s on one line and cuts it to at most n runes.
func oneLineTrim(s string, n int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "`", "'")), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
