package views

import (
	"context"
	"fmt"
	"maps"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// Inbox lists every run of every pipeline that waits for input, oldest
// question first, and answers them in place.
type Inbox struct {
	base
	loaded   bool
	loading  bool
	runs     []store.Run
	info     map[int64]runInfo
	runners  map[string]runnerInfo
	profiles []config.Config
	sel      int
	answer   answerBox
}

type inboxLoaded struct {
	to       int64
	runs     []store.Run
	info     map[int64]runInfo
	runners  map[string]runnerInfo
	profiles []config.Config
	err      error
}

// NewInbox makes the inbox.
func NewInbox(d *Deps) *Inbox {
	return &Inbox{base: newBase(d), info: map[int64]runInfo{}, runners: map[string]runnerInfo{}, answer: newAnswerBox()}
}

// Init loads the inbox.
func (m *Inbox) Init() tea.Cmd { return m.reload() }

// Answering reports whether the answer box is open.
func (m *Inbox) Answering() bool { return m.answer.open }

func (m *Inbox) reload() tea.Cmd {
	if m.loading {
		return nil
	}
	m.loading = true
	info := maps.Clone(m.info)
	return func() tea.Msg { return m.load(info) }
}

func (m *Inbox) load(info map[int64]runInfo) tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	msg := inboxLoaded{to: m.id, runners: map[string]runnerInfo{}}
	runs, err := m.d.Store.Runs(ctx, "", true)
	if err != nil {
		msg.err = err
		return msg
	}
	keep := map[int64]runInfo{}
	for _, r := range runs {
		if r.Status != store.StatusNeedsInput {
			continue
		}
		in := info[r.ID]
		if _, err := m.d.follow(ctx, r, &in); err != nil {
			msg.err = err
			return msg
		}
		keep[r.ID] = in
		msg.runs = append(msg.runs, r)
		if _, ok := msg.runners[r.Pipeline]; !ok {
			if msg.runners[r.Pipeline], err = m.d.runner(ctx, r.Pipeline); err != nil {
				msg.err = err
				return msg
			}
		}
	}
	// The oldest question first: it has waited longest.
	sortWaiting(msg.runs)
	msg.info = keep
	msg.profiles, _ = m.d.Profiles()
	return msg
}

func sortWaiting(runs []store.Run) {
	for i := 1; i < len(runs); i++ {
		for j := i; j > 0 && waitingSince(runs[j]).Before(waitingSince(runs[j-1])); j-- {
			runs[j], runs[j-1] = runs[j-1], runs[j]
		}
	}
}

func (m *Inbox) selected() (store.Run, bool) {
	if len(m.runs) == 0 {
		return store.Run{}, false
	}
	return m.runs[m.sel], true
}

