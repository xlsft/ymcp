package views

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/install"
	"github.com/xlsft/ymcp/internal/pipeline/tui/keys"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// Profiles is the first screen: the YouTrack profiles, each with its
// pipelines, whether they run and what their runs are doing.
type Profiles struct {
	base
	loaded   bool
	err      error
	profiles []profileGroup
	sel      int
	top      int
}

// profileGroup is a profile and its pipelines. A pipeline whose profile is
// not configured gets a group of its own with missing set.
type profileGroup struct {
	cfg       config.Config
	missing   bool
	pipelines []pipelineRow
}

type profilesLoaded struct {
	to       int64
	profiles []profileGroup
	err      error
}

// NewProfiles makes the profiles screen.
func NewProfiles(d *Deps) *Profiles { return &Profiles{base: newBase(d)} }

// Init loads the profiles.
func (m *Profiles) Init() tea.Cmd { return m.load }

// Selected is the key of the profile under the cursor, or "".
func (m *Profiles) Selected() string {
	if len(m.profiles) == 0 {
		return ""
	}
	return m.profiles[m.sel].cfg.Name
}

// Count is how many profiles the screen lists.
func (m *Profiles) Count() int { return len(m.profiles) }

func (m *Profiles) load() tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	msg := profilesLoaded{to: m.id}
	profiles, perr := m.d.Profiles()
	rows, err := m.d.loadPipelines(ctx)
	if err == nil {
		err = perr
	}
	msg.err = err
	index := map[string]int{}
	for _, c := range profiles {
		index[c.Name] = len(msg.profiles)
		msg.profiles = append(msg.profiles, profileGroup{cfg: c})
	}
	for _, r := range rows {
		i, ok := index[r.p.Profile]
		if !ok {
			i = len(msg.profiles)
			index[r.p.Profile] = i
			msg.profiles = append(msg.profiles, profileGroup{cfg: config.Config{Name: r.p.Profile}, missing: true})
		}
		msg.profiles[i].pipelines = append(msg.profiles[i].pipelines, r)
	}
	return msg
}

// Update handles keys, ticks and reloads.
func (m *Profiles) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if done, cmd := m.common(msg); done {
		return m, cmd
	}
	switch msg := msg.(type) {
	case profilesLoaded:
		if msg.to != m.id {
			return m, nil
		}
		m.loaded, m.err = true, msg.err
		keep := m.Selected()
		m.profiles = msg.profiles
		for i, p := range m.profiles {
			if p.cfg.Name == keep {
				m.sel = i
			}
		}
		m.sel = clamp(m.sel, len(m.profiles))
	case Tick:
		return m, m.load
	case tea.KeyMsg:
		switch keys.Of(msg) {
		case "q":
			return m, tea.Quit
		case "?":
			m.fullHelp = !m.fullHelp
		case "up", "k":
			m.sel = clamp(m.sel-1, len(m.profiles))
		case "down", "j":
			m.sel = clamp(m.sel+1, len(m.profiles))
		case "home", "g":
			m.sel = 0
		case "end", "G":
			m.sel = clamp(len(m.profiles)-1, len(m.profiles))
		case "enter", "right", "l":
			if p := m.Selected(); p != "" {
				return m, navigate(OpenPipelines{Profile: p})
			}
		case "n":
			if p := m.Selected(); p != "" {
				return m, navigate(OpenWizard{Profile: p})
			}
		case "i":
			return m, navigate(OpenInbox{})
		}
	}
	return m, nil
}

// navigate is a command that asks the root app to move.
func navigate(msg tea.Msg) tea.Cmd { return func() tea.Msg { return msg } }

// View renders the screen.
func (m *Profiles) View() string {
	w := m.width
	waiting := 0
	for _, p := range m.profiles {
		for _, r := range p.pipelines {
			waiting += r.c.input
		}
	}
	header := []string{spread(title("ymcp pipelines"), inputBadge(waiting), w), ""}
	help := []binding{{"↑↓", "move"}, {"enter", "pipelines"}, {"n", "new pipeline"}, {"i", "inbox"}, {"q", "quit"}}

	var body []string
	switch {
	case !m.loaded:
		body = []string{theme.Dim.Render("  Loading…")}
	case len(m.profiles) == 0:
		body = []string{
			"  No YouTrack profiles yet.",
			"",
			"  Add one with " + theme.Bold.Render(install.AddProfile("")) + ", then come back to create a pipeline.",
		}
	default:
		body, m.top = m.list(w)
	}
	if m.err != nil {
		body = append(body, "", theme.Err.Render("  ! ")+oneLine(m.err.Error()))
	}
	return m.frame(header, body, help)
}

// list renders the profiles with their pipelines, scrolled so the selected
// profile is visible.
func (m *Profiles) list(w int) ([]string, int) {
	var lines []string
	starts := make([]int, len(m.profiles))
	for i, p := range m.profiles {
		starts[i] = len(lines)
		sel := i == m.sel
		name := p.cfg.Title()
		if sel {
			name = theme.Accent.Render(name)
		} else {
			name = theme.Bold.Render(name)
		}
		left := marker(sel) + name
		if p.cfg.DisplayName != "" && p.cfg.DisplayName != p.cfg.Name {
			left += theme.Dim.Render(" (" + p.cfg.Name + ")")
		}
		if p.cfg.URL != "" && w >= 70 {
			left += "  " + theme.Dim.Render(strings.TrimPrefix(strings.TrimPrefix(p.cfg.URL, "https://"), "http://"))
		}
		right := pipelinesWord(len(p.pipelines))
		if running := runningCount(p.pipelines); running > 0 {
			right += fmt.Sprintf(" · %d running", running)
		}
		if p.missing {
			right = theme.Warn.Render("profile not configured")
		}
		lines = append(lines, spread(left, theme.Dim.Render(right), w))
		if len(p.pipelines) == 0 {
			lines = append(lines, theme.Dim.Render("      no pipelines yet — press n to create one"))
		}
		nameW := 4
		for _, r := range p.pipelines {
			nameW = max(nameW, len(r.p.Name))
		}
		nameW = min(nameW, max(w/4, 8))
		for _, r := range p.pipelines {
			row := "      " + pad(r.p.Name, nameW) + "  " + pad(r.runner.label(), 17) + "  " + theme.Dim.Render(r.c.summary())
			if w < 70 {
				row = "      " + pad(r.p.Name, nameW) + "  " + r.runner.label()
			}
			lines = append(lines, spread(row, inputBadge(r.c.input), w))
		}
		lines = append(lines, "")
	}
	rows := max(m.height-5, 1)
	top := m.top
	if len(m.profiles) > 0 {
		end := len(lines)
		if m.sel+1 < len(starts) {
			end = starts[m.sel+1]
		}
		top = scrollTo(top, starts[m.sel], rows)
		if end-1 >= top+rows { // show the whole group when it fits
			top = max(min(end-rows, starts[m.sel]), 0)
		}
	}
	top = min(top, max(len(lines)-rows, 0))
	return lines[top:], top
}

func pipelinesWord(n int) string {
	switch n {
	case 0:
		return "no pipelines"
	case 1:
		return "1 pipeline"
	}
	return fmt.Sprintf("%d pipelines", n)
}

func runningCount(rows []pipelineRow) int {
	n := 0
	for _, r := range rows {
		if r.runner.state == runnerRunning {
			n++
		}
	}
	return n
}
