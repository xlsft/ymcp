package views

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/keys"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// feedKeep is how many of a run's latest events the card keeps.
const feedKeep = 2000

// How often the card rereads the issue and the pull request, and how long
// one request may take.
const (
	issueEvery  = 5 * time.Second
	changeEvery = 15 * time.Second
	ytTimeout   = 20 * time.Second
)

// The panes of the card that take the arrow keys.
const (
	paneThread = iota
	paneLog
)

// The tabs of the thread.
const (
	tabYouTrack = iota
	tabChange
)

// Run is the card of one run, drawn as a modal over the screen it was
// opened from, laid out like a YouTrack issue: the issue with its fields,
// description and comment thread on the left with a reply box that posts
// real comments, the run's event log on the right.
type Run struct {
	base
	pipeline string
	number   int
	home     string

	loaded   bool
	loading  bool
	run      store.Run
	info     runInfo
	runner   runnerInfo
	profiles []config.Config
	events   []store.Event

	// The issue and its comments, from Deps.Issues.
	ytLoaded  bool
	ytLoading bool
	ytAgain   bool // reload once the running load ends
	ytAt      time.Time
	issue     ytwatch.Issue
	issueErr  error
	comments  []IssueComment
	threadErr error
	pending   []pendingComment
	seq       int

	// The pull request conversation, from Deps.Changes.
	tab        int
	prLoaded   bool
	prLoading  bool
	prAt       time.Time
	prComments []vcs.Comment
	prErr      error

	focus     int // paneThread or paneLog
	docScroll int // lines up from the bottom; 0 follows the thread
	docLen    int
	docRows   int
	logScroll int
	logRows   int
	logLines  []string
	logWidth  int
	logDirty  bool
	frameN    int

	reply     replyBox
	wantReply bool
	acSel     int    // the suggestion under the cursor
	acOff     string // the text the suggestions were closed for
}

// pendingComment is a comment posted from the card that YouTrack has not
// confirmed yet.
type pendingComment struct {
	seq  int
	text string
	at   time.Time
}

type runLoaded struct {
	to       int64
	run      store.Run
	info     runInfo
	runner   runnerInfo
	profiles []config.Config
	events   []store.Event
	err      error
}

type issueLoaded struct {
	to        int64
	issue     ytwatch.Issue
	issueErr  error
	comments  []IssueComment
	threadErr error
}

type changeLoaded struct {
	to       int64
	comments []vcs.Comment
	err      error
}

type commentSent struct {
	to      int64
	seq     int
	text    string
	comment IssueComment
	err     error
}

// NewRun makes the card of a pipeline's run; reply focuses the reply box
// at once.
func NewRun(d *Deps, pipeline string, number int, reply bool) *Run {
	home, _ := os.UserHomeDir()
	return &Run{base: newBase(d), pipeline: pipeline, number: number, home: home, reply: newReplyBox(), wantReply: reply}
}

// Init loads the run.
func (m *Run) Init() tea.Cmd { return m.reload() }

// Answering reports whether the reply box has the focus, so the root app
// keeps its own keys out of it.
func (m *Run) Answering() bool { return m.reply.focused }

func (m *Run) reload() tea.Cmd {
	if m.loading {
		return nil
	}
	m.loading = true
	info := m.info
	return func() tea.Msg { return m.load(info) }
}

func (m *Run) load(info runInfo) tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	msg := runLoaded{to: m.id}
	r, err := m.d.Store.RunByNumber(ctx, m.pipeline, m.number)
	if errors.Is(err, store.ErrNotFound) {
		err = fmt.Errorf("pipeline %s has no run #%d", m.pipeline, m.number)
	}
	if err != nil {
		msg.err = err
		return msg
	}
	msg.run = r
	if msg.events, err = m.d.follow(ctx, r, &info); err != nil {
		msg.err = err
		return msg
	}
	msg.info = info
	msg.runner, msg.err = m.d.runner(ctx, m.pipeline)
	msg.profiles, _ = m.d.Profiles()
	return msg
}

// issueRef is how the card asks YouTrack for the run's issue: by database
// id, which survives a move to another project.
func issueRef(r store.Run) string {
	if r.IssueID != "" {
		return r.IssueID
	}
	return r.IssueKey
}

// loadIssue rereads the issue and its comments; force queues a reload when
// one is running already.
func (m *Run) loadIssue(force bool) tea.Cmd {
	if m.d.Issues == nil || !m.loaded {
		return nil
	}
	if m.ytLoading {
		m.ytAgain = m.ytAgain || force
		return nil
	}
	m.ytLoading, m.ytAt = true, m.d.Now()
	iss, profile, ref, to := m.d.Issues, m.run.Profile, issueRef(m.run), m.id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), ytTimeout)
		defer cancel()
		msg := issueLoaded{to: to}
		msg.issue, msg.issueErr = iss.Issue(ctx, profile, ref)
		msg.comments, msg.threadErr = iss.Comments(ctx, profile, ref)
		return msg
	}
}

// hasChange reports whether the card has a pull request tab.
func (m *Run) hasChange() bool { return m.d.Changes != nil && m.run.PRNumber > 0 }

