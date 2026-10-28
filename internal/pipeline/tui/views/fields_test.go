package views

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// fakeFields is the FieldEditor of the card: the fields of project BE, a
// command log, and the answer of Matches.
type fakeFields struct {
	mu       sync.Mutex
	fields   []Field
	reads    []string // projects read
	commands []string // "issue: command"
	applyErr error
	matches  []string // "issue | query"
	match    bool
	matchErr error
	// apply, when set, changes the issue as YouTrack would.
	apply func(issue, command string)
}

func (f *fakeFields) Fields(_ context.Context, _, project string) ([]Field, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, project)
	if project != "BE" {
		return nil, fmt.Errorf("project %s not found", project)
	}
	return f.fields, nil
}

func (f *fakeFields) Apply(_ context.Context, _, issue, command string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return f.applyErr
	}
	f.commands = append(f.commands, issue+": "+command)
	if f.apply != nil {
		f.apply(issue, command)
	}
	return nil
}

func (f *fakeFields) Matches(_ context.Context, _, issue, query string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.matches = append(f.matches, issue+" | "+query)
	return f.match, f.matchErr
}

// last is the last command applied.
func (f *fakeFields) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.commands) == 0 {
		return ""
	}
	return f.commands[len(f.commands)-1]
}

// withFields gives the fixture an editor of the fields of BE, with the
// fields of a YouTrack project like the user's, and BE-131 in YouTrack
// with most of them set.
func withFields(f *fixture, yt *fakeIssues) *fakeFields {
	ff := &fakeFields{match: true, fields: []Field{
		{Name: "Story points", Type: "integer"},
		{Name: "flow", Type: "enum", Multi: true, Values: []string{"frontend", "backend", "mobile"}},
		{Name: "Priority", Type: "enum", Values: []string{"Critical", "Major", "Normal", "Minor"}},
		{Name: "Stage", Type: "state", Values: []string{"Бэклог", "В работе", "Готово"}},
		{Name: "AI", Type: "enum", Values: []string{"true", "false"}},
		{Name: "Assignee", Type: "user"},
		{Name: "Due date", Type: "date"},
		{Name: "Type", Type: "enum", Values: []string{"Bug", "Task", "Feature"}},
		{Name: "State", Type: "state", Values: []string{"Open", "In Progress", "Fixed"}},
	}}
	yt.issues["2-131"] = ytwatch.Issue{ID: "2-131", Key: "BE-131", Summary: "Rate limit the login endpoint",
		Fields: map[string]string{"State": "Open", "Type": "Task", "Priority": "Major", "Stage": "Бэклог", "AI": "true",
			"Assignee": "anna", "flow": "frontend", "Story points": "3", "Due date": ""}}
	f.d.Fields = ff
	return ff
}

// issueCard drives the card of backlog issue BE-131.
func issueCard(t *testing.T, f *fixture, w, h int) *driver {
	t.Helper()
	return drive(t, NewIssueCard(&f.d, "backend", "youtrack-work", f.backlog.issues[0]), w, h)
}

// TestChipGrid: chips line up in columns, at most four a row, a group on
// rows of its own; what does not fit ends in "+N more".
func TestChipGrid(t *testing.T) {
	chips := []string{"State: Open", "Type: Task", "Priority: Major", "AI: true", "Assignee: anna", "flow: frontend"}
	lines, all := chipGrid([][]string{chips, {"backlog"}}, 100, 5)
	got := make([]string, len(lines))
	for i, l := range lines {
		got[i] = strings.TrimRight(ansi.Strip(l), " ")
	}
	want := []string{
		"State: Open     Type: Task      Priority: Major  AI: true",
		"Assignee: anna  flow: frontend",
		"backlog",
	}
	if !all || !slices.Equal(got, want) {
		t.Errorf("grid (all %v):\n%s", all, strings.Join(got, "\n"))
	}
	for _, l := range lines {
		if ansi.StringWidth(l) != ansi.StringWidth(lines[0]) {
			t.Errorf("rows of different widths: %q", lines)
		}
	}

	// Narrower: fewer columns, still lined up.
	lines, all = chipGrid([][]string{chips}, 40, 5)
	if !all || len(lines) != 3 || ansi.StringWidth(lines[0]) > 40 {
		t.Errorf("narrow grid (all %v): %q", all, lines)
	}
	if a, b := strings.Index(ansi.Strip(lines[0]), "Type"), strings.Index(ansi.Strip(lines[1]), "AI"); a != b {
		t.Errorf("columns do not line up: %q", lines)
	}

	// Too many rows: the last one says what is left out.
	lines, all = chipGrid([][]string{chips, {"backlog"}}, 100, 1)
	if all || len(lines) != 1 {
		t.Errorf("one row (all %v): %q", all, lines)
	}
	if got := ansi.Strip(lines[0]); !strings.Contains(got, "+4 more") || strings.Contains(got, "AI: true") {
		t.Errorf("one row: %q", got)
	}
	if lines, all = chipGrid(nil, 10, 1); lines != nil || !all {
		t.Errorf("no chips: %q", lines)
	}
}

