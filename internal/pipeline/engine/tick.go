package engine

import (
	"context"
	"errors"
	"slices"
	"strconv"

	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// Cursor of the YouTrack comment feed in the store.
const (
	cursorSource = "youtrack"
	cursorKey    = "comments"
)

// confirmTicks is how many polls in a row must agree before a run is
// canceled or paused because of where its issue is: a stage command the
// runner applies between two stage searches must not look like a move.
const confirmTicks = 2

// tick is one round of polling. Every step logs its errors and leaves the
// rest to the next round.
func (r *Runner) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	r.reapFinished(ctx)
	source, captured, ok := r.pollSource(ctx)
	if ok {
		r.checkMembership(ctx, source, captured)
	}
	r.pollComments(ctx)
	r.pollVCS(ctx)
	r.pollCI(ctx)
	r.controls(ctx)
	r.schedule(ctx)
}

// localTick is the part of a tick that reads only the local store: it
// carries out at once what was asked in the TUI or the CLI, a cancel, a
// command or an answer, instead of on the next poll. It runs in the
// polling loop, never beside a tick, so a command is not carried out by
// both; consumeCommand claims it besides.
func (r *Runner) localTick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	r.reapFinished(ctx)
	r.controls(ctx)
	r.schedule(ctx)
}

// reapFinished stops the workers of runs that ended outside the runner,
// e.g. canceled with Cancel.
func (r *Runner) reapFinished(ctx context.Context) {
	for _, w := range r.liveWorkers() {
		run, err := r.st.Run(ctx, w.id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			w.stop(errCanceled)
		case err != nil:
			r.log.Warn("reading a run failed", "run", w.number, "err", err)
		case run.Status.Final():
			r.log.Info("run ended", "run", run.Number, "issue", run.IssueKey, "status", run.Status)
			w.stop(errCanceled)
		}
	}
}

// pollSource reads the source and captures new issues while there is
// room. It returns the issues of the source by id and the runs captured
// now; ok is false when the source could not be read.
func (r *Runner) pollSource(ctx context.Context) (source map[string]ytwatch.Issue, captured map[int64]bool, ok bool) {
	issues, err := r.watch.Search(ctx, r.p.Source.Query)
	var nt *ytwatch.NewTagError
	if errors.As(err, &nt) {
		err = nil // nothing has the tag yet
	}
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("reading the source failed", "err", err)
		}
		return nil, nil, false
	}
	source = make(map[string]ytwatch.Issue, len(issues))
	for _, is := range issues {
		source[is.ID] = is
	}
	for id := range r.suppressed {
		if _, in := source[id]; !in {
			delete(r.suppressed, id)
		}
	}
	for id := range r.held {
		if _, in := source[id]; !in {
			delete(r.held, id)
		}
	}

	active, err := r.st.Runs(ctx, r.p.Name, true)
	if err != nil {
		r.log.Warn("listing runs failed", "err", err)
		return source, nil, true
	}
	busy := 0
	taken := map[string]bool{}
	for _, run := range active {
		taken[run.IssueID] = true
		if run.Status == store.StatusQueued || run.Status == store.StatusWorking {
			busy++
		}
	}
	captured = map[int64]bool{}
	for _, is := range issues {
		if busy >= r.p.Workspace.Parallel || ctx.Err() != nil {
			break
		}
		if taken[is.ID] || r.suppressed[is.ID] && r.stillHeld(ctx, is.ID) {
			continue
		}
		run, err := r.st.CreateRun(ctx, r.p.Name, r.profile, is.ID, is.Key)
		if errors.Is(err, store.ErrActiveRun) {
			// Another pipeline of the profile has it; said once while it
			// stays in the source.
			if !r.held[is.ID] {
				r.held[is.ID] = true
				r.log.Info("not capturing an issue another pipeline's run has", "issue", is.Key)
			}
			continue
		}
		if err != nil {
			r.log.Warn("creating a run failed", "issue", is.Key, "err", err)
			continue
		}
		if err := r.st.MarkIssueSeen(ctx, r.p.Name, is.ID); err != nil {
			r.log.Warn("marking an issue seen failed", "issue", is.Key, "err", err)
		}
		busy++
		captured[run.ID] = true
		r.capture(ctx, run, is.Summary)
	}
	return source, captured, true
}

