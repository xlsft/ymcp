package wizard

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/tui/keys"
	"github.com/xlsft/ymcp/internal/tools"
)

// treeRows is how many rows of the tool tree show at a time in a window
// height lines high; the rest scroll with the cursor.
func treeRows(height int) int { return min(max(height-17, 4), 16) }

// toolTreeCtl ticks tools by group: a group row ticks or unticks its
// whole group and opens to list its tools.
type toolTreeCtl struct {
	sh   *shared
	on   map[string]bool
	open map[string]bool
	cat  []tools.Info
	cur  int
	top  int
}

func newToolTree(sh *shared, on map[string]bool) *toolTreeCtl {
	return &toolTreeCtl{sh: sh, on: on, open: map[string]bool{}, cat: tools.Catalog()}
}

// treeRow is a group (tool "") or one of its tools.
type treeRow struct {
	group tools.Group
	tool  tools.Info
}

func (c *toolTreeCtl) rows() []treeRow {
	var out []treeRow
	for _, g := range tools.Groups {
		out = append(out, treeRow{group: g})
		if !c.open[g.Name] {
			continue
		}
		for _, it := range c.cat {
			if it.Group == g.Name {
				out = append(out, treeRow{group: g, tool: it})
			}
		}
	}
	return out
}

// state counts the tools of a group that are on.
func (c *toolTreeCtl) state(g tools.Group) (on, all int) {
	for _, t := range g.Tools {
		if c.on[t] {
			on++
		}
	}
	return on, len(g.Tools)
}

func (c *toolTreeCtl) focus() {}
func (c *toolTreeCtl) blur()  {}
func (c *toolTreeCtl) help() string {
	return keyHelp("↑/↓", "move", "space", "tick/untick", "→/←", "open/close group", "a", "all")
}

func (c *toolTreeCtl) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	rows := c.rows()
	c.cur = min(c.cur, len(rows)-1)
	r := rows[c.cur]
	switch keys.Of(msg) {
	case "up":
		c.cur = max(c.cur-1, 0)
	case "down":
		c.cur = min(c.cur+1, len(rows)-1)
	case "home":
		c.cur = 0
	case "end":
		c.cur = len(rows) - 1
	case "right", "l":
		c.open[r.group.Name] = true
	case "left", "h":
		if c.open[r.group.Name] {
			c.open[r.group.Name] = false
			for i, row := range rows {
				if row.group.Name == r.group.Name {
					c.cur = i // back to the group row
					break
				}
			}
		}
	case " ", "x":
		if r.tool.Name != "" {
			c.on[r.tool.Name] = !c.on[r.tool.Name]
			break
		}
		on, all := c.state(r.group)
		for _, t := range r.group.Tools {
			c.on[t] = on < all
		}
	case "a":
		all := true
		for _, it := range c.cat {
			all = all && c.on[it.Name]
		}
		for _, it := range c.cat {
			c.on[it.Name] = !all
		}
	default:
		return nil, false
	}
	return nil, true
}

func (c *toolTreeCtl) view(width int, focused bool) string {
	rows := c.rows()
	treeRows := treeRows(c.sh.height)
	c.cur = min(c.cur, len(rows)-1)
	if c.cur < c.top {
		c.top = c.cur
	}
	if c.cur >= c.top+treeRows {
		c.top = c.cur - treeRows + 1
	}
	c.top = max(min(c.top, len(rows)-treeRows), 0)
	nameW := 0
	for _, it := range c.cat {
		nameW = max(nameW, lenRunes(it.Name))
	}
	var lines []string
	for i := c.top; i < len(rows) && i < c.top+treeRows; i++ {
		r := rows[i]
		var line, desc string
		if r.tool.Name == "" {
			on, all := c.state(r.group)
			box := "[ ]"
			switch {
			case on == all:
				box = "[x]"
			case on > 0:
				box = "[-]"
			}
			arrow := "▸"
			if c.open[r.group.Name] {
				arrow = "▾"
			}
			line = arrow + " " + box + " " + padRight(r.group.Name, nameW+2)
			desc = fmt.Sprintf("%d/%d · %s", on, all, r.group.Title)
		} else {
			box := "[ ]"
			if c.on[r.tool.Name] {
				box = "[x]"
			}
			line = "    " + box + " " + padRight(r.tool.Name, nameW)
			desc = r.tool.Title
		}
		if focused && i == c.cur {
			line = styleSel.Render(line)
		}
		line += "  " + styleDim.Render(oneLine(desc, max(width-lenVisible(line)-14, 8)))
		if r.tool.Destructive {
			line += " " + styleWarn.Render("destructive")
		}
		lines = append(lines, line)
	}
	if len(rows) > treeRows {
		var more []string
		if c.top > 0 {
			more = append(more, fmt.Sprintf("↑ %d", c.top))
		}
		if rest := len(rows) - c.top - treeRows; rest > 0 {
			more = append(more, fmt.Sprintf("↓ %d", rest))
		}
		if len(more) > 0 {
			lines = append(lines, styleDim.Render("  "+strings.Join(more, " · ")+" more"))
		}
	}
	return strings.Join(lines, "\n")
}
