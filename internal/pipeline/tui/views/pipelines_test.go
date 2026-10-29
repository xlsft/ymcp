package views

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
)

// catalog is a changeable list of pipelines behind a fixture, with fake
// copy and rename.
type catalog struct {
	list    []spec.Pipeline
	copies  []string
	renames []string
}

// withCatalog makes the fixture list backend, frontend and extra more
// pipelines of youtrack-work named svc-01, svc-02, ….
func withCatalog(f *fixture, extra int) *catalog {
	c := &catalog{list: []spec.Pipeline{
		pipelineSpec("backend", "youtrack-work", "project: BE #Unresolved"),
		pipelineSpec("frontend", "youtrack-work", "project: FE #Unresolved"),
	}}
	for i := 1; i <= extra; i++ {
		c.list = append(c.list, pipelineSpec(fmt.Sprintf("svc-%02d", i), "youtrack-work", "project: SVC"))
	}
	f.d.Pipelines = func() ([]spec.Pipeline, error) {
		var out []spec.Pipeline
		for _, p := range c.list {
			if !slices.Contains(f.deleted, p.Name) {
				out = append(out, p)
			}
		}
		return out, nil
	}
	f.d.CopyPipeline = func(from, to string) error {
		p, ok := pipelineByName(c.list, from)
		if !ok {
			return errors.New("no " + from)
		}
		if _, ok := pipelineByName(c.list, to); ok {
			return fmt.Errorf("pipeline %q already exists", to)
		}
		c.copies = append(c.copies, from+">"+to)
		p.Name = to
		c.list = append(c.list, p)
		return nil
	}
	f.d.RenamePipeline = func(_ context.Context, _ *store.Store, from, to string) error {
		for i := range c.list {
			if c.list[i].Name == from {
				c.renames = append(c.renames, from+">"+to)
				c.list[i].Name = to
				return nil
			}
		}
		return errors.New("no " + from)
	}
	return c
}

func (d *driver) pipelines() *Pipelines { return d.m.(*Pipelines) }

// selectedCard is the name on the card with the heavy frame, or "+".
func (d *driver) selectedCard() string {
	for _, l := range strings.Split(d.view(), "\n") {
		if i := strings.Index(l, "┃ "); i >= 0 {
			return strings.Fields(l[i+len("┃ "):])[0]
		}
		if strings.Contains(l, "┇") && strings.Contains(l, "+") {
			return "+"
		}
	}
	return ""
}

func TestPipelines(t *testing.T) {
	f := newFixture(t)
	d := drive(t, NewPipelines(&f.d, "youtrack-work"), 100, 30)
	d.fits(100, 30)
	d.has("ymcp pipelines", "youtrack-work", "⚑ 2 need input",
		// backend's card: name and runner, the agent, the counts with the
		// failed and needs-input flags inside, the source query
		"backend", "● running", "Claude", "working 2 · review 2 · done 1", "✗ 1  ⚑ 1", "project: BE #Unresolved",
		// frontend's card and the + card
		"frontend", "○ stopped", "working 0 · review 0 · done 0",
		"+", "New pipeline",
		"←→↑↓ move", "enter actions", "o board", "n new", "r run", "s stop", "e edit", "? more")
	d.hasNot("NAME", "RUNNER", // no table any more
		"publish", "push the branch", "/home/dev/code/backend") // in the menu only
	// The flags sit inside the cards, not on their frames.
	for _, l := range strings.Split(d.view(), "\n") {
		if strings.Contains(l, "╭") || strings.Contains(l, "┏") {
			if strings.Contains(l, "⚑") || strings.Contains(l, "✗") {
				t.Errorf("a flag on a card's frame: %q", l)
			}
		}
	}
	if got := d.selectedCard(); got != "backend" {
		t.Errorf("selected %q, want backend", got)
	}

	d.keys("o")
	if got := d.lastNav(); got != (OpenBoard{Pipeline: "backend"}) {
		t.Errorf("o: %#v", got)
	}
	d.keys("b")
	if got := d.lastNav(); got != (OpenBoard{Pipeline: "backend"}) {
		t.Errorf("b: %#v", got)
	}
	d.keys("e")
	if got, ok := d.lastNav().(OpenWizard); !ok || got.Existing == nil || got.Existing.Name != "backend" || got.Profile != "youtrack-work" {
		t.Errorf("e: %#v", got)
	}

	// backend runs: r says so, s stops it, d refuses.
	d.keys("r")
	d.has("backend is already running.")
	d.keys("s")
	d.has("Stopped backend.")
	d.keys("d")
	d.has("backend is running: stop it first (s)")

	// frontend is stopped: r starts it, d asks first.
	d.keys("right", "r")
	d.has("Started frontend in the background (pid 777)")
	if want := []string{"stop backend", "start frontend"}; !slices.Equal(f.daemon.calls, want) {
		t.Errorf("daemon calls %v, want %v", f.daemon.calls, want)
	}
	d.keys("d")
	d.has("Delete pipeline frontend? Its run history stays.", "y yes", "n no")
	d.keys("n")
	d.has("Nothing changed.")
	if len(f.deleted) != 0 {
		t.Fatalf("deleted %v after no", f.deleted)
	}
	d.keys("d", "y")
	if !slices.Equal(f.deleted, []string{"frontend"}) {
		t.Fatalf("deleted %v", f.deleted)
	}
	d.has("Deleted pipeline frontend.")
	d.hasNot("○ stopped")
	if got := d.selectedCard(); got != "+" {
		t.Errorf("after deleting the last pipeline the cursor is on %q, want +", got)
	}

	d.keys("?")
	d.has("L logs", "c copy", "R rename", "d delete", "i inbox", "esc back", "q quit")
	d.keys("esc")
	if got := d.lastNav(); got != (Back{}) {
		t.Errorf("esc: %#v", got)
	}
}

