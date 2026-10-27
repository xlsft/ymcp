package views

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// t0 is "now" in every test.
var t0 = time.Date(2026, 10, 27, 15, 30, 0, 0, time.UTC)

type fakeDaemon struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeDaemon) Start(_ context.Context, p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start "+p)
	if f.err != nil {
		return "ymcp: " + f.err.Error() + "\n", f.err
	}
	return "Started " + p + " in the background (pid 777), log: /tmp/x.log\n", nil
}

func (f *fakeDaemon) Stop(_ context.Context, p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop "+p)
	return "Stopped " + p + ".\n", nil
}

type fakeBacklog struct {
	issues []ytwatch.Issue
	err    error
	calls  int
}

func (f *fakeBacklog) Search(_ context.Context, profile, query string) ([]ytwatch.Issue, error) {
	f.calls++
	return f.issues, f.err
}

// fixture is a seeded state database with the services around it.
type fixture struct {
	t       *testing.T
	st      *store.Store
	clock   time.Time
	d       Deps
	daemon  *fakeDaemon
	backlog *fakeBacklog
	opened  []string
	deleted []string
	runs    map[string]store.Run // by issue key
}

func pipelineSpec(name, profile, query string) spec.Pipeline {
	p := spec.Pipeline{Name: name, Profile: profile, Source: spec.Source{Query: query},
		Workspace: spec.Workspace{Dir: "/home/dev/code/" + name}}.WithDefaults()
	return p
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{t: t, st: st, clock: t0, daemon: &fakeDaemon{}, runs: map[string]store.Run{}}
	st.SetClock(func() time.Time { return f.clock })
	f.backlog = &fakeBacklog{issues: []ytwatch.Issue{
		{ID: "2-131", Key: "BE-131", Summary: "Rate limit the login endpoint"},
		{ID: "2-133", Key: "BE-133", Summary: "Pagination for /issues"},
		{ID: "2-140", Key: "BE-140", Summary: "Audit log"},
		{ID: "2-123", Key: "BE-123", Summary: "Fix login"}, // has a run: not in the backlog
	}}
	pipelines := []spec.Pipeline{
		pipelineSpec("backend", "youtrack-xlsft", "project: BE #Unresolved"),
		pipelineSpec("frontend", "youtrack-xlsft", "project: FE #Unresolved"),
	}
	f.d = Deps{
		Store: st,
		Profiles: func() ([]config.Config, error) {
			return []config.Config{
				{Name: "youtrack-beget", URL: "https://projectsystem.online", Login: "x"},
				{Name: "youtrack-xlsft", DisplayName: "YouTrack (xlsft)", URL: "https://xlsft.youtrack.cloud", Login: "x"},
			}, nil
		},
		Pipelines: func() ([]spec.Pipeline, error) {
			var out []spec.Pipeline
			for _, p := range pipelines {
				if !slices.Contains(f.deleted, p.Name) {
					out = append(out, p)
				}
			}
			return out, nil
		},
		DeletePipeline: func(name string) error { f.deleted = append(f.deleted, name); return nil },
		Daemon:         f.daemon,
		Backlog:        f.backlog,
		OpenURL:        func(u string) error { f.opened = append(f.opened, u); return nil },
		Now:            func() time.Time { return f.clock },
	}
	f.d = f.d.WithDefaults()
	f.seed()
	return f
}

// at runs fn with the store's clock set back by ago.
func (f *fixture) at(ago time.Duration, fn func()) {
	saved := f.clock
	f.clock = saved.Add(-ago)
	fn()
	f.clock = saved
}

func (f *fixture) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

