package views

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/engine"
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
//
// A backlog issue opens the same card without a run (see NewIssueCard):
// the issue and its thread, a note where the log would be, no run keys.
type Run struct {
	base
	pipeline string
	number   int
	home     string
	backlog  bool   // an issue the runner has not taken yet: no run
	source   string // the pipeline's source query, for a backlog issue

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

	pics  *pictures
	files []Attachment // the issue's own attachments

	reply     replyBox
	wantReply bool
	acSel     int    // the suggestion under the cursor
	acOff     string // the text the suggestions were closed for

	people      map[string][]Person // who YouTrack found, by mention query
	peopleLast  []Person            // the last answer shown
	peopleAsked map[string]bool     // the queries looked up

	fe fieldEditor // the Fields menu, from Deps.Fields
	tr transcript  // the detailed log over the card
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
	source   string
	err      error
}

type issueLoaded struct {
	to        int64
	issue     ytwatch.Issue
	issueErr  error
	comments  []IssueComment
	threadErr error
	files     []Attachment // the issue's own
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
	queued  bool // the run has it: the runner needn't wait for its poll
	err     error
}

// NewRun makes the card of a pipeline's run; reply focuses the reply box
// at once.
func NewRun(d *Deps, pipeline string, number int, reply bool) *Run {
	home, _ := os.UserHomeDir()
	return &Run{base: newBase(d), pipeline: pipeline, number: number, home: home, reply: newReplyBox(), wantReply: reply,
		pics: newPictures(d.PictureMode, "")}
}

// NewIssueCard makes the card of a backlog issue of a pipeline: the run
// card without a run. It reads no run from the store; the issue and its
// comments load through Deps.Issues as for a run.
func NewIssueCard(d *Deps, pipeline, profile string, issue ytwatch.Issue) *Run {
	m := NewRun(d, pipeline, 0, false)
	m.backlog, m.loaded = true, true
	m.run = store.Run{Pipeline: pipeline, Profile: profile, IssueKey: issue.Key, IssueID: issue.ID}
	m.issue, m.info.summary = issue, issue.Summary
	return m
}

// Init loads the run, or a backlog issue's thread at once.
func (m *Run) Init() tea.Cmd {
	if m.backlog {
		return tea.Batch(m.reload(), m.loadIssue(false))
	}
	return m.reload()
}