func TestPipelinesDaemonError(t *testing.T) {
	f := newFixture(t)
	f.daemon.err = errors.New("pipeline frontend: agent claude not found")
	d := drive(t, NewPipelines(&f.d, "youtrack-work"), 100, 30)
	d.keys("right", "r")
	d.has("✗ could not start frontend: pipeline frontend: agent claude not found")
}

func TestPipelinesEmpty(t *testing.T) {
	f := newFixture(t)
	d := drive(t, NewPipelines(&f.d, "youtrack-acme"), 80, 24)
	d.has("No pipelines in youtrack-acme yet.", "Press n or enter on + to create one.", "New pipeline")
	if got := d.selectedCard(); got != "+" {
		t.Errorf("selected %q, want +", got)
	}
	d.keys("enter")
	if got, ok := d.lastNav().(OpenWizard); !ok || got.Profile != "youtrack-acme" || got.Existing != nil {
		t.Errorf("enter on +: %#v", got)
	}
	d.keys("n")
	if got, ok := d.lastNav().(OpenWizard); !ok || got.Profile != "youtrack-acme" {
		t.Errorf("n: %#v", got)
	}
}

// TestPipelinesGrid lays the cards out at several sizes: as many columns
// as fit, one on narrow terminals, always within the screen, with a
// margin around the grid and every card as tall as the others.
func TestPipelinesGrid(t *testing.T) {
	for _, c := range []struct{ w, h, cols int }{{200, 50, 4}, {120, 30, 2}, {100, 30, 2}, {80, 24, 1}, {50, 20, 1}} {
		t.Run(fmt.Sprintf("%dx%d", c.w, c.h), func(t *testing.T) {
			f := newFixture(t)
			withCatalog(f, 10)
			d := drive(t, NewPipelines(&f.d, "youtrack-work"), c.w, c.h)
			d.fits(c.w, c.h)
			top := strings.Split(d.view(), "\n")[2] // the top edges of the first row
			if got := strings.Count(top, "╭") + strings.Count(top, "┏"); got != c.cols {
				t.Errorf("%d cards in the first row, want %d:\n%s", got, c.cols, d.view())
			}
			if margin := len(top) - len(strings.TrimLeft(top, " ")); margin < 1 {
				t.Errorf("no margin left of the grid: %q", top)
			}
			d.has("backend", "● running")
			d.keys("enter")
			d.fits(c.w, c.h)
			d.has("Open board", "Stop runner", "Delete…")
			d.keys("esc", "G")
			d.fits(c.w, c.h)
			d.has("New pipeline")
			// The + card is as tall as the cards beside or above it.
			lines := strings.Split(d.view(), "\n")
			plusTop, plusBottom := -1, -1
			for i, l := range lines {
				if strings.Contains(l, "┏╍") && plusTop < 0 {
					plusTop = i
				}
				if strings.Contains(l, "┗╍") {
					plusBottom = i
				}
			}
			if plusTop < 0 || plusBottom-plusTop != cardLines+1 {
				t.Errorf("+ card from line %d to %d, want %d lines inside:\n%s", plusTop, plusBottom, cardLines, d.view())
			}
		})
	}
}

