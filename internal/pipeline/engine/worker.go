package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/mcpserver"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
)

// Stage names stored in store.Run.Stage, as in the pipeline JSON.
const (
	stageWorking    = "working"
	stageReview     = "review"
	stageNeedsInput = "needsInput"
	stageDone       = "done"
	stageClosed     = "closed"
)

// Kinds of agent turns (store.Invocation.Kind).
const (
	kindStart    = "start"
	kindFeedback = "feedback"
	kindAnswer   = "answer"
)

// Why a worker stops.
var (
	errShutdown = errors.New("the runner is stopping")
	errCanceled = errors.New("the run was canceled")
	errFinished = errors.New("the run finished")
)

// gitTimeout bounds the git work after a turn.
const gitTimeout = 5 * time.Minute

// worker drives one run: it waits for something to do and runs one agent
// turn at a time.
type worker struct {
	r        *Runner
	id       int64
	number   int
	issueID  string
	issueKey string
	token    string // MCP bearer token of the run

	ctx    context.Context
	cancel context.CancelCauseFunc
	wake   chan struct{}
	intr   chan string // interrupts the running turn with a reason
	done   chan struct{}

	// Touched only by the polling loop.
	leftSource   bool // the issue has been seen outside the source
	backlogTicks int
	outsideTicks int

	// Touched only by the worker goroutine.
	wt workspace.Worktree

	mu        sync.Mutex
	turning   bool
	questions []string        // request_input calls of the running turn
	approved  map[string]bool // tool titles approved for the run
	retry     []string        // denied tools a later /approve or /reject is about
}

// startWorker returns the run's worker, starting it when there is none.
func (r *Runner) startWorker(run store.Run) *worker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w := r.workers[run.ID]; w != nil {
		return w
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	w := &worker{
		r: r, id: run.ID, number: run.Number, issueID: run.IssueID, issueKey: run.IssueKey,
		ctx: ctx, cancel: cancel,
		wake:     make(chan struct{}, 1),
		intr:     make(chan string, 1),
		done:     make(chan struct{}),
		approved: map[string]bool{},
	}
	w.token = r.mcp.Grant(mcpserver.RunInfo{ID: run.ID, Number: run.Number, Pipeline: r.p.Name, Mention: r.mention()})
	r.workers[run.ID] = w
	r.wg.Add(1)
	go w.loop()
	return w
}

// wakeUp asks the worker to look at its run again.
func (w *worker) wakeUp() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// stop ends the worker; a running turn is cancelled.
func (w *worker) stop(cause error) { w.cancel(cause) }

// interrupt stops the running turn with reason; it reports false when no
// turn is running.
func (w *worker) interrupt(reason string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.turning {
		return false
	}
	select {
	case w.intr <- reason:
	default:
	}
	return true
}

func (w *worker) loop() {
	r := w.r
	defer r.wg.Done()
	defer close(w.done)
	defer w.exit()
	for w.ctx.Err() == nil {
		w.step()
		select {
		case <-w.ctx.Done():
		case <-w.wake:
		}
	}
}

