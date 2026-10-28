package views

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/keys"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// Board columns.
const (
	colBacklog = iota
	colWorking
	colInput
	colReview
	colDone
	columns
)

var columnNames = [columns]string{"BACKLOG", "WORKING", "NEEDS INPUT", "REVIEW", "DONE"}

const (
	minColumn = 14 // narrowest column before columns start scrolling
	columnGap = 2
	// With room for every column this wide, the board gets a margin and
	// wider gaps.
	airyColumn  = 20
	airyGap     = 3
	doneShown   = 30 // finished runs the done column keeps
	minBacklog  = 30 * time.Second
	backlogWait = 20 * time.Second // timeout of one backlog search
)

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Board is a pipeline's runs in columns: the backlog of its source, the
// runs being worked on, waiting for input, in review and finished.
type Board struct {
	base
	name     string
	home     string
	loaded   bool
	loading  bool
	p        *spec.Pipeline
	profiles []config.Config
	runner   runnerInfo
	runs     []store.Run
	info     map[int64]runInfo
	frameN   int

	backlog        []ytwatch.Issue
	backlogErr     error
	backlogKnown   bool
	backlogLoading bool
	backlogAt      time.Time
	searches       int            // backlog searches started
	gone           map[string]int // issues that left the backlog, by the last search started before

	col int
	sel [columns]int
}

type boardLoaded struct {
	to       int64
	p        *spec.Pipeline
	profiles []config.Config
	runner   runnerInfo
	runs     []store.Run
	info     map[int64]runInfo
	err      error
}

type backlogLoaded struct {
	to     int64
	n      int // which search it answers
	issues []ytwatch.Issue
	err    error
}

// NewBoard makes the board of a pipeline.
func NewBoard(d *Deps, pipeline string) *Board {
	home, _ := os.UserHomeDir()
	return &Board{base: newBase(d), name: pipeline, home: home, info: map[int64]runInfo{}, col: colWorking}
}

// Pipeline is the name of the pipeline on the board.
func (m *Board) Pipeline() string { return m.name }

// Init loads the board.
func (m *Board) Init() tea.Cmd { return m.reload() }

func (m *Board) reload() tea.Cmd {
	if m.loading {
		return nil
	}
	m.loading = true
	prev := maps.Clone(m.info)
	return func() tea.Msg { return m.load(prev) }
}

func (m *Board) load(info map[int64]runInfo) tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	msg := boardLoaded{to: m.id, info: info}
	list, err := m.d.Pipelines()
	if p, ok := pipelineByName(list, m.name); ok {
		msg.p = &p
	} else if err == nil {
		err = fmt.Errorf("pipeline %s: %w", m.name, spec.ErrNotFound)
	}
	msg.err = err
	msg.profiles, _ = m.d.Profiles()
	if msg.runner, err = m.d.runner(ctx, m.name); err != nil {
		msg.err = err
		return msg
	}
	if msg.runs, err = m.d.Store.Runs(ctx, m.name, false); err != nil {
		msg.err = err
		return msg
	}
	// Only the latest finished runs are shown; older ones are not read.
	var finished []store.Run
	for _, r := range msg.runs {
		if r.Status.Final() {
			finished = append(finished, r)
		}
	}
	sortFinished(finished)
	shown := map[int64]bool{}
	for i, r := range finished {
		if i < doneShown {
			shown[r.ID] = true
		}
	}
	for _, r := range msg.runs {
		in := info[r.ID]
		if r.Status.Final() && (!shown[r.ID] || in.final) {
			continue // finished and already read, or not shown
		}
		if _, err := m.d.follow(ctx, r, &in); err != nil {
			msg.err = err
			return msg
		}
		info[r.ID] = in
	}
	return msg
}

// sortFinished puts the most recently finished runs first.
func sortFinished(runs []store.Run) {
	at := func(r store.Run) time.Time {
		if r.FinishedAt != nil {
			return *r.FinishedAt
		}
		return r.UpdatedAt
	}
	slices.SortStableFunc(runs, func(a, b store.Run) int { return at(b).Compare(at(a)) })
}