// run creates a run that was captured ago, in status, with its summary
// event.
func (f *fixture) run(pipeline, key, summary string, status store.RunStatus, ago time.Duration, change func(*store.Run)) store.Run {
	f.t.Helper()
	ctx := context.Background()
	var r store.Run
	f.at(ago, func() {
		var err error
		r, err = f.st.CreateRun(ctx, pipeline, "youtrack-xlsft", "2-"+strings.Split(key, "-")[1], key)
		f.must(err)
		f.must(f.st.AddEvent(ctx, r.ID, "run.captured", map[string]string{"issue": key, "summary": summary}))
		r.Status, r.Stage, r.Agent = status, "working", "Claude"
		r.Branch = strings.ToLower(key) + "-" + strings.ReplaceAll(strings.ToLower(summary), " ", "-")
		if change != nil {
			change(&r)
		}
		f.must(f.st.UpdateRun(ctx, r))
	})
	r, err := f.st.Run(ctx, r.ID)
	f.must(err)
	f.runs[key] = r
	return r
}

func (f *fixture) event(r store.Run, ago time.Duration, kind string, data any) {
	f.t.Helper()
	f.at(ago, func() { f.must(f.st.AddEvent(context.Background(), r.ID, kind, data)) })
}

func (f *fixture) seed() {
	ctx := context.Background()
	f.at(3*time.Second, func() { f.must(f.st.AcquireRunner(ctx, "backend", 4242, "build-box", 0)) })

	f.run("backend", "BE-110", "Drop legacy endpoints", store.StatusDone, 26*time.Hour, nil)
	f.run("backend", "BE-111", "Flaky migration test", store.StatusFailed, 20*time.Hour, nil)
	f.run("backend", "BE-118", "Cache keys", store.StatusReview, 3*time.Hour, func(r *store.Run) {
		r.Stage, r.PRNumber, r.PRURL = "review", 40, "https://github.com/acme/backend/pull/40"
	})
	rv := f.run("backend", "BE-120", "Retry jobs", store.StatusReview, 2*time.Hour, func(r *store.Run) { r.Stage = "review" })
	_, err := f.st.AddInbox(ctx, store.InboxItem{RunID: rv.ID, Source: "youtrack", ExternalID: "c1", Author: "anna", Body: "Please add a test."})
	f.must(err)

	w := f.run("backend", "BE-123", "Fix login", store.StatusWorking, 30*time.Minute, func(r *store.Run) {
		r.Worktree = "/home/dev/code/backend/.git/ymcp/worktrees/be-123-fix-login"
	})
	f.event(w, 4*time.Minute+12*time.Second, "turn.start", map[string]string{"kind": "start", "prompt": "Fix the login"})
	f.event(w, 4*time.Minute, "agent.thought", map[string]string{"text": "The session cookie is not refreshed."})
	f.event(w, 3*time.Minute, "agent.tool_call", map[string]any{"tool": map[string]string{"title": "Edit internal/auth/login.go", "status": "pending"}})
	f.event(w, 2*time.Minute, "agent.text", map[string]string{"text": "I found the bug: the cookie expiry is computed in local time.\nFixing it now."})

	q := f.run("backend", "BE-127", "CSV export", store.StatusNeedsInput, time.Hour, func(r *store.Run) {
		r.NeedsInputReason = "Which date format should the CSV export use: ISO 8601 or the user's locale?"
	})
	f.event(q, 13*time.Minute, "agent.request_input", map[string]string{"question": q.NeedsInputReason})
	f.at(12*time.Minute, func() { f.must(f.st.UpdateRun(ctx, q)) })
	f.runs["BE-127"], _ = f.st.Run(ctx, q.ID)

	f.run("backend", "BE-130", "Webhooks", store.StatusQueued, time.Minute, nil)

	fe := f.run("frontend", "FE-9", "Dark mode", store.StatusNeedsInput, 3*time.Hour, func(r *store.Run) {
		r.NeedsInputReason = "The agent wants to run: Bash(rm -rf dist). Answer /approve or /reject."
	})
	f.at(2*time.Hour, func() { f.must(f.st.UpdateRun(ctx, fe)) })
	f.runs["FE-9"], _ = f.st.Run(ctx, fe.ID)
}

// driver sends messages to a screen and runs the commands it returns,
// feeding their messages back. Navigation messages are collected instead.
type driver struct {
	t   *testing.T
	m   tea.Model
	nav []tea.Msg
}

