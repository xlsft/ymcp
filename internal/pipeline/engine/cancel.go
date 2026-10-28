package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
)

// stateCanceled is the run state key of a canceled run whose cleanup is
// still due (cancelRecord as JSON). It is written with the cancel, so a
// run canceled while no runner runs is cleaned up when the next one
// starts.
const stateCanceled = "cancel_cleanup"

// Reasons of the runs the runner cancels because of where their issue is.
const (
	reasonDeleted = "The issue was deleted."
	// reasonBacklog: a human moved the issue back to the source to stop
	// the run, so the issue is not captured again while it stays there.
	reasonBacklog = "The issue was moved back to the backlog."
)

// backlogCanceled reports whether a run was canceled because its issue was
// moved back to the backlog.
func backlogCanceled(run store.Run) bool {
	return run.Status == store.StatusCanceled && run.NeedsInputReason == reasonBacklog
}

// cancelRecord is what the cleanup of a canceled run needs.
type cancelRecord struct {
	From string `json:"from"` // the status the run had
	// Why ends "Canceled: …" or "Canceled from …" on the change.
	Why string `json:"why"`
}

// markCanceled ends a run as canceled with reason, and records its cleanup
// in the same transaction. why is the sentence the change is closed with.
// It is store.ErrRunFinished when the run has ended already.
func markCanceled(ctx context.Context, st *store.Store, runID int64, reason, why string) (store.Run, error) {
	var out store.Run
	err := st.Tx(ctx, func(tx *store.Store) error {
		cur, err := tx.Run(ctx, runID)
		if err != nil {
			return err
		}
		if cur.Status.Final() {
			out = cur
			return store.ErrRunFinished
		}
		rec, _ := json.Marshal(cancelRecord{From: string(cur.Status), Why: why})
		cur.Status, cur.NeedsInputReason = store.StatusCanceled, reason
		if err := tx.UpdateRun(ctx, cur); err != nil {
			return err
		}
		if err := tx.SetRunState(ctx, runID, stateCanceled, string(rec)); err != nil {
			return err
		}
		if err := tx.AddEvent(ctx, runID, "run.canceled", map[string]string{"reason": reason}); err != nil {
			return err
		}
		out, err = tx.Run(ctx, runID)
		return err
	})
	return out, err
}

// cleanupCanceled undoes what a canceled run left open: its pull or merge
// request is closed with a comment that says why (the branch stays), the
// needsInput exit command is applied when the run was waiting for a human,
// and its worktree is removed unless the pipeline keeps worktrees; what
// the agent left uncommitted is committed to the branch first. It does
// nothing for a run whose cleanup is not due.
func (r *Runner) cleanupCanceled(ctx context.Context, runID int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitTimeout)
	defer cancel()
	v, ok, err := r.st.RunState(ctx, runID, stateCanceled)
	if err != nil || !ok || v == "" {
		return
	}
	run, err := r.st.Run(ctx, runID)
	if err != nil {
		r.log.Warn("reading a canceled run failed", "run", runID, "err", err)
		return
	}
	var rec cancelRecord
	_ = json.Unmarshal([]byte(v), &rec)
	if run.Status == store.StatusCanceled {
		r.closeCanceledChange(ctx, run, rec)
		if rec.From == string(store.StatusNeedsInput) && run.NeedsInputReason != reasonDeleted {
			r.leaveNeedsInput(ctx, run)
		}
		if !r.removeCanceledWorktree(ctx, run) {
			return // tried again by the next runner
		}
	}
	if err := r.st.SetRunState(ctx, runID, stateCanceled, ""); err != nil {
		r.log.Warn("clearing the cleanup of a canceled run failed", "run", run.Number, "err", err)
	}
}

// closeCanceledChange closes the run's change with a comment.
func (r *Runner) closeCanceledChange(ctx context.Context, run store.Run, rec cancelRecord) {
	if !r.hasChange(run) {
		return
	}
	why := strings.TrimSpace(rec.Why)
	if why == "" {
		why = "Canceled."
	}
	body := "**" + why + "** The branch " + run.Branch + " is kept."
	if _, _, err := r.publishVCS(ctx, run, nil, nil, msgNotice, stateDone, body); err != nil {
		r.log.Warn("commenting on the change of a canceled run failed", "run", run.Number, "err", err)
	}
	if err := r.vcs.Close(ctx, run.PRNumber); err != nil {
		r.log.Warn("closing the change of a canceled run failed", "run", run.Number, "url", run.PRURL, "err", err)
		r.event(ctx, run.ID, "vcs.close_failed", map[string]string{"url": run.PRURL, "error": err.Error()})
		return
	}
	r.log.Info("closed the change of a canceled run", "run", run.Number, "url", run.PRURL)
	r.event(ctx, run.ID, "vcs.closed", map[string]string{"url": run.PRURL, "why": why})
}

// removeCanceledWorktree commits what the agent left in the run's worktree
// and removes it, unless the pipeline keeps worktrees. It reports false
// when the worktree is still there and should be removed later.
func (r *Runner) removeCanceledWorktree(ctx context.Context, run store.Run) bool {
	if r.p.Workspace.KeepWorktree || run.Worktree == "" {
		return true
	}
	wt := workspace.Worktree{Path: run.Worktree, Branch: run.Branch, Base: run.Base}
	ok, err := workspace.Exists(ctx, r.host, wt)
	if err != nil {
		return false
	}
	if ok {
		msg := fmt.Sprintf("%s work left uncommitted when run #%d was canceled", run.IssueKey, run.Number)
		committed, err := workspace.CommitLeftovers(ctx, r.host, wt, strings.TrimSpace(msg))
		if err != nil {
			r.log.Warn("committing the leftovers of a canceled run failed; keeping its worktree", "run", run.Number, "path", run.Worktree, "err", err)
			return true // a human has to look; don't try again
		}
		if committed {
			r.event(ctx, run.ID, "git.commit", map[string]string{"message": msg})
		}
	}
	if err := workspace.Remove(ctx, r.host, r.p.Workspace.Dir, wt); err != nil {
		r.log.Warn("removing the worktree of a canceled run failed", "run", run.Number, "err", err)
		return false
	}
	r.event(ctx, run.ID, "worktree.removed", map[string]string{"path": run.Worktree})
	return true
}

// freshBranch is the branch of a run that has none yet: name, or name-2,
// name-3, … when another run, of this pipeline or another one and in any
// status, has used it, or a worktree has it checked out. A run so never
// takes over the branch, the worktree or the change of another run, such
// as a canceled run of the same issue in another pipeline.
func (r *Runner) freshBranch(ctx context.Context, run store.Run, name string) (string, error) {
	for i := 1; i <= 100; i++ {
		b := name
		if i > 1 {
			b = fmt.Sprintf("%s-%d", name, i)
		}
		used, err := r.st.BranchUsed(ctx, b, run.ID)
		if err != nil {
			return "", err
		}
		if used {
			continue
		}
		path, err := workspace.CheckedOut(ctx, r.host, r.p.Workspace.Dir, b)
		if err != nil {
			return "", err
		}
		if path == "" {
			if b != name {
				r.log.Info("the branch is taken; using another", "run", run.Number, "taken", name, "branch", b)
			}
			return b, nil
		}
	}
	return "", fmt.Errorf("every branch name like %s is taken by another run", name)
}