// backlogDue reports whether the source should be searched again.
func (m *Board) backlogDue(now time.Time) bool {
	if m.d.Backlog == nil || m.p == nil || m.backlogLoading || strings.TrimSpace(m.p.Source.Query) == "" {
		return false
	}
	every := max(m.p.Source.PollInterval.Duration, minBacklog)
	return m.backlogAt.IsZero() || now.Sub(m.backlogAt) >= every
}

func (m *Board) searchBacklog() tea.Cmd {
	m.backlogLoading = true
	m.backlogAt = m.d.Now()
	m.searches++
	profile, query, n := m.p.Profile, m.p.Source.Query, m.searches
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), backlogWait)
		defer cancel()
		issues, err := m.d.Backlog.Search(ctx, profile, query)
		return backlogLoaded{to: m.id, n: n, issues: issues, err: err}
	}
}

// dropGone leaves out of a search's issues those that left the backlog
// after the search started; a later search tells for itself.
func (m *Board) dropGone(n int, issues []ytwatch.Issue) []ytwatch.Issue {
	for key, before := range m.gone {
		if n > before {
			delete(m.gone, key)
		}
	}
	if len(m.gone) == 0 {
		return issues
	}
	return slices.DeleteFunc(slices.Clone(issues), func(is ytwatch.Issue) bool {
		_, gone := m.gone[is.Key]
		return gone
	})
}

// Update handles keys, ticks and reloads.
func (m *Board) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if done, cmd := m.common(msg); done {
		return m, cmd
	}
	switch msg := msg.(type) {
	case boardLoaded:
		if msg.to != m.id {
			return m, nil
		}
		m.loading, m.loaded = false, true
		if msg.p != nil {
			m.p = msg.p
		}
		m.profiles, m.runner, m.runs, m.info = msg.profiles, msg.runner, msg.runs, msg.info
		if msg.err != nil && (m.flash == "" || m.flashErr) {
			m.fail(msg.err)
		}
		m.clampSel()
		if m.backlogDue(m.d.Now()) {
			return m, m.searchBacklog()
		}
	case backlogLoaded:
		if msg.to != m.id {
			return m, nil
		}
		m.backlogLoading, m.backlogKnown = false, true
		m.backlogErr = msg.err
		if msg.err == nil {
			m.backlog = m.dropGone(msg.n, msg.issues)
		}
		m.clampSel()
	case BacklogLeft:
		if msg.Pipeline != m.name {
			return m, nil
		}
		// The card changed the issue: it leaves the backlog now, and a
		// search already on its way does not bring it back.
		if m.gone == nil {
			m.gone = map[string]int{}
		}
		m.gone[msg.Issue] = m.searches
		m.backlog = m.dropGone(m.searches, m.backlog)
		m.clampSel()
		m.say(fmt.Sprintf("%s left the backlog: the source query no longer finds it.", msg.Issue))
		m.backlogAt = time.Time{} // search again at the next tick
	case Tick:
		m.frameN++
		cmds := []tea.Cmd{m.reload()}
		if m.backlogDue(msg.Now) {
			cmds = append(cmds, m.searchBacklog())
		}
		return m, tea.Batch(cmds...)
	case tea.KeyMsg:
		return m, m.key(keys.Of(msg))
	}
	return m, nil
}

// card is one entry of a column: a run, or a backlog issue.
type card struct {
	run   *store.Run
	issue *ytwatch.Issue
}

