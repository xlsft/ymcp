package wizard

import (
	"context"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// requestTimeout bounds every call the wizard makes in the background.
const requestTimeout = 30 * time.Second

// debouncer turns a burst of changes into one tick: every bump starts a
// new tick and only the latest one fires. Results of slow calls carry the
// seq they were started with, so stale ones are dropped.
type debouncer struct {
	id, seq int
}

type tickMsg struct{ id, seq int }

func (d *debouncer) bump(delay time.Duration) tea.Cmd {
	d.seq++
	id, seq := d.id, d.seq
	if delay <= 0 {
		return func() tea.Msg { return tickMsg{id, seq} }
	}
	return tea.Tick(delay, func(time.Time) tea.Msg { return tickMsg{id, seq} })
}

func (d *debouncer) fired(msg tea.Msg) bool {
	t, ok := msg.(tickMsg)
	return ok && t.id == d.id && t.seq == d.seq
}

// current reports whether a result started at seq is still wanted.
func (d *debouncer) current(id, seq int) bool { return id == d.id && seq == d.seq }

// newInput is a single-line text input without a blinking cursor.
func newInput(placeholder string) textinput.Model {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = placeholder
	in.Cursor.SetMode(cursor.CursorStatic)
	in.CharLimit = 0
	return in
}

// suggestFunc is called on the UI goroutine with the text and caret and
// returns the call to run in the background, or nil when there is nothing
// to ask (e.g. no YouTrack connection yet).
type suggestFunc func(text string, caret int) func(context.Context) ([]Suggestion, error)

type suggestMsg struct {
	id, seq int
	list    []Suggestion
	err     error
}

// assistField is a text input with completions under it that follow the
// text as it is typed (debounced).
type assistField struct {
	sh    *shared
	label string
	in    textinput.Model
	src   suggestFunc
	deb   debouncer

	list []Suggestion
	cur  int // highlighted completion, -1 = none
	open bool
	err  error
}

func newAssist(sh *shared, label, placeholder string, src suggestFunc) *assistField {
	return &assistField{sh: sh, label: label, in: newInput(placeholder), src: src, deb: debouncer{id: sh.newID()}, cur: -1}
}

func (f *assistField) value() string { return strings.TrimSpace(f.in.Value()) }

func (f *assistField) set(v string) {
	f.in.SetValue(v)
	f.in.CursorEnd()
	f.list, f.open, f.cur = nil, false, -1
}

func (f *assistField) focus() { f.in.Focus() }
func (f *assistField) blur() {
	f.in.Blur()
	f.open = false
}

// showing reports whether completions are on screen.
func (f *assistField) showing() bool { return f.in.Focused() && f.open && len(f.list) > 0 }

// key handles a key while the field has focus. consumed is false for the
// keys the step uses to move between fields.
func (f *assistField) key(msg tea.KeyMsg) (cmd tea.Cmd, consumed, changed bool) {
	switch msg.String() {
	case "up":
		if !f.showing() {
			return nil, false, false
		}
		f.cur = max(f.cur-1, -1)
		return nil, true, false
	case "down":
		if !f.showing() {
			return nil, false, false
		}
		f.cur = min(f.cur+1, len(f.list)-1)
		return nil, true, false
	case "tab", "enter":
		if !f.showing() || (msg.String() == "enter" && f.cur < 0) {
			return nil, false, false
		}
		s := f.list[max(f.cur, 0)]
		text, caret := s.Apply(f.in.Value())
		f.in.SetValue(text)
		f.in.SetCursor(caret)
		f.list, f.cur = nil, -1
		return f.refresh(), true, true
	case "esc":
		if !f.showing() {
			return nil, false, false
		}
		f.open = false
		return nil, true, false
	case "shift+tab":
		return nil, false, false
	case "ctrl+@", "ctrl+ ":
		return f.refresh(), true, false
	}
	before := f.in.Value()
	var c tea.Cmd
	f.in, c = f.in.Update(msg)
	if f.in.Value() == before {
		return c, true, false
	}
	return tea.Batch(c, f.refresh()), true, true
}

// refresh asks for completions after the debounce delay.
func (f *assistField) refresh() tea.Cmd {
	f.open = true
	f.cur = -1
	if f.src == nil {
		return nil
	}
	return f.deb.bump(f.sh.delay)
}

// msg handles the field's background messages.
func (f *assistField) msg(msg tea.Msg) tea.Cmd {
	if f.deb.fired(msg) {
		call := f.src(f.in.Value(), f.in.Position())
		if call == nil {
			return nil
		}
		id, seq, ctx := f.deb.id, f.deb.seq, f.sh.ctx
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(ctx, requestTimeout)
			defer cancel()
			list, err := call(ctx)
			return suggestMsg{id: id, seq: seq, list: list, err: err}
		}
	}
	if m, ok := msg.(suggestMsg); ok && f.deb.current(m.id, m.seq) {
		f.list, f.err, f.cur = m.list, m.err, -1
	}
	return nil
}