// TestIssueCardFieldsGrid: every field with a value shows as a chip,
// State, Type and Priority first, then by name, four a row in columns.
func TestIssueCardFieldsGrid(t *testing.T) {
	f := newFixture(t)
	yt := withIssues(f)
	withFields(f, yt)
	d := issueCard(t, f, 160, 45)
	d.fits(160, 45)
	d.has("State: Open", "Type: Task", "Priority: Major", "AI: true", "Assignee: anna", "flow: frontend",
		"Stage: Бэклог", "Story points: 3", "backlog", "e fields")
	d.hasNot("Due date")
	lines := strings.Split(d.view(), "\n")
	row := func(chip string) int {
		return slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, chip) })
	}
	first, second := row("State: Open"), row("Assignee: anna")
	if first < 0 || second != first+1 || row("backlog") != second+1 {
		t.Fatalf("not three rows of chips:\n%s", d.view())
	}
	for _, c := range []string{"Type: Task", "Priority: Major", "AI: true"} {
		if row(c) != first {
			t.Errorf("%s is not on the first row:\n%s", c, d.view())
		}
	}
	for _, c := range []string{"flow: frontend", "Stage: Бэклог", "Story points: 3"} {
		if row(c) != second {
			t.Errorf("%s is not on the second row:\n%s", c, d.view())
		}
	}
	col := func(l, chip string) int { return ansi.StringWidth(l[:strings.Index(l, chip)]) }
	if a, b := col(lines[first], "Type: Task"), col(lines[second], "flow: frontend"); a != b {
		t.Errorf("columns do not line up (%d, %d):\n%s", a, b, d.view())
	}
	if row("Rate limit the login endpoint") >= first {
		t.Errorf("the title is not above the grid:\n%s", d.view())
	}

	// Smaller screens keep the card whole.
	for _, size := range [][2]int{{120, 40}, {80, 24}, {60, 20}, {40, 12}, {20, 6}} {
		d.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		d.fits(size[0], size[1])
	}
	d.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	d.has("BE-131", "State: Open")
}

// TestFieldsMenu: e lists the project's fields with their values, "—"
// for an empty one, reading them once.
func TestFieldsMenu(t *testing.T) {
	f := newFixture(t)
	yt := withIssues(f)
	ff := withFields(f, yt)
	d := issueCard(t, f, 160, 45)
	d.keys("e")
	d.has("Fields · BE", "› State", "Story points", "esc close")
	lines := strings.Split(d.view(), "\n")
	for name, value := range map[string]string{"Due date": "—", "Priority": "Major", "flow": "frontend", "Assignee": "anna"} {
		i := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, " "+name+" ") })
		if i < 0 || !strings.Contains(lines[i], value) {
			t.Errorf("%s is not shown with %s:\n%s", name, value, d.view())
		}
	}
	// In order: State, Type, Priority, then by name.
	v := d.view()
	order := []string{"› State", "Type", "Priority", "AI", "Assignee", "Due date", "flow", "Stage", "Story points"}
	at := 0
	for _, name := range order {
		i := strings.Index(v[at:], name)
		if i < 0 {
			t.Fatalf("%s is out of order:\n%s", name, v)
		}
		at += i
	}
	// A date can't be changed here.
	d.keys("down", "down", "down", "down", "down")
	d.has("Due date can't be changed")
	d.keys("enter")
	d.has("Due date can't be changed here; o opens the issue")
	d.keys("esc")
	d.hasNot("Fields · BE")
	d.keys("e")
	if len(ff.reads) != 1 || ff.reads[0] != "BE" {
		t.Errorf("fields read %v, want once for BE", ff.reads)
	}
	d.keys("q")
	d.hasNot("Fields · BE")
	if got := d.lastNav(); got != nil {
		t.Errorf("q in the menu: %#v", got)
	}
}

