package wizard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/notify"
	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/engine"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// Deps are the outside services the wizard talks to. Tests replace them
// with fakes. Every field is required except Strategy (without a detector
// the strategy step lets the user pick one by hand) and Notify (without it
// the notifications step only offers the events).
type Deps struct {
	Local     Local
	YouTrack  YouTrackFactory
	Agents    AgentChecker
	Workspace WorkspaceInspector
	Strategy  StrategyDetector
	Checker   CheckRunner
	Editor    EditorLauncher
	Notify    NotifySettings
	Presets   PresetStore
}

// PresetStore lists the prompt presets and saves the user's own.
type PresetStore interface {
	// List returns the built-in presets, then the user's. Presets that
	// could be read are returned even along with an error.
	List() ([]spec.Preset, error)
	// Save creates a user preset. It fails when the name is invalid or
	// taken, by a built-in preset or one of the user's.
	Save(p spec.Preset) error
}

// Local reads what is stored on this machine: YouTrack profiles and
// pipeline files.
type Local interface {
	Profiles() ([]config.Config, error)
	PipelineExists(name string) (bool, error)
}

// YouTrackFactory signs in to the YouTrack of a profile.
type YouTrackFactory interface {
	Connect(ctx context.Context, profile string) (YouTrack, error)
}

// YouTrack is what the wizard asks YouTrack. NewYouTrack implements it on a
// real client.
type YouTrack interface {
	BaseURL() string
	Me(ctx context.Context) (string, error)
	Search(ctx context.Context, query string) ([]ytwatch.Issue, error)
	Issue(ctx context.Context, key string) (ytwatch.Issue, error)
	Validate(ctx context.Context, command, issueKey string) (ytwatch.CommandCheck, error)
	// SearchAssist completes a search query at caret (POST search/assist).
	SearchAssist(ctx context.Context, query string, caret int) ([]Suggestion, error)
	// CommandAssist completes a command at caret against an issue
	// (POST commands/assist).
	CommandAssist(ctx context.Context, command string, caret int, issueKey string) ([]Suggestion, error)
	// Fields lists the custom fields of a project (by short name) that a
	// stage can be built from.
	Fields(ctx context.Context, project string) ([]Field, error)
	Tags(ctx context.Context) ([]string, error)
	Users(ctx context.Context, query string) ([]User, error)
	SavedSearches(ctx context.Context) ([]SavedSearch, error)
}

// Suggestion is one completion of a query or command, in YouTrack's
// terms: Prefix+Option+Suffix replaces the runes from CompletionStart to
// CompletionEnd, and the caret goes to Caret.
type Suggestion struct {
	Option, Prefix, Suffix, Description string
	CompletionStart, CompletionEnd      int
	Caret                               int
}

// Apply returns text with the suggestion applied and the new caret.
func (s Suggestion) Apply(text string) (string, int) {
	r := []rune(text)
	start := min(max(s.CompletionStart, 0), len(r))
	end := min(max(s.CompletionEnd, start), len(r))
	ins := s.Prefix + s.Option + s.Suffix
	out := string(r[:start]) + ins + string(r[end:])
	caret := s.Caret
	if caret <= 0 || caret > len([]rune(out)) {
		caret = start + len([]rune(ins))
	}
	return out, caret
}

// Field is a custom field of a project with the values a stage can use.
type Field struct {
	Name string
	// Type is state, enum, user, version, build, owned or tag.
	Type   string
	Values []string
}

// User is a YouTrack account.
type User struct{ Login, FullName string }

// SavedSearch is a saved query of the signed-in user.
type SavedSearch struct{ Name, Query string }

// AgentChecker checks an agent configuration: binary, version, warnings.
type AgentChecker interface {
	Check(ctx context.Context, a spec.Agent) (agent.Info, error)
}

// WorkspaceInspector runs workspace.Inspect on a host ("" is this machine).
type WorkspaceInspector interface {
	Inspect(ctx context.Context, host, dir string) ([]workspace.Check, error)
}

// StrategyDetector looks at the origin of a repository and suggests how
// finished work is published.
type StrategyDetector interface {
	Detect(ctx context.Context, host, dir string) (Detection, error)
}

// Detection is what a StrategyDetector found.
type Detection struct {
	// Kind is the suggested strategy kind, e.g. "github-pr"; "" when the
	// hosting is not known.
	Kind string
	// Repo names the repository, e.g. "github.com/xlsft/ymcp".
	Repo string
	// Auth says where the token comes from, or why there is none.
	Auth   string
	AuthOK bool
	Notes  []string
}

// NotifySettings tells which notification providers this machine has.
type NotifySettings interface {
	Providers() ([]NotifyProvider, error)
}

