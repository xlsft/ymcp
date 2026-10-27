package wizard

import (
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xlsft/ymcp/internal/pipeline/tui/keys"
)

// A step is a form: a column of items, most of them fields with a
// label, a one-line description, an input and what the wizard found out
// about the value right under it. The wizard moves the focus through the
// fields and then the buttons (tab, shift+tab, enter); the focused
// control gets every other key.

// control is the input of a field.
type control interface {
	focus()
	blur()
	// key handles a key while the control has focus. consumed is false
	// for the keys the form handles: tab, shift+tab, enter and esc,
	// unless the control has a use for them right now.
	key(msg tea.KeyMsg) (cmd tea.Cmd, consumed bool)
	// view draws the input width cells wide.
	view(width int, focused bool) string
	// help is the key help while the control has focus.
	help() string
}

// item is one entry of a form: a field, or text when ctl is nil.
type item struct {
	id string
	// heading starts a section; it is drawn above the item.
	heading string
	// headMark is the status shown after the heading.
	headMark func() string
	// headNote is dim text after the heading on the same line.
	headNote string
	label    string
	// desc is the one-line description; descFn, when set, makes it
	// follow the value.
	desc   string
	descFn func() string
	ctl    control
	// labelNote is dim text after the label, e.g. a status.
	labelNote func() string
	// right is a field drawn beside this one, in a second column; it
	// is in the items too (for the focus order) with inRow set.
	right *item
	inRow bool
	// inline: the control draws the label itself (check boxes, the
	// Advanced switch); the description goes under it.
	inline bool
	// problem is the field's error, drawn in red under the input.
	problem func() string
	// below draws what belongs to the field under its input: check
	// results, previews, matching issues; or the text of a text item.
	below func(width int) string
	// hidden leaves the item out of the form for now.
	hidden func() bool
}

func (it *item) visible() bool { return it.hidden == nil || !it.hidden() }
func (it *item) focusable() bool {
	return it.ctl != nil && it.visible()
}

// bad reports whether the field shows an error.
func (it *item) bad() bool { return it.problem != nil && it.problem() != "" }

// description is the item's one-line description.
func (it *item) description() string {
	if it.descFn != nil {
		return it.descFn()
	}
	return it.desc
}

// headLine is the section heading drawn above the item, or "".
func (it *item) headLine(width int) string {
	if it.heading == "" {
		return ""
	}
	h := styleSection.Render(it.heading)
	if it.headMark != nil {
		if m := it.headMark(); m != "" {
			h += " " + m
		}
	}
	if it.headNote != "" {
		h += "  " + styleDim.Render(oneLine(it.headNote, max(width-lenVisible(h)-2, 8)))
	}
	return h
}

// render draws the item width cells wide, its heading first. The focused
// field gets an accent bar on its left.
func (it *item) render(width int, focused bool) string {
	body := it.renderBody(width, focused)
	if h := it.headLine(width); h != "" {
		if body == "" {
			return h
		}
		return h + "\n" + body
	}
	return body
}

// renderBody draws the item without its heading.
func (it *item) renderBody(width int, focused bool) string {
	var body []string
	inner := width - 2
	desc := it.description()
	if it.label != "" && !it.inline {
		l := styleLabel.Render(it.label)
		if focused {
			l = styleFocus.Render(it.label)
		}
		if it.labelNote != nil {
			if n := it.labelNote(); n != "" {
				l += "  " + styleDim.Render(oneLine(n, max(inner-lenVisible(l)-2, 8)))
			}
		}
		body = append(body, l)
	}
	if desc != "" && !it.inline {
		body = append(body, styleDim.Render(oneLine(desc, inner)))
	}
	if it.ctl != nil {
		if bc, ok := it.ctl.(badControl); ok {
			bc.setBad(it.bad())
		}
		body = append(body, it.ctl.view(inner, focused))
	}
	if desc != "" && it.inline {
		body = append(body, "    "+styleDim.Render(oneLine(desc, inner-4)))
	}
	if it.problem != nil {
		if p := it.problem(); p != "" {
			body = append(body, errLine(p, inner))
		}
	}
	if it.below != nil {
		if s := strings.TrimRight(it.below(inner), "\n"); s != "" {
			body = append(body, s)
		}
	}
	if len(body) == 0 {
		return ""
	}
	gutter := "  "
	if focused {
		gutter = styleFocus.Render("▌") + " "
	}
	lines := strings.Split(strings.Join(body, "\n"), "\n")
	for i, l := range lines {
		lines[i] = gutter + l
	}
	return strings.Join(lines, "\n")
}

