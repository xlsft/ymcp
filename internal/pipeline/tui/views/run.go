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

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// feedKeep is how many of a run's latest events the card keeps.
const feedKeep = 2000

// Run is the card of one run: what it is, the live event feed, and the
// answer box for a run that waits for input.
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

	feed      []string // rendered events, rebuilt on new events or a resize
	feedWidth int
	scroll    int // lines up from the bottom; 0 follows the feed
	frameN    int

	answer     answerBox
	wantAnswer bool
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

// NewRun makes the card of a pipeline's run; answer opens the answer box
// at once.
func NewRun(d *Deps, pipeline string, number int, answer bool) *Run {
	home, _ := os.UserHomeDir()
	return &Run{base: newBase(d), pipeline: pipeline, number: number, home: home, answer: newAnswerBox(), wantAnswer: answer}
}

// Init loads the run.
func (m *Run) Init() tea.Cmd { return m.reload() }

// Answering reports whether the answer box is open, so the root app keeps
// its own keys out of it.
func (m *Run) Answering() bool { return m.answer.open }

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

// Update handles keys, ticks and reloads.
func (m *Run) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.answer.resize(size.Width - 2)
	}
	if done, cmd := m.common(msg); done {
		return m, cmd
	}
	switch msg := msg.(type) {
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
			before := len(m.feed)
			m.events = append(m.events, msg.events...)
			if len(m.events) > feedKeep {
				m.events = m.events[len(m.events)-feedKeep:]
			}
			m.rebuildFeed()
			if m.scroll > 0 { // keep reading where the user was
				m.scroll += len(m.feed) - before
			}
		}
		if first && m.wantAnswer {
			m.wantAnswer = false
			return m, m.startAnswer()
		}
	case Tick:
		m.frameN++
		return m, m.reload()
	case tea.KeyMsg:
		if m.answer.open {
			send, text, cmd := m.answer.key(msg)
			if send {
				return m, m.send(text)
			}
			return m, cmd
		}
		return m, m.key(msg.String())
	default:
		return m, m.answer.update(msg)
	}
	return m, nil
}

func (m *Run) startAnswer() tea.Cmd {
	if m.run.Status.Final() {
		m.fail(fmt.Errorf("run #%d has ended (%s)", m.number, statusLabel(m.run.Status)))
		return nil
	}
	return m.answer.start(m.width - 2)
}

// send gives the answer to the runner through the store.
func (m *Run) send(text string) tea.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	if err := m.d.Answer(ctx, m.d.Store, m.pipeline, m.number, text); err != nil {
		m.fail(err)
		return nil
	}
	m.answer.close()
	m.say(answerSent(m.number, m.runner))
	return m.reload()
}

// answerSent tells where an answer went.
func answerSent(number int, r runnerInfo) string {
	if r.state == runnerRunning {
		return fmt.Sprintf("Answer sent to #%d; the runner passes it on at its next poll.", number)
	}
	return fmt.Sprintf("Answer saved for #%d; the runner is stopped and passes it on when it starts.", number)
}

func (m *Run) key(k string) tea.Cmd {
	page := max(m.feedRows()-1, 1)
	switch k {
	case "q":
		return tea.Quit
	case "esc", "backspace", "left", "h":
		return navigate(Back{})
	case "?":
		m.fullHelp = !m.fullHelp
	case "up", "k":
		m.scroll++
	case "down", "j":
		m.scroll--
	case "pgup", "b":
		m.scroll += page
	case "pgdown", " ", "f":
		m.scroll -= page
	case "home", "g":
		m.scroll = len(m.feed)
	case "end", "G":
		m.scroll = 0
	case "a", "enter":
		if !m.loaded {
			return nil
		}
		return m.startAnswer()
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
		return openURL(&m.base, issueURL(m.profiles, m.run.Profile, m.run.IssueKey), m.run.IssueKey)
	case "p":
		if m.run.PRURL == "" {
			m.fail(fmt.Errorf("run #%d has no pull request", m.number))
			return nil
		}
		return openURL(&m.base, m.run.PRURL, "the pull request")
	case "i":
		return navigate(OpenInbox{})
	}
	m.scroll = clamp(m.scroll, len(m.feed)+1)
	return nil
}