// TestPipelinesMove moves the cursor in two dimensions.
func TestPipelinesMove(t *testing.T) {
	f := newFixture(t)
	withCatalog(f, 10) // 12 pipelines and the + card
	d := drive(t, NewPipelines(&f.d, "youtrack-work"), 100, 30)
	// Two columns: 0 1 / 2 3 / … / 10 11 / 12(+).
	for _, step := range []struct {
		key  string
		want int
	}{
		{"right", 1}, {"right", 1}, {"down", 3}, {"left", 2}, {"left", 2}, {"up", 0}, {"up", 0},
		{"l", 1}, {"j", 3}, {"h", 2}, {"k", 0},
		{"G", 12}, {"right", 12}, {"up", 10}, {"down", 12}, {"down", 12}, {"k", 10}, {"l", 11}, {"j", 12}, {"g", 0},
	} {
		d.keys(step.key)
		if got := d.pipelines().sel; got != step.want {
			t.Fatalf("after %s the cursor is on %d, want %d", step.key, got, step.want)
		}
	}
	d.keys("right", "down")
	if got := d.selectedCard(); got != "svc-02" {
		t.Errorf("selected %q, want svc-02", got)
	}

	// Four columns: 0-3 / 4-7 / 8-11 / 12(+). Down from 10 lands on the
	// last card of the shorter row.
	d.send(tea.WindowSizeMsg{Width: 200, Height: 50})
	d.keys("g", "down", "down")
	if got := d.pipelines().sel; got != 8 {
		t.Errorf("down down from 0: %d, want 8", got)
	}
	d.keys("down", "down")
	if got := d.pipelines().sel; got != 12 {
		t.Errorf("down on the last row: %d, want 12", got)
	}
	d.keys("g", "right", "right", "down", "down", "down")
	if got := d.pipelines().sel; got != 12 {
		t.Errorf("down from 10: %d, want 12 (+)", got)
	}
	if got := d.selectedCard(); got != "+" {
		t.Errorf("selected %q, want +", got)
	}
	d.keys("enter")
	if got, ok := d.lastNav().(OpenWizard); !ok || got.Existing != nil {
		t.Errorf("enter on +: %#v", got)
	}
}

// TestPipelinesScroll keeps the cursor's row on screen.
func TestPipelinesScroll(t *testing.T) {
	f := newFixture(t)
	withCatalog(f, 10)
	d := drive(t, NewPipelines(&f.d, "youtrack-work"), 100, 24) // two rows of two
	d.fits(100, 24)
	d.has("backend", "frontend", "svc-01", "svc-02", "↓ 9 more")
	d.hasNot("svc-03", "more  ↓")
	d.keys("down", "down")
	d.has("svc-03", "↑ 2 more", "↓ 7 more")
	d.hasNot("backend", "frontend")
	d.keys("G")
	d.has("svc-09", "svc-10", "New pipeline", "↑ 10 more")
	d.hasNot("more  ↓", "svc-08")
	d.keys("g")
	d.has("backend", "↓ 9 more")
	d.fits(100, 24)

	// A short terminal shows one row.
	d.send(tea.WindowSizeMsg{Width: 100, Height: 12})
	d.fits(100, 12)
	d.has("backend", "working 2", "frontend", "↓ 11 more")
	d.hasNot("svc-01")

	// Shorter still: smaller cards, the counts kept.
	d.send(tea.WindowSizeMsg{Width: 100, Height: 9})
	d.fits(100, 9)
	d.has("backend", "working 2", "frontend")
	d.hasNot("project: BE")
}