// columnGap is the room between the two fields of a row.
const columnGap = 3

// renderRow draws an item and the one beside it.
func renderRow(left *item, width int, focus string) string {
	right := left.right
	cw := (width - columnGap) / 2
	lv := left.renderBody(cw, left.ctl != nil && left.id == focus)
	rv := ""
	if right.visible() {
		rv = right.renderBody(cw, right.ctl != nil && right.id == focus)
	}
	col := lipgloss.NewStyle().Width(cw)
	row := lipgloss.JoinHorizontal(lipgloss.Top, col.Render(fit(lv, cw, 0)), strings.Repeat(" ", columnGap), col.Render(fit(rv, cw, 0)))
	lines := strings.Split(row, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	row = strings.Join(lines, "\n")
	if h := left.headLine(width); h != "" {
		return h + "\n" + row
	}
	return row
}

// badControl is a control that draws itself red when its field is wrong.
type badControl interface{ setBad(bool) }

// formState is where a step's form stands.
type formState struct {
	focus  string // id of the focused field or button
	scroll int    // first line shown
	// tried is set once Next was pressed with errors: from then on empty
	// required fields say so.
	tried bool
}

// renderForm lays the items out and scrolls them so that the focused one
// shows, in at most height lines.
func renderForm(items []*item, focus string, width, height int, fs *formState) string {
	var lines []string
	start, end := -1, -1
	first := true
	for _, it := range items {
		if !it.visible() || it.inRow {
			continue
		}
		if !first {
			lines = append(lines, "")
			if it.heading != "" {
				lines = append(lines, "") // sections get more room than fields
			}
		}
		first = false
		var block []string
		focused := it.ctl != nil && it.id == focus
		if it.right != nil {
			block = strings.Split(renderRow(it, width, focus), "\n")
			focused = focused || (it.right.ctl != nil && it.right.id == focus && it.right.visible())
		} else {
			block = strings.Split(it.render(width, focused), "\n")
		}
		if focused {
			start, end = len(lines), len(lines)+len(block)
			if it.heading != "" {
				start++ // the heading may scroll away first
			}
		}
		lines = append(lines, block...)
	}
	if len(lines) <= height {
		fs.scroll = 0
		return strings.Join(lines, "\n")
	}
	room := max(height-1, 1) // the last line says there is more
	if start >= 0 {
		if end-fs.scroll > room {
			fs.scroll = end - room
		}
		if start < fs.scroll {
			fs.scroll = max(start-1, 0)
		}
	}
	fs.scroll = min(max(fs.scroll, 0), len(lines)-room)
	out := append([]string(nil), lines[fs.scroll:fs.scroll+room]...)
	var more []string
	if fs.scroll > 0 {
		more = append(more, "▲ more above")
	}
	if fs.scroll+room < len(lines) {
		more = append(more, "▼ more below")
	}
	out = append(out, styleDim.Render("  "+strings.Join(more, "   ")+"  (tab moves through the fields)"))
	return strings.Join(out, "\n")
}

// textCtl is a single-line text field, with completions when its
// assistField has a source.
type textCtl struct {
	f        *assistField
	onChange func() tea.Cmd
	isBad    bool
}

func newText(f *assistField, onChange func() tea.Cmd) *textCtl {
	return &textCtl{f: f, onChange: onChange}
}

func (c *textCtl) focus()        { c.f.focus() }
func (c *textCtl) blur()         { c.f.blur() }
func (c *textCtl) setBad(b bool) { c.isBad = b }
func (c *textCtl) help() string  { return c.f.help() }

func (c *textCtl) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	cmd, consumed, changed := c.f.key(msg)
	if changed && c.onChange != nil {
		cmd = tea.Batch(cmd, c.onChange())
	}
	return cmd, consumed
}