func (m *Run) loadChange() tea.Cmd {
	if !m.hasChange() || m.prLoading {
		return nil
	}
	m.prLoading, m.prAt = true, m.d.Now()
	ch, run, to := m.d.Changes, m.run, m.id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), ytTimeout)
		defer cancel()
		cs, err := ch.Comments(ctx, run)
		return changeLoaded{to: to, comments: cs, err: err}
	}
}

// Update handles keys, ticks and reloads.
func (m *Run) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if done, cmd := m.common(msg); done {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.reply.resize(m.leftWidth())
		m.logDirty = true
	case runLoaded:
		if msg.to != m.id {
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.fail(msg.err)
			if msg.run.ID == 0 {
				return m, nil
			}
		}
		first := !m.loaded
		m.loaded = true
		m.run, m.info, m.runner, m.profiles = msg.run, msg.info, msg.runner, msg.profiles
		if len(msg.events) > 0 {
			m.events = append(m.events, msg.events...)
			if len(m.events) > feedKeep {
				m.events = m.events[len(m.events)-feedKeep:]
			}
			m.logDirty = true
		}
		if first {
			cmds := []tea.Cmd{m.loadIssue(false)}
			if m.wantReply {
				m.wantReply = false
				cmds = append(cmds, m.focusReply())
			}
			return m, tea.Batch(cmds...)
		}
	case issueLoaded:
		if msg.to != m.id {
			return m, nil
		}
		m.ytLoading, m.ytLoaded = false, true
		m.issueErr, m.threadErr = msg.issueErr, msg.threadErr
		if msg.issueErr == nil {
			m.issue = msg.issue
		}
		if msg.threadErr == nil {
			m.comments = msg.comments
		}
		if m.ytAgain {
			m.ytAgain = false
			return m, m.loadIssue(false)
		}
	case changeLoaded:
		if msg.to != m.id {
			return m, nil
		}
		m.prLoading, m.prLoaded, m.prErr = false, true, msg.err
		if msg.err == nil {
			m.prComments = msg.comments
		}
	case commentSent:
		if msg.to != m.id {
			return m, nil
		}
		for i, p := range m.pending {
			if p.seq == msg.seq {
				m.pending = append(m.pending[:i:i], m.pending[i+1:]...)
				break
			}
		}
		if msg.err != nil {
			m.fail(fmt.Errorf("comment not posted: %w", msg.err))
			if m.reply.draft() == "" {
				m.reply.set(msg.text)
			}
			return m, nil
		}
		if !m.hasComment(msg.comment.ID) {
			m.comments = append(m.comments, msg.comment)
		}
		m.say(m.posted())
		return m, m.loadIssue(true)
	case Tick:
		m.frameN++
		cmds := []tea.Cmd{m.reload()}
		now := m.d.Now()
		if m.loaded && now.Sub(m.ytAt) >= issueEvery {
			cmds = append(cmds, m.loadIssue(false))
		}
		if m.tab == tabChange && now.Sub(m.prAt) >= changeEvery {
			cmds = append(cmds, m.loadChange())
		}
		return m, tea.Batch(cmds...)
	case tea.KeyMsg:
		if m.reply.focused {
			if m.suggestKey(msg.String()) {
				return m, nil
			}
			send, text, cmd := m.reply.key(msg)
			if send {
				m.reply.clear()
				m.reply.blur()
				return m, m.post(text)
			}
			return m, cmd
		}
		return m, m.key(keys.Of(msg))
	default:
		return m, m.reply.update(msg)
	}
	return m, nil
}

func (m *Run) hasComment(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range m.comments {
		if c.ID == id {
			return true
		}
	}
	return false
}

// posted says where a comment went.
func (m *Run) posted() string {
	key := m.run.IssueKey
	switch {
	case m.run.Status.Final():
		return fmt.Sprintf("Commented on %s. Run #%d has ended, so no agent reads it.", key, m.number)
	case m.runner.state != runnerRunning:
		return fmt.Sprintf("Commented on %s. The runner is stopped; it reads the comment when it starts.", key)
	}
	return fmt.Sprintf("Commented on %s. The runner passes it to the agent at its next poll.", key)
}

// answerSent tells where an answer given through the store went.
func answerSent(number int, r runnerInfo) string {
	if r.state == runnerRunning {
		return fmt.Sprintf("Answer sent to #%d; the runner passes it on at its next poll.", number)
	}
	return fmt.Sprintf("Answer saved for #%d; the runner is stopped and passes it on when it starts.", number)
}

// post comments on the issue as the profile's user, showing the comment at
// once.
func (m *Run) post(text string) tea.Cmd {
	if m.d.Issues == nil {
		m.fail(errors.New("YouTrack is not connected here; open the issue with o to comment"))
		m.reply.set(text)
		return nil
	}
	m.seq++
	m.pending = append(m.pending, pendingComment{seq: m.seq, text: text, at: m.d.Now()})
	m.docScroll, m.focus = 0, paneThread
	iss, profile, ref, to, seq := m.d.Issues, m.run.Profile, issueRef(m.run), m.id, m.seq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), ytTimeout)
		defer cancel()
		c, err := iss.AddComment(ctx, profile, ref, text)
		return commentSent{to: to, seq: seq, text: text, comment: c, err: err}
	}
}