// cards sorts the runs and the backlog into the columns.
func (m *Board) cards() [columns][]card {
	var out [columns][]card
	active := map[string]bool{}
	var finished []store.Run
	for i := range m.runs {
		r := &m.runs[i]
		switch r.Status {
		case store.StatusQueued, store.StatusWorking:
			out[colWorking] = append(out[colWorking], card{run: r})
		case store.StatusNeedsInput:
			out[colInput] = append(out[colInput], card{run: r})
		case store.StatusReview:
			out[colReview] = append(out[colReview], card{run: r})
		default:
			finished = append(finished, *r)
			continue
		}
		active[r.IssueKey], active[r.IssueID] = true, true
	}
	sortFinished(finished)
	for i := range finished[:min(len(finished), doneShown)] {
		out[colDone] = append(out[colDone], card{run: &finished[i]})
	}
	for i := range m.backlog {
		is := &m.backlog[i]
		if !active[is.Key] && !active[is.ID] {
			out[colBacklog] = append(out[colBacklog], card{issue: is})
		}
	}
	return out
}

func (m *Board) clampSel() {
	cs := m.cards()
	for c := range m.sel {
		m.sel[c] = clamp(m.sel[c], len(cs[c]))
	}
}

func (m *Board) current() (card, bool) {
	cs := m.cards()[m.col]
	if len(cs) == 0 {
		return card{}, false
	}
	return cs[clamp(m.sel[m.col], len(cs))], true
}

func (m *Board) key(k string) tea.Cmd {
	switch k {
	case "q":
		return tea.Quit
	case "esc", "backspace":
		return navigate(Back{})
	case "?":
		m.fullHelp = !m.fullHelp
		return nil
	case "left", "h", "shift+tab":
		m.col = (m.col + columns - 1) % columns
		return nil
	case "right", "l", "tab":
		m.col = (m.col + 1) % columns
		return nil
	case "up", "k":
		m.sel[m.col] = clamp(m.sel[m.col]-1, len(m.cards()[m.col]))
		return nil
	case "down", "j":
		m.sel[m.col] = clamp(m.sel[m.col]+1, len(m.cards()[m.col]))
		return nil
	case "i":
		return navigate(OpenInbox{})
	case "r":
		m.say("Reloading…")
		cmds := []tea.Cmd{m.reload()}
		if m.d.Backlog != nil && m.p != nil && !m.backlogLoading && strings.TrimSpace(m.p.Source.Query) != "" {
			cmds = append(cmds, m.searchBacklog())
		}
		return tea.Batch(cmds...)
	case "s":
		if m.runner.state == runnerRunning {
			return stopRunner(&m.base, m.name, m.runner)
		}
		return startRunner(&m.base, m.name, m.runner)
	case "e":
		if m.p == nil {
			return nil
		}
		p := *m.p
		return navigate(OpenWizard{Profile: p.Profile, Existing: &p})
	}
	c, ok := m.current()
	if !ok {
		return nil
	}
	switch k {
	case "enter":
		if c.run != nil {
			return navigate(OpenRun{Pipeline: m.name, Number: c.run.Number})
		}
		profile := ""
		if m.p != nil {
			profile = m.p.Profile
		}
		return navigate(OpenIssue{Pipeline: m.name, Profile: profile, Issue: *c.issue})
	case "a":
		if c.run == nil || c.run.Status.Final() {
			m.fail(fmt.Errorf("only an active run can be answered"))
			return nil
		}
		return navigate(OpenRun{Pipeline: m.name, Number: c.run.Number, Answer: true})
	case "c":
		if c.run == nil || c.run.Status.Final() {
			m.fail(fmt.Errorf("only an active run can be canceled"))
			return nil
		}
		return cancelRun(&m.base, m.name, *c.run, m.reload)
	case "d":
		if c.run == nil || !c.run.Status.Final() {
			m.fail(fmt.Errorf("only a run that has ended can be deleted"))
			return nil
		}
		return deleteRun(&m.base, m.name, *c.run, m.reload)
	case "o":
		return m.openIssue(c)
	case "p":
		if c.run == nil || c.run.PRURL == "" {
			m.fail(fmt.Errorf("this card has no pull request"))
			return nil
		}
		return m.open(c.run.PRURL, "the pull request")
	}
	return nil
}