func (c *textCtl) view(width int, focused bool) string { return c.f.view(width, focused, c.isBad) }

// option is one choice of a choiceCtl.
type option struct {
	value, title, desc string
	// status is drawn after the description: e.g. whether a provider is
	// installed.
	status func(width int) string
}

// choiceCtl picks one of a few options; the arrows change the choice at
// once. A list shows every option with its description; a segmented
// control shows them on one line and describes the chosen one.
type choiceCtl struct {
	opts    func() []option
	get     func() string
	set     func(v string) tea.Cmd
	segment bool
	titleW  int // width of the title column of a list
}

func (c *choiceCtl) focus() {}
func (c *choiceCtl) blur()  {}

func (c *choiceCtl) index(opts []option) int {
	v := c.get()
	for i, o := range opts {
		if o.value == v {
			return i
		}
	}
	return -1
}

func (c *choiceCtl) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	opts := c.opts()
	if len(opts) == 0 {
		return nil, false
	}
	i := c.index(opts)
	to := i
	switch msg.String() {
	case "up", "left":
		to = max(i-1, 0)
	case "down", "right":
		to = min(i+1, len(opts)-1)
	case "home":
		to = 0
	case "end":
		to = len(opts) - 1
	default:
		return nil, false
	}
	if to == i || to < 0 {
		return nil, true
	}
	return c.set(opts[to].value), true
}

func (c *choiceCtl) help() string {
	if c.segment {
		return keyHelp("←/→", "change")
	}
	return keyHelp("↑/↓", "change")
}

func (c *choiceCtl) view(width int, focused bool) string {
	opts := c.opts()
	if len(opts) == 0 {
		return styleDim.Render("nothing to choose from")
	}
	cur := c.index(opts)
	if c.segment {
		var cells []string
		for i, o := range opts {
			switch {
			case i == cur && focused:
				cells = append(cells, styleSel.Render(" "+o.title+" "))
			case i == cur:
				cells = append(cells, styleFocus.Render(" "+o.title+" "))
			default:
				cells = append(cells, " "+o.title+" ")
			}
		}
		out := strings.Join(cells, styleBorder.Render("│"))
		if cur >= 0 && opts[cur].desc != "" {
			out += "\n" + styleDim.Render(oneLine(opts[cur].desc, width))
		}
		return out
	}
	tw := c.titleW
	if tw == 0 {
		for _, o := range opts {
			tw = max(tw, lenRunes(o.title))
		}
	}
	var lines []string
	for i, o := range opts {
		dot, title := styleDim.Render("○"), padRight(o.title, tw)
		switch {
		case i == cur && focused:
			dot, title = styleFocus.Render("●"), styleSel.Render(" "+title+" ")
		case i == cur:
			dot, title = styleFocus.Render("●"), " "+styleBold.Render(title)+" "
		default:
			title = " " + title + " "
		}
		line := dot + title
		if o.desc != "" {
			line += " " + styleDim.Render(oneLine(o.desc, max(width-lenVisible(line)-2, 8)))
		}
		if o.status != nil {
			if st := o.status(max(width-lenVisible(line)-2, 10)); st != "" {
				line = padRight(line, min(tw+30, width-24)) + " " + st
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// toggleCtl is a check box: [x] or [ ].
type toggleCtl struct {
	label string
	get   func() bool
	set   func(bool) tea.Cmd
}

func (c *toggleCtl) focus() {}
func (c *toggleCtl) blur()  {}
func (c *toggleCtl) help() string {
	return keyHelp("space", "tick/untick")
}

func (c *toggleCtl) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch keys.Of(msg) {
	case " ", "x":
		return c.set(!c.get()), true
	}
	return nil, false
}

func (c *toggleCtl) view(_ int, focused bool) string {
	box := "[ ]"
	if c.get() {
		box = "[x]"
	}
	if focused {
		return styleFocus.Render(box + " " + c.label)
	}
	return box + " " + styleLabel.Render(c.label)
}

// disclosureCtl opens and closes a section of rarely changed fields.
type disclosureCtl struct {
	label string
	open  *bool
	// force keeps the section open, e.g. while a field in it is wrong.
	force func() bool
	// note is shown after the label, e.g. how many problems hide there.
	note func() string
}

func (c *disclosureCtl) focus() {}
func (c *disclosureCtl) blur()  {}

// shown reports whether the section is open.
func (c *disclosureCtl) shown() bool { return *c.open || (c.force != nil && c.force()) }

func (c *disclosureCtl) help() string {
	if c.force != nil && c.force() {
		return keyHelp("", "stays open while a field in it is wrong")
	}
	if *c.open {
		return keyHelp("enter", "hide")
	}
	return keyHelp("enter", "show")
}

func (c *disclosureCtl) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "enter", " ", "right", "left":
		*c.open = !*c.open
		if msg.String() == "right" {
			*c.open = true
		}
		if msg.String() == "left" {
			*c.open = false
		}
		return nil, true
	}
	return nil, false
}

func (c *disclosureCtl) view(_ int, focused bool) string {
	arrow := "▸ "
	if c.shown() {
		arrow = "▾ "
	}
	l := arrow + c.label
	if focused {
		l = styleFocus.Render(l)
	} else {
		l = styleLabel.Render(l)
	}
	if c.note != nil {
		if n := c.note(); n != "" {
			l += "  " + n
		}
	}
	return l
}

// checklistCtl ticks any number of rows, one row per option.
type checklistCtl struct {
	rows func() []option
	on   func(v string) bool
	flip func(v string)
	cur  int
}

func (c *checklistCtl) focus() {}
func (c *checklistCtl) blur()  {}
func (c *checklistCtl) help() string {
	return keyHelp("↑/↓", "move", "space", "tick/untick")
}

func (c *checklistCtl) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	rows := c.rows()
	switch keys.Of(msg) {
	case "up":
		c.cur = max(c.cur-1, 0)
		return nil, true
	case "down":
		c.cur = min(c.cur+1, max(len(rows)-1, 0))
		return nil, true
	case " ", "x":
		if c.cur < len(rows) {
			c.flip(rows[c.cur].value)
		}
		return nil, true
	}
	return nil, false
}