func (m *Run) focusReply() tea.Cmd {
	if !m.loaded {
		return nil
	}
	if m.d.Issues == nil {
		m.fail(errors.New("YouTrack is not connected here; open the issue with o to comment"))
		return nil
	}
	m.focus = paneThread
	return m.reply.focus(m.leftWidth())
}

// waitsOnPermission reports whether the run waits for /approve or /reject.
func (m *Run) waitsOnPermission() bool {
	r := m.run
	if r.Status != store.StatusNeedsInput {
		return false
	}
	return strings.Contains(r.NeedsInputReason, "/approve") || strings.Contains(strings.ToLower(r.NeedsInputReason), "permission")
}

// quick posts /approve or /reject after a yes.
func (m *Run) quick(command string) tea.Cmd {
	m.confirm(fmt.Sprintf("Post %s on %s?", command, m.run.IssueKey), func() tea.Cmd { return m.post(command) })
	return nil
}

func (m *Run) key(k string) tea.Cmd {
	page := max(m.docRows-1, 1)
	scroll := &m.docScroll
	if m.focus == paneLog {
		page, scroll = max(m.logRows-1, 1), &m.logScroll
	}
	switch k {
	case "q":
		return tea.Quit
	case "esc", "backspace":
		return navigate(Back{})
	case "?":
		m.fullHelp = !m.fullHelp
	case "tab", "shift+tab":
		m.focus = 1 - m.focus
	case "up", "k":
		*scroll++
	case "down", "j":
		*scroll--
	case "pgup", "b":
		*scroll += page
	case "pgdown", " ", "f":
		*scroll -= page
	case "home", "g":
		*scroll = 1 << 30
	case "end", "G":
		*scroll = 0
	case "r", "enter":
		return m.focusReply()
	case "a":
		if m.waitsOnPermission() {
			return m.quick("/approve")
		}
		return m.focusReply()
	case "x":
		if m.waitsOnPermission() {
			return m.quick("/reject")
		}
		m.fail(errors.New("the run does not wait for a permission"))
	case "t", "[", "]":
		if !m.hasChange() {
			m.fail(fmt.Errorf("run #%d has no pull request to show", m.number))
			return nil
		}
		m.tab = 1 - m.tab
		m.docScroll = 0
		if m.tab == tabChange && (!m.prLoaded || m.d.Now().Sub(m.prAt) >= changeEvery) {
			return m.loadChange()
		}
	case "c":
		if !m.loaded {
			return nil
		}
		if m.run.Status.Final() {
			m.fail(fmt.Errorf("run #%d has already ended (%s)", m.number, statusLabel(m.run.Status)))
			return nil
		}
		return cancelRun(&m.base, m.pipeline, m.run, m.reload)
	case "o":
		url := m.issue.URL
		if url == "" {
			url = issueURL(m.profiles, m.run.Profile, m.run.IssueKey)
		}
		return openURL(&m.base, url, m.run.IssueKey)
	case "p":
		if m.run.PRURL == "" {
			m.fail(fmt.Errorf("run #%d has no pull request", m.number))
			return nil
		}
		return openURL(&m.base, m.run.PRURL, "the pull request")
	case "i":
		return navigate(OpenInbox{})
	}
	*scroll = max(*scroll, 0)
	return nil
}

// Layout.

// wideFrom is the inner width from which the log sits beside the issue.
const wideFrom = 70

// gutter is what separates the issue from the log, and its width.
const gutterWidth = 5

func gutter() string { return "  " + theme.Dim.Render("│") + "  " }

// modalPad is the padding inside the card's frame: two cells left and
// right and a row above and below, less on small screens.
func modalPad(bw, bh int) (px, py int) {
	switch {
	case bw >= 60:
		px = 2
	case bw >= 30:
		px = 1
	}
	if bh >= 22 {
		py = 1
	}
	return px, py
}

// inner is the content width and height of the box at the current size,
// and whether it has a frame.
func (m *Run) inner() (cw, ch int, border bool) {
	_, _, bw, bh := modalRect(max(m.width, 1), max(m.height, 1))
	if bw < 24 || bh < 6 {
		return bw, bh, false
	}
	px, py := modalPad(bw, bh)
	return max(bw-2-2*px, 1), max(bh-2-2*py, 1), true
}

// split is the width of the issue and the log side by side, or 0 for the
// log when they take turns.
func split(cw int) (lw, rw int) {
	if cw < wideFrom {
		return cw, 0
	}
	rw = min(max(cw*32/100, 24), 64)
	return cw - rw - gutterWidth, rw
}

func (m *Run) leftWidth() int {
	cw, _, _ := m.inner()
	lw, _ := split(cw)
	return lw
}

// View renders the card over a blank screen; the root app draws it over
// the screen it was opened from through Overlay.
func (m *Run) View() string { return m.Overlay("") }