func TestPipelinesMenu(t *testing.T) {
	f := newFixture(t)
	withCatalog(f, 1)
	d := drive(t, NewPipelines(&f.d, "youtrack-work"), 120, 30)
	d.keys("enter")
	d.fits(120, 30)
	d.has("─ backend ─", "› Open board", "Stop runner", "Edit", "Logs", "Copy…", "Export…", "Rename…", "Delete…",
		"↑↓ choose", "enter run", "esc close",
		// what the card leaves out
		"agent    Claude", "publish  push the branch", "repo     /home/dev/code/backend", "source   project: BE #Unresolved")
	d.hasNot("Run in background", "o board")
	// Each item shows its key on the right edge of the box.
	for _, item := range [][2]string{{"Open board", "o"}, {"Stop runner", "s"}, {"Edit", "e"}, {"Logs", "L"},
		{"Copy…", "c"}, {"Export…", "x"}, {"Rename…", "R"}, {"Delete…", "d"}} {
		re := regexp.MustCompile(regexp.QuoteMeta(item[0]) + ` +` + item[1] + ` │`)
		if !re.MatchString(d.view()) {
			t.Errorf("no %q with key %s in the menu:\n%s", item[0], item[1], d.view())
		}
	}

	// Logs is off without a log reader; the reason shows when it is
	// chosen, and enter refuses it.
	d.keys("down", "down", "down")
	d.has("› Logs", "The runner log can't be")
	d.hasNot("Stop the runner")
	d.keys("enter")
	d.has("› Logs", "✗ The runner log can't be read here; run `ymcp pipeline backend logs`.")
	// Rename and delete wait for the runner to stop.
	d.keys("down", "down", "down")
	d.has("› Rename…", "Stop the runner before", "renaming")
	d.keys("down")
	d.has("› Delete…", "Stop the runner before", "deleting")
	d.keys("enter")
	if len(f.deleted) != 0 {
		t.Fatal("deleted a running pipeline")
	}
	d.keys("down") // stays on the last item
	d.has("› Delete…")
	d.keys("esc")
	d.hasNot("Open board", "Delete…")
	d.has("enter actions")

	// The first item opens the board.
	d.keys("enter", "enter")
	if got := d.lastNav(); got != (OpenBoard{Pipeline: "backend"}) {
		t.Errorf("Open board: %#v", got)
	}
	d.hasNot("Open board")

	// frontend is stopped: its menu runs it; a key runs its item.
	d.keys("right", "enter")
	d.has("─ frontend ─", "Run in background")
	d.hasNot("Stop runner")
	d.keys("r")
	d.hasNot("Run in background")
	if want := []string{"start frontend"}; !slices.Equal(f.daemon.calls, want) {
		t.Errorf("daemon calls %v, want %v", f.daemon.calls, want)
	}
	// Rename refuses while it has active runs.
	d.keys("enter", "R")
	d.has("✗ frontend has active runs", "─ frontend ─")
	d.keys("q") // q closes the menu, not the TUI
	d.hasNot("─ frontend ─")
	if got := d.lastNav(); got != nil {
		t.Errorf("q in the menu: %#v", got)
	}

	// With a log reader, Logs opens the log.
	f.d.RunnerLog = func(context.Context, string) (string, error) { return "", nil }
	d.keys("enter", "L")
	if got := d.lastNav(); got != (OpenLogs{Pipeline: "frontend"}) {
		t.Errorf("Logs: %#v", got)
	}
	d.keys("L")
	if got := d.lastNav(); got != (OpenLogs{Pipeline: "frontend"}) {
		t.Errorf("L: %#v", got)
	}

	// The menu stays on screen at small sizes.
	for _, size := range [][2]int{{80, 24}, {50, 20}, {40, 12}} {
		d.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		d.keys("enter")
		d.fits(size[0], size[1])
		d.has("Open board")
		d.keys("esc")
	}
}