// TestFieldsSetValue: enter on a field lists its values, the current one
// under the cursor; typing narrows them; enter sets one with a command,
// values with spaces in braces, and the issue is read again.
func TestFieldsSetValue(t *testing.T) {
	f := newFixture(t)
	yt := withIssues(f)
	ff := withFields(f, yt)
	ff.apply = func(_, command string) {
		yt.mu.Lock()
		defer yt.mu.Unlock()
		is := yt.issues["2-131"]
		is.Fields = maps.Clone(is.Fields)
		switch command {
		case "Priority Critical":
			is.Fields["Priority"] = "Critical"
		case "Stage {В работе}":
			is.Fields["Stage"] = "В работе"
		}
		yt.issues["2-131"] = is
	}
	d := issueCard(t, f, 160, 45)
	d.keys("e", "down", "down", "enter")
	d.has("Priority", "› Major", "Critical", "type to filter", "enter set")
	d.typeText("cr")
	d.has("› Critical")
	d.hasNot("Minor")
	d.keys("enter")
	if got := ff.last(); got != "2-131: Priority Critical" {
		t.Errorf("applied %q", got)
	}
	d.has("Set Priority of BE-131 to Critical.", "Priority: Critical", "Fields · BE")

	// A value with a space goes in braces; Cyrillic filters as typed.
	d.keys("down", "down", "down", "down", "down")
	d.has("› Stage")
	d.keys("enter")
	d.typeText("раб")
	d.has("› В работе")
	d.keys("enter")
	if got := ff.last(); got != "2-131: Stage {В работе}" {
		t.Errorf("applied %q", got)
	}
	// The source query still finds the issue: the card stays.
	if len(ff.matches) != 2 || ff.matches[1] != "BE-131 | project: BE #Unresolved" {
		t.Errorf("matches %q", ff.matches)
	}
	if got := d.lastNav(); got != nil {
		t.Errorf("the card moved: %#v", got)
	}

	// The current value again changes nothing.
	d.keys("enter", "enter")
	d.has("Stage is В работе already.")
	if len(ff.commands) != 2 {
		t.Errorf("applied %q", ff.commands)
	}

	// A failure shows on the status line.
	ff.applyErr = errors.New("400 Bad Request")
	d.keys("up", "up", "up", "up", "up", "enter", "down", "enter")
	d.has("could not change Priority of BE-131: 400 Bad Request")
}

// TestFieldsMultiValue: on a field with several values enter adds a value
// or removes it, and the list stays open.
func TestFieldsMultiValue(t *testing.T) {
	f := newFixture(t)
	yt := withIssues(f)
	ff := withFields(f, yt)
	d := issueCard(t, f, 160, 45)
	d.keys("e", "down", "down", "down", "down", "down", "down", "enter")
	d.has("flow · add or remove", "› frontend ", "enter add/remove")
	d.keys("enter")
	if got := ff.last(); got != "2-131: remove flow frontend" {
		t.Errorf("applied %q", got)
	}
	d.has("Removed frontend from flow of BE-131.", "flow · add or remove")
	d.keys("down", "down", "enter")
	if got := ff.last(); got != "2-131: add flow mobile" {
		t.Errorf("applied %q", got)
	}
	d.has("Added mobile to flow of BE-131.")
}

// TestFieldsUserAndTyped: a user field lists the people found for what is
// typed; a typed field opens a box.
func TestFieldsUserAndTyped(t *testing.T) {
	f := newFixture(t)
	yt := withIssues(f)
	ff := withFields(f, yt)
	d := issueCard(t, f, 160, 45)
	d.keys("e", "down", "down", "down", "down", "enter")
	d.has("Assignee", "› Anna Petrova")
	d.typeText("bor")
	d.has("› Boris Ivanov", "boris")
	d.keys("enter")
	if got := ff.last(); got != "2-131: Assignee boris" {
		t.Errorf("applied %q", got)
	}

	d.keys("G", "enter")
	d.has("Story points", "A whole number", "› 3")
	d.send(tea.KeyMsg{Type: tea.KeyBackspace})
	d.typeText("5")
	d.keys("enter")
	if got := ff.last(); got != "2-131: Story points 5" {
		t.Errorf("applied %q", got)
	}
	d.has("Set Story points of BE-131 to 5.")
}