// Overlay draws the card over under, which is dimmed.
func (m *Run) Overlay(under string) string {
	w, h := max(m.width, 1), max(m.height, 1)
	x, y, bw, bh := modalRect(w, h)
	return modalOver(under, m.box(bw, bh), x, y, w, h)
}

// box is the card: bw×bh with its frame. Inside, padded: the top bar
// (the issue, its fields, the agent, the tabs), the issue's thread with
// the comment box beside the run's log, and the status and help lines.
func (m *Run) box(bw, bh int) []string {
	cw, ih, border := m.inner()
	// The comment box shows its own keys while it has the focus. The
	// status line shows only when it says something; on tiny screens it
	// takes the help's place.
	var foot []string
	if !m.reply.focused {
		foot = m.helpLines(m.help(), cw)
		if ih < 8 {
			foot = foot[:1]
		}
	}
	if s := m.statusLine(); s != "" {
		if ih < 8 {
			foot = nil
		}
		foot = append([]string{s}, foot...)
	}
	gap := 0
	if len(foot) > 0 && ih >= 16 {
		gap = 1
	}
	rows := max(ih-len(foot)-gap, 0)
	var body []string
	ruleAt := -1
	if !border && rows > 0 {
		right := ""
		if m.loaded {
			right = statusStyle(m.run.Status)
		}
		body = append(body, spread(title(fmt.Sprintf("%s #%d", m.pipeline, m.number)), right, cw))
		rows--
	}
	switch {
	case rows <= 0:
	case !m.loaded:
		body = append(body, theme.Dim.Render("Loading…"))
	default:
		top := m.topBar(cw, border, rows)
		if len(top) > rows/2 { // small: the title line only
			top = top[:min(1, len(top))]
		}
		body = append(body, top...)
		if rows > len(top) {
			ruleAt = len(body) // panes start with the rule
		}
		body = append(body, m.panes(cw, rows-len(top))...)
	}
	inner := make([]string, 0, ih)
	inner = append(inner, body[:min(len(body), max(ih-len(foot)-gap, 0))]...)
	for len(inner) < ih-len(foot) {
		inner = append(inner, "")
	}
	inner = append(inner, foot...)
	if len(inner) > ih {
		ruleAt -= len(inner) - ih
		inner = inner[len(inner)-ih:]
	}
	if !border {
		for i, l := range inner {
			inner[i] = pad(l, bw)
		}
		return inner
	}
	px, py := modalPad(bw, bh)
	side := strings.Repeat(" ", px)
	padded := make([]string, 0, ih+2*py)
	for range py {
		padded = append(padded, "")
	}
	for i, l := range inner {
		if i == ruleAt { // the rule runs through the padding to the frame
			padded = append(padded, m.ruleLine(cw, px))
			continue
		}
		padded = append(padded, side+pad(l, cw)+side)
	}
	for range py {
		padded = append(padded, "")
	}
	return frameBox(padded, bw-2, "", "")
}

// topBar is the head of the card across its width: the pipeline and run,
// the issue key and summary on the left, the issue's fields and the run's
// state as chips on the right (on a second line when they need it), then
// the agent line and, with a pull request, the tabs of the thread.
func (m *Run) topBar(w int, border bool, rows int) []string {
	summary := m.issue.Summary
	if summary == "" {
		summary = m.info.summary
	}
	head := theme.Accent.Render(m.run.IssueKey) + " " + theme.Bold.Render(oneLine(summary))
	if border {
		head = title(fmt.Sprintf("%s #%d", m.pipeline, m.number)) + "  " + head
	}
	chips := m.chips()
	var out []string
	if w >= wideFrom {
		out = sideBySide(head, chips, w)
	} else {
		out = wrapLines(head, w, 2)
		rows, _ := chipRows(chips, w, 2)
		out = append(out, rows...)
	}
	out = append(out, cut(m.agentLine(), w))
	if m.hasChange() {
		if rows >= 20 {
			out = append(out, "")
		}
		out = append(out, m.tabsLine(w))
	}
	if rows >= 24 { // air above the rule
		out = append(out, "")
	}
	return out
}

// sideBySide lays a title out on the left and chips on the right of w
// cells: the chips take up to two right-aligned lines, the title its
// whole width when the chips still fit beside it, else it wraps into what
// they leave.
func sideBySide(head string, chips []string, w int) []string {
	headW := ansi.StringWidth(head)
	var rows []string
	for _, leftW := range []int{headW, max(w*2/5, 24), max(w/4, 20)} {
		var all bool
		rows, all = chipRows(chips, max(w-leftW-3, 1), 2)
		if all {
			break
		}
	}
	widest := 0
	for _, r := range rows {
		widest = max(widest, ansi.StringWidth(r))
	}
	left := wrapLines(head, max(w-widest-3, 1), 2)
	n := max(len(left), len(rows))
	out := make([]string, n)
	for i := range n {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(rows) {
			r = rows[i]
		}
		out[i] = spread(l, r, w)
	}
	return out
}

// wrapLines wraps s to w cells in at most n lines, the last ending with
// "…" when something was left out.
func wrapLines(s string, w, n int) []string {
	lines := wrap(s, max(w, 1))
	if len(lines) > n {
		lines = append(lines[:n-1], cut(lines[n-1]+" …", w))
	}
	return lines
}

