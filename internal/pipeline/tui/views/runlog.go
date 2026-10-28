package views

import (
	"fmt"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// logEntry is one line of a run card's log panel: an event, or several
// alike ones in a row.
type logEntry struct {
	at   time.Time
	kind string
	text string
	raw  string // the text as it came, for the next piece of a streamed one
	n    int
}

// logNoisy are event kinds that repeat without news: a row of them is one
// entry whatever their texts.
var logNoisy = map[string]bool{
	"stage.command":    true,
	"run.needs_input":  true,
	"worktree":         true,
	"git.push_skipped": true,
	"feedback.skipped": true,
}

// logStreamed are event kinds recorded in pieces while the agent streams.
var logStreamed = map[string]bool{"agent.text": true, "agent.thought": true}

// logEntries turns events into log entries: hidden kinds left out, a row
// of the same noisy kind, or of the same kind and text, folded into one.
func logEntries(events []store.Event) []logEntry {
	var out []logEntry
	for _, e := range events {
		if feedHidden[e.Kind] && !failedTool(e) {
			continue
		}
		raw := feedText(e)
		if logStreamed[e.Kind] {
			raw = fields(e.Data)["text"] // untrimmed: a piece may end in a space
		}
		t := oneLine(raw)
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.kind == e.Kind && logStreamed[e.Kind] {
				// The pieces of one answer or thought, recorded as it
				// streamed: one entry.
				last.raw += raw
				last.at, last.text = e.At, oneLine(last.raw)
				continue
			}
			if last.kind == e.Kind && (logNoisy[e.Kind] || last.text == t) {
				last.n++
				last.at, last.text = e.At, t
				continue
			}
		}
		out = append(out, logEntry{at: e.At, kind: e.Kind, text: t, raw: raw, n: 1})
	}
	return out
}

// line renders an entry as "15:04 label text" cut to w.
func (e logEntry) line(w int) string {
	kind, ok := feedKinds[e.kind]
	if !ok {
		kind.label, kind.style = e.kind, theme.Dim.Render
		if e.kind == "agent.tool_update" {
			kind.label, kind.style = "tool", theme.Err.Render
		}
	}
	stamp := e.at.Local().Format("15:04")
	if w >= 56 {
		stamp = e.at.Local().Format("15:04:05")
	}
	lw := 10
	if w < 36 {
		lw = 8
	}
	text := e.text
	if e.n > 1 {
		text = theme.Dim.Render(fmt.Sprintf("×%d ", e.n)) + text
	}
	gap := " "
	if w >= 36 {
		gap = "  "
	}
	return cut(theme.Dim.Render(stamp)+gap+kind.style(pad(kind.label, lw))+" "+text, w)
}
