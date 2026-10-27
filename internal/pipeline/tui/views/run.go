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
			send, text, cmd := m.reply.key(msg)
			if send {
				m.reply.clear()
				m.reply.blur()
				return m, m.post(text)
			}
			return m, cmd
		}
		return m, m.key(msg.String())
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

// inner is the content width and height of the box at the current size,
// and whether it has a border.
func (m *Run) inner() (cw, ch int, border bool) {
	_, _, bw, bh := modalRect(max(m.width, 1), max(m.height, 1))
	border = bw >= 24 && bh >= 6
	if border {
		return max(bw-4, 1), bh - 2, true
	}
	return bw, bh, false
}

// split is the width of the issue and the log side by side, or 0 for the
// log when they take turns.
func split(cw int) (lw, rw int) {
	if cw < wideFrom {
		return cw, 0
	}
	rw = min(max(cw*32/100, 24), 64)
	return cw - rw - 3, rw
}

func (m *Run) leftWidth() int {
	cw, _, _ := m.inner()
	lw, _ := split(cw)
	return lw
}

// View renders the card over a blank screen; the root app draws it over
// the screen it was opened from through Overlay.
func (m *Run) View() string { return m.Overlay("") }

// Overlay draws the card over under.
func (m *Run) Overlay(under string) string {
	w, h := max(m.width, 1), max(m.height, 1)
	x, y, bw, bh := modalRect(w, h)
	return modalOver(under, m.box(bw, bh), x, y, w, h)
}

// box is the card: bw×bh with its border.
func (m *Run) box(bw, bh int) []string {
	cw, ih, border := m.inner()
	// The status line shows only when it says something; on tiny screens
	// it takes the help's place.
	foot := m.helpLines(m.help(), cw)
	if ih < 8 {
		foot = foot[:1]
	}
	if s := m.statusLine(); s != "" {
		if ih < 8 {
			foot = nil
		}
		foot = append([]string{s}, foot...)
	}
	rows := max(ih-len(foot), 0)
	head := title(fmt.Sprintf("%s #%d", m.pipeline, m.number))
	right := ""
	if m.loaded {
		right = statusStyle(m.run.Status)
	}
	var body []string
	if !border && rows > 0 {
		body = append(body, spread(head, right, cw))
		rows--
	}
	switch {
	case rows <= 0:
	case !m.loaded:
		body = append(body, theme.Dim.Render("Loading…"))
	default:
		body = append(body, m.panes(cw, rows)...)
	}
	inner := make([]string, 0, ih)
	inner = append(inner, body[:min(len(body), max(ih-len(foot), 0))]...)
	for len(inner) < ih-len(foot) {
		inner = append(inner, "")
	}
	inner = append(inner, foot...)
	if len(inner) > ih {
		inner = inner[len(inner)-ih:]
	}
	if !border {
		for i, l := range inner {
			inner[i] = pad(l, bw)
		}
		return inner
	}
	for i, l := range inner {
		inner[i] = " " + pad(l, cw) + " "
	}
	return frameBox(inner, bw-2, head, right)
}

// panes lays out the issue and the log, side by side when wide enough,
// else the focused one.
func (m *Run) panes(cw, rows int) []string {
	lw, rw := split(cw)
	if rw == 0 {
		if m.focus == paneLog {
			return m.logPane(cw, rows, false)
		}
		return m.issuePane(cw, rows, false)
	}
	left := m.issuePane(lw, rows, true)
	right := m.logPane(rw, rows, true)
	sep := theme.Dim.Render(" │ ")
	out := make([]string, rows)
	for i := range rows {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		out[i] = pad(l, lw) + sep + pad(r, rw)
	}
	return out
}

// rule is a horizontal line, in the brand colour on the focused pane.
func rule(w int, focused bool) string {
	line := strings.Repeat("─", max(w, 1))
	if focused {
		return theme.Accent.Render(line)
	}
	return theme.Dim.Render(line)
}