func (m *Run) rebuildFeed() {
	w := max(m.width, 20)
	m.feedWidth = w
	m.feed = m.feed[:0]
	for _, e := range m.events {
		m.feed = append(m.feed, feedLines(e, w)...)
	}
}

// View renders the card.
func (m *Run) View() string {
	w := m.width
	if m.loaded && m.feedWidth != max(w, 20) {
		m.rebuildFeed()
	}
	help := []binding{
		{"a", "answer"}, {"c", "cancel"}, {"o", "issue"}, {"p", "PR"}, {"↑↓", "scroll"},
		{"end", "follow"}, {"i", "inbox"}, {"esc", "back"}, {"q", "quit"},
	}
	if m.answer.open {
		help = answerHelp
	}
	head := title(fmt.Sprintf("%s #%d", m.pipeline, m.number))
	if !m.loaded {
		return m.frame([]string{head, ""}, []string{theme.Dim.Render("  Loading…")}, help)
	}
	r := m.run
	header := []string{
		spread(head+" "+theme.Bold.Render(r.IssueKey)+" "+oneLine(m.info.summary), statusStyle(r.Status), w),
		theme.Dim.Render(m.facts()),
		m.agentLine(),
		theme.Dim.Render(strings.Repeat("─", max(w, 1))),
	}
	bottom := m.bottom(w)
	rows := m.height - len(header) - len(bottom) - 1 - len(m.helpLines(help, max(w, 1)))
	body := m.feedWindow(max(rows, 0))
	body = append(body, bottom...)
	return m.frame(header, body, help)
}

// facts is the second header line: stage, branch, worktree, PR.
func (m *Run) facts() string {
	r := m.run
	var parts []string
	if r.Stage != "" {
		parts = append(parts, "stage "+r.Stage)
	}
	if r.Branch != "" {
		parts = append(parts, "branch "+r.Branch)
	}
	if r.PRURL != "" {
		parts = append(parts, fmt.Sprintf("PR #%d %s", r.PRNumber, r.PRURL))
	}
	parts = append(parts, "started "+short(m.d.Now().Sub(r.CreatedAt))+" ago")
	if r.Worktree != "" {
		parts = append(parts, homeShort(r.Worktree, m.home))
	}
	return strings.Join(parts, " · ")
}

// agentLine says what the agent is doing.
func (m *Run) agentLine() string {
	r, in := m.run, m.info
	who := r.Agent
	if who == "" {
		who = "agent"
	}
	switch {
	case r.Status.Final():
		return theme.Dim.Render(fmt.Sprintf("%s · finished %s ago", who, short(m.d.Now().Sub(*finishedAt(r)))))
	case in.inTurn:
		what := in.activity
		if what == "" {
			what = "working"
		}
		return theme.Accent.Render(spinner[m.frameN%len(spinner)]) + " " + who + " · " + what +
			theme.Dim.Render(" · "+elapsed(m.d.Now().Sub(in.turnStart)))
	case m.runner.state != runnerRunning:
		return theme.Dim.Render(who + " · idle · the runner is stopped")
	case in.pending > 0:
		return theme.Warn.Render(fmt.Sprintf("⟳ %d new message(s) for the agent", in.pending))
	}
	return theme.Dim.Render(who + " · idle")
}

func finishedAt(r store.Run) *time.Time {
	if r.FinishedAt != nil {
		return r.FinishedAt
	}
	return &r.UpdatedAt
}

