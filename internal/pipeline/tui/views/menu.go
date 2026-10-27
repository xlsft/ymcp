package views

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// boxEdges are the characters of a box's frame.
type boxEdges struct {
	tl, tr, bl, br, h, v string
}

// Frames: heavy ones mark the selected card, so it shows without colour
// too.
var (
	solidEdges       = boxEdges{"╭", "╮", "╰", "╯", "─", "│"}
	heavyEdges       = boxEdges{"┏", "┓", "┗", "┛", "━", "┃"}
	dashedEdges      = boxEdges{"╭", "╮", "╰", "╯", "╌", "┆"}
	heavyDashedEdges = boxEdges{"┏", "┓", "┗", "┛", "╍", "┇"}
)

// box draws lines inside a frame exactly w cells wide, with one space of
// padding on each side. title sits in the top edge on the left, badge on
// the right; either may be "". The frame is drawn in edge.
func box(lines []string, w int, e boxEdges, edge lipgloss.Style, title, badge string) []string {
	w = max(w, 4)
	inner := w - 4
	top := e.h
	room := w - 2 - 1 // the edge between the corners, after the first ─
	if title != "" {
		title = " " + cut(title, max(room-2, 0)) + " "
		room -= ansi.StringWidth(title)
	}
	if badge != "" && ansi.StringWidth(badge)+3 <= room {
		room -= ansi.StringWidth(badge) + 3
	} else {
		badge = ""
	}
	out := make([]string, 0, len(lines)+2)
	first := edge.Render(e.tl+top) + title + edge.Render(strings.Repeat(e.h, max(room, 0)))
	if badge != "" {
		first += " " + badge + " " + edge.Render(e.h)
	}
	out = append(out, first+edge.Render(e.tr))
	side := edge.Render(e.v)
	for _, l := range lines {
		out = append(out, side+" "+pad(l, inner)+" "+side)
	}
	return append(out, edge.Render(e.bl+strings.Repeat(e.h, w-2)+e.br))
}

// popOver draws pop over bg with its top-left corner at column x of line
// y, padding bg with empty lines when it is too short.
func popOver(bg, pop []string, x, y int) []string {
	x, y = max(x, 0), max(y, 0)
	for len(bg) < y+len(pop) {
		bg = append(bg, "")
	}
	out := append([]string(nil), bg...)
	const reset = "\x1b[0m"
	for i, p := range pop {
		line := out[y+i]
		pw := ansi.StringWidth(p)
		left := ansi.Truncate(line, x, "")
		if n := x - ansi.StringWidth(left); n > 0 {
			left += strings.Repeat(" ", n)
		}
		right := ""
		if ansi.StringWidth(line) > x+pw {
			right = ansi.TruncateLeft(line, x+pw, "")
		}
		out[y+i] = left + reset + p + reset + right
	}
	return out
}

// placePopup picks where a popup of pw×ph cells goes next to an anchor (a card
// at ax, ay, aw cells wide) inside an area of w×h: to the right of the
// anchor, else to its left, else over it; centred when it fits nowhere
// near.
func placePopup(pw, ph, ax, ay, aw, w, h int) (x, y int) {
	switch {
	case ax+aw+pw <= w:
		x = ax + aw
	case ax-pw >= 0:
		x = ax - pw
	case pw <= w:
		x = min(ax+2, w-pw)
	default:
		x = 0
	}
	y = ay + 1
	if y+ph > h {
		y = max(h-ph, 0)
	}
	if pw > w || ph > h {
		x, y = max((w-pw)/2, 0), max((h-ph)/2, 0)
	}
	return x, y
}

// menuItem is one action of a context menu.
type menuItem struct {
	label string
	key   string // the direct shortcut, shown on the right
	// why is the reason the item cannot run now; "" means it can.
	why string
	run func() tea.Cmd
}

// menuMinWidth is the narrowest a menu's inside gets, so the reason an
// item is off stays readable.
const menuMinWidth = 24

// menu is a context menu: a list of actions in a small box.
type menu struct {
	open bool
	sel  int
}

// move selects the next or previous item.
func (mn *menu) move(d, n int) { mn.sel = clamp(mn.sel+d, n) }