// TestFieldsLeaveBacklog: a change after which the source query no longer
// finds a backlog issue closes its card and drops it from the board.
func TestFieldsLeaveBacklog(t *testing.T) {
	f := newFixture(t)
	yt := withIssues(f)
	ff := withFields(f, yt)
	ff.match = false
	d := issueCard(t, f, 160, 45)
	d.keys("e", "down", "down", "down", "enter")
	d.has("› true")
	d.keys("down", "enter")
	if got := ff.last(); got != "2-131: AI false" {
		t.Errorf("applied %q", got)
	}
	if len(ff.matches) != 1 || ff.matches[0] != "BE-131 | project: BE #Unresolved" {
		t.Errorf("matches %q", ff.matches)
	}
	want := []tea.Msg{Back{}, BacklogLeft{Pipeline: "backend", Issue: "BE-131"}}
	if !slices.ContainsFunc(d.nav, func(m tea.Msg) bool { return m == want[0] }) ||
		!slices.ContainsFunc(d.nav, func(m tea.Msg) bool { return m == want[1] }) {
		t.Errorf("nav %#v, want %#v", d.nav, want)
	}

	// The board drops the issue at once; a search that started before
	// does not bring it back, a later one tells for itself.
	b := drive(t, NewBoard(&f.d, "backend"), 100, 30)
	b.has("BACKLOG 3", "BE-131")
	board := b.m.(*Board)
	b.send(BacklogLeft{Pipeline: "frontend", Issue: "BE-131"})
	b.has("BACKLOG 3")
	b.send(BacklogLeft{Pipeline: "backend", Issue: "BE-131"})
	b.has("BACKLOG 2", "BE-131 left the backlog")
	b.send(backlogLoaded{to: board.id, n: board.searches, issues: f.backlog.issues})
	b.has("BACKLOG 2")
	be131 := f.backlog.issues[0]
	f.backlog.issues = f.backlog.issues[1:]
	calls := f.backlog.calls
	b.tick()
	if f.backlog.calls != calls+1 {
		t.Errorf("no new search after the issue left (%d calls)", f.backlog.calls)
	}
	b.has("BACKLOG 2")
	if len(board.gone) != 0 {
		t.Errorf("gone %v after a later search", board.gone)
	}

	// Unsure whether it still matches: the card stays and says so.
	ff.match, ff.matchErr = true, errors.New("timeout")
	d = drive(t, NewIssueCard(&f.d, "backend", "youtrack-work", be131), 160, 45)
	d.keys("e", "down", "down", "down", "enter", "down", "enter")
	d.has("changed AI of BE-131, but could not tell whether the source query", "still finds it:", "timeout")
	if got := d.lastNav(); got != nil {
		t.Errorf("the card moved: %#v", got)
	}
}

// TestFieldsOnRun: a run's card edits fields too, without asking the
// source query.
func TestFieldsOnRun(t *testing.T) {
	f := newFixture(t)
	yt := withIssues(f)
	ff := withFields(f, yt)
	ff.match = false
	d := drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, false), 160, 45)
	d.has("e fields")
	d.keys("e", "down", "down", "enter", "down", "down", "enter")
	if got := ff.last(); got != "2-127: Priority Minor" {
		t.Errorf("applied %q", got)
	}
	if len(ff.matches) != 0 || d.lastNav() != nil {
		t.Errorf("a run's card asked the source query %q", ff.matches)
	}
	d.has("Set Priority of BE-127 to Minor.")
}

// TestFieldsOff: without a FieldEditor, e does nothing and the help does
// not offer it.
func TestFieldsOff(t *testing.T) {
	f := newFixture(t)
	withIssues(f)
	d := issueCard(t, f, 160, 45)
	d.hasNot("e fields")
	d.keys("e")
	d.hasNot("Fields", "Loading fields")
	d = drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, false), 160, 45)
	d.hasNot("e fields")
	d.keys("e")
	d.hasNot("Loading fields")
}

// TestFieldsMenuSizes opens the menu and a list of values at small sizes.
func TestFieldsMenuSizes(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {80, 24}, {60, 20}, {40, 12}, {20, 6}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			f := newFixture(t)
			yt := withIssues(f)
			withFields(f, yt)
			d := issueCard(t, f, size[0], size[1])
			d.keys("e")
			d.fits(size[0], size[1])
			d.keys("down", "down", "enter")
			d.fits(size[0], size[1])
			d.keys("esc", "G", "enter")
			d.fits(size[0], size[1])
		})
	}
}
