// Package views holds the screens of the pipeline TUI: the profiles, the
// pipelines of a profile, a pipeline's board, a run card and the inbox of
// runs that wait for input. Every screen reads the state database; the
// only YouTrack call is the backlog count of the board, through Backlog.
//
// Screens do not know about each other: they ask the root app to move by
// returning navigation messages (OpenBoard, OpenRun, Back, …). The root app
// sends a Tick every second to the screen on top, which reloads what it
// shows.
package views

import (
	"context"
	"time"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/engine"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// Daemon starts and stops background runners. The TUI's default runs the
// CLI (`ymcp pipeline NAME run -d` / `stop`); output is what the command
// printed, for the status line.
type Daemon interface {
	Start(ctx context.Context, pipeline string) (output string, err error)
	Stop(ctx context.Context, pipeline string) (output string, err error)
}

// Backlog reads a pipeline's source: the issues its query finds now.
type Backlog interface {
	Search(ctx context.Context, profile, query string) ([]ytwatch.Issue, error)
}

// Deps is what the screens read and act through. Store is required; the
// rest default to the real thing when nil (see WithDefaults), and tests
// replace them.
type Deps struct {
	Store *store.Store
	// Profiles lists the YouTrack profiles. Default: config.Profiles.
	Profiles func() ([]config.Config, error)
	// Pipelines lists the pipelines. Default: spec.List.
	Pipelines func() ([]spec.Pipeline, error)
	// DeletePipeline removes a pipeline file. Default: spec.Delete.
	DeletePipeline func(name string) error
	// Answer and Cancel act on a run. Defaults: engine.Answer, engine.Cancel.
	Answer func(ctx context.Context, st *store.Store, pipeline string, run int, text string) error
	Cancel func(ctx context.Context, st *store.Store, pipeline string, run int) error
	// Daemon starts and stops runners; nil disables r and s.
	Daemon Daemon
	// Backlog counts the source issues; nil leaves the backlog column
	// without a count.
	Backlog Backlog
	// OpenURL opens a link in the browser; nil disables o and p.
	OpenURL func(url string) error
	// Now is the clock of the elapsed times. Default: time.Now.
	Now func() time.Time
	// RunnerStale is the heartbeat age after which a runner counts as
	// gone. Default: one minute, as in the CLI.
	RunnerStale time.Duration
}

// WithDefaults fills the unset fields.
func (d Deps) WithDefaults() Deps {
	if d.Profiles == nil {
		d.Profiles = config.Profiles
	}
	if d.Pipelines == nil {
		d.Pipelines = spec.List
	}
	if d.DeletePipeline == nil {
		d.DeletePipeline = spec.Delete
	}
	if d.Answer == nil {
		d.Answer = engine.Answer
	}
	if d.Cancel == nil {
		d.Cancel = engine.Cancel
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.RunnerStale <= 0 {
		d.RunnerStale = time.Minute
	}
	return d
}

// Tick is sent by the root app every second to the screen on top.
type Tick struct{ Now time.Time }

// Notice puts a message on the status line of the screen that gets it;
// the root app sends it to the screen on top.
type Notice struct {
	Text string
	Err  bool
}

// Navigation messages, handled by the root app.
type (
	// Back closes the screen on top.
	Back struct{}
	// OpenPipelines shows the pipelines of a profile.
	OpenPipelines struct{ Profile string }
	// OpenBoard shows a pipeline's board.
	OpenBoard struct{ Pipeline string }
	// OpenRun shows a run card; Answer focuses its answer box.
	OpenRun struct {
		Pipeline string
		Number   int
		Answer   bool
	}
	// OpenInbox shows every run that waits for input.
	OpenInbox struct{}
	// OpenWizard opens the pipeline wizard: for a new pipeline of Profile
	// when Existing is nil, else to edit Existing.
	OpenWizard struct {
		Profile  string
		Existing *spec.Pipeline
	}
)
