package views

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/keys"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// daemonTimeout bounds `run -d` and `stop`, which wait for the runner.
const daemonTimeout = 90 * time.Second

// The grid of pipeline cards.
const (
	// cardMinWidth is the narrowest a card gets while another column
	// still fits; cards stretch to fill the width.
	cardMinWidth = 40
	// cardGap is the space between two cards of a row, rowGap the blank
	// lines between two rows.
	cardGap = 2
	rowGap  = 1
	// gridMargin is the space left and right of the grid.
	gridMargin = 2
	// cardLines is what a card shows inside its frame when the terminal
	// is tall enough: name and runner, a blank line, the agent, the run
	// counts and the source query.
	cardLines = 5
	// detailsMax is the widest the details of a pipeline's menu get.
	detailsMax = 48
)

// Pipelines shows the pipelines of one profile as a grid of cards, with
// a last card that makes a new one. Enter opens a menu of what can be done
// with the pipeline under the cursor.
type Pipelines struct {
	base
	profile string
	loaded  bool
	err     error
	rows    []pipelineRow
	sel     int // index into rows; len(rows) is the + card
	top     int // first grid row on screen
	want    string
	home    string
	menu    menu
	prompt  namePrompt
}

type pipelinesLoaded struct {
	to   int64
	rows []pipelineRow
	err  error
}

// NewPipelines makes the pipelines screen of a profile.
func NewPipelines(d *Deps, profile string) *Pipelines {
	home, _ := os.UserHomeDir()
	return &Pipelines{base: newBase(d), profile: profile, home: home}
}

// Init loads the pipelines.
func (m *Pipelines) Init() tea.Cmd { return m.load }

// Profile is the profile the screen shows.
func (m *Pipelines) Profile() string { return m.profile }

func (m *Pipelines) load() tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	rows, err := m.d.loadPipelines(ctx)
	out := rows[:0]
	for _, r := range rows {
		if r.p.Profile == m.profile {
			out = append(out, r)
		}
	}
	return pipelinesLoaded{to: m.id, rows: out, err: err}
}

// selected is the pipeline under the cursor; false on the + card.
func (m *Pipelines) selected() (pipelineRow, bool) {
	if m.sel < 0 || m.sel >= len(m.rows) {
		return pipelineRow{}, false
	}
	return m.rows[m.sel], true
}

// Update handles keys, ticks and reloads.
func (m *Pipelines) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if done, cmd := m.common(msg); done {
		return m, cmd
	}
	switch msg := msg.(type) {
	case pipelinesLoaded:
		if msg.to != m.id {
			return m, nil
		}
		plus := m.loaded && m.sel >= len(m.rows)
		m.loaded, m.err = true, msg.err
		keep, found := m.want, false
		if r, ok := m.selected(); ok && keep == "" {
			keep = r.p.Name
		}
		m.want = ""
		m.rows = msg.rows
		m.sel = clamp(m.sel, len(m.rows)+1)
		if plus && keep == "" {
			m.sel = len(m.rows)
		}
		for i, r := range m.rows {
			if r.p.Name == keep {
				m.sel, found = i, true
			}
		}
		if !found { // the pipeline of the menu is gone
			m.menu.open, m.prompt.open = false, false
		}
	case fileChosen:
		if msg.to == m.id {
			return m, m.chosen(msg)
		}
	case imported:
		if msg.to != m.id {
			return m, nil
		}
		m.say(importSummary(msg.res))
		if len(msg.res.Pipelines) > 0 {
			m.want = msg.res.Pipelines[0].To
		}
		return m, m.load
	case Tick:
		return m, m.load
	case tea.KeyMsg:
		switch {
		case m.prompt.open:
			return m, m.prompt.key(msg)
		case m.menu.open:
			return m, m.menuKey(keys.Of(msg))
		}
		return m, m.key(keys.Of(msg))
	default:
		return m, m.prompt.update(msg)
	}
	return m, nil
}