// chipRows fills lines of w cells with chips two spaces apart, at most n
// lines; all reports whether every chip fit, else the last line ends with
// "…".
func chipRows(chips []string, w, n int) (lines []string, all bool) {
	const sep = "  "
	line := ""
	for _, c := range chips {
		next := c
		if line != "" {
			next = line + sep + c
		}
		if line != "" && ansi.StringWidth(next) > w {
			lines = append(lines, line)
			line = c
			continue
		}
		line = next
	}
	if line != "" {
		lines = append(lines, line)
	}
	all = len(lines) <= n
	if !all {
		lines = append(lines[:n-1], lines[n-1]+" …")
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > w {
			all = false
		}
		lines[i] = cut(l, w)
	}
	return lines, all
}

// panes lays out the issue and the log under a rule that marks the
// focused one, side by side when wide enough, else the focused one.
func (m *Run) panes(cw, rows int) []string {
	if rows <= 0 {
		return nil
	}
	lw, rw := split(cw)
	air := rows >= 14
	out := make([]string, 0, rows)
	if rw == 0 {
		out = append(out, m.ruleLine(cw, 0))
		if air {
			out = append(out, "")
		}
		left := rows - len(out)
		if m.focus == paneLog {
			return append(out, m.logPane(cw, left, false)...)
		}
		return append(out, m.issuePane(cw, left, false)...)
	}
	out = append(out, m.ruleLine(cw, 0))
	if air {
		out = append(out, pad("", lw)+gutter())
	}
	n := rows - len(out)
	left := m.issuePane(lw, n, true)
	right := m.logPane(rw, n, true)
	for i := range n {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		out = append(out, pad(l, lw)+gutter()+pad(r, rw))
	}
	return out
}

// ruleLine is the line between the top bar and the panes, over cw cells
// plus extra on each side: in the brand colour over the focused pane, with
// a junction where the gutter between the panes starts.
func (m *Run) ruleLine(cw, extra int) string {
	lw, rw := split(cw)
	if rw == 0 {
		return rule(cw+2*extra, false)
	}
	thread := m.focus == paneThread && !m.reply.focused
	log := m.focus == paneLog && !m.reply.focused
	return rule(lw+extra, thread) + theme.Dim.Render("──┬──") + rule(rw+extra, log)
}

// rule is a horizontal line, in the brand colour on the focused pane.
func rule(w int, focused bool) string {
	line := strings.Repeat("─", max(w, 1))
	if focused {
		return theme.Accent.Render(line)
	}
	return theme.Dim.Render(line)
}

// issuePane is the left side: the thread, then the comment box at the
// bottom like the input of a chat.
func (m *Run) issuePane(w, rows int, wide bool) []string {
	if rows <= 0 {
		return nil
	}
	var head []string
	if !wide {
		head = []string{cut(theme.Dim.Render("tab shows the log"), w), ""}
	}
	foot := m.replyArea(w, rows)
	doc := m.docLines(w)
	air := 1 // between the thread and the comment box
	docRows := rows - len(head) - len(foot) - air
	if docRows < 2 {
		head, air = nil, 0
		docRows = rows - len(foot)
	}
	if docRows < 0 {
		keep := max(rows, 0)
		foot = foot[max(len(foot)-keep, 0):]
		docRows = 0
	}
	if m.docScroll > 0 && m.docLen > 0 && len(doc) != m.docLen {
		m.docScroll += len(doc) - m.docLen // keep reading where the user was
	}
	m.docLen, m.docRows = len(doc), docRows
	out := append([]string(nil), head...)
	win := scrollWindow(doc, docRows, &m.docScroll)
	out = append(out, win...)
	for range docRows - len(win) {
		out = append(out, "")
	}
	for range air {
		out = append(out, "")
	}
	at := len(out) // the popup of suggestions ends above the chip and the box
	out = append(out, foot...)
	if len(out) > rows {
		out = out[:rows]
	}
	if items := m.suggestions(); len(items) > 0 && at > 0 {
		pop := m.suggestBox(items, w)
		out = popOver(out, pop, 0, max(at-len(pop), 0))
		if len(out) > rows {
			out = out[:rows]
		}
	}
	return out
}

// keyFields are the YouTrack fields shown as chips, in this order.
var keyFields = []string{"State", "Type", "Priority"}

// chips are the issue's key fields and the run's stage, status, branch
// and pull request, as chips.
func (m *Run) chips() []string {
	r := m.run
	var chips []string
	for _, f := range keyFields {
		if v := strings.TrimSpace(m.issue.Fields[f]); v != "" {
			chips = append(chips, chipStyle.Render(f+": "+v))
		}
	}
	if r.Stage != "" {
		chips = append(chips, chipStyle.Render("stage "+r.Stage))
	}
	chips = append(chips, statusChip(r.Status))
	if r.Branch != "" {
		chips = append(chips, chipStyle.Render("⎇ "+r.Branch))
	}
	if r.PRURL != "" {
		chips = append(chips, chipStyle.Render(fmt.Sprintf("PR #%d", r.PRNumber)))
	}
	return chips
}