func (m *Board) open(url, what string) tea.Cmd { return openURL(&m.base, url, what) }

// openIssue opens the YouTrack issue of a card.
func (m *Board) openIssue(c card) tea.Cmd {
	if c.issue != nil {
		url := c.issue.URL
		if url == "" && m.p != nil {
			url = issueURL(m.profiles, m.p.Profile, c.issue.Key)
		}
		return m.open(url, c.issue.Key)
	}
	return m.open(issueURL(m.profiles, c.run.Profile, c.run.IssueKey), c.run.IssueKey)
}

// openURL opens a link in the browser.
func openURL(b *base, url, what string) tea.Cmd {
	if url == "" {
		b.fail(fmt.Errorf("no link for %s", what))
		return nil
	}
	if b.d.OpenURL == nil {
		b.say(url)
		return nil
	}
	return func() tea.Msg {
		return b.report("Opened "+what+" in the browser.", b.d.OpenURL(url))
	}
}

// cancelRun asks, then cancels a run; after runs once it is canceled.
func cancelRun(b *base, pipeline string, r store.Run, after func() tea.Cmd) tea.Cmd {
	b.confirm(fmt.Sprintf("Cancel run #%d (%s)? The agent stops, the worktree stays.", r.Number, r.IssueKey), func() tea.Cmd {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		if err := b.d.Cancel(ctx, b.d.Store, pipeline, r.Number); err != nil {
			b.fail(err)
			return nil
		}
		b.say(fmt.Sprintf("Canceled run #%d.", r.Number))
		return after()
	})
	return nil
}

// deleteRun asks, then deletes a run that has ended from the local store;
// after runs once it is gone.
func deleteRun(b *base, pipeline string, r store.Run, after func() tea.Cmd) tea.Cmd {
	b.confirm(fmt.Sprintf("Delete run #%d (%s) from the local store? Its log goes; the issue, branch and PR stay.", r.Number, r.IssueKey), func() tea.Cmd {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		if err := b.d.Delete(ctx, b.d.Store, pipeline, r.Number); err != nil {
			b.fail(err)
			return nil
		}
		b.say(fmt.Sprintf("Deleted run #%d.", r.Number))
		return after()
	})
	return nil
}

// View renders the board.
func (m *Board) View() string {
	w := m.width
	header := []string{m.headerLine(w), ""}
	help := []binding{
		{"←→↑↓", "move"}, {"enter", "open"}, {"a", "answer"}, {"c", "cancel"}, {"d", "delete"}, {"o", "issue"}, {"p", "PR"},
		{"r", "reload"}, {"s", m.runnerAction()}, {"e", "edit"}, {"i", "inbox"}, {"esc", "back"}, {"q", "quit"},
	}
	if !m.loaded {
		return m.frame(header, []string{theme.Dim.Render("  Loading…")}, help)
	}
	return m.frame(header, m.columnsView(w, m.bodyRows(len(header), help)), help)
}

func (m *Board) bodyRows(headerLines int, help []binding) int {
	return m.height - headerLines - 1 - len(m.helpLines(help, max(m.width, 1)))
}

func (m *Board) headerLine(w int) string {
	left := title(m.name)
	if m.p != nil {
		where := homeShort(m.p.Workspace.Dir, m.home)
		if m.p.Workspace.Host != "" {
			where = m.p.Workspace.Host + ":" + where
		}
		left += " " + theme.Dim.Render(strings.Join([]string{m.p.Profile, m.p.Agent.Name, where}, " · "))
	}
	right := m.runner.label()
	if m.p != nil && m.runner.state == runnerRunning {
		busy := 0
		for _, in := range m.info {
			if in.inTurn {
				busy++
			}
		}
		right += theme.Dim.Render(fmt.Sprintf("  agents %d/%d", busy, m.p.Workspace.Parallel))
	}
	return spread(left, right, w)
}