// view draws the label, the input and, when focused, the completions.
func (f *assistField) view(width int, focused bool) string {
	var b strings.Builder
	label := styleLabel.Render(f.label)
	marker := "  "
	if focused {
		label = styleFocus.Render(f.label)
		marker = styleFocus.Render("› ")
	}
	b.WriteString(label + "\n")
	f.in.Width = max(width-4, 10)
	b.WriteString(marker + f.in.View())
	if f.showing() {
		for i, s := range f.list {
			line := s.Prefix + s.Option + s.Suffix
			if room := width - 8 - lenRunes(s.Description); lenRunes(line) > room && room > 10 {
				// Long completions (paths) matter at their end.
				r := []rune(line)
				line = "…" + string(r[len(r)-room+1:])
			}
			if i == f.cur {
				line = styleFocus.Render("›") + styleSel.Render(strings.TrimRight(line, " ")+" ")
			} else {
				line = " " + strings.TrimRight(line, " ") + " "
			}
			if s.Description != "" && s.Description != s.Option {
				line += " " + styleDim.Render(s.Description)
			}
			b.WriteString("\n    " + line)
		}
	} else if focused && f.err != nil {
		b.WriteString("\n    " + styleDim.Render("no completions: "+oneLine(f.err.Error(), width-20)))
	}
	return b.String()
}

// staticSuggest completes from a fixed list: items containing the typed
// text, the whole value replaced.
func staticSuggest(items []string) suggestFunc {
	return func(text string, _ int) func(context.Context) ([]Suggestion, error) {
		return func(context.Context) ([]Suggestion, error) {
			t := strings.ToLower(strings.TrimSpace(text))
			var out []Suggestion
			for _, it := range items {
				if strings.Contains(strings.ToLower(it), t) && it != text {
					out = append(out, Suggestion{Option: it, CompletionEnd: len([]rune(text))})
				}
			}
			return out[:min(len(out), maxSuggestions)], nil
		}
	}
}

// pickItem is one row of a pickList.
type pickItem struct {
	title, desc, value string
}

// pickList is a list to choose from, optionally filtered by typing.
type pickList struct {
	items    []pickItem
	filter   textinput.Model
	filtered bool
	cur      int
	// allowNew offers the typed filter text as an extra item.
	allowNew string
}

func newPickList(items []pickItem, filterable bool) *pickList {
	p := &pickList{items: items, filtered: filterable, filter: newInput("type to filter")}
	if filterable {
		p.filter.Focus()
	}
	return p
}

func (p *pickList) visible() []pickItem {
	if !p.filtered {
		return p.items
	}
	t := strings.ToLower(strings.TrimSpace(p.filter.Value()))
	var out []pickItem
	exact := false
	for _, it := range p.items {
		if strings.Contains(strings.ToLower(it.title), t) {
			out = append(out, it)
		}
		exact = exact || strings.EqualFold(it.title, strings.TrimSpace(p.filter.Value()))
	}
	if p.allowNew != "" && t != "" && !exact {
		v := strings.TrimSpace(p.filter.Value())
		out = append(out, pickItem{title: v, desc: p.allowNew, value: v})
	}
	return out
}

func (p *pickList) selected() (pickItem, bool) {
	v := p.visible()
	if p.cur < 0 || p.cur >= len(v) {
		return pickItem{}, false
	}
	return v[p.cur], true
}

func (p *pickList) selectValue(value string) {
	for i, it := range p.visible() {
		if it.value == value {
			p.cur = i
			return
		}
	}
}

// key moves the cursor or edits the filter; it reports whether it used
// the key.
func (p *pickList) key(msg tea.KeyMsg) bool {
	n := len(p.visible())
	switch msg.String() {
	case "up", "ctrl+k":
		if p.cur > 0 {
			p.cur--
		}
		return true
	case "down", "ctrl+j":
		if p.cur < n-1 {
			p.cur++
		}
		return true
	case "home":
		p.cur = 0
		return true
	case "end":
		p.cur = max(n-1, 0)
		return true
	}
	if !p.filtered || msg.Type == tea.KeyEnter || msg.Type == tea.KeyTab || msg.Type == tea.KeyEsc {
		return false
	}
	before := p.filter.Value()
	p.filter, _ = p.filter.Update(msg)
	if p.filter.Value() != before {
		p.cur = 0
	}
	return true
}

// view draws at most height rows around the cursor.
func (p *pickList) view(width, height int) string {
	var b strings.Builder
	if p.filtered {
		p.filter.Width = max(width-12, 10)
		b.WriteString(styleDim.Render("filter: ") + p.filter.View() + "\n")
		height--
	}
	items := p.visible()
	if len(items) == 0 {
		b.WriteString(styleDim.Render("  nothing matches"))
		return b.String()
	}
	height = max(height, 1)
	start := 0
	if p.cur >= height {
		start = p.cur - height + 1
	}
	for i := start; i < len(items) && i < start+height; i++ {
		it := items[i]
		line := it.title
		if i == p.cur {
			line = styleFocus.Render("›") + styleSel.Render(line+" ")
		} else {
			line = " " + line + " "
		}
		if it.desc != "" {
			line += " " + styleDim.Render(oneLine(it.desc, max(width-lenRunes(it.title)-6, 10)))
		}
		if i > start {
			b.WriteString("\n")
		}
		b.WriteString(" " + line)
	}
	return b.String()
}

func lenRunes(s string) int { return len([]rune(s)) }
