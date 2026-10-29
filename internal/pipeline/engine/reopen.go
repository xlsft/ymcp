package engine

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
)

// stateLeft marks a run that ended as done whose issue has been outside
// the working, review and needs-input stages since: moving it back into
// one of them reopens the run. An issue still in review when its change
// was merged has not left, so it doesn't reopen the run.
const stateLeft = "left"

// reopenWithin is how long after a run ended as done moving its issue
// back reopens it; later, the source captures it as a new run.
const reopenWithin = 30 * 24 * time.Hour

// checkReopened reopens the runs that ended as done whose issue was moved
// back into the working, review or needs-input stage, for polls in a row
// (confirmTicks). Only the last run of an issue reopens, and only when no
// other run has the issue.
func (r *Runner) checkReopened(ctx context.Context) {
	var back []string
	needsInput := -1 // the index of the needs-input query in back
	for _, s := range []struct {
		on         bool
		match      string
		needsInput bool
	}{
		{r.p.Stages.Working.On(), r.p.Stages.Working.Match, false},
		{r.p.Stages.Review.On(), r.p.Stages.Review.Match, false},
		{r.p.Stages.NeedsInput.On(), r.p.Stages.NeedsInput.Match, true},
	} {
		if s.on && s.match != "" {
			if s.needsInput {
				needsInput = len(back)
			}
			back = append(back, s.match)
		}
	}
	if len(back) == 0 || ctx.Err() != nil {
		return
	}
	var runs []store.Run
	var ids []string
	for _, run := range r.endedRuns(ctx) {
		if run.Status == store.StatusDone {
			runs = append(runs, run)
			ids = append(ids, run.IssueID)
		}
	}
	for id := range r.reopenTicks {
		if !slices.ContainsFunc(runs, func(run store.Run) bool { return run.ID == id }) {
			delete(r.reopenTicks, id)
		}
	}
	if len(runs) == 0 {
		return
	}
	in, inNeedsInput := map[string]bool{}, map[string]bool{}
	for i, q := range back {
		m, err := r.watch.Members(ctx, q, ids)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("reading stage membership failed", "err", err)
			}
			return
		}
		for id, ok := range m {
			in[id] = in[id] || ok
			if i == needsInput {
				inNeedsInput[id] = inNeedsInput[id] || ok
			}
		}
	}
	for _, run := range runs {
		_, left, err := r.st.RunState(ctx, run.ID, stateLeft)
		if err != nil {
			r.log.Warn("reading a run's state failed", "run", run.Number, "err", err)
			continue
		}
		if !in[run.IssueID] {
			delete(r.reopenTicks, run.ID)
			if !left {
				if err := r.st.SetRunState(ctx, run.ID, stateLeft, "1"); err != nil {
					r.log.Warn("saving a run's state failed", "run", run.Number, "err", err)
				}
			}
			continue
		}
		if !left {
			continue
		}
		if r.reopenTicks[run.ID]++; r.reopenTicks[run.ID] >= confirmTicks {
			delete(r.reopenTicks, run.ID)
			r.reopen(ctx, run, inNeedsInput[run.IssueID])
		}
	}
}

// endedRuns are the runs that ended as done or closed within
// reopenWithin and are the last run of their issue: their issue may still
// be talked about, with /clear and /compact, and a done one reopened.
func (r *Runner) endedRuns(ctx context.Context) []store.Run {
	all, err := r.st.Runs(ctx, r.p.Name, false)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("listing runs failed", "err", err)
		}
		return nil
	}
	last := map[string]store.Run{}
	for _, run := range all {
		if cur, ok := last[run.IssueID]; !ok || run.Number > cur.Number {
			last[run.IssueID] = run
		}
	}
	var out []store.Run
	for _, run := range last {
		if followedAfterEnd(run, r.now()) {
			out = append(out, run)
		}
	}
	slices.SortFunc(out, func(a, b store.Run) int { return int(a.Number - b.Number) })
	return out
}

