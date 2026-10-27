package views

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// answerBox is the text box for answering a run: enter sends, alt+enter
// starts a new line, esc closes it.
type answerBox struct {
	ta   textarea.Model
	open bool
}

func newAnswerBox() answerBox {
	ta := textarea.New()
	ta.Placeholder = "Your answer…"
	ta.ShowLineNumbers = false
	ta.Prompt = theme.Accent.Render("┃") + " "
	ta.CharLimit = 8000
	ta.SetHeight(3)
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	ta.FocusedStyle.CursorLine = ta.FocusedStyle.CursorLine.UnsetBackground()
	return answerBox{ta: ta}
}

// start opens the box, empty, at the given width.
func (a *answerBox) start(width int) tea.Cmd {
	a.open = true
	a.ta.Reset()
	a.resize(width)
	return a.ta.Focus()
}

func (a *answerBox) resize(width int) { a.ta.SetWidth(max(width, 10)) }

func (a *answerBox) close() {
	a.open = false
	a.ta.Blur()
}

// key handles a key while the box is open. send is true when the user
// pressed enter with some text; esc closes the box.
func (a *answerBox) key(msg tea.KeyMsg) (send bool, text string, cmd tea.Cmd) {
	switch msg.String() {
	case "esc":
		a.close()
		return false, "", nil
	case "enter":
		text = strings.TrimSpace(a.ta.Value())
		return text != "", text, nil
	}
	a.ta, cmd = a.ta.Update(msg)
	return false, "", cmd
}

// update passes other messages (cursor blinks) to the text box.
func (a *answerBox) update(msg tea.Msg) tea.Cmd {
	if !a.open {
		return nil
	}
	var cmd tea.Cmd
	a.ta, cmd = a.ta.Update(msg)
	return cmd
}

func (a *answerBox) view() []string { return strings.Split(a.ta.View(), "\n") }

// answerHelp is the help line while the box is open.
var answerHelp = []binding{{"enter", "send"}, {"alt+enter", "new line"}, {"esc", "close"}}