// NotifyProvider is a notification provider as set up on this machine.
type NotifyProvider struct {
	Name    string
	Enabled bool
	Events  []string
}

// CheckRunner verifies a whole pipeline like `ymcp pipeline NAME check`.
type CheckRunner interface {
	Check(ctx context.Context, p spec.Pipeline) ([]engine.CheckResult, error)
}

// EditorLauncher builds the command that opens path in the user's editor.
type EditorLauncher interface {
	Command(path string) *exec.Cmd
}

// DefaultDeps are the real services, except Strategy, which the caller
// sets when it has one (internal/pipeline/vcs).
func DefaultDeps() Deps {
	return Deps{
		Local:     localFiles{},
		YouTrack:  profileConnector{},
		Agents:    agentChecker{},
		Workspace: inspector{},
		Checker:   checkRunner{},
		Editor:    envEditor{},
		Notify:    notifySettings{},
		Presets:   presetFiles{},
	}
}

// presetFiles are the presets of internal/pipeline/spec: the built-in ones
// and the user's files.
type presetFiles struct{}

func (presetFiles) List() ([]spec.Preset, error) { return spec.AllPresets() }

func (presetFiles) Save(p spec.Preset) error {
	taken, err := spec.PresetExists(p.Name)
	if err != nil {
		return err
	}
	if taken {
		return presetTaken(p.Name)
	}
	return spec.SavePreset(p)
}

// presetTaken is the error for a preset name that is in use.
func presetTaken(name string) error {
	if spec.IsBuiltinPreset(name) {
		return fmt.Errorf("%q is a built-in preset; choose another name", name)
	}
	return fmt.Errorf("preset %q already exists; choose another name", name)
}

// notifySettings reads internal/notify's settings; it sees the providers
// the binary registered (cmd/ymcp imports the toast provider).
type notifySettings struct{}

func (notifySettings) Providers() ([]NotifyProvider, error) {
	s, err := notify.Load()
	if err != nil {
		return nil, err
	}
	states, err := s.State()
	if err != nil {
		return nil, err
	}
	out := make([]NotifyProvider, 0, len(states))
	for _, st := range states {
		p := NotifyProvider{Name: st.Name, Enabled: st.Enabled}
		for _, k := range st.Events {
			p.Events = append(p.Events, string(k))
		}
		out = append(out, p)
	}
	return out, nil
}

type localFiles struct{}

func (localFiles) Profiles() ([]config.Config, error) { return config.Profiles() }

func (localFiles) PipelineExists(name string) (bool, error) {
	_, err := spec.Load(name)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, spec.ErrNotFound):
		return false, nil
	}
	// A file that does not parse still takes the name.
	return true, nil
}

type profileConnector struct{}

func (profileConnector) Connect(_ context.Context, profile string) (YouTrack, error) {
	cfg, err := config.Load(profile)
	if err != nil {
		return nil, err
	}
	if !cfg.Complete() {
		return nil, fmt.Errorf("YouTrack profile %q is not configured; run `ymcp setup`", cfg.Name)
	}
	yt, err := youtrack.New(youtrack.Options{BaseURL: cfg.URL, Login: cfg.Login, Password: cfg.Password, Proxy: cfg.Proxy})
	if err != nil {
		return nil, err
	}
	return NewYouTrack(yt), nil
}

type agentChecker struct{}

func (agentChecker) Check(ctx context.Context, a spec.Agent) (agent.Info, error) {
	ag, err := agent.New(a.Provider, a.Command, a.Args, agent.WithMode(a.Mode), agent.WithModel(a.Model))
	if err != nil {
		return agent.Info{}, err
	}
	return ag.Check(ctx)
}

type inspector struct{}

func (inspector) Inspect(ctx context.Context, host, dir string) ([]workspace.Check, error) {
	if h := strings.TrimSpace(host); h != "" && h != "local" {
		return nil, fmt.Errorf("workspace host %q: ssh hosts are not supported yet", h)
	}
	return workspace.Inspect(ctx, workspace.Local(), expandHome(dir))
}

type checkRunner struct{}

func (checkRunner) Check(ctx context.Context, p spec.Pipeline) ([]engine.CheckResult, error) {
	return engine.Check(ctx, p)
}

type envEditor struct{}

func (envEditor) Command(path string) *exec.Cmd {
	argv := []string{"vi"}
	if runtime.GOOS == "windows" {
		argv = []string{"notepad"}
	}
	for _, k := range []string{"VISUAL", "EDITOR"} {
		if f := strings.Fields(os.Getenv(k)); len(f) > 0 {
			argv = f
			break
		}
	}
	return exec.Command(argv[0], append(argv[1:], path)...)
}