// gridLayout is how many cards fit side by side, how wide each is and the
// margin left of the grid.
func gridLayout(w int) (cols, cw, mx int) {
	w = max(w, 1)
	switch {
	case w >= 60:
		mx = gridMargin
	case w >= 30:
		mx = 1
	}
	inner := max(w-2*mx, 1)
	cols = max((inner+cardGap)/(cardMinWidth+cardGap), 1)
	return cols, max((inner-cardGap*(cols-1))/cols, 1), mx
}

// move moves the cursor across the grid of n cards in cols columns.
func (m *Pipelines) move(k string) {
	cols, _, _ := gridLayout(m.width)
	n := len(m.rows) + 1
	switch k {
	case "left", "h":
		if m.sel%cols > 0 {
			m.sel--
		}
	case "right", "l":
		if m.sel%cols < cols-1 && m.sel+1 < n {
			m.sel++
		}
	case "up", "k":
		if m.sel >= cols {
			m.sel -= cols
		}
	case "down", "j":
		switch {
		case m.sel+cols < n:
			m.sel += cols
		case m.sel/cols < (n-1)/cols: // the row below is shorter
			m.sel = n - 1
		}
	case "home", "g":
		m.sel = 0
	case "end", "G":
		m.sel = n - 1
	}
}

func (m *Pipelines) key(k string) tea.Cmd {
	switch k {
	case "q":
		return tea.Quit
	case "esc":
		return navigate(Back{})
	case "?":
		m.fullHelp = !m.fullHelp
		return nil
	case "left", "h", "right", "l", "up", "k", "down", "j", "home", "g", "end", "G":
		m.move(k)
		return nil
	case "n":
		return navigate(OpenWizard{Profile: m.profile})
	case "i":
		return navigate(OpenInbox{})
	case "I":
		return m.importFile()
	case "X":
		if len(m.rows) == 0 {
			m.fail(errors.New("no pipelines to export"))
			return nil
		}
		names := make([]string, len(m.rows))
		for i, r := range m.rows {
			names[i] = r.p.Name
		}
		return m.exportFile(names, "the pipelines of "+m.profile, exportName(m.profile))
	}
	r, ok := m.selected()
	if !ok {
		if k == "enter" || k == " " {
			return navigate(OpenWizard{Profile: m.profile})
		}
		return nil
	}
	if k == "enter" || k == " " {
		m.menu = menu{open: true}
		return nil
	}
	return m.act(k, r)
}

// menuKey handles a key while the menu is open.
func (m *Pipelines) menuKey(k string) tea.Cmd {
	r, ok := m.selected()
	if !ok {
		m.menu.open = false
		return nil
	}
	items := m.menuItems(r)
	pick, _ := m.menu.key(k, items)
	if pick == nil {
		return nil
	}
	if pick.why != "" {
		m.fail(errors.New(pick.why))
		return nil
	}
	m.menu.open = false
	return pick.run()
}

// menuItems are the actions of a pipeline's menu, each with its direct
// key and, when it cannot run now, why.
func (m *Pipelines) menuItems(r pipelineRow) []menuItem {
	name := r.p.Name
	live, running := r.runner.state != runnerStopped, r.runner.state == runnerRunning
	act := func(k string) func() tea.Cmd { return func() tea.Cmd { return m.act(k, r) } }
	runner := menuItem{label: "Run in background", key: "r", run: act("r")}
	if live {
		runner = menuItem{label: "Stop runner", key: "s", run: act("s")}
	}
	if m.d.Daemon == nil {
		runner.why = "Runners can't be started or stopped from here; use the ymcp pipeline command."
	}
	logs := menuItem{label: "Logs", key: "L", run: act("L")}
	if m.d.RunnerLog == nil {
		logs.why = "The runner log can't be read here; run `ymcp pipeline " + name + " logs`."
	}
	rename := menuItem{label: "Rename…", key: "R", run: act("R")}
	del := menuItem{label: "Delete…", key: "d", run: act("d")}
	switch {
	case running:
		rename.why = "Stop the runner before renaming (s)."
		del.why = "Stop the runner before deleting (s)."
	case r.active() > 0:
		rename.why = fmt.Sprintf("%s has active runs, which stay filed under its name; finish or cancel them first.", name)
	}
	return []menuItem{
		{label: "Open board", key: "o", run: act("o")},
		runner,
		{label: "Edit", key: "e", run: act("e")},
		logs,
		{label: "Copy…", key: "c", run: act("c")},
		{label: "Export…", key: "x", run: act("x")},
		rename,
		del,
	}
}

