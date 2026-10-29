package wizard

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/engine"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// fakeYT is a scripted YouTrack.
type fakeYT struct {
	mu       sync.Mutex
	login    string
	searches map[string][]ytwatch.Issue
	full     map[string]ytwatch.Issue
	fields   map[string][]Field
	tags     []string
	users    []User
	saved    []SavedSearch
	calls    []string
}

func (f *fakeYT) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeYT) called(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeYT) BaseURL() string                    { return "https://example.youtrack.cloud" }
func (f *fakeYT) Me(context.Context) (string, error) { return f.login, nil }

func (f *fakeYT) Search(_ context.Context, q string) ([]ytwatch.Issue, error) {
	f.record("search " + q)
	if q == "bogus:" {
		return nil, errors.New("400 Bad Request: Could not parse query")
	}
	return f.searches[q], nil
}

func (f *fakeYT) Issue(_ context.Context, key string) (ytwatch.Issue, error) {
	f.record("issue " + key)
	if is, ok := f.full[key]; ok {
		return is, nil
	}
	return ytwatch.Issue{}, errors.New("404 not found")
}

func (f *fakeYT) Validate(_ context.Context, command, key string) (ytwatch.CommandCheck, error) {
	f.record("validate " + command + " @" + key)
	switch {
	case strings.HasPrefix(command, "tag ") || strings.HasPrefix(command, "untag "):
		return ytwatch.CommandCheck{State: ytwatch.Invalid, Text: "tag expected: " + strings.Fields(command)[1]}, nil
	case strings.Contains(command, "Bogus"):
		return ytwatch.CommandCheck{State: ytwatch.Invalid, Text: "State expected: Bogus"}, nil
	case command == "State In":
		return ytwatch.CommandCheck{State: ytwatch.Incomplete, Text: "State expected: In", Suggestions: []string{"In Progress", "Incomplete"}}, nil
	}
	return ytwatch.CommandCheck{State: ytwatch.Valid, Text: "Set " + command}, nil
}

func (f *fakeYT) SearchAssist(_ context.Context, q string, caret int) ([]Suggestion, error) {
	f.record("search-assist " + q)
	if strings.HasSuffix(q, "#Unr") {
		return []Suggestion{
			{Option: "Unresolved", Prefix: "#", Suffix: " ", CompletionStart: caret - 4, CompletionEnd: caret, Caret: caret + 8, Description: "State"},
			{Option: "Unreviewed", Prefix: "#", Suffix: " ", CompletionStart: caret - 4, CompletionEnd: caret, Caret: caret + 8, Description: "tag"},
		}, nil
	}
	return nil, nil
}

func (f *fakeYT) CommandAssist(_ context.Context, command string, caret int, key string) ([]Suggestion, error) {
	f.record("command-assist " + command + " @" + key)
	if command == "State In" {
		return []Suggestion{
			{Option: "In Progress", Suffix: " ", CompletionStart: 6, CompletionEnd: 8, Caret: 18, Description: "State"},
			{Option: "Incomplete", Suffix: " ", CompletionStart: 6, CompletionEnd: 8, Caret: 17, Description: "State"},
		}, nil
	}
	return nil, nil
}

func (f *fakeYT) Fields(_ context.Context, project string) ([]Field, error) {
	f.record("fields " + project)
	return f.fields[project], nil
}

func (f *fakeYT) Tags(context.Context) ([]string, error) {
	f.record("tags")
	return f.tags, nil
}