// stillHeld reports whether a suppressed issue is still held by its run
// canceled from the backlog; it is let go once that run is deleted.
func (r *Runner) stillHeld(ctx context.Context, issueID string) bool {
	run, err := r.st.LastRun(ctx, r.p.Name, issueID)
	switch {
	case err == nil && backlogCanceled(run):
		return true
	case err != nil && !errors.Is(err, store.ErrNotFound):
		r.log.Warn("reading the last run of an issue failed", "issue", issueID, "err", err)
		return true
	}
	delete(r.suppressed, issueID)
	return false
}

// capture moves a new run into the working stage and starts its worker.
// A working command YouTrack refuses puts the run into needs_input.
func (r *Runner) capture(ctx context.Context, run store.Run, summary string) {
	run, err := r.st.ModifyRun(context.WithoutCancel(ctx), run.ID, func(run *store.Run) error {
		run.Stage, run.Status = stageWorking, store.StatusWorking
		run.Agent, run.Host = r.p.Agent.Name, r.host.Name()
		return nil
	})
	if err != nil {
		r.log.Warn("starting a run failed", "issue", run.IssueKey, "err", err)
		return
	}
	r.log.Info("captured an issue", "run", run.Number, "issue", run.IssueKey)
	r.event(ctx, run.ID, "run.captured", map[string]string{"issue": run.IssueKey, "summary": summary})
	if st := r.p.Stages.Working; st.On() && st.Enter != "" {
		if err := r.watch.Apply(ctx, st.Enter, run.IssueKey); err != nil {
			reason := "The working command `" + st.Enter + "` failed: " + describeErr(err)
			r.toNeedsInput(ctx, run, reason, needsInputBody(reason), nil, true)
		} else {
			r.event(ctx, run.ID, "stage.command", map[string]string{"stage": stageWorking, "command": st.Enter})
		}
	}
	r.startWorker(run).wakeUp()
}

// checkMembership compares where the issues of active runs are with the
// pipeline's stages: an issue back in the source cancels its run, one in
// the done or closed stage ends it, a deleted one cancels it, and one in no
// stage pauses it.
func (r *Runner) checkMembership(ctx context.Context, source map[string]ytwatch.Issue, captured map[int64]bool) {
	type tracked struct {
		w   *worker
		run store.Run
	}
	var runs []tracked
	var ids []string
	for _, w := range r.liveWorkers() {
		if captured[w.id] {
			continue
		}
		run, err := r.st.Run(ctx, w.id)
		if err != nil || run.Status.Final() || run.Status == store.StatusQueued {
			continue
		}
		runs = append(runs, tracked{w, run})
		ids = append(ids, run.IssueID)
	}
	if len(runs) == 0 {
		return
	}

	type stageQuery struct {
		name  string
		match string
	}
	var queries []stageQuery
	for _, s := range []struct {
		name string
		on   bool
		q    string
	}{
		{stageWorking, r.p.Stages.Working.On(), r.p.Stages.Working.Match},
		{stageReview, r.p.Stages.Review.On(), r.p.Stages.Review.Match},
		{stageNeedsInput, r.p.Stages.NeedsInput.On(), r.p.Stages.NeedsInput.Match},
		{stageDone, r.p.Stages.Done.On(), r.p.Stages.Done.Match},
		{stageClosed, r.p.Stages.Closed.On(), r.p.Stages.Closed.Match},
	} {
		if s.on && s.q != "" {
			queries = append(queries, stageQuery{s.name, s.q})
		}
	}
	in := map[string]map[string]bool{}
	for _, q := range queries {
		m, err := r.watch.Members(ctx, q.match, ids)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("reading stage membership failed", "stage", q.name, "err", err)
			}
			return
		}
		in[q.name] = m
	}

	for _, t := range runs {
		w, run := t.w, t.run
		if _, inSource := source[run.IssueID]; inSource {
			w.outsideTicks = 0
			if !w.leftSource {
				continue // captured but never left the source, e.g. the working command failed
			}
			if w.backlogTicks++; w.backlogTicks >= confirmTicks {
				r.suppressed[run.IssueID] = true
				r.cancelRun(ctx, w, reasonBacklog, "Canceled: the issue was moved back to the backlog.")
			}
			continue
		}
		w.leftSource, w.backlogTicks = true, 0
		switch {
		case in[stageDone][run.IssueID]:
			r.finishRun(ctx, w, store.StatusDone, stageDone)
			continue
		case in[stageClosed][run.IssueID]:
			r.finishRun(ctx, w, store.StatusClosed, stageClosed)
			continue
		}
		inStage := len(queries) == 0
		for _, name := range []string{stageWorking, stageReview, stageNeedsInput} {
			inStage = inStage || in[name][run.IssueID]
		}
		if inStage {
			w.outsideTicks = 0
			continue
		}
		exists, err := r.watch.Exists(ctx, run.IssueID)
		if err != nil {
			r.log.Warn("checking an issue failed", "issue", run.IssueKey, "err", err)
			continue
		}
		if !exists {
			r.cancelRun(ctx, w, reasonDeleted, "Canceled: the issue was deleted.")
			continue
		}
		if w.outsideTicks++; w.outsideTicks >= confirmTicks && run.Status != store.StatusNeedsInput {
			r.pauseRun(ctx, w, run)
		}
	}
}