func drive(t *testing.T, m tea.Model, w, h int) *driver {
	t.Helper()
	d := &driver{t: t, m: m}
	d.send(tea.WindowSizeMsg{Width: w, Height: h})
	d.run(m.Init(), 0)
	return d
}

func (d *driver) send(msgs ...tea.Msg) *driver {
	d.t.Helper()
	for _, msg := range msgs {
		var cmd tea.Cmd
		d.m, cmd = d.m.Update(msg)
		d.run(cmd, 0)
	}
	return d
}

// keys sends key presses: single runes, or names like "enter", "esc",
// "up".
func (d *driver) keys(keys ...string) *driver {
	d.t.Helper()
	for _, k := range keys {
		d.send(keyMsg(k))
	}
	return d
}

// typeText types a text, rune by rune.
func (d *driver) typeText(s string) *driver {
	d.t.Helper()
	d.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	return d
}

func (d *driver) tick() *driver {
	d.t.Helper()
	return d.send(Tick{Now: t0})
}

func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "alt+enter":
		return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func (d *driver) run(cmd tea.Cmd, depth int) {
	d.t.Helper()
	if cmd == nil || depth > 20 {
		return
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(100 * time.Millisecond): // a timer (cursor blink): skip
		return
	}
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			d.run(c, depth+1)
		}
	case tea.QuitMsg, Back, OpenPipelines, OpenBoard, OpenRun, OpenIssue, OpenInbox, OpenWizard, OpenLogs:
		d.nav = append(d.nav, msg)
	default:
		var next tea.Cmd
		d.m, next = d.m.Update(msg)
		d.run(next, depth+1)
	}
}

// lastNav is the last navigation message, cleared.
func (d *driver) lastNav() tea.Msg {
	if len(d.nav) == 0 {
		return nil
	}
	m := d.nav[len(d.nav)-1]
	d.nav = nil
	return m
}

func (d *driver) view() string { return ansi.Strip(d.m.View()) }

func (d *driver) has(subs ...string) {
	d.t.Helper()
	v := d.view()
	for _, s := range subs {
		if !strings.Contains(v, s) {
			d.t.Errorf("view lacks %q:\n%s", s, v)
		}
	}
}

func (d *driver) hasNot(subs ...string) {
	d.t.Helper()
	v := d.view()
	for _, s := range subs {
		if strings.Contains(v, s) {
			d.t.Errorf("view has %q:\n%s", s, v)
		}
	}
}

// fits checks that the view is exactly w×h at most.
func (d *driver) fits(w, h int) {
	d.t.Helper()
	lines := strings.Split(d.m.View(), "\n")
	if len(lines) != h {
		d.t.Errorf("view has %d lines, want %d:\n%s", len(lines), h, d.view())
	}
	for i, l := range lines {
		if n := ansi.StringWidth(l); n > w {
			d.t.Errorf("line %d is %d wide, more than %d: %q", i+1, n, w, ansi.Strip(l))
		}
	}
}

func TestProfiles(t *testing.T) {
	f := newFixture(t)
	d := drive(t, NewProfiles(&f.d), 100, 30)
	d.has("ymcp pipelines", "⚑ 2 need input",
		"YouTrack (xlsft) (youtrack-xlsft)", "xlsft.youtrack.cloud", "2 pipelines · 1 running",
		"backend", "● running", "2 working · 1 needs input · 2 review", "⚑ 1 needs input",
		"frontend", "○ stopped",
		"youtrack-beget", "no pipelines yet — press n to create one",
		"enter pipelines", "n new pipeline", "i inbox", "q quit")

	// The cursor starts on the first profile.
	d.keys("enter")
	if got := d.lastNav(); got != (OpenPipelines{Profile: "youtrack-beget"}) {
		t.Errorf("enter: %#v", got)
	}
	d.keys("down", "enter")
	if got := d.lastNav(); got != (OpenPipelines{Profile: "youtrack-xlsft"}) {
		t.Errorf("down enter: %#v", got)
	}
	d.keys("n")
	if got, ok := d.lastNav().(OpenWizard); !ok || got.Profile != "youtrack-xlsft" || got.Existing != nil {
		t.Errorf("n: %#v", got)
	}
	d.keys("i")
	if got := d.lastNav(); got != (OpenInbox{}) {
		t.Errorf("i: %#v", got)
	}
	d.keys("q")
	if _, ok := d.lastNav().(tea.QuitMsg); !ok {
		t.Error("q does not quit")
	}
}