// reopen brings a run that ended as done back into review: its agent
// session and branch carry on, the worktree is made again for its next
// turn, and a change that was merged or closed is let go, so the next push
// opens a new one. The comments written on the issue since the run ended
// are queued as feedback. An issue moved back into the needs-input stage
// leaves it, as the run is in review.
func (r *Runner) reopen(ctx context.Context, run store.Run, fromNeedsInput bool) {
	forget, merged := false, false
	if r.hasChange(run) {
		ch, err := r.vcs.Change(ctx, run.PRNumber)
		if err != nil {
			r.log.Warn("reading the change of a reopened run failed", "run", run.Number, "err", err)
			return // the next poll tries again
		}
		merged = ch.Merged || ch.State == vcs.StateMerged
		forget = merged || ch.State != vcs.StateOpen
	}
	ended := *run.FinishedAt
	old := run.PRURL
	saved, err := r.st.ReopenRun(context.WithoutCancel(ctx), run.ID, func(run *store.Run) error {
		run.Status, run.Stage, run.NeedsInputReason = store.StatusReview, stageReview, ""
		if forget {
			run.PRNumber, run.PRURL = 0, ""
		}
		return nil
	})
	if errors.Is(err, store.ErrActiveRun) {
		r.log.Info("not reopening a run: its issue has another run", "run", run.Number, "issue", run.IssueKey)
		return
	}
	if err != nil {
		r.log.Warn("reopening a run failed", "run", run.Number, "err", err)
		return
	}
	ctx = context.WithoutCancel(ctx)
	if err := r.st.SetRunState(ctx, saved.ID, stateLeft, ""); err != nil {
		r.log.Warn("saving a run's state failed", "run", saved.Number, "err", err)
	}
	data := map[string]string{"issue": saved.IssueKey}
	if forget {
		data["change"] = old
	}
	r.log.Info("run reopened", "run", saved.Number, "issue", saved.IssueKey)
	r.event(ctx, saved.ID, "run.reopened", data)
	if fromNeedsInput {
		r.leaveNeedsInput(ctx, saved)
	}
	queued := r.queueSince(ctx, saved, ended)
	w := r.startWorker(saved)
	w.leftSource = true
	body := "**Reopened**: the issue was moved back. Comment with what to change, and the agent picks it up."
	if queued > 0 {
		body = "**Reopened**: the issue was moved back. The agent picks up the comments written since the run ended."
	}
	switch {
	case forget && merged:
		body += " Its " + r.changeNoun() + " was merged; the next push opens a new one."
	case forget:
		body += " Its " + r.changeNoun() + " was closed; the next push opens a new one."
	}
	if _, err := r.publish(ctx, saved, nil, msgNotice, stateDone, body); err != nil {
		r.warnUnlessGone("publishing the reopen failed", saved, err)
	}
	w.wakeUp()
}

// followedAfterEnd reports whether the issue of a run that ended is still
// followed, for /clear and /compact and to reopen the run: it ended as done
// or closed within reopenWithin. Only the last run of an issue is.
func followedAfterEnd(run store.Run, now time.Time) bool {
	return (run.Status == store.StatusDone || run.Status == store.StatusClosed) &&
		run.FinishedAt != nil && run.FinishedAt.After(now.Add(-reopenWithin))
}

// queueSince queues the comments written on the issue of run after since,
// as the poll would have had it been following the issue; it returns how
// many it queued.
func (r *Runner) queueSince(ctx context.Context, run store.Run, since time.Time) int {
	cs, err := r.watch.IssueComments(ctx, run.IssueID)
	if err != nil {
		r.log.Warn("reading the comments of a reopened run failed", "run", run.Number, "err", err)
		return 0
	}
	allowed, own := r.feedbackAuthors(), r.ownComments(ctx, run)
	n := 0
	err = r.st.Tx(ctx, func(tx *store.Store) error {
		for _, c := range cs {
			at := c.Created
			if !c.Updated.IsZero() {
				at = c.Updated
			}
			if !at.After(since) {
				continue
			}
			queued, err := r.queueComment(ctx, tx, run, c, allowed, own)
			if err != nil {
				return err
			}
			if queued {
				n++
			}
		}
		return nil
	})
	if err != nil {
		r.log.Warn("queuing the comments of a reopened run failed", "run", run.Number, "err", err)
	}
	return n
}
