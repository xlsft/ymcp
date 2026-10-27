package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/xlsft/ymcp/internal/notify"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
)

// recordingProvider keeps what it was asked to send.
type recordingProvider struct{}

var (
	recordedMu sync.Mutex
	recorded   []notify.Event
)

func (recordingProvider) Name() string                    { return "recording" }
func (recordingProvider) Configure(json.RawMessage) error { return nil }
func (recordingProvider) Check(context.Context) error     { return nil }
func (recordingProvider) Send(_ context.Context, e notify.Event) error {
	recordedMu.Lock()
	defer recordedMu.Unlock()
	recorded = append(recorded, e)
	return nil
}

func init() { notify.Register("recording", func() notify.Provider { return recordingProvider{} }) }

func TestRunnerNotifier(t *testing.T) {
	recordedMu.Lock()
	recorded = nil
	recordedMu.Unlock()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	settings := `{"notifications":{"providers":{"recording":{"enabled":true,"events":["needs_input","runner_stopped"]}}}}`
	if err := os.WriteFile(cfg, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YMCP_CONFIG", cfg)

	p := spec.Pipeline{Name: "backend", Notifications: &spec.Notifications{Providers: []string{"recording"}}}
	notes := newRunnerNotifier(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hook := notes.hook()
	if hook == nil {
		t.Fatal("no hook")
	}
	run := store.Run{Pipeline: "backend", Number: 17, IssueKey: "BE-123"}
	hook(context.Background(), "needs_input", run, "BE-123 · backend #17: needs input", "Which date format?", "https://yt/issue/BE-123")
	hook(context.Background(), "run_done", run, "done", "", "") // not in the provider's events
	notes.stopped("backend", errors.New("youtrack: 401"))
	notes.close()

	recordedMu.Lock()
	defer recordedMu.Unlock()
	// Sends are asynchronous, so the order is not fixed.
	got := map[notify.Kind]notify.Event{}
	for _, e := range recorded {
		got[e.Kind] = e
	}
	if len(recorded) != 2 || len(got) != 2 {
		t.Fatalf("recorded = %+v", recorded)
	}
	if e := got[notify.KindNeedsInput]; e.Pipeline != "backend" || e.Run != 17 || e.Issue != "BE-123" ||
		e.Body != "Which date format?" || e.URL != "https://yt/issue/BE-123" {
		t.Errorf("needs input event = %+v", e)
	}
	if e := got[notify.KindRunnerStopped]; e.Body != "youtrack: 401" {
		t.Errorf("stopped event = %+v", e)
	}
}

func TestRunnerNotifierWithoutSettings(t *testing.T) {
	t.Setenv("YMCP_CONFIG", filepath.Join(t.TempDir(), "missing", "config.json"))
	notes := newRunnerNotifier(spec.Pipeline{Name: "x"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	notes.stopped("x", errors.New("boom")) // must not panic
	notes.close()
}
