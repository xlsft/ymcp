// Package views holds the screens of the pipeline TUI: the profiles, the
// pipelines of a profile, a pipeline's board, a run card and the inbox of
// runs that wait for input. Every screen reads the state database; YouTrack
// is read through Backlog (the board's backlog count) and Issues (the run
// card's issue and comments), the code host through Changes.
//
// The run card is a modal (see Overlay): the root app draws it over the
// screen it was opened from.
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
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
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

// Issues reads a run's YouTrack issue and comments on it, as the profile's
// user. issue is a database id (2-127) or a readable id (BE-127).
type Issues interface {
	// Issue returns the issue with its description and custom fields.
	Issue(ctx context.Context, profile, issue string) (ytwatch.Issue, error)
	// Comments lists the issue's comments, oldest first, deleted ones left
	// out; Text is raw, agent markers included.
	Comments(ctx context.Context, profile, issue string) ([]IssueComment, error)
	// AddComment posts a comment and returns it as YouTrack stored it.
	AddComment(ctx context.Context, profile, issue, text string) (IssueComment, error)
}

// IssueComment is a comment on a YouTrack issue.
type IssueComment struct {
	ID         string
	Author     string // login
	AuthorName string // full name, "" when YouTrack has none
	Text       string // raw, with any agent marker
	Created    time.Time
	Updated    time.Time // zero when never edited
}

// Changes reads the conversation of a run's pull or merge request.
type Changes interface {
	Comments(ctx context.Context, run store.Run) ([]vcs.Comment, error)
}

// Overlay is a screen drawn over the one under it, as a modal: the root
// app passes the view of the screen under it.
type Overlay interface {
	Overlay(under string) string
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
	// CopyPipeline saves a pipeline under a new name. Default: a copy of
	// the file, as `ymcp pipeline NAME copy NEW`.
	CopyPipeline func(from, to string) error
	// RenamePipeline gives a pipeline a new name. Default: the rules of
	// `ymcp pipeline NAME rename NEW` (no live runner, no active runs).
	RenamePipeline func(ctx context.Context, st *store.Store, from, to string) error
	// RunnerLog reads the tail of a pipeline's runner log; nil disables
	// the logs screen. The TUI's default runs `ymcp pipeline NAME logs`.
	RunnerLog func(ctx context.Context, pipeline string) (string, error)
	// Answer and Cancel act on a run. Defaults: engine.Answer, engine.Cancel.
	Answer func(ctx context.Context, st *store.Store, pipeline string, run int, text string) error
	Cancel func(ctx context.Context, st *store.Store, pipeline string, run int) error
	// Daemon starts and stops runners; nil disables r and s.
	Daemon Daemon
	// Backlog counts the source issues; nil leaves the backlog column
	// without a count.
	Backlog Backlog
	// Issues reads and comments on the issue of a run card; nil leaves the
	// card without the issue and its comments.
	Issues Issues
	// Changes reads the pull request conversation of a run card; nil
	// leaves the card without the pull request tab.
	Changes Changes
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
	if d.CopyPipeline == nil {
		d.CopyPipeline = copyPipeline
	}
	if d.RenamePipeline == nil {
		d.RenamePipeline = renamePipeline
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
	// OpenRun shows a run card over the screen on top; Answer focuses its
	// reply box.
	OpenRun struct {
		Pipeline string
		Number   int
		Answer   bool
	}
	// OpenInbox shows every run that waits for input.
	OpenInbox struct{}
	// OpenLogs shows a pipeline's runner log.
	OpenLogs struct{ Pipeline string }
	// OpenWizard opens the pipeline wizard: for a new pipeline of Profile
	// when Existing is nil, else to edit Existing.
	OpenWizard struct {
		Profile  string
		Existing *spec.Pipeline
	}
)