// statusChip is the run status as a chip; needs input stands out.
func statusChip(s store.RunStatus) string {
	label := statusLabel(s)
	switch s {
	case store.StatusNeedsInput:
		return theme.Badge.Render("⚑ " + label)
	case store.StatusDone, store.StatusClosed, store.StatusReview:
		return chipStyle.Foreground(theme.Green).Render(label)
	case store.StatusFailed:
		return chipStyle.Foreground(theme.Red).Render(label)
	}
	return chipStyle.Bold(true).Render(label)
}

// tabsLine switches between the YouTrack thread and the pull request's.
func (m *Run) tabsLine(w int) string {
	names := []string{"YouTrack", fmt.Sprintf("Pull request #%d", m.run.PRNumber)}
	for i, n := range names {
		if i == m.tab {
			names[i] = tabActive.Render(n)
		} else {
			names[i] = theme.Dim.Render(n)
		}
	}
	return cut(names[0]+"     "+names[1]+theme.Dim.Render("     t switches"), w)
}

// agentLine says what the agent is doing.
func (m *Run) agentLine() string {
	r, in := m.run, m.info
	who := r.Agent
	if who == "" {
		who = "agent"
	}
	started := theme.Dim.Render(" · started " + short(m.d.Now().Sub(r.CreatedAt)) + " ago")
	switch {
	case r.Status.Final():
		return theme.Dim.Render(fmt.Sprintf("%s · finished %s ago", who, short(m.d.Now().Sub(*finishedAt(r)))))
	case in.inTurn:
		what := in.activity
		if what == "" {
			what = "working"
		}
		return theme.Accent.Render(spinner[m.frameN%len(spinner)]) + " " + who + " · " + what +
			theme.Dim.Render(" · "+elapsed(m.d.Now().Sub(in.turnStart))) + started
	case m.runner.state != runnerRunning:
		return theme.Dim.Render(who+" · idle · the runner is stopped") + started
	case in.pending > 0:
		return theme.Warn.Render(fmt.Sprintf("⟳ %d new message(s) for the agent", in.pending)) + started
	}
	return theme.Dim.Render(who+" · idle") + started
}

func finishedAt(r store.Run) *time.Time {
	if r.FinishedAt != nil {
		return r.FinishedAt
	}
	return &r.UpdatedAt
}

// replyCommand is a command the comment box completes after "/".
type replyCommand struct{ cmd, desc string }

// replyCommands answer a permission request.
var replyCommands = []replyCommand{
	{"/approve", "allow what the agent asked for"},
	{"/reject", "refuse it; the agent goes on without"},
}

// suggestions are the commands that complete what is typed in the comment
// box: shown while the run waits on a permission and the text is the
// start of one.
func (m *Run) suggestions() []replyCommand {
	if !m.reply.focused || !m.waitsOnPermission() {
		return nil
	}
	v := m.reply.ta.Value()
	if v == m.acOff || !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \n") {
		return nil
	}
	var out []replyCommand
	for _, c := range replyCommands {
		if strings.HasPrefix(c.cmd, v) && c.cmd != v {
			out = append(out, c)
		}
	}
	return out
}

// suggestKey handles a key while suggestions show; used reports whether
// it took the key.
func (m *Run) suggestKey(k string) (used bool) {
	items := m.suggestions()
	n := len(items)
	if n == 0 {
		return false
	}
	m.acSel = clamp(m.acSel, n)
	switch k {
	case "up":
		m.acSel = (m.acSel + n - 1) % n
	case "down":
		m.acSel = (m.acSel + 1) % n
	case "tab", "enter":
		m.reply.set(items[m.acSel].cmd)
		m.acSel = 0
	case "esc":
		m.acOff = m.reply.ta.Value()
	default:
		return false
	}
	return true
}

// suggestBox is the popup of suggestions over the thread.
func (m *Run) suggestBox(items []replyCommand, w int) []string {
	sel := clamp(m.acSel, len(items))
	cmdW := 0
	for _, it := range items {
		cmdW = max(cmdW, len(it.cmd))
	}
	inner := 0
	lines := make([]string, len(items))
	for i, it := range items {
		c := pad(it.cmd, cmdW)
		if i == sel {
			c = theme.Bold.Render(c)
		}
		lines[i] = marker(i == sel) + c + "  " + theme.Dim.Render(it.desc)
		inner = max(inner, ansi.StringWidth(lines[i]))
	}
	inner = min(inner, max(w-4, 4))
	return box(lines, inner+4, solidEdges, theme.Accent, "", "")
}