// cancelRun ends a run as canceled and stops its agent; when the worker
// exits, the run is cleaned up (see cleanupCanceled). why is the sentence
// the run's change is closed with.
func (r *Runner) cancelRun(ctx context.Context, w *worker, reason, why string) {
	run, err := markCanceled(context.WithoutCancel(ctx), r.st, w.id, reason, why)
	if err != nil {
		if !errors.Is(err, store.ErrRunFinished) {
			r.log.Warn("canceling a run failed", "run", w.number, "err", err)
			return
		}
	} else {
		r.log.Info("run canceled", "run", run.Number, "issue", run.IssueKey, "reason", reason)
	}
	w.stop(errCanceled)
}

// finishRun ends a run as done or closed: its issue was moved to the done
// or closed stage, or its change was merged or closed. The worktree is
// removed unless the pipeline keeps it. It returns the run as saved and
// whether this call ended it.
func (r *Runner) finishRun(ctx context.Context, w *worker, status store.RunStatus, stage string) (store.Run, bool) {
	run, err := r.st.ModifyRun(context.WithoutCancel(ctx), w.id, func(run *store.Run) error {
		run.Status, run.Stage, run.NeedsInputReason = status, stage, ""
		return nil
	})
	if err != nil {
		if !errors.Is(err, store.ErrRunFinished) {
			r.log.Warn("finishing a run failed", "run", w.number, "err", err)
		}
		return run, false
	}
	r.log.Info("run finished", "run", run.Number, "issue", run.IssueKey, "status", status)
	r.event(ctx, w.id, "run.finished", map[string]string{"status": string(status)})
	w.stop(errFinished)
	what, url := string(status), r.issueURL(run)
	if run.PRURL != "" {
		url = run.PRURL
	}
	r.notify(ctx, NotifyRunDone, run, what, "The run ended: "+what+".", url)
	if r.p.AutoCompact {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.compactRun(ctx, run, "auto-compact")
		}()
	}
	return run, true
}

// pauseRun puts a run whose issue left the pipeline's stages into
// needs_input, stopping a running turn. No stage command is applied: a
// human moved the issue on purpose.
func (r *Runner) pauseRun(ctx context.Context, w *worker, run store.Run) {
	const reason = "The issue was moved outside the pipeline's stages."
	if w.interrupt(reason) {
		return // the turn ends and publishes the reason itself
	}
	r.toNeedsInput(ctx, run, reason, needsInputBody(reason), nil, false)
}