// Answering reports whether the reply box or the Fields menu has the
// focus, so the root app keeps its own keys out of them.
func (m *Run) Answering() bool { return m.reply.focused || m.fe.open }

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
	if m.backlog { // no run: the runner, the profiles and the source
		msg.runner, msg.err = m.d.runner(ctx, m.pipeline)
		msg.profiles, _ = m.d.Profiles()
		if list, err := m.d.Pipelines(); err == nil {
			if p, ok := pipelineByName(list, m.pipeline); ok {
				msg.source = p.Source.Query
			}
		}
		return msg
	}
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
	iss, pics, profile, ref, to := m.d.Issues, m.d.Pictures, m.run.Profile, issueRef(m.run), m.id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), ytTimeout)
		defer cancel()
		msg := issueLoaded{to: to}
		msg.issue, msg.issueErr = iss.Issue(ctx, profile, ref)
		msg.comments, msg.threadErr = iss.Comments(ctx, profile, ref)
		if pics != nil {
			msg.files, _ = pics.Attachments(ctx, profile, ref) // without them, names stay names
		}
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
			if msg.run.ID == 0 && !m.backlog {
				return m, nil
			}
		}
		first := !m.loaded
		m.loaded = true
		m.runner, m.profiles = msg.runner, msg.profiles
		m.pics.base = strings.TrimRight(profileURL(m.profiles, cmp.Or(msg.run.Profile, m.run.Profile)), "/")
		if !m.backlog { // a backlog card keeps the run it was made with
			m.run, m.info = msg.run, msg.info
		} else if msg.source != "" {
			m.source = msg.source
		}
		if len(msg.events) > 0 {
			m.events = append(m.events, msg.events...)
			if len(m.events) > feedKeep {
				m.events = m.events[len(m.events)-feedKeep:]
			}
			m.logDirty = true
			if m.tr.open {
				m.transcriptAdd(msg.events)
			}
		}
		if first {
			cmds := []tea.Cmd{m.loadIssue(false)}
			if m.wantReply {
				m.wantReply = false
				cmds = append(cmds, m.focusReply())
			}
			return m, tea.Batch(cmds...)
		}
	case transcriptLoaded:
		if msg.to == m.id {
			m.transcriptRead(msg)
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
		if msg.issueErr == nil {
			m.files, m.pics.files = msg.files, msg.files
		}
		fetch := m.pics.fetch(m.d, m.run.Profile, m.id, m.issuePictures())
		if m.ytAgain {
			m.ytAgain = false
			return m, tea.Batch(fetch, m.loadIssue(false))
		}
		return m, fetch
	case changeLoaded:
		if msg.to != m.id {
			return m, nil
		}
		m.prLoading, m.prLoaded, m.prErr = false, true, msg.err
		if msg.err == nil {
			m.prComments = msg.comments
			var urls []string
			for _, c := range m.prComments {
				urls = append(urls, pictureRefs(c.Body)...)
			}
			return m, m.pics.fetch(m.d, m.run.Profile, m.id, urls)
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
		m.say(m.posted(msg.queued))
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
	case fieldsLoaded:
		if msg.to == m.id {
			m.fieldsRead(msg)
		}
		return m, nil
	case fieldApplied:
		if msg.to != m.id {
			return m, nil
		}
		return m, m.applied(msg)
	case picturesLoaded:
		if msg.to == m.id {
			m.pics.loaded(msg)
		}
		return m, nil
	case peopleFound:
		if msg.to == m.id {
			if m.people == nil {
				m.people = map[string][]Person{}
			}
			m.people[msg.query] = msg.people // an error finds nobody
		}
		return m, nil
	case tea.KeyMsg:
		if m.fe.open {
			return m, m.fieldKey(msg)
		}
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
			return m, tea.Batch(cmd, m.askPeople())
		}
		return m, m.key(keys.Of(msg))
	default:
		if m.fe.typing {
			var cmd tea.Cmd
			m.fe.input, cmd = m.fe.input.Update(msg)
			return m, cmd
		}
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

// posted says where a comment went; queued is whether the run has it
// already.
func (m *Run) posted(queued bool) string {
	key := m.run.IssueKey
	switch {
	case queued:
		return fmt.Sprintf("Commented on %s; run #%d has it.", key, m.number)
	case m.backlog:
		return fmt.Sprintf("Commented on %s.", key)
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
		return fmt.Sprintf("Answer sent to #%d; the runner has it.", number)
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
	hand := !m.backlog && (!m.run.Status.Final() || engine.EndedCommand(text)) && m.d.Store != nil && m.d.Commented != nil
	commented, st, pipeline, number := m.d.Commented, m.d.Store, m.pipeline, m.number
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), ytTimeout)
		defer cancel()
		c, err := iss.AddComment(ctx, profile, ref, text)
		sent := commentSent{to: to, seq: seq, text: text, comment: c, err: err}
		if err == nil && hand {
			// Failing here only leaves the comment to the runner's poll.
			sent.queued = commented(ctx, st, pipeline, number, engine.Comment{
				ID: c.ID, Author: c.Author, Text: c.Text, URL: c.URL, Created: c.Created, Updated: c.Updated,
			}) == nil
		}
		return sent
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
	if m.tr.open {
		return m.transcriptKey(k)
	}
	page := max(m.docRows-1, 1)
	scroll := &m.docScroll
	if m.focus == paneLog {
		page, scroll = max(m.logRows-1, 1), &m.logScroll
	}
	if m.backlog {
		switch k {
		case "tab", "shift+tab": // no log to switch to
			return nil
		case "a", "x", "c", "d", "t", "[", "]", "p":
			m.fail(fmt.Errorf("%s is not taken yet, so it has no run", m.run.IssueKey))
			return nil
		}
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
	case "enter":
		return m.focusReply()
	case "r":
		return m.refresh()
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
	case "t":
		if !m.loaded {
			return nil
		}
		return m.openTranscript()
	case "[", "]":
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
	case "d":
		if !m.loaded {
			return nil
		}
		if !m.run.Status.Final() {
			m.fail(fmt.Errorf("run #%d is still %s; cancel it first", m.number, statusLabel(m.run.Status)))
			return nil
		}
		n := m.number
		return deleteRun(&m.base, m.pipeline, m.run, func() tea.Cmd {
			return navigate(Back{Note: fmt.Sprintf("Deleted run #%d.", n)})
		})
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
	case "e":
		return m.openFields()
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

func gutter() string { return "  " + modalBorder.Render("│") + "  " }

// footIndent puts the help and status lines under the issue in line with
// the text of the comment box.
const footIndent = "  "

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
	v := modalOver(under, m.box(bw, bh), x, y, w, h)
	if m.tr.open { // the transcript, over the card
		tx, ty, tw, th := transcriptRect(x, y, bw, bh)
		v = modalOver(v, m.transcriptBox(tw, th), tx, ty, w, h)
	}
	return v
}

// box is the card: bw×bh with its frame. Inside, padded: the top bar
// (the issue, its fields, the agent, the tabs), the issue's thread with
// the comment box beside the run's log, and the status and help lines.
func (m *Run) box(bw, bh int) []string {
	cw, ih, border := m.inner()
	// Side by side, the gutter runs down to the frame and the help and
	// status lines sit under the issue.
	lw, rw := split(cw)
	beside := border && rw > 0 && m.loaded
	footW := cw
	if beside {
		footW = max(lw-len(footIndent), 1)
	}
	// The comment box shows its own keys while it has the focus. The
	// status line shows only when it says something; on tiny screens it
	// takes the help's place.
	var foot []string
	if !m.reply.focused {
		foot = m.helpLines(m.help(), footW)
		if ih < 8 {
			foot = foot[:1]
		}
	}
	if s := m.statusLine(); s != "" {
		if ih < 8 {
			foot = nil
		}
		status := []string{s}
		if beside { // narrower under the issue: wrap rather than cut
			status = wrap(s, footW)
		}
		foot = append(status, foot...)
	}
	if beside {
		for i, l := range foot {
			foot[i] = footIndent + cut(l, footW)
		}
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
		switch {
		case m.backlog:
			right = theme.Dim.Render("backlog")
		case m.loaded:
			right = statusStyle(m.run.Status)
		}
		body = append(body, spread(title(m.heading()), right, cw))
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
	panesEnd := len(inner)
	for len(inner) < ih-len(foot) {
		inner = append(inner, "")
	}
	inner = append(inner, foot...)
	if len(inner) > ih {
		ruleAt -= len(inner) - ih
		panesEnd -= len(inner) - ih
		inner = inner[len(inner)-ih:]
	}
	beside = beside && ruleAt >= 0
	if beside {
		for i := max(panesEnd, ruleAt+1); i < len(inner); i++ {
			inner[i] = pad(inner[i], lw) + gutter()
		}
	}
	if m.fe.open { // the Fields menu, over the thread under the top bar
		inner = m.fieldsOver(inner, cw, lw, beside, max(ruleAt+1, 0))
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
		if beside {
			padded = append(padded, side+pad("", lw)+gutter())
		} else {
			padded = append(padded, "")
		}
	}
	out := frameBox(padded, bw-2, "", "")
	b := lipgloss.RoundedBorder()
	if ruleAt >= 0 { // the rule meets the frame
		out[1+py+ruleAt] = modalBorder.Render("├") + m.ruleLine(cw, px) + modalBorder.Render("┤")
	}
	if beside { // so does the gutter, at the bottom
		at := px + lw + 2
		out[len(out)-1] = modalBorder.Render(b.BottomLeft + strings.Repeat(b.Bottom, at) + "┴" + strings.Repeat(b.Bottom, max(bw-3-at, 0)) + b.BottomRight)
	}
	return out
}

// fieldsOver lays the Fields menu over the lines of the card from line
// y, in the middle of the issue's side (lw cells beside the log, else cw),
// moved up or cut when the card is too short for it.
func (m *Run) fieldsOver(lines []string, cw, lw int, beside bool, y int) []string {
	h := len(lines)
	if h == 0 {
		return lines
	}
	y = min(y, h-1)
	pop := m.fieldsBox(cw, h-y)
	if len(pop) > h {
		pop = pop[:h]
	}
	y = min(y, h-len(pop))
	area := cw
	if beside {
		area = lw
	}
	return popOver(lines, pop, max((area-ansi.StringWidth(pop[0]))/2, 0), y)
}

// heading is the title of the card: the pipeline and the run number, the
// pipeline alone for a backlog issue.
func (m *Run) heading() string {
	if m.backlog {
		return m.pipeline
	}
	return fmt.Sprintf("%s #%d", m.pipeline, m.number)
}

// topBar is the head of the card across its width: the pipeline and run,
// the issue key and summary on the left, the issue's fields and the run's
// state as a grid of chips on the right (under the title when they don't
// fit there), then the agent line and, with a pull request, the tabs of
// the thread. The grid takes no more rows than leave the top bar within
// half of rows.
func (m *Run) topBar(w int, border bool, rows int) []string {
	summary := m.issue.Summary
	if summary == "" {
		summary = m.info.summary
	}
	head := theme.Accent.Render(m.run.IssueKey) + " " + theme.Bold.Render(oneLine(summary))
	if border {
		head = title(m.heading()) + "  " + head
	}
	var tail []string
	if !m.backlog { // no agent yet; the note sits where the log would be
		tail = append(tail, cut(m.agentLine(), w))
	}
	if m.hasChange() {
		if rows >= 20 {
			tail = append(tail, "")
		}
		tail = append(tail, m.tabsLine(w))
	}
	if rows >= 24 { // air above the rule
		tail = append(tail, "")
	}
	groups := m.chipGroups()
	var out []string
	if w >= wideFrom {
		out = sideBySide(head, groups, w, max(rows/2-len(tail), 2))
	}
	if out == nil { // the title, then the grid under it
		out = wrapLines(head, w, 2)
		grid, _ := chipGrid(groups, w, max(rows/2-len(out)-len(tail), 1))
		out = append(out, grid...)
	}
	return append(out, tail...)
}

// sideBySide lays a title out on the left and the grid of chips on the
// right of w cells, when the whole grid fits there in n rows: the title
// takes its whole width when the grid still fits beside it, else it wraps
// into what the grid leaves. It returns nil when the grid does not fit.
func sideBySide(head string, groups [][]string, w, n int) []string {
	headW := ansi.StringWidth(head)
	var rows []string
	fit := false
	for _, leftW := range []int{headW, max(w*2/5, 24)} {
		if rows, fit = chipGrid(groups, w-leftW-3, n); fit {
			break
		}
	}
	if !fit {
		return nil
	}
	widest := 0
	for _, r := range rows {
		widest = max(widest, ansi.StringWidth(r))
	}
	left := wrapLines(head, max(w-widest-3, 1), 2)
	out := make([]string, max(len(left), len(rows)))
	for i := range out {
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
		if m.focus == paneLog && !m.backlog {
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
	var right []string
	if m.backlog {
		right = m.notePane(rw, n)
	} else {
		right = m.logPane(rw, n, true)
	}
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
	return rule(lw+extra, thread) + modalBorder.Render("──┬──") + rule(rw+extra, log)
}

// rule is a horizontal line in the colour of the frame, bold over the
// focused pane.
func rule(w int, focused bool) string {
	line := strings.Repeat("─", max(w, 1))
	if focused {
		return theme.Accent.Render(line)
	}
	return modalBorder.Render(line)
}

// issuePane is the left side: the thread, then the comment box at the
// bottom like the input of a chat.
func (m *Run) issuePane(w, rows int, wide bool) []string {
	if rows <= 0 {
		return nil
	}
	var head []string
	switch {
	case !wide && m.backlog: // the note of the log pane, over the thread
		head = append(wrapLines(theme.Dim.Render(backlogNote), w, 2), "")
	case !wide:
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
	if title, items := m.suggestions(); len(items) > 0 && at > 0 {
		pop := m.suggestBox(title, items, w)
		out = popOver(out, pop, 0, max(at-len(pop), 0))
		if len(out) > rows {
			out = out[:rows]
		}
	}
	return out
}

// chipGroups are the chips of the top bar's grid: every field of the
// issue that has a value, then on their own rows the run's stage, status,
// branch and pull request, or "backlog" for a backlog issue.
func (m *Run) chipGroups() [][]string {
	r := m.run
	fields := fieldChips(m.issue.Fields)
	if m.backlog {
		return [][]string{fields, {chipStyle.Render("backlog")}}
	}
	var run []string
	if r.Stage != "" {
		run = append(run, chipStyle.Render("stage "+r.Stage))
	}
	run = append(run, statusChip(r.Status))
	if r.Branch != "" {
		run = append(run, chipStyle.Render(cut("⎇ "+r.Branch, chipMax-2)))
	}
	if r.PRURL != "" {
		run = append(run, chipStyle.Render(fmt.Sprintf("PR #%d", r.PRNumber)))
	}
	return [][]string{fields, run}
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

// The commands the runner reads in a comment: the answers to a permission
// request, and those that act on any run or on a run still going.
var (
	permissionCommands = []replyCommand{
		{"/approve", "allow what the agent asked for"},
		{"/reject", "refuse it; the agent goes on without"},
	}
	runCommands = []replyCommand{
		{"/compact", "hide the run's comments and leave a short summary"},
		{"/clear", "hide the run's comments; they stay in the history"},
	}
	cancelCommand = replyCommand{"/cancel", "cancel the run and close the issue"}
)

// commands are the commands that apply to the card now; none for a
// backlog issue.
func (m *Run) commands() []replyCommand {
	if m.backlog || !m.loaded {
		return nil
	}
	var out []replyCommand
	if m.waitsOnPermission() {
		out = append(out, permissionCommands...)
	}
	out = append(out, runCommands...)
	if !m.run.Status.Final() {
		out = append(out, cancelCommand)
	}
	return out
}

// suggestion is what the comment box offers to complete: a command after
// a leading "/", a person after "@".
type suggestion struct {
	label, right, sub string // sub is a second, dim line
	insert            string // what replaces the word being typed
}

// suggestions are what completes the text in the comment box, under a
// title: the commands that apply now while the text is the start of one,
// the people matching the word after "@" being typed.
func (m *Run) suggestions() (title string, items []suggestion) {
	if !m.reply.focused {
		return "", nil
	}
	v := m.reply.ta.Value()
	if v == m.acOff {
		return "", nil
	}
	if q, ok := m.mention(); ok {
		for _, p := range m.peopleFor(q) {
			items = append(items, suggestion{label: cmp.Or(p.FullName, p.Login), right: p.Login, sub: p.Email, insert: "@" + p.Login})
		}
		return "People", items
	}
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \n") {
		return "", nil
	}
	for _, c := range m.commands() {
		if strings.HasPrefix(c.cmd, v) && c.cmd != v {
			items = append(items, suggestion{label: c.cmd, right: c.desc, insert: c.cmd})
		}
	}
	return "Commands", items
}

// mention is the word after "@" being typed at the end of the comment.
func (m *Run) mention() (string, bool) {
	if m.d.People == nil || !m.reply.focused {
		return "", false
	}
	v := m.reply.ta.Value()
	word := v[strings.LastIndexAny(v, " \t\n")+1:]
	if !strings.HasPrefix(word, "@") {
		return "", false
	}
	return word[1:], true
}

// peopleFor are the people YouTrack found for query; until it answers,
// those of the last answer that still match.
func (m *Run) peopleFor(query string) []Person {
	if found, ok := m.people[query]; ok {
		m.peopleLast = found
		return found
	}
	q := strings.ToLower(query)
	var out []Person
	for _, p := range m.peopleLast {
		if strings.Contains(strings.ToLower(p.Login+" "+p.FullName+" "+p.Email), q) {
			out = append(out, p)
		}
	}
	return out
}

// askPeople looks up the people for the mention being typed, once per
// query.
func (m *Run) askPeople() tea.Cmd {
	q, ok := m.mention()
	if !ok {
		return nil
	}
	return m.lookUpPeople(q)
}

// lookUpPeople asks YouTrack for the people matching q, once per query;
// the answer lands in m.people.
func (m *Run) lookUpPeople(q string) tea.Cmd {
	if m.peopleAsked[q] {
		return nil
	}
	if m.peopleAsked == nil {
		m.peopleAsked = map[string]bool{}
	}
	m.peopleAsked[q] = true
	people, profile, to := m.d.People, m.run.Profile, m.id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), ytTimeout)
		defer cancel()
		found, err := people.Users(ctx, profile, q)
		return peopleFound{to: to, query: q, people: found, err: err}
	}
}

// peopleFound is YouTrack's answer to a mention lookup.
type peopleFound struct {
	to     int64
	query  string
	people []Person
	err    error
}

// suggestKey handles a key while suggestions show; used reports whether
// it took the key.
func (m *Run) suggestKey(k string) (used bool) {
	_, items := m.suggestions()
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
		m.complete(items[m.acSel])
		m.acSel = 0
	case "esc":
		m.acOff = m.reply.ta.Value()
	default:
		return false
	}
	return true
}

// complete puts a suggestion in the comment: a command replaces the text,
// a person the word after "@", followed by a space.
func (m *Run) complete(s suggestion) {
	if strings.HasPrefix(s.insert, "/") {
		m.reply.set(s.insert)
		return
	}
	v := m.reply.ta.Value()
	m.reply.set(v[:strings.LastIndexAny(v, " \t\n")+1] + s.insert + " ")
}

// suggestBox is the popup of suggestions over the thread, drawn like the
// pipelines' menu.
func (m *Run) suggestBox(title string, items []suggestion, w int) []string {
	sel := clamp(m.acSel, len(items))
	labelW, rightW := 0, 0
	for _, it := range items {
		labelW = max(labelW, ansi.StringWidth(it.label))
		rightW = max(rightW, ansi.StringWidth(it.right))
	}
	inner := min(max(2+labelW+3+rightW, ansi.StringWidth(title)+1, menuMinWidth), max(w-4, 8))
	var lines []string
	for i, it := range items {
		label := it.label
		if i == sel {
			label = theme.Bold.Render(label)
		}
		lines = append(lines, spread(marker(i == sel)+label, theme.Dim.Render(it.right), inner))
		if it.sub != "" {
			lines = append(lines, cut("  "+theme.Dim.Render(it.sub), inner))
		}
	}
	return box(lines, inner+4, solidEdges, theme.Accent, theme.Bold.Render(title), "")
}

// placeholder is what the empty comment box says: what it completes.
func (m *Run) placeholder() string {
	var can []string
	if len(m.commands()) > 0 {
		can = append(can, "/command")
	}
	if m.d.People != nil {
		can = append(can, "@mention people")
	}
	if len(can) == 0 {
		return replyPlaceholder
	}
	return "Write a comment, " + strings.Join(can, ", ")
}

// replyArea is the bottom of the issue pane: a small chip with what the
// run waits for, the comment box and, while it has the focus, its keys.
func (m *Run) replyArea(w, rows int) (out []string) {
	r := m.run
	switch {
	case r.Status == store.StatusNeedsInput && m.info.pending > 0:
		out = append(out, cut(theme.OK.Render("✓ reply queued")+theme.Dim.Render(" · the runner has it"), w))
	case r.Status == store.StatusNeedsInput:
		out = append(out, cut(theme.Badge.Render("⚑ needs input")+theme.Dim.Render(" · waiting "+short(m.d.Now().Sub(waitingSince(r)))), w))
	}
	hint := m.placeholder()
	m.reply.ta.Placeholder = hint
	if r.Status.Final() {
		hint = "The run has ended; a comment goes to " + r.IssueKey + " only, but /clear and /compact still work"
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
		if _, items := m.suggestions(); len(items) > 0 {
			keys = []binding{{"↑↓", "choose"}, {"tab", "complete"}, {"esc", "close"}}
		}
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = theme.Key.Render(k.key) + " " + theme.Dim.Render(k.desc)
		}
		out = append(out, cut(footIndent+strings.Join(parts, "  "), w)) // in line with the text in the box
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
	} else if desc := mdLinesWith(m.issue.Description, w, m.drawPicture(nil)); len(desc) > 0 {
		out = append(out, desc...)
	} else {
		out = append(out, theme.Dim.Render("No description."))
	}
	if files := m.looseFiles(); len(files) > 0 {
		for _, f := range files {
			out = append(out, "")
			out = append(out, m.pics.file(f, w)...)
		}
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
		out = append(out, issueItem(c).lines(w, now, m.frameN, m.pics)...)
	}
	for _, p := range m.pending {
		out = append(out, "")
		out = append(out, threadItem{author: "you", body: p.text, at: p.at, pending: true}.lines(w, now, m.frameN, m.pics)...)
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
		out = append(out, changeItem(c).lines(w, now, m.frameN, m.pics)...)
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

// backlogNote stands where the log of a run would be on a backlog card.
const backlogNote = "Not taken yet — the runner picks it up from the backlog."

// notePane is the right side of a backlog card: a dim header with the
// runner's state, then backlogNote.
func (m *Run) notePane(w, rows int) []string {
	out := []string{spread(theme.Header.Render("Run"), m.runner.label(), w)}
	if rows >= 8 {
		out = append(out, "")
	}
	return append(out, wrap(theme.Dim.Render(backlogNote), w)...)
}

// help is the help line of the card.
func (m *Run) help() []binding {
	if m.reply.focused {
		return []binding{{"enter", "send"}, {"alt+enter", "new line"}, {"esc", "leave the box"}}
	}
	if m.fe.open {
		return m.fieldsHelp()
	}
	if m.backlog {
		h := []binding{{"enter", "comment"}, {"esc", "close"}, {"↑↓", "scroll"}, {"r", "reload"}}
		if m.d.Fields != nil {
			h = append(h, binding{"e", "fields"})
		}
		return append(h, binding{"o", "issue"}, binding{"i", "inbox"}, binding{"q", "quit"})
	}
	h := []binding{{"enter", "reply"}}
	if m.waitsOnPermission() {
		h = append(h, binding{"a", "approve"}, binding{"x", "reject"})
	}
	h = append(h, binding{"esc", "close"})
	other := "log"
	if m.focus == paneLog {
		other = "issue"
	}
	h = append(h, binding{"tab", other}, binding{"t", "transcript"}, binding{"↑↓", "scroll"}, binding{"r", "reload"})
	if m.hasChange() {
		h = append(h, binding{"[ ]", "PR thread"})
	}
	if m.d.Fields != nil {
		h = append(h, binding{"e", "fields"})
	}
	h = append(h, binding{"o", "issue"})
	if m.run.PRURL != "" {
		h = append(h, binding{"p", "PR"})
	}
	if m.run.Status.Final() {
		h = append(h, binding{"d", "delete"})
	} else {
		h = append(h, binding{"c", "cancel"})
	}
	return append(h, binding{"i", "inbox"}, binding{"q", "quit"})
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
	"describe.start":       {"describe", theme.Dim.Render},
	"describe.end":         {"described", theme.Dim.Render},
	"agent.set_change":     {"PR/MR", theme.OK.Render},
	"vcs.updated":          {"PR/MR", theme.Dim.Render},
	"vcs.update_skipped":   {"PR/MR", theme.Warn.Render},
	"ci.running":           {"ci", theme.Dim.Render},
	"ci.passed":            {"ci", theme.OK.Render},
	"ci.failed":            {"ci", theme.Err.Render},
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
	case "describe.start":
		return "asking for the title and description"
	case "agent.set_change":
		return f["title"]
	case "vcs.updated":
		return "updated: " + f["title"]
	case "vcs.update_skipped":
		return "not updated: " + f["reason"]
	case "ci.running":
		return "running " + f["url"]
	case "ci.passed":
		return "passed " + f["url"]
	case "ci.failed":
		var v struct{ Jobs []string }
		_ = json.Unmarshal(e.Data, &v)
		return strings.TrimSpace("failed: " + strings.Join(v.Jobs, ", ") + " " + f["url"])
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

// refresh reads the run, the issue and the pull request again now.
func (m *Run) refresh() tea.Cmd {
	m.say("Reloading…")
	cmds := []tea.Cmd{m.reload(), m.loadIssue(true)}
	if m.hasChange() {
		cmds = append(cmds, m.loadChange())
	}
	return tea.Batch(cmds...)
}

// drawPicture draws a picture of the description, or of a comment with
// files, at its place in the text; names are looked up among the files and
// the issue's attachments.
func (m *Run) drawPicture(files []Attachment) pictureLines {
	return func(alt, src string, w int) []string {
		return m.pics.lines(cmp.Or(alt, pictureName("", src)), resolve(src, files, m.files), w, pictureRows)
	}
}

// looseFiles are the issue's attachments its description doesn't show.
func (m *Run) looseFiles() []Attachment {
	shown := map[string]bool{}
	for _, src := range pictureRefs(m.issue.Description, m.files) {
		shown[src] = true
	}
	var out []Attachment
	for _, f := range m.files {
		if !shown[f.URL] {
			out = append(out, f)
		}
	}
	return out
}

// issuePictures are the addresses of every picture of the issue and its
// comments, to download.
func (m *Run) issuePictures() []string {
	urls := pictureRefs(m.issue.Description, m.files)
	for _, f := range m.files {
		if isPicture(f) {
			urls = append(urls, f.URL)
		}
	}
	for _, c := range m.comments {
		urls = append(urls, pictureRefs(c.Text, c.Files, m.files)...)
		for _, f := range c.Files {
			if isPicture(f) {
				urls = append(urls, f.URL)
			}
		}
	}
	return urls
}
