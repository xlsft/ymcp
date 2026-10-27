package tui

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/views"
	"github.com/xlsft/ymcp/internal/pipeline/tui/wizard"
)

type fakeDaemon struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeDaemon) Start(_ context.Context, p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start "+p)
	return "Started " + p + " in the background.\n", nil
}

func (f *fakeDaemon) Stop(_ context.Context, p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop "+p)
	return "", nil
}

// fakeWizard ends with Done on "s" (saving result), Cancelled on "x", and
// records the keys it gets.
type fakeWizard struct {
	existing *spec.Pipeline
	result   spec.Pipeline
	runNow   bool
	keys     []string
}

func (w *fakeWizard) Init() tea.Cmd { return nil }

func (w *fakeWizard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		w.keys = append(w.keys, k.String())
		switch k.String() {
		case "s":
			return w, func() tea.Msg { return wizard.Done{Pipeline: w.result, RunNow: w.runNow} }
		case "x", "ctrl+c":
			return w, func() tea.Msg { return wizard.Cancelled{} }
		}
	}
	return w, nil
}

func (w *fakeWizard) View() string {
	name := "new"
	if w.existing != nil {
		name = w.existing.Name
	}
	return "WIZARD " + name + "\nsecond line\nthird line"
}

type saved struct{ old, name string }

type fixture struct {
	app      *App
	st       *store.Store
	daemon   *fakeDaemon
	profiles []config.Config
	list     []spec.Pipeline
	wizards  []*fakeWizard
	saves    []saved
	saveErr  error
	quit     bool
	next     spec.Pipeline // what the next wizard saves
	runNow   bool
}

func newFixture(t *testing.T, profiles ...string) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{st: st, daemon: &fakeDaemon{}}
	for _, p := range profiles {
		f.profiles = append(f.profiles, config.Config{Name: p, URL: "https://" + p + ".example", Login: "x"})
	}
	if len(profiles) > 0 {
		f.list = []spec.Pipeline{{Name: "backend", Profile: profiles[0]}}
	}
	f.app = New(context.Background(), Options{
		Deps: views.Deps{
			Store:     st,
			Profiles:  func() ([]config.Config, error) { return f.profiles, nil },
			Pipelines: func() ([]spec.Pipeline, error) { return f.list, nil },
			Daemon:    f.daemon,
			RunnerLog: func(context.Context, string) (string, error) { return "runner started\n", nil },
		},
		TickEvery: -1,
		Wizard: func(_ context.Context, existing *spec.Pipeline) tea.Model {
			w := &fakeWizard{existing: existing, result: f.next, runNow: f.runNow}
			f.wizards = append(f.wizards, w)
			return w
		},
		Save: func(_ context.Context, _ *store.Store, old string, p spec.Pipeline) error {
			if f.saveErr != nil {
				return f.saveErr
			}
			f.saves = append(f.saves, saved{old, p.Name})
			if i := slices.IndexFunc(f.list, func(q spec.Pipeline) bool { return q.Name == old }); old != "" && i >= 0 {
				f.list[i] = p
			} else {
				f.list = append(f.list, p)
			}
			return nil
		},
	})
	f.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	f.run(f.app.Init(), 0)
	return f
}

func (f *fixture) send(msgs ...tea.Msg) {
	for _, msg := range msgs {
		_, cmd := f.app.Update(msg)
		f.run(cmd, 0)
	}
}

func (f *fixture) keys(keys ...string) {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "ctrl+c":
			msg = tea.KeyMsg{Type: tea.KeyCtrlC}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		f.send(msg)
	}
}

// run executes a command and feeds its messages back into the app.
func (f *fixture) run(cmd tea.Cmd, depth int) {
	if cmd == nil || depth > 30 {
		return
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(100 * time.Millisecond):
		return
	}
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			f.run(c, depth+1)
		}
	case tea.QuitMsg:
		f.quit = true
	default:
		_, next := f.app.Update(msg)
		f.run(next, depth+1)
	}
}

func (f *fixture) view() string { return ansi.Strip(f.app.View()) }