// key handles a key while the menu is open. It returns the item to run,
// or nil; close reports that the menu closed.
func (mn *menu) key(k string, items []menuItem) (pick *menuItem, closed bool) {
	switch k {
	case "esc", "q":
		mn.open = false
		return nil, true
	case "up", "k":
		mn.move(-1, len(items))
	case "down", "j":
		mn.move(1, len(items))
	case "home", "g":
		mn.sel = 0
	case "end", "G":
		mn.sel = len(items) - 1
	case "enter", " ":
		mn.sel = clamp(mn.sel, len(items))
		if len(items) > 0 {
			return &items[mn.sel], false
		}
	default:
		for i := range items {
			if items[i].key == k {
				mn.sel = i
				return &items[i], false
			}
		}
	}
	return nil, false
}

// view renders the menu in a box titled title.
func (mn *menu) view(title string, items []menuItem, maxW int) []string {
	labelW, keyW := 0, 0
	for _, it := range items {
		labelW = max(labelW, ansi.StringWidth(it.label))
		keyW = max(keyW, ansi.StringWidth(it.key))
	}
	// The reason an item is off wraps to the items' width, so the box
	// keeps its size while the cursor moves.
	inner := max(2+labelW+3+keyW, ansi.StringWidth(title)+1, menuMinWidth)
	sel := clamp(mn.sel, len(items))
	why := ""
	if len(items) > 0 {
		why = items[sel].why
	}
	inner = min(inner, max(maxW-4, 8))
	lines := make([]string, 0, len(items)+2)
	for i, it := range items {
		label, k := it.label, theme.Dim.Render(it.key)
		switch {
		case it.why != "":
			label = theme.Dim.Render(label)
		case i == sel:
			label = theme.Bold.Render(label)
		}
		lines = append(lines, spread(marker(i == sel)+label, k, inner))
	}
	if why != "" {
		lines = append(lines, "")
		for _, l := range wrap(why, inner) {
			lines = append(lines, theme.Warn.Render(l))
		}
	}
	return box(lines, inner+4, solidEdges, theme.Accent, theme.Bold.Render(title), "")
}

// menuHelp is the help line while a menu is open.
var menuHelp = []binding{{"↑↓", "choose"}, {"enter", "run"}, {"esc", "close"}}

// namePrompt asks for a pipeline name in a small box: for Copy… and
// Rename….
type namePrompt struct {
	open  bool
	title string
	in    textinput.Model
	err   string
	// done runs with the name on enter; it returns an error to keep the
	// box open with the reason.
	done func(name string) (tea.Cmd, error)
}

func (p *namePrompt) start(title, value string, done func(string) (tea.Cmd, error)) tea.Cmd {
	in := textinput.New()
	in.Prompt = theme.Accent.Render("›") + " "
	in.CharLimit = 40
	in.SetValue(value)
	in.CursorEnd()
	*p = namePrompt{open: true, title: title, in: in, done: done}
	return p.in.Focus()
}

// key handles a key while the prompt is open.
func (p *namePrompt) key(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		p.open = false
		return nil
	case "enter":
		cmd, err := p.done(strings.TrimSpace(p.in.Value()))
		if err != nil {
			p.err = err.Error()
			return nil
		}
		p.open = false
		return cmd
	}
	p.err = ""
	var cmd tea.Cmd
	p.in, cmd = p.in.Update(msg)
	return cmd
}

// update passes other messages (cursor blinks) to the input.
func (p *namePrompt) update(msg tea.Msg) tea.Cmd {
	if !p.open {
		return nil
	}
	var cmd tea.Cmd
	p.in, cmd = p.in.Update(msg)
	return cmd
}

func (p *namePrompt) view(maxW int) []string {
	inner := min(44, max(maxW-4, 10))
	p.in.Width = max(inner-3, 1)
	lines := []string{theme.Dim.Render("New name"), p.in.View()}
	if p.err != "" {
		lines = append(lines, "")
		for _, l := range wrap(p.err, inner) {
			lines = append(lines, theme.Err.Render(l))
		}
	}
	return box(lines, inner+4, solidEdges, theme.Accent, theme.Bold.Render(p.title), "")
}

// promptHelp is the help line while the name prompt is open.
var promptHelp = []binding{{"enter", "save"}, {"esc", "cancel"}}
