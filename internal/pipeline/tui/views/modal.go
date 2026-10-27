package views

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// Styles of the modal.
var (
	modalBorder = lipgloss.NewStyle().Foreground(theme.Brand)
	chipStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Background(lipgloss.Color("237")).Padding(0, 1)
	tabActive   = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(theme.Brand)
	// underStyle is the screen under a modal: faint and grey, its own
	// colours and bold stripped, so it recedes behind the box.
	underStyle = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.AdaptiveColor{Light: "250", Dark: "240"})
)

// modalRect is where a modal sits on a w×h screen: a margin around it
// that shrinks with the screen, none on tiny screens.
func modalRect(w, h int) (x, y, bw, bh int) {
	if w < 50 || h < 14 {
		return 0, 0, w, h
	}
	mx := min(max(w/24, 1), 6)
	my := min(max(h/12, 1), 3)
	return mx, my, w - 2*mx, h - 2*my
}

// modalOver draws box (lines of equal width) at x, y over under, which is
// dimmed whole: its escape sequences (colours, bold, links) are stripped
// and every line redrawn faint and grey; the box keeps its own. The
// result is exactly w×h.
func modalOver(under string, box []string, x, y, w, h int) string {
	bg := strings.Split(under, "\n")
	bw := 0
	if len(box) > 0 {
		bw = ansi.StringWidth(box[0])
	}
	out := make([]string, h)
	for i := range h {
		line := ""
		if i < len(bg) {
			line = ansi.Strip(bg[i])
		}
		line = pad(line, w)
		j := i - y
		if j >= len(box) {
			// Below the card, where the screen's own key help is: blank,
			// so its keys aren't mistaken for the card's.
			out[i] = strings.Repeat(" ", w)
			continue
		}
		if j < 0 {
			out[i] = dimLine(line)
			continue
		}
		left := ansi.Truncate(line, x, "")
		right := ansi.TruncateLeft(line, x+bw, "")
		out[i] = cut(dimLine(pad(left, x))+pad(box[j], bw)+dimLine(pad(right, max(w-x-bw, 0))), w)
	}
	return strings.Join(out, "\n")
}

// dimLine draws plain text (no escape sequences) as the screen under a
// modal.
func dimLine(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	return underStyle.Render(s)
}

// frameBox draws a rounded border around inner lines (cut or padded to
// iw), with left and right in the top edge.
func frameBox(inner []string, iw int, left, right string) []string {
	b := lipgloss.RoundedBorder()
	edge := func(s string) string { return modalBorder.Render(s) }
	if left == "" && right == "" {
		out := []string{edge(b.TopLeft + strings.Repeat(b.Top, max(iw, 0)) + b.TopRight)}
		for _, l := range inner {
			out = append(out, edge(b.Left)+pad(l, iw)+edge(b.Right))
		}
		return append(out, edge(b.BottomLeft+strings.Repeat(b.Bottom, max(iw, 0))+b.BottomRight))
	}
	top := b.TopLeft + b.Top + " " + left + " "
	rest := iw + 2 - ansi.StringWidth(top) - ansi.StringWidth(right) - 4
	var head string
	if rest >= 1 {
		head = edge(b.TopLeft+b.Top+" ") + left + edge(" "+strings.Repeat(b.Top, rest)+" ") + right + edge(" "+b.Top+b.TopRight)
	} else {
		mid := cut(edge(b.Top+" ")+left+" ", iw)
		head = edge(b.TopLeft) + mid + edge(strings.Repeat(b.Top, max(iw-ansi.StringWidth(mid), 0))+b.TopRight)
	}
	out := []string{head}
	for _, l := range inner {
		out = append(out, edge(b.Left)+pad(l, iw)+edge(b.Right))
	}
	return append(out, edge(b.BottomLeft+strings.Repeat(b.Bottom, max(iw, 0))+b.BottomRight))
}

// window is the part of lines that fits in rows, scroll lines up from the
// bottom (0 follows the end); scroll is clamped. When scrolled up, the last
// row says how much is below.
func scrollWindow(lines []string, rows int, scroll *int) []string {
	if rows <= 0 {
		return nil
	}
	if len(lines) <= rows {
		*scroll = 0
		return append([]string(nil), lines...)
	}
	*scroll = clamp(*scroll, len(lines)-rows+2)
	if *scroll == 0 {
		return append([]string(nil), lines[len(lines)-rows:]...)
	}
	end := len(lines) - *scroll
	out := append([]string(nil), lines[max(end-(rows-1), 0):end]...)
	return append(out, theme.Dim.Render(fmt.Sprintf("↓ %d more · G to follow", *scroll)))
}

// replyPlaceholder is what the empty comment box says.
const replyPlaceholder = "Write a comment…"

// replyBox is the comment box of the run card, drawn like the input of a
// chat: enter sends, alt+enter starts a new line, esc leaves it with the
// draft kept.
type replyBox struct {
	ta      textarea.Model
	focused bool
}

func newReplyBox() replyBox {
	ta := textarea.New()
	ta.Placeholder = replyPlaceholder
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	ta.CharLimit = 16000
	ta.SetHeight(2)
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	ta.FocusedStyle.CursorLine = ta.FocusedStyle.CursorLine.UnsetBackground()
	return replyBox{ta: ta}
}

func (r *replyBox) focus(width int) tea.Cmd {
	r.focused = true
	r.resize(width)
	return r.ta.Focus()
}

// resize fits the text to a box w cells wide, its frame included.
func (r *replyBox) resize(w int) { r.ta.SetWidth(max(w-4, 6)) }

func (r *replyBox) blur() {
	r.focused = false
	r.ta.Blur()
}

// draft is the text typed so far.
func (r *replyBox) draft() string { return strings.TrimSpace(r.ta.Value()) }

func (r *replyBox) set(text string) { r.ta.SetValue(text) }

func (r *replyBox) clear() { r.ta.Reset() }

// key handles a key while the box has the focus. send is true when the
// user pressed enter with some text.
func (r *replyBox) key(msg tea.KeyMsg) (send bool, text string, cmd tea.Cmd) {
	switch msg.String() {
	case "esc":
		r.blur()
		return false, "", nil
	case "enter":
		text = r.draft()
		return text != "", text, nil
	}
	r.ta, cmd = r.ta.Update(msg)
	return false, "", cmd
}

// update passes other messages (cursor blinks) to the text box.
func (r *replyBox) update(msg tea.Msg) tea.Cmd {
	if !r.focused {
		return nil
	}
	var cmd tea.Cmd
	r.ta, cmd = r.ta.Update(msg)
	return cmd
}

// lines is the box, w cells wide with its frame: the text area, height
// lines tall, while focused; else one line with the draft or hint and the
// key that opens it.
func (r *replyBox) lines(w, height int, hint string) []string {
	iw := max(w-4, 1)
	if r.focused {
		r.ta.SetWidth(iw)
		r.ta.SetHeight(max(height, 1))
		body := strings.Split(r.ta.View(), "\n")
		return box(body, w, solidEdges, theme.Accent, "", "")
	}
	line := theme.Dim.Render(hint)
	if d := r.draft(); d != "" {
		first, _, more := strings.Cut(d, "\n")
		if more {
			first += " …"
		}
		line = first + theme.Dim.Render("  (draft)")
	}
	return box([]string{spread(line, theme.Dim.Render("r"), iw)}, w, solidEdges, theme.Dim, "", "")
}