func (f *fixture) has(t *testing.T, subs ...string) {
	t.Helper()
	v := f.view()
	for _, s := range subs {
		if !strings.Contains(v, s) {
			t.Errorf("view lacks %q:\n%s", s, v)
		}
	}
}

func topIs[T any](t *testing.T, f *fixture) T {
	t.Helper()
	m, ok := f.app.Top().(T)
	if !ok {
		t.Fatalf("top screen is %T", f.app.Top())
	}
	return m
}

func TestOneProfileOpensItsPipelines(t *testing.T) {
	f := newFixture(t, "youtrack-xlsft")
	if p := topIs[*views.Pipelines](t, f); p.Profile() != "youtrack-xlsft" {
		t.Errorf("pipelines of %s", p.Profile())
	}
	f.has(t, "youtrack-xlsft", "backend")
	f.keys("esc")
	topIs[*views.Profiles](t, f)
	f.keys("esc") // the first screen stays
	topIs[*views.Profiles](t, f)
}

func TestNavigation(t *testing.T) {
	f := newFixture(t, "a", "b")
	topIs[*views.Profiles](t, f)
	f.keys("enter")
	if p := topIs[*views.Pipelines](t, f); p.Profile() != "a" {
		t.Errorf("pipelines of %s", p.Profile())
	}
	f.keys("enter")
	f.has(t, "Open board", "Run in background")
	f.keys("enter")
	if b := topIs[*views.Board](t, f); b.Pipeline() != "backend" {
		t.Errorf("board of %s", b.Pipeline())
	}
	f.keys("i")
	topIs[*views.Inbox](t, f)
	f.has(t, "Inbox", "Nothing waits for you.")
	if f.app.Depth() != 4 {
		t.Errorf("depth %d", f.app.Depth())
	}
	f.keys("esc", "esc")
	topIs[*views.Pipelines](t, f)
	f.keys("L")
	if l := topIs[*views.Logs](t, f); l.Pipeline() != "backend" {
		t.Errorf("logs of %s", l.Pipeline())
	}
	f.has(t, "runner log", "runner started")
	f.keys("esc")
	topIs[*views.Pipelines](t, f)
	f.keys("q")
	if !f.quit {
		t.Error("q does not quit")
	}
}

func TestCtrlCQuits(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.keys("ctrl+c")
	if !f.quit {
		t.Error("ctrl+c does not quit")
	}
}

func TestStartOnBoard(t *testing.T) {
	f := newFixture(t, "a", "b")
	app := New(context.Background(), Options{Deps: f.app.opts.Deps, Pipeline: "backend", TickEvery: -1})
	if b, ok := app.Top().(*views.Board); !ok || b.Pipeline() != "backend" {
		t.Errorf("top is %T", app.Top())
	}
}

func TestWizardNewAndRun(t *testing.T) {
	f := newFixture(t, "a")
	f.next, f.runNow = spec.Pipeline{Name: "frontend", Profile: "a"}, true
	f.keys("n")
	w := topIs[*fakeWizard](t, f)
	if w.existing != nil {
		t.Errorf("new pipeline wizard got %v", w.existing)
	}
	f.has(t, "WIZARD new")
	// ctrl+c belongs to the wizard while it is open.
	f.keys("ctrl+c")
	if f.quit || !slices.Contains(w.keys, "ctrl+c") {
		t.Fatalf("ctrl+c: quit %v, wizard keys %v", f.quit, w.keys)
	}
	topIs[*views.Pipelines](t, f) // Cancelled closed it

	f.keys("n", "s")
	if want := []saved{{"", "frontend"}}; !slices.Equal(f.saves, want) {
		t.Errorf("saves %v, want %v", f.saves, want)
	}
	if b := topIs[*views.Board](t, f); b.Pipeline() != "frontend" {
		t.Errorf("board of %s", b.Pipeline())
	}
	if want := []string{"start frontend"}; !slices.Equal(f.daemon.calls, want) {
		t.Errorf("daemon %v, want %v", f.daemon.calls, want)
	}
	f.has(t, "Saved pipeline frontend.", "Started frontend in the background.")
	f.keys("esc")
	topIs[*views.Pipelines](t, f)
}

