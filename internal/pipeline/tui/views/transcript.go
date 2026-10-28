package views

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// transcript is the detailed log of a run over its card: what the agent
// thought, said and looked at, turn by turn, followed as it goes.
type transcript struct {
	open    bool
	loading bool
	events  []store.Event
	lastID  int64 // the last event kept, so live ones are not added twice
	scroll  int   // lines up from the bottom; 0 follows the end
	rows    int   // body rows at the last draw
	lines   []string
	width   int
	dirty   bool
}

// transcriptLoaded is the whole timeline of a run, read when the
// transcript opens.
type transcriptLoaded struct {
	to     int64
	events []store.Event
	err    error
}

// transcriptKeep is how many events the transcript keeps.
const transcriptKeep = 20000

// transcriptMore is how many lines a tool's input may take.
const transcriptMore = 6

// openTranscript opens the transcript and reads the run's timeline.
func (m *Run) openTranscript() tea.Cmd {
	m.tr = transcript{open: true, loading: true, events: m.tr.events[:0], dirty: true}
	runID := m.run.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		events, err := m.d.Store.Events(ctx, runID, 0, 0)
		return transcriptLoaded{to: m.id, events: events, err: err}
	}
}

// transcriptRead keeps the timeline read on opening, with the live events
// that came meanwhile.
func (m *Run) transcriptRead(msg transcriptLoaded) {
	if !m.tr.open {
		return
	}
	m.tr.loading = false
	if msg.err != nil {
		m.fail(msg.err)
		return
	}
	var last int64
	if n := len(msg.events); n > 0 {
		last = msg.events[n-1].ID
	}
	events := msg.events
	for _, e := range m.tr.events {
		if e.ID > last {
			events = append(events, e)
		}
	}
	m.tr.events = nil
	m.tr.lastID = 0
	m.transcriptAdd(events)
}

// transcriptAdd appends new events of the run.
func (m *Run) transcriptAdd(events []store.Event) {
	for _, e := range events {
		if e.ID <= m.tr.lastID {
			continue
		}
		m.tr.events = append(m.tr.events, e)
		m.tr.lastID = e.ID
		m.tr.dirty = true
	}
	if len(m.tr.events) > transcriptKeep {
		m.tr.events = m.tr.events[len(m.tr.events)-transcriptKeep:]
	}
}

// transcriptKey handles a key while the transcript is open.
func (m *Run) transcriptKey(k string) tea.Cmd {
	page := max(m.tr.rows-1, 1)
	switch k {
	case "q":
		return tea.Quit
	case "esc", "backspace", "t":
		m.tr.open = false
	case "r":
		return m.reload()
	case "up", "k":
		m.tr.scroll++
	case "down", "j":
		m.tr.scroll = max(m.tr.scroll-1, 0)
	case "pgup", "b":
		m.tr.scroll += page
	case "pgdown", " ", "f":
		m.tr.scroll = max(m.tr.scroll-page, 0)
	case "home", "g":
		m.tr.scroll = len(m.tr.lines)
	case "end", "G":
		m.tr.scroll = 0
	}
	return nil
}

// transcriptRect is where the transcript sits over a card at x, y, bw×bh:
// inside it, down to its bottom edge, or over all of it when it is small.
func transcriptRect(x, y, bw, bh int) (int, int, int, int) {
	if bw < 40 || bh < 12 {
		return x, y, bw, bh
	}
	return x + 3, y + 1, bw - 6, bh - 1
}

// transcriptBox is the transcript, tw×th with its frame.
func (m *Run) transcriptBox(tw, th int) []string {
	iw := max(tw-2, 1)
	px, _ := modalPad(tw, th)
	cw := max(iw-2*px, 1)
	foot := m.helpLines([]binding{{"↑↓", "scroll"}, {"pgup/pgdn", "page"}, {"g/G", "top/follow"}, {"r", "reload"}, {"esc", "close"}}, cw)
	if s := m.statusLine(); s != "" {
		foot = append([]string{cut(s, cw)}, foot...)
	}
	rows := max(th-2-len(foot)-1, 1)
	var body []string
	switch {
	case m.tr.loading && len(m.tr.events) == 0:
		body = []string{theme.Dim.Render("Loading…")}
	default:
		if m.tr.dirty || m.tr.width != cw {
			m.tr.lines = transcriptLines(m.tr.events, cw)
			m.tr.width, m.tr.dirty = cw, false
		}
		if len(m.tr.lines) == 0 {
			body = []string{theme.Dim.Render("Nothing from the agent yet.")}
		} else {
			body = scrollWindow(m.tr.lines, rows, &m.tr.scroll)
		}
	}
	m.tr.rows = rows
	inner := make([]string, 0, th-2)
	inner = append(inner, body...)
	for len(inner) < th-2-len(foot) {
		inner = append(inner, "")
	}
	inner = append(inner, foot...)
	if len(inner) > th-2 {
		inner = inner[len(inner)-(th-2):]
	}
	side := strings.Repeat(" ", px)
	for i, l := range inner {
		inner[i] = side + pad(l, cw) + side
	}
	left := theme.Accent.Render("Transcript") + theme.Dim.Render(fmt.Sprintf(" · run #%d · %s", m.run.Number, m.run.IssueKey))
	right := theme.Dim.Render(statusLabel(m.run.Status))
	if m.info.inTurn {
		right = theme.OK.Render("● live")
	}
	return frameBox(inner, iw, left, right)
}