// pollComments reads new comments on the issues of active runs and queues
// the ones from allowed authors in the inbox of their run. The cursor and
// the seen marks are saved in one transaction with the inbox items.
func (r *Runner) pollComments(ctx context.Context) {
	workers := r.liveWorkers()
	if len(workers) == 0 || ctx.Err() != nil {
		return
	}
	byIssue := map[string]store.Run{}
	var projects []string
	own := map[string]bool{}
	for _, w := range workers {
		run, err := r.st.Run(ctx, w.id)
		if err != nil || run.Status.Final() {
			continue
		}
		byIssue[run.IssueID] = run
		byIssue[run.IssueKey] = run
		if p := projectOf(run.IssueKey); p != "" && !slices.Contains(projects, p) {
			projects = append(projects, p)
		}
		msgs, err := r.st.Messages(ctx, run.ID)
		if err != nil {
			r.log.Warn("listing messages failed", "run", run.Number, "err", err)
		}
		for _, m := range msgs {
			if m.Source == sourceYouTrack {
				own[m.ExternalID] = true
			}
		}
	}
	if len(projects) == 0 {
		return
	}
	cursor, err := r.st.Cursor(ctx, r.p.Name, cursorSource, cursorKey)
	if err != nil {
		r.log.Warn("reading the comment cursor failed", "err", err)
		return
	}
	items, next, err := r.watch.Comments(ctx, projects, cursor)
	if errors.Is(err, ytwatch.ErrBadCursor) {
		r.log.Warn("YouTrack rejected the comment cursor; starting from now", "err", err)
		if next, err = r.watch.BootstrapCursor(ctx); err == nil {
			err = r.st.SetCursor(ctx, r.p.Name, cursorSource, cursorKey, next)
		}
		if err != nil {
			r.log.Warn("resetting the comment cursor failed", "err", err)
		}
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("reading comments failed", "err", err)
		}
		return
	}

	allowed := map[string]bool{r.me: true}
	for _, l := range r.p.Feedback.YouTrack {
		allowed[l] = true
	}
	var queued []store.Run
	err = r.st.Tx(context.WithoutCancel(ctx), func(tx *store.Store) error {
		for _, c := range items {
			if c.Deleted {
				continue
			}
			run, ok := byIssue[c.IssueID]
			if !ok {
				if run, ok = byIssue[c.IssueKey]; !ok {
					continue
				}
			}
			at := c.Created
			if !c.Updated.IsZero() {
				at = c.Updated
			}
			stamp := strconv.FormatInt(at.UnixMilli(), 10)
			prev, seen, err := tx.Seen(ctx, sourceYouTrack, c.ID)
			if err != nil {
				return err
			}
			mark := seenMark(stamp, c.Text)
			if seen && (prev == stamp || prev == mark) {
				continue
			}
			if seen && sameText(prev, c.Text) {
				// Restored after a /clear, or touched without an edit: not
				// a new message, so a command in it doesn't run again.
				if err := tx.MarkSeen(ctx, sourceYouTrack, c.ID, mark); err != nil {
					return err
				}
				continue
			}
			skip := ""
			switch _, marked := text.ParseMarker(c.Text); {
			case marked || own[c.ID]:
				skip = "agent message"
			case !allowed[c.Author]:
				skip = "author not in feedback.youtrack"
			case at.Before(run.CreatedAt):
				skip = "written before the run"
			}
			if skip == "" {
				ext := c.ID
				if seen {
					ext = c.ID + "@" + stamp // an edit is a new message
				}
				added, err := tx.AddInbox(ctx, store.InboxItem{RunID: run.ID, Source: sourceYouTrack, ExternalID: ext, Author: c.Author, Body: c.Text, URL: c.URL})
				if err != nil {
					return err
				}
				if added {
					queued = append(queued, run)
					if err := tx.AddEvent(ctx, run.ID, "inbox", map[string]string{"source": sourceYouTrack, "author": c.Author, "url": c.URL, "text": trimText(c.Text, 500)}); err != nil {
						return err
					}
				}
			} else if skip != "agent message" {
				if err := tx.AddEvent(ctx, run.ID, "feedback.skipped", map[string]string{"author": c.Author, "url": c.URL, "reason": skip}); err != nil {
					return err
				}
			}
			if err := tx.MarkSeen(ctx, sourceYouTrack, c.ID, mark); err != nil {
				return err
			}
		}
		return tx.SetCursor(ctx, r.p.Name, cursorSource, cursorKey, next)
	})
	if err != nil {
		r.log.Warn("saving comments failed", "err", err)
		return
	}
	for _, run := range queued {
		r.log.Info("feedback queued", "run", run.Number, "issue", run.IssueKey)
	}
}

// schedule wakes the workers of runs with pending inbox items; a worker
// starts a turn once the batch is older than Feedback.Debounce, and answers
// an open permission request at once.
func (r *Runner) schedule(ctx context.Context) {
	for _, w := range r.liveWorkers() {
		items, err := r.st.PendingInbox(ctx, w.id)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("reading the inbox failed", "run", w.number, "err", err)
			}
			continue
		}
		if len(items) > 0 {
			w.wakeUp()
		}
	}
}