// active is how many of a pipeline's runs are not finished.
func (r pipelineRow) active() int { return r.c.working + r.c.input + r.c.review }

// act runs one of a pipeline's actions by its key.
func (m *Pipelines) act(k string, r pipelineRow) tea.Cmd {
	name := r.p.Name
	switch k {
	case "o", "b":
		return navigate(OpenBoard{Pipeline: name})
	case "e":
		p := r.p
		return navigate(OpenWizard{Profile: m.profile, Existing: &p})
	case "r":
		return startRunner(&m.base, name, r.runner)
	case "s":
		return stopRunner(&m.base, name, r.runner)
	case "L":
		if m.d.RunnerLog == nil {
			m.fail(fmt.Errorf("the runner log can't be read here; run `ymcp pipeline %s logs`", name))
			return nil
		}
		return navigate(OpenLogs{Pipeline: name})
	case "c":
		return m.prompt.start("Copy "+name, name+"-copy", func(to string) (tea.Cmd, error) {
			if err := spec.CheckName(to); err != nil {
				return nil, err
			}
			if err := m.d.CopyPipeline(name, to); err != nil {
				return nil, err
			}
			m.say("Copied " + name + " to " + to + ".")
			m.want = to
			return m.load, nil
		})
	case "x":
		return m.exportFile([]string{name}, name, exportName(name))
	case "R":
		switch {
		case r.runner.state == runnerRunning:
			m.fail(fmt.Errorf("%s is running: stop it before renaming (s)", name))
			return nil
		case r.active() > 0:
			m.fail(fmt.Errorf("%s has %d active run(s), which stay filed under its name; finish or cancel them before renaming", name, r.active()))
			return nil
		}
		return m.prompt.start("Rename "+name, name, func(to string) (tea.Cmd, error) {
			if to == name {
				m.say("Nothing changed.")
				return nil, nil
			}
			if err := spec.CheckName(to); err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
			defer cancel()
			if err := m.d.RenamePipeline(ctx, m.d.Store, name, to); err != nil {
				return nil, err
			}
			m.say("Renamed " + name + " to " + to + ".")
			m.want = to
			return m.load, nil
		})
	case "d":
		if r.runner.state == runnerRunning {
			m.fail(fmt.Errorf("%s is running: stop it first (s)", name))
			return nil
		}
		m.confirm("Delete pipeline "+name+"? Its run history stays.", func() tea.Cmd {
			if err := m.d.DeletePipeline(name); err != nil {
				m.fail(err)
				return nil
			}
			m.say("Deleted pipeline " + name + ".")
			return m.load
		})
	}
	return nil
}

// startRunner starts a pipeline's runner in the background through the
// CLI, reporting the outcome on b's status line.
func startRunner(b *base, name string, r runnerInfo) tea.Cmd {
	switch {
	case b.d.Daemon == nil:
		b.fail(fmt.Errorf("starting runners is not available here; run `ymcp pipeline %s run -d`", name))
		return nil
	case r.state == runnerRunning:
		b.say(name + " is already running.")
		return nil
	}
	b.say("Starting " + name + "…")
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
		defer cancel()
		out, err := b.d.Daemon.Start(ctx, name)
		if err != nil {
			return b.report("", daemonErr("start "+name, out, err))
		}
		return b.report(lastLine(out, "Started "+name+"."), nil)
	}
}