// transcriptLines draws a run's events w cells wide: the agent's thoughts,
// its answers and the tools it ran with what they looked at, in turns; the
// replies it got; and the rest of the timeline as one line each.
func transcriptLines(events []store.Event, w int) []string {
	var out []string
	tools := map[string]int{} // a tool call's id → its line
	var textKind, text string
	var textAt string
	flush := func() {
		if textKind == "" {
			return
		}
		label, style := theme.Accent.Render("says"), func(s string) string { return s }
		if textKind == "agent.thought" {
			label, style = theme.Dim.Render("thinks"), func(s string) string { return theme.Dim.Render(s) }
		}
		out = append(out, theme.Dim.Render(textAt)+" "+label)
		for _, l := range wrap(strings.TrimSpace(text), max(w-2, 1)) {
			out = append(out, "  "+style(l))
		}
		textKind, text = "", ""
	}
	for _, e := range events {
		stamp := e.At.Local().Format("15:04:05")
		if e.Kind == "agent.text" || e.Kind == "agent.thought" {
			if e.Kind != textKind {
				flush()
				textKind, textAt = e.Kind, stamp
			}
			text += fields(e.Data)["text"]
			continue
		}
		flush()
		switch e.Kind {
		case "agent.usage", "agent.session":
		case "turn.start":
			if len(out) > 0 {
				out = append(out, "")
			}
			out = append(out, turnRule(theme.Dim.Render(stamp+" · "+fields(e.Data)["kind"]+" turn"), w))
		case "turn.end":
			out = append(out, theme.Dim.Render(stamp+" turn ended: "+fields(e.Data)["reason"]))
		case "agent.tool_call":
			call := toolOf(e.Data)
			head := theme.Dim.Render(stamp) + " " + theme.Bold.Render("▸ "+cmp.Or(call.Title, "tool"))
			if call.ID != "" {
				tools[call.ID] = len(out)
			}
			out = append(out, cut(head, w))
			detail := toolDetail(call.Input)
			if detail == "" {
				continue
			}
			lines := wrap(detail, max(w-4, 1))
			if len(lines) > transcriptMore {
				lines = append(lines[:transcriptMore-1], "…")
			}
			for _, l := range lines {
				out = append(out, "    "+theme.Dim.Render(l))
			}
		case "agent.tool_update":
			call := toolOf(e.Data)
			mark := ""
			switch call.Status {
			case "completed":
				mark = theme.OK.Render(" ✓")
			case "failed":
				mark = theme.Err.Render(" ✗ failed")
			case "denied":
				mark = theme.Warn.Render(" ⊘ denied")
			}
			if i, ok := tools[call.ID]; ok && mark != "" {
				out[i] = cut(out[i]+mark, w)
				delete(tools, call.ID)
			} else if call.Status == "failed" || call.Status == "denied" {
				out = append(out, cut(theme.Dim.Render(stamp)+" "+theme.Bold.Render("▸ "+cmp.Or(call.Title, "tool"))+mark, w))
			}
		case "inbox":
			f := fields(e.Data)
			out = append(out, theme.Dim.Render(stamp)+" "+theme.OK.Render("← "+cmp.Or(f["author"], "reply")))
			for _, l := range wrap(strings.TrimSpace(f["text"]), max(w-2, 1)) {
				out = append(out, "  "+l)
			}
		default:
			kind, ok := feedKinds[e.Kind]
			if !ok {
				kind.label, kind.style = e.Kind, theme.Dim.Render
			}
			out = append(out, cut(theme.Dim.Render(stamp)+" "+kind.style(kind.label)+" "+oneLine(feedText(e)), w))
		}
	}
	flush()
	return out
}

// turnRule is s on a dim line w cells wide.
func turnRule(s string, w int) string {
	head := theme.Dim.Render("── ") + s + " "
	return cut(head+theme.Dim.Render(strings.Repeat("─", max(w-ansi.StringWidth(head), 0))), w)
}

// transcriptTool is a tool call of an event.
type transcriptTool struct {
	ID, Title, Status string
	Input             json.RawMessage
}

func toolOf(data json.RawMessage) transcriptTool {
	var v struct {
		Tool transcriptTool `json:"tool"`
	}
	_ = json.Unmarshal(data, &v)
	v.Tool.Title = oneLine(v.Tool.Title)
	return v.Tool
}

// toolDetail is what a tool's input says it looks at or does: the
// command, the file, the pattern and where, the address or the query, or
// the input itself.
func toolDetail(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return string(raw)
	}
	str := func(k string) string {
		s, _ := in[k].(string)
		return strings.TrimSpace(s)
	}
	if p := str("pattern"); p != "" {
		if where := cmp.Or(str("path"), str("glob")); where != "" {
			return p + "  in " + where
		}
		return p
	}
	for _, k := range []string{"command", "file_path", "notebook_path", "path", "url", "query", "prompt", "description", "skill"} {
		if s := str(k); s != "" {
			return s
		}
	}
	b, _ := json.Marshal(in)
	return string(b)
}