// columnsView lays the columns side by side. When the terminal is too
// narrow for all five, it shows the ones around the selected column.
func (m *Board) columnsView(w, rows int) []string {
	if rows <= 0 {
		return nil
	}
	cs := m.cards()
	full, mx, gap := w, 0, columnGap
	if inner := w - 2*gridMargin; (inner-airyGap*(columns-1))/columns >= airyColumn {
		mx, gap, w = gridMargin, airyGap, inner
	}
	visible := max(min((w+gap)/(minColumn+gap), columns), 1)
	first := min(max(m.col-visible/2, 0), columns-visible)
	cw := (w - gap*(visible-1)) / visible
	var cols [][]string
	for c := first; c < first+visible; c++ {
		cols = append(cols, m.column(c, cs[c], cw, rows))
	}
	lines := make([]string, rows)
	for i := range lines {
		parts := make([]string, len(cols))
		for j, col := range cols {
			cell := ""
			if i < len(col) {
				cell = col[i]
			}
			parts[j] = pad(cell, cw)
		}
		lines[i] = strings.Repeat(" ", mx) + strings.Join(parts, strings.Repeat(" ", gap))
	}
	if visible < columns && rows > 0 {
		hint := ""
		if first > 0 {
			hint += "‹ "
		}
		hint += fmt.Sprintf("column %d of %d", m.col+1, columns)
		if first+visible < columns {
			hint += " ›"
		}
		lines[rows-1] = spread("", theme.Dim.Render(hint)+strings.Repeat(" ", mx), full)
	}
	return lines
}

// column renders a column header, its rule and the cards that fit,
// scrolled to the selected one.
func (m *Board) column(c int, cards []card, cw, rows int) []string {
	count := fmt.Sprint(len(cards))
	switch {
	case c == colBacklog && m.backlogErr != nil:
		count = "!"
	case c == colBacklog && !m.backlogKnown:
		count = "…"
		if m.d.Backlog == nil || m.p == nil {
			count = "–"
		}
	case c == colDone && len(cards) == doneShown:
		count += "+"
	}
	head := columnNames[c] + " " + count
	rule := strings.Repeat("─", cw)
	if c == m.col {
		head, rule = theme.Accent.Render(head), theme.Accent.Render(rule)
	} else {
		head, rule = theme.Bold.Render(head), theme.Dim.Render(rule)
	}
	out := []string{head, rule}
	if rows >= 12 {
		out = append(out, "")
	}
	room := rows - len(out)
	if c == colBacklog && m.backlogErr != nil {
		for _, l := range wrap("Search failed: "+oneLine(m.backlogErr.Error()), cw) {
			out = append(out, theme.Err.Render(l))
		}
		return out
	}
	if len(cards) == 0 {
		if c == colBacklog && !m.backlogKnown && m.backlogLoading {
			return append(out, theme.Dim.Render("searching…"))
		}
		return append(out, theme.Dim.Render(emptyColumn(c)))
	}
	// Three lines and a gap (two for the backlog); a summary gets a
	// second line when every card still fits.
	height, summaryLines := 4, 1
	if c == colBacklog {
		height = 3
	}
	if len(cards)*(height+1) <= room {
		height, summaryLines = height+1, 2
	}
	sel := clamp(m.sel[c], len(cards))
	fit := max(room/height, 1)
	top := 0
	if len(cards) > fit { // leave a line each for "↑ n more" and "↓ n more"
		fit = max((room-2)/height, 1)
		if sel >= fit {
			top = sel - fit + 1
		}
	}
	if top > 0 {
		out = append(out, theme.Dim.Render(fmt.Sprintf("↑ %d more", top)))
	}
	end := min(top+fit, len(cards))
	for i := top; i < end; i++ {
		lines := m.cardLines(cards[i], cw-2, summaryLines)
		bar := "  "
		if c == m.col && i == sel {
			bar = theme.Accent.Render("▌") + " "
		}
		for _, l := range lines {
			out = append(out, bar+l)
		}
		out = append(out, "")
	}
	if end < len(cards) {
		out = append(out, theme.Dim.Render(fmt.Sprintf("↓ %d more", len(cards)-end)))
	}
	return out
}