func (f *fakeYT) Users(_ context.Context, q string) ([]User, error) {
	f.record("users " + q)
	var out []User
	for _, u := range f.users {
		if strings.Contains(strings.ToLower(u.Login+" "+u.FullName), strings.ToLower(q)) {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeYT) SavedSearches(context.Context) ([]SavedSearch, error) { return f.saved, nil }

func newFakeYT() *fakeYT {
	return &fakeYT{
		login: "admin",
		searches: map[string][]ytwatch.Issue{
			"project: YPC #Unresolved": {
				{ID: "2-3", Key: "YPC-3", Summary: "Fix login redirect"},
				{ID: "2-4", Key: "YPC-4", Summary: "Add CSV export"},
				{ID: "2-7", Key: "YPC-7", Summary: "Rate limit the API"},
			},
			"State: {In Progress}": {{ID: "2-1", Key: "YPC-1", Summary: "Old work"}},
			"tag: ai":              {{ID: "2-3", Key: "YPC-3", Summary: "Fix login redirect"}},
			"(project: YPC #Unresolved) and (tag: ai)": {{ID: "2-3", Key: "YPC-3", Summary: "Fix login redirect"}},
		},
		full: map[string]ytwatch.Issue{
			"YPC-3": {ID: "2-3", Key: "YPC-3", Summary: "Fix login redirect", Description: "Login redirects to /404.", URL: "https://example.youtrack.cloud/issue/YPC-3", Fields: map[string]string{"Priority": "Major"}},
		},
		fields: map[string][]Field{
			"YPC": {
				{Name: "State", Type: "state", Values: []string{"Open", "In Progress", "Fixed", "Verified", "Won't fix"}},
				{Name: "Priority", Type: "enum", Values: []string{"Critical", "Major", "Normal"}},
				{Name: "Assignee", Type: "user", Values: []string{"me"}},
			},
		},
		tags:  []string{"ai", "needs-input"},
		users: []User{{Login: "ivan.petrov", FullName: "Ivan Petrov"}, {Login: "admin", FullName: "admin"}},
		saved: []SavedSearch{{Name: "Assigned to me", Query: "for: me"}, {Name: "AI backlog", Query: "project: YPC #Unresolved"}},
	}
}

type fakeFactory struct {
	yt    *fakeYT
	mu    sync.Mutex
	calls []string
}

func (f *fakeFactory) Connect(_ context.Context, profile string) (YouTrack, error) {
	f.mu.Lock()
	f.calls = append(f.calls, profile)
	f.mu.Unlock()
	if profile == "youtrack-acme" {
		return nil, errors.New("dial tcp: connection refused")
	}
	return f.yt, nil
}

type fakeLocal struct {
	profiles []config.Config
	existing map[string]bool
}

func (f fakeLocal) Profiles() ([]config.Config, error) { return f.profiles, nil }
func (f fakeLocal) PipelineExists(name string) (bool, error) {
	return f.existing[name], nil
}

type fakeAgents struct {
	mu  sync.Mutex
	got []spec.Agent
}

func (f *fakeAgents) Check(_ context.Context, a spec.Agent) (agent.Info, error) {
	f.mu.Lock()
	f.got = append(f.got, a)
	f.mu.Unlock()
	switch a.Provider {
	case "claude":
		info := agent.Info{Path: "/usr/local/bin/claude", Version: "2.1.0 (Claude Code)"}
		switch {
		case slices.Contains(a.Args, "--dangerously-skip-permissions"):
			info.Warnings = []string{"claude runs every tool without asking (bypassPermissions)"}
		case a.Mode == "":
			info.Warnings = []string{"claude has no --permission-mode or --allowedTools: every tool call will need your approval"}
		}
		return info, nil
	case "opencode":
		return agent.Info{Path: "/opt/homebrew/bin/opencode", Version: "0.15.3"}, nil
	}
	return agent.Info{}, errors.New(a.Provider + `: executable file not found in $PATH`)
}

type fakeInspector struct {
	mu   sync.Mutex
	dirs []string
}

func (f *fakeInspector) Inspect(_ context.Context, host, dir string) ([]workspace.Check, error) {
	f.mu.Lock()
	f.dirs = append(f.dirs, dir)
	f.mu.Unlock()
	if !strings.HasSuffix(dir, "repo") {
		return []workspace.Check{
			{Name: workspace.CheckRepo, OK: false, Detail: dir + " is not usable as a git repository: not a git repository"},
			{Name: workspace.CheckOrigin, OK: false, Detail: "not checked: no repository"},
		}, nil
	}
	return []workspace.Check{
		{Name: workspace.CheckRepo, OK: true, Detail: dir},
		{Name: workspace.CheckOrigin, OK: true, Detail: "git@github.com:xlsft/ymcp.git"},
		{Name: workspace.CheckDefaultBranch, OK: true, Detail: "main"},
		{Name: workspace.CheckUserEmail, OK: true, Detail: "dev@xlsft.ru"},
		{Name: workspace.CheckPush, OK: true, Detail: "allowed"},
		{Name: workspace.CheckWorktreeConfig, OK: true, Detail: "off; ymcp turns it on with the first worktree"},
	}, nil
}

// Dirs lists the directories here, wherever the host.
func (f *fakeInspector) Dirs(ctx context.Context, _, dir string) ([]workspace.Dir, error) {
	return workspace.Dirs(ctx, workspace.Local(), dir)
}

type fakeDetector struct{}

func (fakeDetector) Detect(context.Context, string, string) (Detection, error) {
	return Detection{Kind: "github-pr", Repo: "github.com/xlsft/ymcp", Auth: "token from `gh auth token`", AuthOK: true}, nil
}

type fakeChecker struct {
	mu  sync.Mutex
	got []spec.Pipeline
}

func (f *fakeChecker) Check(_ context.Context, p spec.Pipeline) ([]engine.CheckResult, error) {
	f.mu.Lock()
	f.got = append(f.got, p)
	f.mu.Unlock()
	return []engine.CheckResult{
		{Name: "pipeline", OK: true, Detail: "the configuration is valid"},
		{Name: "YouTrack", OK: true, Detail: "signed in to https://example.youtrack.cloud as admin"},
		{Name: "source query", OK: true, Detail: "3 issues match: YPC-3, YPC-4, YPC-7"},
		{Name: "stages.needsInput.enter command", OK: true, Warning: true, Detail: "tag needs-input doesn't exist yet; YouTrack creates it on first use"},
		{Name: "workspace: git repository", OK: true, Detail: p.Workspace.Dir},
		{Name: "agent", OK: true, Detail: "/usr/local/bin/claude 2.1.0 (Claude Code)"},
	}, nil
}

type fakeEditor struct{}

func (fakeEditor) Command(path string) *exec.Cmd { return exec.Command("true", path) }

type fakes struct {
	deps    Deps
	yt      *fakeYT
	factory *fakeFactory
	agents  *fakeAgents
	inspect *fakeInspector
	checker *fakeChecker
	presets *fakePresets
}

func newFakes() *fakes {
	f := &fakes{yt: newFakeYT(), agents: &fakeAgents{}, inspect: &fakeInspector{}, checker: &fakeChecker{}, presets: &fakePresets{}}
	f.factory = &fakeFactory{yt: f.yt}
	f.deps = Deps{
		Local: fakeLocal{
			profiles: []config.Config{
				{Name: "youtrack-acme", DisplayName: "YouTrack (acme)", URL: "https://acme.youtrack.cloud", Login: "dev"},
				{Name: "youtrack-work", DisplayName: "YouTrack (work)", URL: "https://example.youtrack.cloud", Login: "admin"},
			},
			existing: map[string]bool{"frontend": true},
		},
		YouTrack:  f.factory,
		Agents:    f.agents,
		Workspace: f.inspect,
		Strategy:  fakeDetector{},
		Checker:   f.checker,
		Editor:    fakeEditor{},
		Notify:    fakeNotify{},
		Presets:   f.presets,
	}
	return f
}

// fakePresets are the built-in presets plus user presets in memory.
type fakePresets struct {
	user    []spec.Preset
	listErr error
	saveErr error
}

func (f *fakePresets) List() ([]spec.Preset, error) {
	return append(spec.Presets(), f.user...), f.listErr
}

func (f *fakePresets) Save(p spec.Preset) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	list, _ := f.List()
	for _, q := range list {
		if q.Name == p.Name {
			return presetTaken(p.Name)
		}
	}
	f.user = append(f.user, p)
	return nil
}

// driver runs the wizard like a tea.Program would, synchronously: every
// command is run and its message fed back. Commands that don't return
// quickly (cursor blinks of huh) are dropped.
type driver struct {
	t   *testing.T
	m   *Model
	out []tea.Msg
	// top gets the messages when set: the profile wizard around m.
	top tea.Model
}

// model is what gets the messages.
func (d *driver) model() tea.Model {
	if d.top != nil {
		return d.top
	}
	return d.m
}

func start(t *testing.T, deps Deps, existing *spec.Pipeline) *driver {
	t.Helper()
	m := New(context.Background(), deps, existing).(*Model)
	m.sh.delay = 0
	d := &driver{t: t, m: m}
	d.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	d.run(m.Init(), 0)
	return d
}

var cmdType = reflect.TypeOf((tea.Cmd)(nil))

func (d *driver) run(cmd tea.Cmd, depth int) {
	if cmd == nil || depth > 60 {
		return
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(150 * time.Millisecond):
		return
	}
	switch m := msg.(type) {
	case nil:
		return
	case tea.BatchMsg:
		for _, c := range m {
			d.run(c, depth+1)
		}
		return
	case Done, Cancelled, ProfileDone:
		d.out = append(d.out, msg)
		return
	}
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == cmdType {
		for i := 0; i < v.Len(); i++ {
			d.run(v.Index(i).Interface().(tea.Cmd), depth+1)
		}
		return
	}
	_, next := d.model().Update(msg)
	d.run(next, depth+1)
}

func (d *driver) send(msg tea.Msg) {
	_, cmd := d.model().Update(msg)
	d.run(cmd, 0)
}

var specialKeys = map[string]tea.KeyType{
	"tab": tea.KeyTab, "shift+tab": tea.KeyShiftTab, "enter": tea.KeyEnter, "esc": tea.KeyEsc,
	"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
	"backspace": tea.KeyBackspace, "ctrl+n": tea.KeyCtrlN, "ctrl+p": tea.KeyCtrlP, "ctrl+f": tea.KeyCtrlF,
	"ctrl+s": tea.KeyCtrlS, "ctrl+c": tea.KeyCtrlC, "ctrl+r": tea.KeyCtrlR, "space": tea.KeySpace,
	"ctrl+u": tea.KeyCtrlU,
}

// keys presses special keys by name.
func (d *driver) keys(names ...string) {
	for _, n := range names {
		k, ok := specialKeys[n]
		if !ok {
			d.t.Fatalf("unknown key %q", n)
		}
		msg := tea.KeyMsg{Type: k}
		if k == tea.KeySpace {
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		}
		d.send(msg)
	}
}

// typ types text one rune at a time.
func (d *driver) typ(s string) {
	for _, r := range s {
		d.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func (d *driver) view() string { return d.model().View() }

// must fails unless the view contains every part.
func (d *driver) must(parts ...string) {
	d.t.Helper()
	v := d.view()
	for _, p := range parts {
		if !strings.Contains(v, p) {
			d.t.Fatalf("view lacks %q:\n%s", p, v)
		}
	}
}

func (d *driver) mustNot(parts ...string) {
	d.t.Helper()
	v := d.view()
	for _, p := range parts {
		if strings.Contains(v, p) {
			d.t.Fatalf("view has %q:\n%s", p, v)
		}
	}
}

// dump checks that the view fits the window and writes it to
// $YMCP_WIZARD_DUMP/<name>.txt when set.
func (d *driver) dump(name string) {
	d.t.Helper()
	v := d.view()
	lines := strings.Split(v, "\n")
	w, h := d.m.sh.width, d.m.sh.height
	if len(lines) > h {
		d.t.Errorf("%s: view has %d lines, more than the %d of the window", name, len(lines), h)
	}
	for i, l := range lines {
		if lw := lenVisible(l); lw > w {
			d.t.Errorf("%s: line %d is %d cells wide: %q", name, i+1, lw, l)
		}
	}
	dir := os.Getenv("YMCP_WIZARD_DUMP")
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(v+"\n"), 0o644); err != nil {
		d.t.Fatal(err)
	}
}

func (d *driver) step() string { return d.m.steps[d.m.cur].title() }

// focused is the id of the focused field or button.
func (d *driver) focused() string { return d.m.focusID() }

// focus puts the focus on a field or button directly.
func (d *driver) focus(id string) {
	d.t.Helper()
	ok := false
	for _, f := range d.m.focusables() {
		ok = ok || f == id
	}
	if !ok {
		d.t.Fatalf("no field %q on %s: %v", id, d.step(), d.m.focusables())
	}
	d.m.steps[d.m.cur].form().focus = id
	d.run(d.m.syncFocus(), 0)
}

// goStep shows step i (0-based) without the checks of Next.
func (d *driver) goStep(i int) {
	d.run(d.m.goTo(i), 0)
}

// alt presses alt+r.
func (d *driver) alt(r rune) {
	d.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: true})
}

// ctrlU clears the input before the cursor.
func ctrlU() tea.Msg { return tea.KeyMsg{Type: tea.KeyCtrlU} }

type fakeNotify struct{}

func (fakeNotify) Providers() ([]NotifyProvider, error) {
	return []NotifyProvider{{Name: "toast", Enabled: true, Events: []string{"needs_input", "run_failed"}}}, nil
}