func TestProfilesEmpty(t *testing.T) {
	f := newFixture(t)
	f.d.Profiles = func() ([]config.Config, error) { return nil, nil }
	f.d.Pipelines = func() ([]spec.Pipeline, error) { return nil, nil }
	d := drive(t, NewProfiles(&f.d), 80, 24)
	d.has("No YouTrack profiles yet.", "setup")
}

func TestProfilesUnconfigured(t *testing.T) {
	f := newFixture(t)
	f.d.Profiles = func() ([]config.Config, error) { return nil, nil }
	d := drive(t, NewProfiles(&f.d), 100, 30)
	d.has("youtrack-xlsft", "profile not configured", "backend")
}

func TestBoard(t *testing.T) {
	f := newFixture(t)
	d := drive(t, NewBoard(&f.d, "backend"), 100, 30)
	d.has("backend", "youtrack-xlsft · Claude · /home/dev/code/backend", "● running", "agents 1/1",
		"BACKLOG 3", "WORKING 2", "NEEDS INPUT 1", "REVIEW 2", "DONE 2",
		"BE-131", "Rate limit the", "login endpoint", "BE-140",
		"BE-123", "#5", "Fix login", "4m12s writing",
		"BE-130", "◌ queued",
		"BE-127", "CSV export", "“Which date for", "⚑ 12m waiting",
		"BE-118", "PR #40", "BE-120", "⟳ 1 feedback",
		"BE-110", "✓ done", "BE-111", "✗ failed",
		"enter open", "a answer")
	if f.backlog.calls != 1 {
		t.Errorf("backlog searched %d times, want 1", f.backlog.calls)
	}

	// Live: a new tool call shows on the next tick; the backlog is not
	// searched again before its interval.
	f.event(f.runs["BE-123"], 0, "agent.tool_call", map[string]any{"tool": map[string]string{"title": "go test"}})
	d.tick()
	d.has("4m12s go test")
	if f.backlog.calls != 1 {
		t.Errorf("backlog searched %d times after a tick, want 1", f.backlog.calls)
	}

	// Wide enough, the columns keep a margin from the screen's edges and
	// a blank line under their rules.
	d.send(tea.WindowSizeMsg{Width: 160, Height: 40})
	d.fits(160, 40)
	lines := strings.Split(d.view(), "\n")
	if !strings.HasPrefix(lines[2], "  BACKLOG") || strings.TrimSpace(lines[4]) != "" {
		t.Errorf("no margin or air around the columns:\n%s", d.view())
	}
	d.send(tea.WindowSizeMsg{Width: 100, Height: 30})

	// The cursor starts on the working column: enter opens BE-123.
	d.keys("enter")
	if got := d.lastNav(); got != (OpenRun{Pipeline: "backend", Number: f.runs["BE-123"].Number}) {
		t.Errorf("enter: %#v", got)
	}
	d.keys("o")
	if want := "https://xlsft.youtrack.cloud/issue/BE-123"; len(f.opened) != 1 || f.opened[0] != want {
		t.Errorf("opened %v, want %s", f.opened, want)
	}

	// Needs input: a opens the card with the answer box.
	d.keys("right", "a")
	if got := d.lastNav(); got != (OpenRun{Pipeline: "backend", Number: f.runs["BE-127"].Number, Answer: true}) {
		t.Errorf("a: %#v", got)
	}

	// Review: p opens the pull request; c cancels after asking.
	d.keys("right", "p")
	if got := f.opened[len(f.opened)-1]; got != "https://github.com/acme/backend/pull/40" {
		t.Errorf("p opened %s", got)
	}
	d.keys("c")
	d.has("Cancel run #3 (BE-118)?")
	d.keys("y")
	d.has("Canceled run #3.", "REVIEW 1", "DONE 3", "⊘ canceled")
	r, err := f.st.Run(context.Background(), f.runs["BE-118"].ID)
	if err != nil || r.Status != store.StatusCanceled {
		t.Errorf("BE-118 is %s (%v), want canceled", r.Status, err)
	}

	// Backlog cards have no run: enter opens the issue's card, o the issue
	// in the browser.
	opened := len(f.opened)
	d.keys("left", "left", "left", "enter")
	if got, ok := d.lastNav().(OpenIssue); !ok || got.Pipeline != "backend" || got.Profile != "youtrack-xlsft" || got.Issue.Key != "BE-131" {
		t.Errorf("enter on the backlog: %#v", got)
	}
	if len(f.opened) != opened {
		t.Errorf("enter on the backlog opened %v", f.opened[opened:])
	}
	d.keys("o")
	if got := f.opened[len(f.opened)-1]; got != "https://xlsft.youtrack.cloud/issue/BE-131" {
		t.Errorf("o on the backlog opened %s", got)
	}
	d.keys("esc")
	if got := d.lastNav(); got != (Back{}) {
		t.Errorf("esc: %#v", got)
	}
}