// replyArea is the bottom of the issue pane: a small chip with what the
// run waits for, the comment box and, while it has the focus, its keys.
func (m *Run) replyArea(w, rows int) (out []string) {
	r := m.run
	switch {
	case r.Status == store.StatusNeedsInput && m.info.pending > 0:
		out = append(out, cut(theme.OK.Render("✓ reply queued")+theme.Dim.Render(" · the agent gets it at the next poll"), w))
	case r.Status == store.StatusNeedsInput:
		out = append(out, cut(theme.Badge.Render("⚑ needs input")+theme.Dim.Render(" · waiting "+short(m.d.Now().Sub(waitingSince(r)))), w))
	}
	hint := replyPlaceholder
	if r.Status.Final() {
		hint = "The run has ended; a comment goes to " + r.IssueKey + " only"
	}
	if m.d.Issues == nil {
		hint = "YouTrack is not connected here: no comments"
	}
	height := 2
	if rows >= 24 {
		height = 3
	}
	out = append(out, m.reply.lines(w, height, hint)...)
	if m.reply.focused {
		keys := []binding{{"enter", "send"}, {"alt+enter", "new line"}, {"esc", "leave the box"}}
		if len(m.suggestions()) > 0 {
			keys = []binding{{"↑↓", "choose"}, {"tab", "complete"}, {"esc", "close"}}
		}
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = theme.Key.Render(k.key) + " " + theme.Dim.Render(k.desc)
		}
		out = append(out, cut(strings.Join(parts, "  "), w))
	}
	return out
}

// docLines is the scrolling part of the issue pane: the description and
// the comments, or the pull request's conversation.
func (m *Run) docLines(w int) []string {
	if m.tab == tabChange && m.hasChange() {
		return m.changeLines(w)
	}
	now := m.d.Now()
	if m.d.Issues == nil {
		return wrap(theme.Dim.Render("YouTrack is not connected here, so the issue and its comments are not shown. o opens the issue in the browser."), w)
	}
	if !m.ytLoaded {
		return []string{theme.Dim.Render("Loading the issue…")}
	}
	var out []string
	if m.issueErr != nil {
		out = append(out, wrap(theme.Err.Render("✗ ")+"Could not read the issue: "+oneLine(m.issueErr.Error()), w)...)
	} else if desc := mdLines(m.issue.Description, w); len(desc) > 0 {
		out = append(out, desc...)
	} else {
		out = append(out, theme.Dim.Render("No description."))
	}
	out = append(out, "")
	n := len(m.comments)
	head := "Comments"
	if n > 0 {
		head = fmt.Sprintf("Comments · %d", n)
	}
	out = append(out, theme.Header.Render(head))
	if m.threadErr != nil {
		out = append(out, wrap(theme.Err.Render("✗ ")+"Could not read the comments: "+oneLine(m.threadErr.Error()), w)...)
	}
	if n == 0 && len(m.pending) == 0 && m.threadErr == nil {
		out = append(out, theme.Dim.Render("No comments yet."))
	}
	for _, c := range m.comments {
		out = append(out, "")
		out = append(out, issueItem(c).lines(w, now, m.frameN)...)
	}
	for _, p := range m.pending {
		out = append(out, "")
		out = append(out, threadItem{author: "you", body: p.text, at: p.at, pending: true}.lines(w, now, m.frameN)...)
	}
	return out
}

// changeLines is the pull request's conversation, read-only.
func (m *Run) changeLines(w int) []string {
	now := m.d.Now()
	out := wrap(theme.Dim.Render("Read-only: the reply box comments on the YouTrack issue; p opens the pull request."), w)
	switch {
	case !m.prLoaded:
		return append(out, "", theme.Dim.Render("Loading the pull request…"))
	case m.prErr != nil:
		return append(append(out, ""), wrap(theme.Err.Render("✗ ")+"Could not read the pull request: "+oneLine(m.prErr.Error()), w)...)
	case len(m.prComments) == 0:
		return append(out, "", theme.Dim.Render("No comments on the pull request yet."))
	}
	for _, c := range m.prComments {
		out = append(out, "")
		out = append(out, changeItem(c).lines(w, now, m.frameN)...)
	}
	return out
}

// logPane is the right side: a dim header, then the run's events, one
// line each.
func (m *Run) logPane(w, rows int, wide bool) []string {
	if m.logDirty || m.logWidth != w {
		m.logLines = m.logLines[:0]
		for _, e := range logEntries(m.events) {
			m.logLines = append(m.logLines, e.line(w))
		}
		m.logWidth, m.logDirty = w, false
	}
	name := theme.Header.Render("Log")
	if m.focus == paneLog && !m.reply.focused && wide {
		name = theme.Accent.Render("Log")
	}
	if !wide {
		name += theme.Dim.Render(" · tab shows the issue")
	}
	out := []string{spread(name, m.runner.label(), w)}
	if m.run.Worktree != "" {
		out = append(out, cut(theme.Dim.Render(homeShort(m.run.Worktree, m.home)), w))
	}
	if rows >= 8 {
		out = append(out, "")
	}
	m.logRows = max(rows-len(out), 0)
	if len(m.logLines) == 0 {
		return append(out, theme.Dim.Render("No events yet."))
	}
	return append(out, scrollWindow(m.logLines, m.logRows, &m.logScroll)...)
}