// stopRunner stops a pipeline's runner through the CLI.
func stopRunner(b *base, name string, r runnerInfo) tea.Cmd {
	switch {
	case b.d.Daemon == nil:
		b.fail(fmt.Errorf("stopping runners is not available here; run `ymcp pipeline %s stop`", name))
		return nil
	case r.state == runnerStopped:
		b.say(name + " is not running.")
		return nil
	}
	b.say("Stopping " + name + "…")
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
		defer cancel()
		out, err := b.d.Daemon.Stop(ctx, name)
		if err != nil {
			return b.report("", daemonErr("stop "+name, out, err))
		}
		return b.report(lastLine(out, "Stopped "+name+"."), nil)
	}
}

// daemonErr is the error of a CLI command, with the last line it printed.
func daemonErr(what, out string, err error) error {
	if line := lastLine(out, ""); line != "" {
		return fmt.Errorf("could not %s: %s", what, line)
	}
	return fmt.Errorf("could not %s: %w", what, err)
}

// lastLine is the last non-empty line of out, or def.
func lastLine(out, def string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "ymcp:")); l != "" {
			return l
		}
	}
	return def
}

// View renders the screen.
func (m *Pipelines) View() string {
	w := max(m.width, 1)
	waiting := 0
	for _, r := range m.rows {
		waiting += r.c.input
	}
	header := []string{spread(title("ymcp pipelines")+" "+theme.Bold.Render(m.profile), inputBadge(waiting), w), ""}
	help := []binding{
		{"←→↑↓", "move"}, {"enter", "actions"}, {"o", "board"}, {"n", "new"}, {"r", "run"}, {"s", "stop"},
		{"e", "edit"}, {"L", "logs"}, {"c", "copy"}, {"R", "rename"}, {"d", "delete"}, {"x", "export"},
		{"X", "export all"}, {"I", "import"}, {"i", "inbox"},
		{"esc", "back"}, {"q", "quit"},
	}
	switch {
	case m.prompt.open:
		help = promptHelp
	case m.menu.open:
		help = menuHelp
	}
	if !m.loaded {
		return m.frame(header, []string{theme.Dim.Render("  Loading…")}, help)
	}
	room := m.height - len(header) - 1 - len(m.helpLines(help, w))
	var body []string
	if len(m.rows) == 0 {
		body = append(body, "  No pipelines in "+m.profile+" yet. Press "+theme.Key.Render("n")+" or "+
			theme.Key.Render("enter")+" on "+theme.Key.Render("+")+" to create one.", "")
	}
	if m.err != nil {
		body = append(body, theme.Err.Render("  ! ")+oneLine(m.err.Error()), "")
	}
	grid, ax, ay, aw := m.grid(w, max(room-len(body), 1))
	ay += len(body)
	body = append(body, grid...)
	for len(body) < room {
		body = append(body, "")
	}
	var pop []string
	if r, ok := m.selected(); ok && m.menu.open {
		items := m.menuItems(r)
		var details []string
		if room >= len(items)+9 { // room for the details too
			details = m.details(r)
		}
		pop = m.menu.view(r.p.Name, details, items, w)
	}
	if m.prompt.open {
		pop = m.prompt.view(w)
	}
	if pop != nil {
		x, y := placePopup(ansi.StringWidth(pop[0]), len(pop), ax, ay, aw, w, max(room, 1))
		body = popOver(body, pop, x, y)
	}
	return m.frame(header, body, help)
}

