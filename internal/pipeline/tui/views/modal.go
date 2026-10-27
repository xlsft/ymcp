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
	tabActive   = lipgloss.NewStyle().Bold(true).Underline(true)
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

// overlay draws box (lines of equal width) at x, y over under, which is
// dimmed; the result is exactly w×h.
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
		if j < 0 || j >= len(box) {
			out[i] = dimLine(line)
			continue
		}
		left := ansi.Truncate(line, x, "")
		right := ansi.TruncateLeft(line, x+bw, "")
		out[i] = cut(dimLine(pad(left, x))+pad(box[j], bw)+dimLine(pad(right, max(w-x-bw, 0))), w)
	}
	return strings.Join(out, "\n")
}

func dimLine(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	return theme.Dim.Render(s)
}

// frameBox draws a rounded border around inner lines (cut or padded to
// iw), with left and right in the top edge.
func frameBox(inner []string, iw int, left, right string) []string {
	b := lipgloss.RoundedBorder()
	edge := func(s string) string { return modalBorder.Render(s) }
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

// replyBox is the comment box of the run card: enter sends, alt+enter
// starts a new line, esc leaves it with the draft kept.
type replyBox struct {
	ta      textarea.Model
	focused bool
}

func newReplyBox() replyBox {
	ta := textarea.New()
	ta.Placeholder = "Comment on the issue — /approve or /reject answers a permission request"
	ta.ShowLineNumbers = false
	ta.Prompt = theme.Accent.Render("┃") + " "
	ta.CharLimit = 16000
	ta.SetHeight(3)
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	ta.FocusedStyle.CursorLine = ta.FocusedStyle.CursorLine.UnsetBackground()
	return replyBox{ta: ta}
}

func (r *replyBox) focus(width int) tea.Cmd {
	r.focused = true
	r.resize(width)
	return r.ta.Focus()
}

func (r *replyBox) resize(width int) { r.ta.SetWidth(max(width, 10)) }

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

// lines is the box: the text area while focused, else one line with the
// draft or a hint.
func (r *replyBox) lines(w int, hint string) []string {
	if r.focused {
		out := strings.Split(r.ta.View(), "\n")
		for i, l := range out {
			out[i] = cut(l, w)
		}
		return out
	}
	bar := theme.Dim.Render("┃ ")
	if d := r.draft(); d != "" {
		first, _, more := strings.Cut(d, "\n")
		if more {
			first += " …"
		}
		return []string{cut(bar+first+theme.Dim.Render("  (draft)"), w)}
	}
	return []string{cut(bar+theme.Dim.Render(hint), w)}
}