func TestBoardBacklogError(t *testing.T) {
	f := newFixture(t)
	f.backlog.err = errors.New("401 Unauthorized")
	d := drive(t, NewBoard(&f.d, "backend"), 100, 30)
	d.has("BACKLOG !", "Search failed: 401")
}

func TestBoardBacklogSearching(t *testing.T) {
	f := newFixture(t)
	m := NewBoard(&f.d, "backend")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(m.load(map[int64]runInfo{})) // the search command is not run
	v := ansi.Strip(m.View())
	for _, s := range []string{"BACKLOG …", "searching…"} {
		if !strings.Contains(v, s) {
			t.Errorf("view lacks %q:\n%s", s, v)
		}
	}
}

func TestBoardNoBacklog(t *testing.T) {
	f := newFixture(t)
	f.d.Backlog = nil
	d := drive(t, NewBoard(&f.d, "backend"), 100, 30)
	d.has("BACKLOG –", "WORKING 2")
}

func TestBoardNarrow(t *testing.T) {
	f := newFixture(t)
	d := drive(t, NewBoard(&f.d, "backend"), 80, 24)
	d.has("BACKLOG", "WORKING", "NEEDS INPUT", "REVIEW", "DONE")
	d.fits(80, 24)

	d = drive(t, NewBoard(&f.d, "backend"), 50, 20)
	d.has("WORKING 2", "column 2 of 5")
	d.hasNot("DONE")
	d.fits(50, 20)
	d.keys("right", "right", "right")
	d.has("DONE 2", "column 5 of 5")
}

func TestBoardScrollsCards(t *testing.T) {
	f := newFixture(t)
	for i := range 8 {
		f.run("backend", fmt.Sprintf("BE-%d", 200+i), "Task", store.StatusWorking, time.Duration(i)*time.Minute, nil)
	}
	d := drive(t, NewBoard(&f.d, "backend"), 100, 20)
	d.has("↓ ")
	d.keys("down", "down", "down", "down", "down", "down", "down", "down", "down")
	d.has("↑ ")
	d.fits(100, 20)
}

// The run card is tested in run_test.go.