// grid renders the cards that fit in rows lines, scrolled to the
// selected one, and where the selected card is (x, y, width).
func (m *Pipelines) grid(w, rows int) (lines []string, ax, ay, aw int) {
	cols, cw, mx := gridLayout(w)
	n := len(m.rows) + 1
	gridRows := (n + cols - 1) / cols
	content, fit := cardShape(gridRows, rows)
	height := content + 2
	m.sel = clamp(m.sel, n)
	m.top = scrollTo(m.top, m.sel/cols, fit)
	m.top = min(max(m.top, 0), max(gridRows-fit, 0))
	margin := strings.Repeat(" ", mx)
	for row := m.top; row < min(m.top+fit, gridRows); row++ {
		if row > m.top {
			for range rowGap {
				lines = append(lines, "")
			}
		}
		cards := make([][]string, 0, cols)
		for c := range cols {
			i := row*cols + c
			if i >= n {
				break
			}
			if i == m.sel {
				ax, ay, aw = mx+c*(cw+cardGap), len(lines), cw
			}
			if i == len(m.rows) {
				cards = append(cards, plusCard(cw, content, i == m.sel))
			} else {
				cards = append(cards, m.card(m.rows[i], cw, content, i == m.sel))
			}
		}
		for l := range height {
			parts := make([]string, len(cards))
			for j, card := range cards {
				parts[j] = card[l]
			}
			lines = append(lines, margin+strings.Join(parts, strings.Repeat(" ", cardGap)))
		}
	}
	if gridRows > fit {
		var hint []string
		if above := m.top * cols; above > 0 {
			hint = append(hint, fmt.Sprintf("↑ %d more", above))
		}
		if below := n - min((m.top+fit)*cols, n); below > 0 {
			hint = append(hint, fmt.Sprintf("↓ %d more", below))
		}
		lines = append(lines, spread("", theme.Dim.Render(strings.Join(hint, "  "))+margin, w))
	}
	return lines, ax, ay, aw
}

// cardShape picks how many lines the cards show inside their frames and
// how many rows of them fit in rows lines: every row of full cards when
// they fit, else at least two rows of smaller cards (with a line for the
// scroll hint), else one row of the tallest that fit.
func cardShape(gridRows, rows int) (content, fit int) {
	span := func(n, h int) int { return n*h + max(n-1, 0)*rowGap }
	fits := func(h int) int { return max((rows-1+rowGap)/(h+rowGap), 1) }
	for _, c := range []int{cardLines, 4, 3, 2, 1} {
		if span(gridRows, c+2) <= rows {
			return c, gridRows
		}
	}
	if gridRows >= 2 {
		for _, c := range []int{cardLines, 4, 3, 2} {
			if span(2, c+2)+1 <= rows {
				return c, fits(c + 2)
			}
		}
	}
	for _, c := range []int{cardLines, 4, 3, 2} {
		if c+2+1 <= rows {
			return c, 1
		}
	}
	return 1, 1
}

// card is one pipeline's card, cw cells wide with content lines inside:
// the name and the runner, then a blank line, the agent, the run counts
// with the failed and needs-input flags, and the source query, the last
// ones left out first when the card is short. How the work is published
// and the repository show in the card's menu (details).
func (m *Pipelines) card(r pipelineRow, cw, content int, selected bool) []string {
	px := cardPad(cw)
	iw := max(cw-2-2*px, 1)
	edges, edge, name := solidEdges, theme.Dim, theme.Bold.Render(r.p.Name)
	if selected {
		edges, edge, name = heavyEdges, theme.Accent, theme.Accent.Render(r.p.Name)
	}
	head := spread(name, cardRunner(r.runner), iw)
	agent := cut(cardAgent(r.p.Agent), iw)
	stats := statsLine(r.c, iw)
	source := theme.Dim.Render(cut(oneLine(r.p.Source.Query), iw))
	var lines []string
	switch content {
	case 1:
		lines = []string{head}
	case 2:
		lines = []string{head, stats}
	case 3:
		lines = []string{head, agent, stats}
	case 4:
		lines = []string{head, "", agent, stats}
	default:
		lines = []string{head, "", agent, stats, source}
	}
	return paddedBox(lines, cw, px, edges, edge, "", "")
}

// cardPad is the space inside a card's frame on each side: two cells on
// roomy cards, one on narrow ones.
func cardPad(cw int) int {
	if cw >= 44 {
		return 2
	}
	return 1
}