// help is the help line of the card.
func (m *Run) help() []binding {
	if m.reply.focused {
		return []binding{{"enter", "send"}, {"alt+enter", "new line"}, {"esc", "leave the box"}}
	}
	h := []binding{{"r", "reply"}}
	if m.waitsOnPermission() {
		h = append(h, binding{"a", "approve"}, binding{"x", "reject"})
	}
	other := "log"
	if m.focus == paneLog {
		other = "issue"
	}
	h = append(h, binding{"tab", other}, binding{"↑↓", "scroll"})
	if m.hasChange() {
		h = append(h, binding{"t", "PR thread"})
	}
	h = append(h, binding{"o", "issue"})
	if m.run.PRURL != "" {
		h = append(h, binding{"p", "PR"})
	}
	return append(h, binding{"c", "cancel"}, binding{"i", "inbox"}, binding{"esc", "close"}, binding{"q", "quit"})
}

// feedKinds are the labels and colours of the event kinds in the log.
var feedKinds = map[string]struct {
	label string
	style func(...string) string
}{
	"run.captured":         {"captured", theme.Bold.Render},
	"turn.start":           {"turn", theme.Dim.Render},
	"turn.end":             {"turn end", theme.Dim.Render},
	"agent.text":           {"agent", theme.Bold.Render},
	"agent.thought":        {"thinking", theme.Dim.Render},
	"agent.tool_call":      {"tool", theme.Dim.Render},
	"agent.permission":     {"permission", theme.Warn.Render},
	"agent.error":          {"error", theme.Err.Render},
	"agent.request_input":  {"question", theme.Warn.Render},
	"agent.report":         {"report", theme.OK.Render},
	"agent.no_resume":      {"new session", theme.Dim.Render},
	"inbox":                {"reply", theme.OK.Render},
	"run.needs_input":      {"needs input", theme.Warn.Render},
	"run.review":           {"review", theme.OK.Render},
	"run.finished":         {"finished", theme.OK.Render},
	"run.canceled":         {"canceled", theme.Err.Render},
	"stage.command":        {"stage", theme.Dim.Render},
	"stage.command_failed": {"stage", theme.Err.Render},
	"git.commit":           {"commit", theme.Dim.Render},
	"git.push":             {"push", theme.Dim.Render},
	"git.push_skipped":     {"push", theme.Dim.Render},
	"git.failed":           {"git", theme.Err.Render},
	"permission.answer":    {"permission", theme.Dim.Render},
	"permission.approved":  {"approved", theme.OK.Render},
	"permission.rejected":  {"rejected", theme.Err.Render},
	"feedback.skipped":     {"skipped", theme.Dim.Render},
	"worktree":             {"worktree", theme.Dim.Render},
	"worktree.missing":     {"worktree", theme.Err.Render},
	"worktree.removed":     {"worktree", theme.Dim.Render},
}

// feedHidden are event kinds the log leaves out: noise for people.
var feedHidden = map[string]bool{"agent.usage": true, "agent.session": true, "agent.tool_update": true}

func failedTool(e store.Event) bool {
	if e.Kind != "agent.tool_update" {
		return false
	}
	var v struct {
		Tool struct{ Status string } `json:"tool"`
	}
	return json.Unmarshal(e.Data, &v) == nil && v.Tool.Status == "failed"
}

// feedText is the text of an event for the log.
func feedText(e store.Event) string {
	f := fields(e.Data)
	switch e.Kind {
	case "run.captured":
		return strings.TrimSpace(f["issue"] + " " + f["summary"])
	case "turn.start":
		return f["kind"] + " turn"
	case "turn.end":
		return f["reason"]
	case "agent.tool_call", "agent.tool_update":
		if t := toolTitle(e.Data); t != "" {
			return t
		}
	case "inbox":
		return f["author"] + ": " + f["text"]
	case "stage.command":
		return f["stage"] + ": " + f["command"]
	case "stage.command_failed":
		return f["stage"] + ": " + f["command"] + " — " + f["error"]
	case "git.push":
		return f["branch"]
	case "git.push_skipped":
		return "skipped: " + f["reason"]
	case "git.failed":
		return f["step"] + ": " + f["error"]
	case "run.review":
		return "branch " + f["branch"]
	case "run.finished":
		return f["status"]
	case "feedback.skipped":
		return f["author"] + ": " + f["reason"]
	case "worktree", "worktree.missing", "worktree.removed":
		return f["path"]
	case "permission.answer":
		var v struct {
			Tool  string `json:"tool"`
			Allow bool   `json:"allow"`
			Why   string `json:"why"`
		}
		if json.Unmarshal(e.Data, &v) == nil {
			verdict := "rejected"
			if v.Allow {
				verdict = "allowed"
			}
			return strings.TrimSpace(verdict + " " + v.Tool + " " + v.Why)
		}
	case "permission.approved", "permission.rejected":
		var v struct {
			Tools []string `json:"tools"`
		}
		if json.Unmarshal(e.Data, &v) == nil {
			return strings.Join(v.Tools, ", ")
		}
	}
	for _, k := range []string{"text", "question", "reason", "message", "error", "command", "summary", "title"} {
		if s := strings.TrimSpace(f[k]); s != "" {
			return s
		}
	}
	if len(e.Data) == 0 || string(e.Data) == "null" || string(e.Data) == "{}" {
		return ""
	}
	return oneLine(string(e.Data))
}
