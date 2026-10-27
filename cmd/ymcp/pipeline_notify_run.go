package main

import (
	"context"
	"log/slog"

	"github.com/xlsft/ymcp/internal/notify"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
)

// runnerNotifier connects a runner's notification hook to the notification
// module. A broken notification setup is logged and never stops the
// runner: the hook is then nil.
type runnerNotifier struct {
	n *notify.Notifier
}

func newRunnerNotifier(p spec.Pipeline, logger *slog.Logger) runnerNotifier {
	s, err := notify.Load()
	if err != nil {
		logger.Warn("notifications are off: can't read their settings", "err", err)
		return runnerNotifier{}
	}
	var filter *notify.Filter
	if p.Notifications != nil {
		filter = notify.FilterOf(p.Notifications.Providers, p.Notifications.Events)
	}
	n, err := notify.New(s, filter, logger)
	if err != nil {
		logger.Warn("notifications are off", "err", err)
		return runnerNotifier{}
	}
	n.OnError(func(e notify.Event, provider string, err error) {
		logger.Warn("notification failed", "provider", provider, "event", string(e.Kind), "run", e.Run, "err", err)
	})
	return runnerNotifier{n: n}
}

// hook is engine.Options.Notify.
func (r runnerNotifier) hook() func(context.Context, string, store.Run, string, string, string) {
	if r.n == nil {
		return nil
	}
	return func(_ context.Context, kind string, run store.Run, title, body, url string) {
		r.n.Notify(notify.Event{
			Kind: notify.Kind(kind), Pipeline: run.Pipeline, Run: run.Number, Issue: run.IssueKey,
			Title: title, Body: body, URL: url,
		})
	}
}

// stopped tells that the runner of pipeline ended with err.
func (r runnerNotifier) stopped(pipeline string, err error) {
	if r.n != nil {
		r.n.Notify(notify.Event{Kind: notify.KindRunnerStopped, Pipeline: pipeline, Body: err.Error()})
	}
}

// close waits for notifications still being sent.
func (r runnerNotifier) close() {
	if r.n != nil {
		r.n.Close()
	}
}