func TestWizardEditRenames(t *testing.T) {
	f := newFixture(t, "a")
	f.keys("o") // backend's board
	f.next = spec.Pipeline{Name: "api", Profile: "a"}
	f.keys("e")
	w := topIs[*fakeWizard](t, f)
	if w.existing == nil || w.existing.Name != "backend" {
		t.Fatalf("edit wizard got %v", w.existing)
	}
	f.keys("s")
	if want := []saved{{"backend", "api"}}; !slices.Equal(f.saves, want) {
		t.Errorf("saves %v, want %v", f.saves, want)
	}
	if b := topIs[*views.Board](t, f); b.Pipeline() != "api" {
		t.Errorf("board of %s", b.Pipeline())
	}
	if f.app.Depth() != 3 { // profiles, pipelines, the new board
		t.Errorf("depth %d", f.app.Depth())
	}
	if len(f.daemon.calls) != 0 {
		t.Errorf("daemon %v without RunNow", f.daemon.calls)
	}
	f.has(t, "Saved pipeline api.")
}

func TestWizardSaveErrorKeepsDraft(t *testing.T) {
	f := newFixture(t, "a")
	f.saveErr = errors.New("pipeline backend has 2 active run(s)")
	f.next = spec.Pipeline{Name: "api", Profile: "a"}
	f.keys("e", "s")
	topIs[*fakeWizard](t, f)
	v := f.view()
	if !strings.HasPrefix(v, "✗ Not saved: pipeline backend has 2 active run(s)\nsecond line") {
		t.Errorf("view:\n%s", v)
	}
	f.keys("x")
	topIs[*views.Pipelines](t, f)
}

func TestRealWizardOpens(t *testing.T) {
	t.Setenv("YMCP_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	f := newFixture(t, "a")
	f.app.opts.Wizard = newWizard
	f.keys("n")
	if _, ok := f.app.Top().(*wizard.Model); !ok {
		t.Fatalf("top is %T", f.app.Top())
	}
	if f.view() == "" {
		t.Error("the wizard renders nothing")
	}
	f.send(wizard.Cancelled{})
	topIs[*views.Pipelines](t, f)
}

func TestSavePipeline(t *testing.T) {
	t.Setenv("YMCP_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	p := spec.Pipeline{Name: "backend", Profile: "a", Source: spec.Source{Query: "project: BE"}, Workspace: spec.Workspace{Dir: "/tmp/x"}}
	if err := SavePipeline(ctx, st, "", p); err != nil {
		t.Fatal(err)
	}
	// A run in progress pins the name.
	r, err := st.CreateRun(ctx, "backend", "a", "2-1", "BE-1")
	if err != nil {
		t.Fatal(err)
	}
	p.Name = "api"
	if err := SavePipeline(ctx, st, "backend", p); err == nil || !strings.Contains(err.Error(), "active run") {
		t.Fatalf("rename with an active run: %v", err)
	}
	r.Status = store.StatusDone
	if err := st.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := SavePipeline(ctx, st, "backend", p); err != nil {
		t.Fatal(err)
	}
	if _, err := spec.Load("backend"); !errors.Is(err, spec.ErrNotFound) {
		t.Errorf("old file: %v", err)
	}
	if got, err := spec.Load("api"); err != nil || got.Source.Query != "project: BE" {
		t.Errorf("new file: %+v, %v", got, err)
	}
}

func TestLastLine(t *testing.T) {
	if got := lastLine("Started backend (pid 7)\nlog: /x\n\n", "def"); got != "log: /x" {
		t.Errorf("got %q", got)
	}
	if got := lastLine("ymcp: boom\n", "def"); got != "boom" {
		t.Errorf("got %q", got)
	}
	if got := lastLine(" \n", "def"); got != "def" {
		t.Errorf("got %q", got)
	}
}

func TestOpenURLRejectsNonWeb(t *testing.T) {
	if err := OpenURL("file:///etc/passwd"); err == nil {
		t.Error("file URL accepted")
	}
}
