package engine

import (
	"context"
	"errors"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/store"
)

// stopPoll is how often a runner looks for a stop request, and for what
// the TUI or the CLI asked of its runs.
var stopPoll = time.Second

// errStopRequested is the cause with which a stop request ends Run.
var errStopRequested = errors.New("engine: stop requested")

// RequestStop asks the runner of a pipeline to stop, wherever it runs: the
// request goes through the store, so it also works on Windows, which has no
// signal to send. The runner notices within a second or so, stops its
// agents, releases its lock and returns nil from Run. A request made while
// no runner is running does not affect the next one.
func RequestStop(ctx context.Context, st *store.Store, pipeline string) error {
	return st.RequestStop(ctx, pipeline)
}

// ClearStop withdraws a stop request that no runner has honored, e.g. after
// the runner was stopped by other means.
func ClearStop(ctx context.Context, st *store.Store, pipeline string) error {
	return st.ClearStop(ctx, pipeline)
}

// watchStop ends the run when a stop was requested after since.
func (r *Runner) watchStop(ctx context.Context, since time.Time, stop context.CancelCauseFunc) {
	t := time.NewTicker(stopPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		requested, err := r.st.StopRequested(ctx, r.p.Name, since)
		switch {
		case err != nil && ctx.Err() == nil:
			r.log.Warn("reading the stop request failed", "err", err)
		case requested:
			r.log.Info("stop requested")
			if err := r.st.ClearStop(context.WithoutCancel(ctx), r.p.Name); err != nil {
				r.log.Warn("clearing the stop request failed", "err", err)
			}
			stop(errStopRequested)
			return
		}
	}
}

// watchLocal signals local when the store's LocalMark of the pipeline
// changes: an answer, a command or a cancel from the TUI or the CLI that
// the polling loop then acts on at once (see localTick). A change the
// runner made itself only costs a look at the local store.
func (r *Runner) watchLocal(ctx context.Context, local chan<- struct{}) {
	t := time.NewTicker(stopPoll)
	defer t.Stop()
	last, _ := r.st.LocalMark(ctx, r.p.Name)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		mark, err := r.st.LocalMark(ctx, r.p.Name)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("reading the local mark failed", "err", err)
			}
			continue
		}
		if mark == last {
			continue
		}
		last = mark
		select {
		case local <- struct{}{}:
		default: // one is pending already
		}
	}
}
