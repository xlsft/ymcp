package views

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// The size a screen assumes until the terminal reports its own.
const (
	defaultWidth  = 80
	defaultHeight = 24
)

// flashFor is how long a status message stays.
const flashFor = 8 * time.Second

var screenIDs atomic.Int64

// base is what every screen has: its size, the status line, a pending
// yes/no question and the help toggle. Messages produced by a screen's
// commands carry its id, so a reply that arrives after the user moved on
// is dropped by the screen now on top.
type base struct {
	d             *Deps
	id            int64
	width, height int

	flash    string
	flashErr bool
	flashAt  time.Time

	ask      string
	onYes    func() tea.Cmd
	fullHelp bool
}

func newBase(d *Deps) base {
	return base{d: d, id: screenIDs.Add(1), width: defaultWidth, height: defaultHeight}
}

// flashMsg puts a message on a screen's status line.
type flashMsg struct {
	to   int64
	text string
	err  bool
}

// say shows a status message.
func (b *base) say(text string) {
	b.flash, b.flashErr, b.flashAt = text, false, b.d.Now()
}

// fail shows an error on the status line.
func (b *base) fail(err error) {
	b.flash, b.flashErr, b.flashAt = err.Error(), true, b.d.Now()
}

// report is a command result for this screen's status line: err when it
// failed, else text.
func (b *base) report(text string, err error) tea.Msg {
	if err != nil {
		return flashMsg{to: b.id, text: err.Error(), err: true}
	}
	return flashMsg{to: b.id, text: text}
}

// confirm asks a yes/no question on the status line; yes runs on y.
func (b *base) confirm(question string, yes func() tea.Cmd) {
	b.ask, b.onYes = question, yes
}

// common handles what every screen handles the same way. It reports
// whether msg was used up.
func (b *base) common(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		b.width, b.height = msg.Width, msg.Height
		return false, nil // screens may resize their own parts too
	case flashMsg:
		if msg.to != b.id {
			return true, nil
		}
		b.flash, b.flashErr, b.flashAt = msg.text, msg.err, b.d.Now()
		return true, nil
	case Notice:
		b.flash, b.flashErr, b.flashAt = msg.Text, msg.Err, b.d.Now()
		return true, nil
	case Tick:
		if b.flash != "" && msg.Now.Sub(b.flashAt) > flashFor {
			b.flash = ""
		}
		return false, nil
	case tea.KeyMsg:
		if b.onYes == nil {
			return false, nil
		}
		yes := b.onYes
		b.ask, b.onYes = "", nil
		switch msg.String() {
		case "y", "Y":
			return true, yes()
		}
		b.say("Nothing changed.")
		return true, nil
	}
	return false, nil
}

// binding is one entry of the help line.
type binding struct{ key, desc string }

// frame lays a screen out to exactly its height: the header, the body
// (cut or padded), the status line and the help. Every line is cut to the
// width.
func (b *base) frame(header, body []string, help []binding) string {
	w, h := max(b.width, 1), max(b.height, 1)
	foot := []string{b.statusLine()}
	foot = append(foot, b.helpLines(help, w)...)
	room := h - len(header) - len(foot)
	if room < 0 { // tiny terminal: keep the header and the help line
		header = header[:min(len(header), max(h-1, 0))]
		room = max(h-len(header)-1, 0)
		foot = foot[len(foot)-1:]
		if room+len(header)+len(foot) > h {
			foot = nil
		}
	}
	if len(body) > room {
		body = body[:room]
	}
	lines := make([]string, 0, h)
	lines = append(lines, header...)
	lines = append(lines, body...)
	for len(lines) < h-len(foot) {
		lines = append(lines, "")
	}
	lines = append(lines, foot...)
	for i, l := range lines {
		lines[i] = cut(l, w)
	}
	return strings.Join(lines, "\n")
}

func (b *base) statusLine() string {
	switch {
	case b.onYes != nil:
		return theme.Warn.Render(b.ask) + "  " + theme.Key.Render("y") + " yes  " + theme.Key.Render("n") + " no"
	case b.flash == "":
		return ""
	case b.flashErr:
		return theme.Err.Render("✗ ") + oneLine(b.flash)
	}
	return theme.OK.Render("✓ ") + oneLine(b.flash)
}

// helpLines renders the help: one line cut to the width with "? more"
// when it does not fit, or every entry over several lines after ?.
func (b *base) helpLines(help []binding, w int) []string {
	items := make([]string, len(help))
	for i, h := range help {
		items[i] = theme.Key.Render(h.key) + " " + theme.Dim.Render(h.desc)
	}
	const gap = "  "
	if b.fullHelp {
		var lines []string
		line := ""
		for _, it := range items {
			if line != "" && ansi.StringWidth(line+gap+it) > w {
				lines = append(lines, line)
				line = ""
			}
			if line != "" {
				line += gap
			}
			line += it
		}
		return append(lines, line)
	}
	more := theme.Key.Render("?") + " " + theme.Dim.Render("more")
	line := ""
	for i, it := range items {
		next := it
		if line != "" {
			next = line + gap + it
		}
		rest := i < len(items)-1
		limit := w
		if rest {
			limit = w - ansi.StringWidth(gap+more)
		}
		if ansi.StringWidth(next) > limit {
			if line == "" {
				return []string{cut(it, w)}
			}
			return []string{line + gap + more}
		}
		line = next
	}
	return []string{line}
}

// cut shortens s to w cells, ending with "…" when something was cut.
func cut(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// pad cuts or pads s to exactly w cells.
func pad(s string, w int) string {
	s = cut(s, w)
	if n := w - ansi.StringWidth(s); n > 0 {
		s += strings.Repeat(" ", n)
	}
	return s
}

// spread puts left and right on one line of width w, right-aligned; the
// left part gives way when both do not fit.
func spread(left, right string, w int) string {
	rw := ansi.StringWidth(right)
	if rw >= w {
		return cut(right, w)
	}
	left = cut(left, w-rw-1)
	return left + strings.Repeat(" ", max(w-ansi.StringWidth(left)-rw, 1)) + right
}

// oneLine collapses whitespace, newlines included.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// wrap breaks text into lines of at most w cells, at spaces where it can.
func wrap(s string, w int) []string {
	if w < 1 {
		w = 1
	}
	var out []string
	for _, para := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		para = strings.TrimRight(para, " \t\r")
		if para == "" {
			out = append(out, "")
			continue
		}
		wrapped := ansi.Hardwrap(ansi.Wordwrap(para, w, ""), w, true)
		out = append(out, strings.Split(wrapped, "\n")...)
	}
	return out
}

// elapsed is a short duration: 42s, 4m12s, 3h05m, 2d.
func elapsed(d time.Duration) string {
	d = max(d, 0).Truncate(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// short is a coarse duration for waiting times: 12m, 3h, 2d.
func short(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// title is the red label at the start of every header.
func title(s string) string { return theme.Title.Render(s) }

// marker is the selection marker of lists.
func marker(selected bool) string {
	if selected {
		return theme.Accent.Render("›") + " "
	}
	return "  "
}

// clamp keeps a selection inside [0, n).
func clamp(i, n int) int {
	if n <= 0 {
		return 0
	}
	return min(max(i, 0), n-1)
}

// scrollTo returns the first visible line so that line sel stays inside a
// window of rows lines starting at top.
func scrollTo(top, sel, rows int) int {
	if rows <= 0 {
		return sel
	}
	if sel < top {
		return sel
	}
	if sel >= top+rows {
		return sel - rows + 1
	}
	return top
}