// exit revokes the run's MCP access, forgets the worker and, for a run
// that finished, removes its worktree unless the pipeline keeps it.
func (w *worker) exit() {
	r := w.r
	r.mcp.Revoke(w.token)
	r.mu.Lock()
	delete(r.workers, w.id)
	r.mu.Unlock()
	if !errors.Is(context.Cause(w.ctx), errFinished) || r.p.Workspace.KeepWorktree {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	run, err := r.st.Run(ctx, w.id)
	if err != nil || run.Worktree == "" {
		return
	}
	if err := workspace.Remove(ctx, r.host, r.p.Workspace.Dir, workspace.Worktree{Path: run.Worktree, Branch: run.Branch, Base: run.Base}); err != nil {
		r.log.Warn("removing the worktree failed", "run", run.Number, "err", err)
		return
	}
	r.event(ctx, run.ID, "worktree.removed", map[string]string{"path": run.Worktree})
}

// step decides what the run needs now and runs a turn when it is due.
func (w *worker) step() {
	r := w.r
	ctx := w.ctx
	run, err := r.st.Run(ctx, w.id)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("reading a run failed", "run", w.number, "err", err)
		}
		return
	}
	if run.Status.Final() {
		w.stop(errCanceled)
		return
	}
	var kind string
	var items []store.InboxItem
	switch run.Status {
	case store.StatusWorking:
		if run.AgentSession != "" {
			// A turn was running when the last runner stopped.
			r.interrupted(ctx, run)
			return
		}
		kind = kindStart
	case store.StatusReview, store.StatusNeedsInput:
		pending, err := r.st.PendingInbox(ctx, w.id)
		if err != nil || len(pending) == 0 {
			return
		}
		if r.now().Sub(pending[0].CreatedAt) < r.p.Feedback.Debounce.Duration {
			return // the batch is still growing; the next poll looks again
		}
		switch {
		case run.Status == store.StatusReview:
			kind, items = kindFeedback, pending
		case run.AgentSession == "":
			kind, items = kindStart, pending[:1] // the agent never started, e.g. the working command failed
		default:
			// The first reply answers; the rest is the next batch.
			kind, items = kindAnswer, pending[:1]
		}
	default:
		return
	}
	follow := r.turn(w, run, kind, items, "")
	for follow != "" && ctx.Err() == nil {
		if run, err = r.st.Run(ctx, w.id); err != nil || run.Status.Final() {
			return
		}
		follow = r.turn(w, run, kindAnswer, nil, follow)
	}
}

// interrupted handles a run whose turn the last runner did not finish:
// its placeholder says so and the run waits for a human.
func (r *Runner) interrupted(ctx context.Context, run store.Run) {
	const reason = "The runner stopped while the agent was working, so this turn did not finish."
	m := r.finalizeThinking(ctx, run)
	r.toNeedsInput(ctx, run, reason, needsInputBody(reason), m, true)
}

// finalizeThinking marks placeholders left over from an unfinished turn;
// it returns the newest one so the caller can reuse it.
func (r *Runner) finalizeThinking(ctx context.Context, run store.Run) *store.Message {
	msgs, err := r.st.Messages(ctx, run.ID)
	if err != nil {
		return nil
	}
	var last *store.Message
	for i := range msgs {
		m := msgs[i]
		if m.State != stateThinking || m.Source != sourceYouTrack {
			continue
		}
		if last != nil {
			if _, err := r.publish(ctx, run, last, msgAnswer, stateFailed, "This turn did not finish."); err != nil {
				r.log.Warn("editing a placeholder failed", "run", run.Number, "err", err)
			}
		}
		last = &m
	}
	return last
}

// needsInputBody is the comment for a run that waits for a human.
func needsInputBody(reason string) string {
	return "**Needs input.** " + reason + "\n\nReply with a comment to continue."
}

// setNeedsInput saves the needs_input status with its reason and, when
// applyEnter is set, applies the needsInput stage command. It returns the
// run as saved.
func (r *Runner) setNeedsInput(ctx context.Context, runID int64, reason string, applyEnter bool) (store.Run, error) {
	run, err := r.updateRun(ctx, runID, func(run *store.Run) {
		run.Status, run.NeedsInputReason = store.StatusNeedsInput, reason
	})
	if err != nil {
		return run, err
	}
	r.log.Info("run needs input", "run", run.Number, "issue", run.IssueKey, "reason", firstLine(reason))
	r.event(ctx, runID, "run.needs_input", map[string]string{"reason": trimText(reason, 2000)})
	if st := r.p.Stages.NeedsInput; applyEnter && st.On() && st.Enter != "" {
		r.applyStage(ctx, run, stageNeedsInput, st.Enter)
	}
	return run, nil
}

// toNeedsInput puts the run into needs_input and publishes body: into the
// message m when given, as a new comment otherwise.
func (r *Runner) toNeedsInput(ctx context.Context, run store.Run, reason, body string, m *store.Message, applyEnter bool) {
	saved, err := r.setNeedsInput(ctx, run.ID, reason, applyEnter)
	if err != nil {
		if !errors.Is(err, errFinal) {
			r.log.Warn("saving needs_input failed", "run", run.Number, "err", err)
		}
		return
	}
	kind := msgNotice
	if m != nil {
		kind = m.Kind
	}
	if _, err := r.publish(ctx, saved, m, kind, stateDone, body); err != nil {
		r.log.Warn("publishing the needs-input reason failed", "run", run.Number, "err", err)
	}
}