// Update handles keys, ticks and reloads.
func (m *Inbox) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.answer.resize(size.Width - 4)
	}
	if done, cmd := m.common(msg); done {
		return m, cmd
	}
	switch msg := msg.(type) {
	case inboxLoaded:
		if msg.to != m.id {
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.fail(msg.err)
			return m, nil
		}
		m.loaded = true
		keep := int64(0)
		if r, ok := m.selected(); ok {
			keep = r.ID
		}
		m.runs, m.info, m.runners, m.profiles = msg.runs, msg.info, msg.runners, msg.profiles
		found := false
		for i, r := range m.runs {
			if r.ID == keep {
				m.sel, found = i, true
			}
		}
		if !found && m.answer.open { // the run got its answer elsewhere
			m.answer.close()
		}
		m.sel = clamp(m.sel, len(m.runs))
	case Tick:
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

func (m *Inbox) send(text string) tea.Cmd {
	r, ok := m.selected()
	if !ok {
		m.answer.close()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	if err := m.d.Answer(ctx, m.d.Store, r.Pipeline, r.Number, text); err != nil {
		m.fail(err)
		return nil
	}
	m.answer.close()
	m.say(answerSent(r.Number, m.runners[r.Pipeline]))
	return m.reload()
}

func (m *Inbox) key(k string) tea.Cmd {
	switch k {
	case "q":
		return tea.Quit
	case "esc", "backspace", "left", "h":
		return navigate(Back{})
	case "?":
		m.fullHelp = !m.fullHelp
	case "up", "k":
		m.sel = clamp(m.sel-1, len(m.runs))
	case "down", "j":
		m.sel = clamp(m.sel+1, len(m.runs))
	}
	r, ok := m.selected()
	if !ok {
		return nil
	}
	switch k {
	case "enter", "a":
		return m.answer.start(m.width - 4)
	case "v", "right", "l":
		return navigate(OpenRun{Pipeline: r.Pipeline, Number: r.Number})
	case "o":
		return openURL(&m.base, issueURL(m.profiles, r.Profile, r.IssueKey), r.IssueKey)
	}
	return nil
}

// View renders the inbox.
func (m *Inbox) View() string {
	w := m.width
	right := ""
	switch n := len(m.runs); {
	case !m.loaded:
	case n == 0:
		right = theme.OK.Render("all answered")
	case n == 1:
		right = theme.Badge.Render("⚑ 1 run needs input")
	default:
		right = theme.Badge.Render(fmt.Sprintf("⚑ %d runs need input", n))
	}
	header := []string{spread(title("Inbox"), right, w), ""}
	help := []binding{{"↑↓", "move"}, {"enter", "answer"}, {"v", "view run"}, {"o", "issue"}, {"esc", "back"}, {"q", "quit"}}
	if m.answer.open {
		help = answerHelp
	}
	var body []string
	switch {
	case !m.loaded:
		body = []string{theme.Dim.Render("  Loading…")}
	case len(m.runs) == 0:
		body = []string{"  Nothing waits for you. Runs that ask a question show up here."}
	default:
		body = m.list(w, m.height-len(header)-1-len(m.helpLines(help, max(w, 1))))
	}
	return m.frame(header, body, help)
}

// list renders the questions; the selected one shows in full with the
// answer box under it, the others in one line each.
func (m *Inbox) list(w, rows int) []string {
	var lines []string
	selStart, selEnd := 0, 0
	for i, r := range m.runs {
		sel := i == m.sel
		in := m.info[r.ID]
		if sel {
			selStart = len(lines)
		}
		who := fmt.Sprintf("%s #%d", r.Pipeline, r.Number)
		if sel {
			who = theme.Accent.Render(who)
		} else {
			who = theme.Bold.Render(who)
		}
		left := marker(sel) + who + "  " + theme.Bold.Render(r.IssueKey) + " " + oneLine(in.summary)
		state := theme.Dim.Render("waiting " + short(m.d.Now().Sub(waitingSince(r))))
		if in.pending > 0 {
			state = theme.OK.Render("✓ answer queued")
		}
		lines = append(lines, spread(left, state, w))
		reason := strings.TrimSpace(r.NeedsInputReason)
		if reason == "" {
			reason = "The agent waits for an answer."
		}
		text := wrap(reason, max(w-4, 10))
		if !sel {
			text = []string{cut(oneLine(reason), max(w-4, 10))}
		} else if limit := max(rows/2, 3); len(text) > limit {
			text = append(text[:limit-1], "…")
		}
		for _, l := range text {
			lines = append(lines, "    "+theme.Dim.Render(l))
		}
		if sel && m.runners[r.Pipeline].state != runnerRunning {
			lines = append(lines, "    "+theme.Warn.Render("! the runner of "+r.Pipeline+" is stopped; the answer waits until it starts"))
		}
		if sel && m.answer.open {
			for _, l := range m.answer.view() {
				lines = append(lines, "  "+l)
			}
		}
		if sel {
			selEnd = len(lines)
		}
		lines = append(lines, "")
	}
	if rows <= 0 || len(lines) <= rows {
		return lines
	}
	top := 0
	if selEnd > rows {
		top = min(selStart, selEnd-rows)
	}
	return lines[top:]
}