func TestPipelinesCopyRename(t *testing.T) {
	f := newFixture(t)
	c := withCatalog(f, 1)
	d := drive(t, NewPipelines(&f.d, "youtrack-work"), 120, 30)

	// Copy… from the menu suggests a name.
	d.keys("enter", "c")
	d.has("─ Copy backend ─", "New name", "backend-copy", "enter save", "esc cancel")
	d.typeText("X!")
	d.keys("enter")
	d.has("─ Copy backend ─", "is invalid")
	d.keys("esc")
	d.hasNot("Copy backend")
	if len(c.copies) != 0 {
		t.Fatalf("copied %v", c.copies)
	}
	d.keys("c", "enter")
	if want := []string{"backend>backend-copy"}; !slices.Equal(c.copies, want) {
		t.Fatalf("copies %v, want %v", c.copies, want)
	}
	d.has("✓ Copied backend to backend-copy.", "backend-copy")
	if got := d.selectedCard(); got != "backend-copy" {
		t.Errorf("selected %q after the copy, want backend-copy", got)
	}
	// A name that is taken keeps the box open.
	d.keys("g", "c")
	for range len("backend-copy") {
		d.send(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	d.typeText("frontend")
	d.keys("enter")
	d.has(`pipeline "frontend" already exists`, "─ Copy backend ─")
	d.keys("esc")

	// Rename refuses a running pipeline and one with active runs.
	d.keys("R")
	d.has("✗ backend is running: stop it before renaming (s)")
	d.keys("right", "R")
	d.has("✗ frontend has 1 active run(s)")

	// svc-01 has no runs: rename it.
	d.keys("down", "left")
	if got := d.selectedCard(); got != "svc-01" {
		t.Fatalf("selected %q, want svc-01", got)
	}
	d.keys("enter", "R")
	d.has("─ Rename svc-01 ─", "svc-01")
	for range len("svc-01") {
		d.send(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	d.typeText("api")
	d.keys("enter")
	if want := []string{"svc-01>api"}; !slices.Equal(c.renames, want) {
		t.Fatalf("renames %v, want %v", c.renames, want)
	}
	d.has("✓ Renamed svc-01 to api.")
	if got := d.selectedCard(); got != "api" {
		t.Errorf("selected %q after the rename, want api", got)
	}
	// The same name changes nothing.
	d.keys("R", "enter")
	d.has("Nothing changed.")
	if len(c.renames) != 1 {
		t.Errorf("renames %v", c.renames)
	}
}

func TestCountsLine(t *testing.T) {
	c := counts{working: 2, input: 1, review: 3, done: 15, failed: 1}
	for _, tc := range []struct {
		w    int
		want string
	}{
		{60, "working 2 · review 3 · done 15"},
		{28, "working 2  review 3  done 15"},
		{27, "work 2 · review 3 · done 15"},
		{25, "work 2  review 3  done 15"},
		{23, "work 2 review 3 done 15"},
		{12, "work 2 revi…"},
	} {
		if got := stripped(countsLine(c, tc.w)); got != tc.want {
			t.Errorf("counts at width %d: %q, want %q", tc.w, got, tc.want)
		}
	}
	// The flags go right; they shorten before the counts do.
	for _, tc := range []struct {
		w    int
		want string
	}{
		{60, "working 2 · review 3 · done 15  ✗ 1 failed  ⚑ 1 needs input"},
		{44, "working 2 · review 3 · done 15     ✗ 1  ⚑ 1"},
		{36, "work 2  review 3  done 15  ✗ 1  ⚑ 1"},
		{20, "work 2 r…  ✗ 1  ⚑ 1"},
	} {
		if got := stripped(statsLine(c, tc.w)); got != tc.want {
			t.Errorf("stats at width %d: %q, want %q", tc.w, got, tc.want)
		}
	}
	if got := stripped(statsLine(counts{done: 3}, 40)); got != "working 0 · review 0 · done 3" {
		t.Errorf("without flags: %q", got)
	}
}

func TestCutLeft(t *testing.T) {
	for _, tc := range []struct {
		s    string
		w    int
		want string
	}{
		{"~/code/api", 20, "~/code/api"},
		{"/home/dev/code/backend", 12, "…ode/backend"},
		{"abc", 1, "…"},
		{"abc", 0, ""},
	} {
		if got := cutLeft(tc.s, tc.w); got != tc.want {
			t.Errorf("cutLeft(%q, %d) = %q, want %q", tc.s, tc.w, got, tc.want)
		}
	}
}

func TestLogs(t *testing.T) {
	f := newFixture(t)
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf("log line %d", i))
	}
	calls := 0
	f.d.RunnerLog = func(_ context.Context, name string) (string, error) {
		calls++
		if name != "backend" {
			return "", errors.New("wrong pipeline " + name)
		}
		return strings.Join(lines, "\n") + "\n", nil
	}
	d := drive(t, NewLogs(&f.d, "backend"), 80, 24)
	d.fits(80, 24)
	d.has("backend", "runner log", "following", "log line 100", "log line 81")
	d.hasNot("log line 80\n", "log line 1\n")
	d.keys("g")
	d.has("log line 1\n", "lines 1–20 of 100")
	d.keys("down")
	d.has("lines 2–21 of 100")
	d.keys("G")
	d.has("following", "log line 100")
	lines = append(lines, "log line 101")
	d.keys("r")
	d.has("log line 101")
	if calls != 2 {
		t.Errorf("%d reads, want 2", calls)
	}
	d.keys("esc")
	if got := d.lastNav(); got != (Back{}) {
		t.Errorf("esc: %#v", got)
	}

	f.d.RunnerLog = func(context.Context, string) (string, error) {
		return "ymcp: no runner log yet at /tmp/backend.log; it appears once `ymcp pipeline backend run` starts\n", errors.New("exit status 1")
	}
	d = drive(t, NewLogs(&f.d, "backend"), 80, 24)
	d.has("could not read the log of backend: no runner log yet")

	f.d.RunnerLog = nil
	d = drive(t, NewLogs(&f.d, "backend"), 80, 24)
	d.has("can't be read here", "ymcp pipeline backend logs")
}

func stripped(s string) string { return strings.TrimSpace(ansi.Strip(s)) }