// details are the lines of a pipeline's menu above its actions: what the
// card leaves out.
func (m *Pipelines) details(r pipelineRow) []string {
	p := r.p
	where := homeShort(p.Workspace.Dir, m.home)
	if p.Workspace.Host != "" {
		where = p.Workspace.Host + ":" + where
	}
	field := func(label, value string, fit func(string, int) string) string {
		if value == "" {
			value = theme.Dim.Render("–")
		}
		return theme.Dim.Render(pad(label, 9)) + fit(value, detailsMax-9)
	}
	return []string{
		field("agent", agentLabel(p.Agent), cut),
		field("publish", strategyLabel(p.Strategy), cut),
		field("repo", where, cutLeft),
		field("source", oneLine(p.Source.Query), cut),
	}
}

// cardBadges flag a card's failed runs and the runs that wait for input,
// in short forms ("✗ 1", "⚑ 1") when short is set.
func cardBadges(c counts, short bool) string {
	var out []string
	if c.failed > 0 {
		f := fmt.Sprintf("✗ %d failed", c.failed)
		if short {
			f = fmt.Sprintf("✗ %d", c.failed)
		}
		out = append(out, theme.Err.Render(f))
	}
	if c.input > 0 {
		b := inputBadge(c.input)
		if short {
			b = theme.Badge.Render(fmt.Sprintf("⚑ %d", c.input))
		}
		out = append(out, b)
	}
	return strings.Join(out, " ")
}

// statsLine is a card's run counts with its flags on the right: the
// longest forms that fit w, the flags shortened before the counts.
func statsLine(c counts, w int) string {
	badges := []string{cardBadges(c, false), cardBadges(c, true)}
	if badges[0] == "" {
		return countsLine(c, w)
	}
	forms := countsForms(c)
	for _, f := range forms {
		for _, b := range badges {
			if ansi.StringWidth(f)+2+ansi.StringWidth(b) <= w {
				return spread(f, b, w)
			}
		}
	}
	b := badges[1]
	if ansi.StringWidth(b)+3 > w {
		return countsLine(c, w)
	}
	return spread(cut(forms[len(forms)-1], w-ansi.StringWidth(b)-2), b, w)
}

// plusCard is the last card, which makes a new pipeline; as tall as the
// others.
func plusCard(cw, content int, selected bool) []string {
	px := cardPad(cw)
	iw := max(cw-2-2*px, 1)
	edges, edge, plus, text := dashedEdges, theme.Dim, theme.Bold.Render("+"), theme.Dim.Render("New pipeline")
	if selected {
		edges, edge, plus, text = heavyDashedEdges, theme.Accent, theme.Accent.Render("+"), theme.Bold.Render("New pipeline")
	}
	var lines []string
	switch content {
	case 1:
		lines = []string{centered(plus+" "+text, iw)}
	case 2:
		lines = []string{centered(plus, iw), centered(text, iw)}
	default:
		lines = make([]string, content)
		mid := (content - 1) / 2
		lines[mid], lines[mid+1] = centered(plus, iw), centered(text, iw)
	}
	return paddedBox(lines, cw, px, edges, edge, "", "")
}

// cardRunner is a runner's state as a card shows it.
func cardRunner(r runnerInfo) string {
	switch r.state {
	case runnerRunning:
		return theme.OK.Render("●") + " running"
	case runnerSilent:
		return theme.Err.Render("! not responding")
	}
	return theme.Dim.Render("○ stopped")
}

// agentLabel is "name · model · mode", then the extra arguments, leaving
// out what is unset.
func agentLabel(a spec.Agent) string {
	parts := []string{cardAgent(a)}
	if a.Mode != "" {
		parts = append(parts, a.Mode)
	}
	if len(a.Args) > 0 {
		parts = append(parts, strings.Join(a.Args, " "))
	}
	return strings.Join(parts, " · ")
}

// cardAgent is the agent as a card names it: "Claude · sonnet".
func cardAgent(a spec.Agent) string {
	name := a.Name
	if name == "" {
		name = a.Provider
	}
	if a.Model != "" {
		return name + " · " + a.Model
	}
	return name
}