// issuePane is the left side: title, chips, the thread and the reply box.
func (m *Run) issuePane(w, rows int, wide bool) []string {
	head := m.titleLines(w)
	head = append(head, m.chipLines(w)...)
	head = append(head, cut(m.agentLine(), w))
	if m.hasChange() {
		head = append(head, m.tabsLine(w))
	}
	if !wide {
		head = append(head, cut(theme.Dim.Render("tab shows the log"), w))
	}
	head = append(head, rule(w, wide && m.focus == paneThread && !m.reply.focused))
	foot := m.replyArea(w, rows)
	doc := m.docLines(w)
	docRows := rows - len(head) - len(foot)
	if docRows < 2 && len(head) > 2 { // small: keep the title and the rule
		head = []string{head[0], head[len(head)-1]}
		docRows = rows - len(head) - len(foot)
	}
	if docRows < 0 {
		keep := max(rows-len(head), 0)
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
	out = append(out, foot...)
	if len(out) > rows {
		out = out[:rows]
	}
	return out
}

// titleLines is the issue key and summary, on up to two lines.
func (m *Run) titleLines(w int) []string {
	summary := m.issue.Summary
	if summary == "" {
		summary = m.info.summary
	}
	lines := wrap(theme.Accent.Render(m.run.IssueKey)+" "+theme.Bold.Render(oneLine(summary)), max(w, 1))
	if len(lines) > 2 {
		lines = []string{lines[0], cut(lines[1]+" …", w)}
	}
	return lines
}

// keyFields are the YouTrack fields shown as chips, in this order.
var keyFields = []string{"State", "Type", "Priority", "Assignee"}

// chipLines are the chips of the issue's key fields and the run, on up to
// two lines.
func (m *Run) chipLines(w int) []string {
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
	var lines []string
	line := ""
	for _, c := range chips {
		next := c
		if line != "" {
			next = line + " " + c
		}
		if line != "" && ansi.StringWidth(next) > w {
			lines = append(lines, line)
			line = c
			continue
		}
		line = next
	}
	lines = append(lines, line)
	if len(lines) > 2 {
		lines = []string{lines[0], lines[1] + " …"}
	}
	for i, l := range lines {
		lines[i] = cut(l, w)
	}
	return lines
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
	return cut(names[0]+theme.Dim.Render(" │ ")+names[1]+theme.Dim.Render("   t switches"), w)
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

// replyArea is what sits under the thread: what the run waits for, the
// quick answers to a permission request and the reply box.
func (m *Run) replyArea(w, rows int) []string {
	r := m.run
	out := []string{rule(w, m.reply.focused)}
	if r.Status == store.StatusNeedsInput {
		head := theme.Badge.Render("⚑ NEEDS INPUT") + theme.Dim.Render(" · waiting "+short(m.d.Now().Sub(waitingSince(r))))
		if m.info.pending > 0 {
			head += theme.OK.Render("  ✓ reply queued")
		}
		out = append(out, cut(head, w))
		reason := strings.TrimSpace(r.NeedsInputReason)
		if reason == "" {
			reason = "The agent waits for an answer."
		}
		limit := 1
		if rows >= 24 {
			limit = 2
		}
		lines := wrap(oneLine(reason), max(w, 1))
		if len(lines) > limit {
			lines = append(lines[:limit-1], cut(lines[limit-1]+" …", w))
		}
		out = append(out, lines...)
	}
	if m.waitsOnPermission() && !m.reply.focused {
		out = append(out, cut(theme.Warn.Render("Permission")+"  "+theme.Key.Render("a")+" /approve  "+
			theme.Key.Render("x")+" /reject  "+theme.Dim.Render("posts the comment"), w))
	}
	hint := "r to comment on " + r.IssueKey
	if r.Status.Final() {
		hint += " (the run has ended)"
	}
	if m.d.Issues == nil {
		hint = "YouTrack is not connected here: no comments"
	}
	return append(out, m.reply.lines(w, hint)...)
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

// logPane is the right side: the run's events, one line each.
func (m *Run) logPane(w, rows int, wide bool) []string {
	if m.logDirty || m.logWidth != w {
		m.logLines = m.logLines[:0]
		for _, e := range logEntries(m.events) {
			m.logLines = append(m.logLines, e.line(w))
		}
		m.logWidth, m.logDirty = w, false
	}
	name := "Log"
	focused := m.focus == paneLog && !m.reply.focused
	if focused && wide {
		name = theme.Accent.Render(name)
	} else {
		name = theme.Bold.Render(name)
	}
	if !wide {
		name += theme.Dim.Render(" · tab shows the issue")
	}
	out := []string{spread(name, m.runner.label(), w)}
	if m.run.Worktree != "" {
		out = append(out, cut(theme.Dim.Render(homeShort(m.run.Worktree, m.home)), w))
	}
	out = append(out, rule(w, focused && wide))
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