func (c *checklistCtl) view(width int, focused bool) string {
	rows := c.rows()
	if len(rows) == 0 {
		return styleDim.Render("nothing here")
	}
	tw := 0
	for _, r := range rows {
		tw = max(tw, lenRunes(r.title))
	}
	var lines []string
	for i, r := range rows {
		box := "[ ]"
		if c.on(r.value) {
			box = "[x]"
		}
		line := box + " " + padRight(r.title, tw)
		if focused && i == c.cur {
			line = styleSel.Render(line)
		}
		if r.desc != "" {
			line += "  " + styleDim.Render(oneLine(r.desc, max(width-tw-7, 8)))
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// areaCtl is a multi-line text field: enter starts a new line.
type areaCtl struct {
	ta       textarea.Model
	onChange func() tea.Cmd
}

func newArea(placeholder string, height int) *areaCtl {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	ta.Placeholder = placeholder
	ta.FocusedStyle.Placeholder = styleHolder
	ta.BlurredStyle.Placeholder = styleHolder
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.SetHeight(height)
	ta.Cursor.SetMode(cursor.CursorStatic)
	return &areaCtl{ta: ta}
}

func (c *areaCtl) focus() { c.ta.Focus() }
func (c *areaCtl) blur()  { c.ta.Blur() }
func (c *areaCtl) help() string {
	return keyHelp("enter", "new line")
}

func (c *areaCtl) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "tab", "shift+tab", "esc":
		return nil, false
	}
	before := c.ta.Value()
	var cmd tea.Cmd
	c.ta, cmd = c.ta.Update(msg)
	if c.ta.Value() != before && c.onChange != nil {
		cmd = tea.Batch(cmd, c.onChange())
	}
	return cmd, true
}

func (c *areaCtl) view(width int, focused bool) string {
	c.ta.SetWidth(max(boxWidth(width)-4, 10))
	return inputBox(c.ta.View(), width, focused, false)
}