func emptyColumn(c int) string {
	switch c {
	case colBacklog:
		return "nothing waiting"
	case colInput:
		return "no questions"
	}
	return "–"
}

// cardLines are the lines of one card: the issue and run number, the
// summary and what is happening.
func (m *Board) cardLines(c card, w, summaryLines int) []string {
	if c.issue != nil {
		return append([]string{theme.Bold.Render(c.issue.Key)}, summary(c.issue.Summary, w, summaryLines)...)
	}
	r := c.run
	in := m.info[r.ID]
	top := spread(theme.Bold.Render(r.IssueKey), theme.Dim.Render(fmt.Sprintf("#%d", r.Number)), w)
	text := in.summary
	if text == "" {
		text = theme.Dim.Render(statusLabel(r.Status))
	}
	if r.Status == store.StatusNeedsInput && summaryLines > 1 && r.NeedsInputReason != "" {
		// Room for the question on a line of its own.
		return []string{top, cut(oneLine(text), w), cut(theme.Dim.Render("“"+oneLine(r.NeedsInputReason)+"”"), w),
			theme.Warn.Render("⚑ "+short(m.d.Now().Sub(waitingSince(*r)))) + theme.Dim.Render(" waiting")}
	}
	lines := append([]string{top}, summary(text, w, summaryLines)...)
	return append(lines, cut(m.state(*r, in), w))
}

// summary is an issue summary in exactly n lines of width w.
func summary(s string, w, n int) []string {
	lines := wrap(oneLine(s), max(w, 1))
	if len(lines) > n {
		rest := strings.Join(lines[n-1:], " ")
		lines = append(lines[:n-1], cut(rest, w))
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines
}

// state is the third line of a run card.
func (m *Board) state(r store.Run, in runInfo) string {
	now := m.d.Now()
	switch r.Status {
	case store.StatusQueued:
		return theme.Dim.Render("◌ queued")
	case store.StatusNeedsInput:
		reason := oneLine(r.NeedsInputReason)
		line := theme.Warn.Render("⚑ " + short(now.Sub(waitingSince(r))))
		if reason != "" {
			line += " " + theme.Dim.Render("“"+reason+"”")
		}
		return line
	case store.StatusDone, store.StatusClosed:
		return theme.OK.Render("✓ "+statusLabel(r.Status)) + theme.Dim.Render(" "+finishedSince(r, now))
	case store.StatusFailed:
		return theme.Err.Render("✗ failed") + theme.Dim.Render(" "+finishedSince(r, now))
	case store.StatusCanceled:
		return theme.Dim.Render("⊘ canceled " + finishedSince(r, now))
	}
	if in.inTurn {
		what := in.activity
		if what == "" {
			what = "agent"
		}
		return theme.Accent.Render(spinner[m.frameN%len(spinner)]) + " " + elapsed(now.Sub(in.turnStart)) + " " + theme.Dim.Render(what)
	}
	if in.pending > 0 {
		return theme.Warn.Render(fmt.Sprintf("⟳ %d feedback", in.pending))
	}
	if r.Status == store.StatusReview {
		if r.PRNumber > 0 {
			return fmt.Sprintf("PR #%d", r.PRNumber)
		}
		return theme.Dim.Render("branch pushed")
	}
	return theme.Dim.Render("◌ waiting for a slot")
}

// finishedSince is how long ago a run ended, e.g. "26h".
func finishedSince(r store.Run, now time.Time) string {
	at := r.UpdatedAt
	if r.FinishedAt != nil {
		at = *r.FinishedAt
	}
	return short(now.Sub(at))
}

// runnerAction is what s does to the pipeline's runner now.
func (m *Board) runnerAction() string {
	if m.runner.state == runnerRunning {
		return "stop"
	}
	return "start"
}