// leaveNeedsInput applies the needsInput exit command of a run that gets
// an answer.
func (r *Runner) leaveNeedsInput(ctx context.Context, run store.Run) {
	if st := r.p.Stages.NeedsInput; st.On() && st.Exit != "" {
		r.applyStage(ctx, run, stageNeedsInput+".exit", st.Exit)
	}
}

// applyStage applies a stage command whose failure is only noted.
func (r *Runner) applyStage(ctx context.Context, run store.Run, stage, command string) {
	if err := r.watch.Apply(context.WithoutCancel(ctx), command, run.IssueKey); err != nil {
		r.log.Warn("stage command failed", "run", run.Number, "stage", stage, "command", command, "err", err)
		r.event(ctx, run.ID, "stage.command_failed", map[string]string{"stage": stage, "command": command, "error": describeErr(err)})
		return
	}
	r.event(ctx, run.ID, "stage.command", map[string]string{"stage": stage, "command": command})
}

// callbacks receive the pipeline tool calls of the agents.
type callbacks struct{ r *Runner }

// RequestInput records the agent's question; it is published when the
// turn ends.
func (c callbacks) RequestInput(ctx context.Context, runID int64, question string) error {
	w := c.r.worker(runID)
	if w == nil {
		return errors.New("the run is not active")
	}
	w.mu.Lock()
	turning := w.turning
	if turning {
		w.questions = append(w.questions, strings.TrimSpace(question))
	}
	w.mu.Unlock()
	if !turning {
		return errors.New("no agent turn is running for this run")
	}
	c.r.event(ctx, runID, "agent.request_input", map[string]string{"question": trimText(question, 2000)})
	return nil
}

// Report posts the agent's interim status as a separate comment.
func (c callbacks) Report(ctx context.Context, runID int64, body string) error {
	r := c.r
	run, err := r.st.Run(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status.Final() {
		return errors.New("the run has ended")
	}
	if _, err := r.publish(ctx, run, nil, msgReport, stateDone, body); err != nil {
		return err
	}
	r.event(ctx, runID, "agent.report", map[string]string{"text": trimText(body, 2000)})
	return nil
}

// approve remembers tools approved for the run.
func (w *worker) approve(titles []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, t := range titles {
		w.approved[t] = true
	}
}

func (w *worker) isApproved(title string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.approved[title]
}

func (w *worker) approvedTools() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.approved))
	for t := range w.approved {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// setRetry records the denied tools a later /approve or /reject is about.
func (w *worker) setRetry(titles []string) {
	w.mu.Lock()
	w.retry = slices.Clone(titles)
	w.mu.Unlock()
}

func (w *worker) takeRetry() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.retry
	w.retry = nil
	return out
}

func (w *worker) beginTurn() {
	w.mu.Lock()
	w.turning, w.questions = true, nil
	w.mu.Unlock()
	select { // a stale interrupt must not stop the new turn
	case <-w.intr:
	default:
	}
}

func (w *worker) endTurn() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	q := w.questions
	w.turning, w.questions = false, nil
	return q
}

// firstLine is the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// trimText cuts s to n bytes on a rune boundary, marking the cut.
func trimText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "… [trimmed]"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// tail keeps the last lines of the agent's text output.
type tail struct {
	lines   []string
	partial string
}

const tailLines = 20

func (t *tail) add(s string) {
	s = t.partial + s
	parts := strings.Split(s, "\n")
	t.partial = parts[len(parts)-1]
	t.lines = append(t.lines, parts[:len(parts)-1]...)
	if len(t.lines) > tailLines {
		t.lines = t.lines[len(t.lines)-tailLines:]
	}
}

func (t *tail) String() string {
	lines := t.lines
	if t.partial != "" {
		lines = append(slices.Clone(lines), t.partial)
	}
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// codeBlock fences s for a YouTrack comment.
func codeBlock(s string) string {
	return "```\n" + strings.ReplaceAll(s, "```", "'''") + "\n```"
}

// withTail appends the agent's last output to a reason.
func withTail(reason string, t *tail) string {
	if s := t.String(); s != "" {
		return reason + "\n\nLast output:\n" + codeBlock(s)
	}
	return reason
}

// fmtDuration prints a duration without zero units: 10m, 1h30m, 300ms.
func fmtDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