// bottom is what sits under the feed: the question of a run that waits
// for input and the answer box.
func (m *Run) bottom(w int) []string {
	var out []string
	r := m.run
	if r.Status == store.StatusNeedsInput {
		out = append(out, theme.Dim.Render(strings.Repeat("─", max(w, 1))))
		head := theme.Badge.Render("⚑ NEEDS INPUT") + theme.Dim.Render(" · waiting "+short(m.d.Now().Sub(waitingSince(r))))
		if m.info.pending > 0 {
			head += theme.OK.Render("  ✓ answer queued")
		}
		out = append(out, head)
		reason := strings.TrimSpace(r.NeedsInputReason)
		if reason == "" {
			reason = "The agent waits for an answer."
		}
		lines := wrap(reason, max(w-2, 10))
		if limit := max(m.height/4, 2); len(lines) > limit {
			lines = append(lines[:limit-1], "…")
		}
		for _, l := range lines {
			out = append(out, "  "+l)
		}
		if !m.answer.open {
			out = append(out, theme.Dim.Render("  Press a to answer."))
		}
	}
	if m.answer.open {
		if r.Status != store.StatusNeedsInput {
			out = append(out, theme.Dim.Render(strings.Repeat("─", max(w, 1))),
				theme.Dim.Render("Feedback for the agent; it starts a new turn with it."))
		}
		out = append(out, m.answer.view()...)
	}
	return out
}

func (m *Run) feedRows() int {
	return max(m.height-8, 1)
}

// feedWindow is the part of the feed that fits in rows lines.
func (m *Run) feedWindow(rows int) []string {
	if rows <= 0 {
		return nil
	}
	if len(m.feed) == 0 {
		return []string{theme.Dim.Render("No events yet.")}
	}
	m.scroll = clamp(m.scroll, max(len(m.feed)-rows+2, 1))
	if m.scroll == 0 {
		return append([]string(nil), m.feed[max(len(m.feed)-rows, 0):]...)
	}
	end := len(m.feed) - m.scroll
	out := append([]string(nil), m.feed[max(end-(rows-1), 0):end]...)
	return append(out, theme.Dim.Render(fmt.Sprintf("↓ %d more lines · end to follow", m.scroll)))
}

// feedKinds are the labels and colours of the event kinds in the feed.
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

// feedHidden are event kinds the feed leaves out: noise for people.
var feedHidden = map[string]bool{"agent.usage": true, "agent.session": true, "agent.tool_update": true}

const feedLabel = 11

// feedLines renders one event as "15:04:05  label  text", the text
// wrapped under itself and cut to a few lines.
func feedLines(e store.Event, w int) []string {
	if feedHidden[e.Kind] && !failedTool(e) {
		return nil
	}
	kind, ok := feedKinds[e.Kind]
	if !ok {
		kind.label, kind.style = e.Kind, theme.Dim.Render
		if e.Kind == "agent.tool_update" {
			kind.label, kind.style = "tool", theme.Err.Render
		}
	}
	text := feedText(e)
	limit := 3
	if e.Kind == "agent.text" || e.Kind == "agent.request_input" || e.Kind == "agent.report" || e.Kind == "inbox" {
		limit = 8
	}
	prefix := theme.Dim.Render(e.At.Local().Format("15:04:05")) + " " + kind.style(pad(kind.label, feedLabel)) + " "
	indent := strings.Repeat(" ", 9+feedLabel+1)
	tw := w - len(indent)
	if tw < 16 { // too narrow for a side-by-side layout: text under the label
		prefix, indent, tw = kind.style(kind.label)+" "+theme.Dim.Render(e.At.Local().Format("15:04")), "  ", w-2
		lines := []string{prefix}
		for _, l := range limitLines(wrap(text, max(tw, 1)), limit) {
			lines = append(lines, indent+l)
		}
		return lines
	}
	body := limitLines(wrap(text, tw), limit)
	if len(body) == 0 {
		body = []string{""}
	}
	lines := []string{prefix + body[0]}
	for _, l := range body[1:] {
		lines = append(lines, indent+l)
	}
	return lines
}

func limitLines(lines []string, n int) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = append(lines[:n-1], theme.Dim.Render("…"))
	}
	return lines
}

func failedTool(e store.Event) bool {
	if e.Kind != "agent.tool_update" {
		return false
	}
	var v struct {
		Tool struct{ Status string } `json:"tool"`
	}
	return json.Unmarshal(e.Data, &v) == nil && v.Tool.Status == "failed"
}

// feedText is the text of an event for the feed.
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