func TestInbox(t *testing.T) {
	f := newFixture(t)
	d := drive(t, NewInbox(&f.d), 100, 30)
	d.has("Inbox", "⚑ 2 runs need input",
		"frontend #1", "FE-9", "Dark mode", "waiting 2h", "Bash(rm -rf dist)",
		"the runner of frontend is stopped",
		"backend #6", "BE-127", "waiting 12m")
	// The oldest question comes first.
	v := d.view()
	if strings.Index(v, "frontend #1") > strings.Index(v, "backend #6") {
		t.Errorf("frontend's older question is not first:\n%s", v)
	}

	// enter opens the run card with its reply box; a answers in place.
	d.keys("down", "enter")
	if got := d.lastNav(); got != (OpenRun{Pipeline: "backend", Number: 6, Answer: true}) {
		t.Errorf("enter: %#v", got)
	}
	d.keys("a")
	d.has("enter send")
	d.typeText("ISO")
	d.keys("enter")
	d.has("Answer sent to #6", "✓ answer queued")
	items, _ := f.st.PendingInbox(context.Background(), f.runs["BE-127"].ID)
	if len(items) != 1 || items[0].Body != "ISO" {
		t.Errorf("inbox %+v", items)
	}

	d.keys("v")
	if got := d.lastNav(); got != (OpenRun{Pipeline: "backend", Number: 6}) {
		t.Errorf("v: %#v", got)
	}

	// Once the runner moves the run on, it leaves the inbox.
	r := f.runs["BE-127"]
	r.Status = store.StatusWorking
	f.must(f.st.UpdateRun(context.Background(), r))
	d.tick()
	d.has("⚑ 1 run needs input")
	d.hasNot("BE-127")
}

func TestInboxEmpty(t *testing.T) {
	f := newFixture(t)
	for _, k := range []string{"BE-127", "FE-9"} {
		f.must(f.d.Cancel(context.Background(), f.st, f.runs[k].Pipeline, f.runs[k].Number))
	}
	d := drive(t, NewInbox(&f.d), 80, 24)
	d.has("all answered", "Nothing waits for you.")
}

// TestSizes renders every screen at common and tiny sizes: always exactly
// the terminal's height, never wider than it.
func TestSizes(t *testing.T) {
	f := newFixture(t)
	screens := map[string]func() tea.Model{
		"profiles":  func() tea.Model { return NewProfiles(&f.d) },
		"pipelines": func() tea.Model { return NewPipelines(&f.d, "youtrack-xlsft") },
		"board":     func() tea.Model { return NewBoard(&f.d, "backend") },
		"run":       func() tea.Model { return NewRun(&f.d, "backend", f.runs["BE-127"].Number, true) },
		"inbox":     func() tea.Model { return NewInbox(&f.d) },
	}
	for name, mk := range screens {
		for _, size := range [][2]int{{100, 30}, {80, 24}, {60, 20}, {40, 12}, {20, 6}} {
			t.Run(fmt.Sprintf("%s/%dx%d", name, size[0], size[1]), func(t *testing.T) {
				d := drive(t, mk(), size[0], size[1])
				d.fits(size[0], size[1])
				d.keys("?")
				d.fits(size[0], size[1])
			})
		}
	}
}

func TestHelpLine(t *testing.T) {
	f := newFixture(t)
	b := newBase(&f.d)
	help := []binding{{"enter", "open"}, {"a", "answer"}, {"c", "cancel"}, {"q", "quit"}}
	if got := ansi.Strip(b.helpLines(help, 80)[0]); got != "enter open  a answer  c cancel  q quit" {
		t.Errorf("wide help %q", got)
	}
	if got := ansi.Strip(b.helpLines(help, 30)[0]); got != "enter open  a answer  ? more" {
		t.Errorf("narrow help %q", got)
	}
	b.fullHelp = true
	if got := b.helpLines(help, 22); len(got) != 2 {
		t.Errorf("full help %q", got)
	}
}

func TestElapsed(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{{42 * time.Second, "42s"}, {4*time.Minute + 12*time.Second, "4m12s"}, {3*time.Hour + 5*time.Minute, "3h05m"}, {72 * time.Hour, "3d"}} {
		if got := elapsed(c.d); got != c.want {
			t.Errorf("elapsed(%s) = %s, want %s", c.d, got, c.want)
		}
	}
}