// cutLeft shortens s to w cells from the left, starting with "…" when
// something was cut: the end of a path is the part that tells it apart.
func cutLeft(s string, w int) string {
	sw := ansi.StringWidth(s)
	switch {
	case sw <= w:
		return s
	case w <= 1:
		return cut("…", w)
	}
	return "…" + ansi.TruncateLeft(s, sw-(w-1), "")
}

// centered puts s in the middle of w cells.
func centered(s string, w int) string {
	return strings.Repeat(" ", max((w-ansi.StringWidth(s))/2, 0)) + s
}

// countsForms are a card's run counts, "working 2 · review 3 · done 5"
// with zeros dimmed, from the longest form to the shortest. The runs that
// wait for input and the failed ones are flagged apart (cardBadges).
func countsForms(c counts) []string {
	type part struct {
		long, short string
		n           int
		style       lipgloss.Style
	}
	parts := []part{
		{"working", "work", c.working, theme.Bold},
		{"review", "review", c.review, theme.Bold},
		{"done", "done", c.done, theme.OK},
	}
	render := func(short bool, sep string) string {
		out := make([]string, len(parts))
		for i, p := range parts {
			label := p.long
			if short {
				label = p.short
			}
			if p.n == 0 {
				out[i] = theme.Dim.Render(fmt.Sprintf("%s %d", label, p.n))
			} else {
				out[i] = label + " " + p.style.Render(fmt.Sprint(p.n))
			}
		}
		return strings.Join(out, sep)
	}
	dot := theme.Dim.Render(" · ")
	return []string{render(false, dot), render(false, "  "), render(true, dot), render(true, "  "), render(true, " ")}
}

// countsLine is the longest form of the run counts that fits w.
func countsLine(c counts, w int) string {
	forms := countsForms(c)
	for _, f := range forms {
		if ansi.StringWidth(f) <= w {
			return f
		}
	}
	return cut(forms[len(forms)-1], w)
}

// strategyLabel says in words how finished work is published.
func strategyLabel(s spec.Strategy) string {
	label := map[string]string{
		"branch":          "push the branch",
		"github-pr":       "GitHub pull request",
		"github-issue-pr": "GitHub issue and pull request",
		"gitlab-mr":       "GitLab merge request",
	}[s.Kind]
	if label == "" {
		label = s.Kind
	}
	if opts := s.Options(); len(opts) > 0 {
		label += " (" + strings.Join(opts, ", ") + ")"
	}
	return label
}

// pipelineByName finds a pipeline among the listed ones.
func pipelineByName(list []spec.Pipeline, name string) (spec.Pipeline, bool) {
	for _, p := range list {
		if p.Name == name {
			return p, true
		}
	}
	return spec.Pipeline{}, false
}

// copyPipeline saves a pipeline under a new name, as `ymcp pipeline NAME
// copy NEW` does.
func copyPipeline(from, to string) error {
	if err := spec.CheckName(to); err != nil {
		return err
	}
	p, err := spec.Load(from)
	if err != nil {
		return err
	}
	if _, err := spec.Load(to); err == nil {
		return fmt.Errorf("pipeline %q already exists", to)
	} else if !errors.Is(err, spec.ErrNotFound) {
		return err
	}
	p.Name = to
	return spec.Save(p)
}

// renamePipeline gives a pipeline a new name under the CLI's rules: its
// runner must be stopped and it must have no active runs, since runs are
// filed under the name.
func renamePipeline(ctx context.Context, st *store.Store, from, to string) error {
	if err := spec.CheckName(to); err != nil {
		return err
	}
	if _, _, beat, ok, err := st.Runner(ctx, from); err != nil {
		return err
	} else if ok && time.Since(beat) <= time.Minute {
		return fmt.Errorf("pipeline %s is running; stop it before renaming", from)
	}
	active, err := st.Runs(ctx, from, true)
	if err != nil {
		return err
	}
	if len(active) > 0 {
		return fmt.Errorf("pipeline %s has %d active run(s), which stay filed under its name; finish or cancel them before renaming", from, len(active))
	}
	return spec.Rename(from, to)
}
