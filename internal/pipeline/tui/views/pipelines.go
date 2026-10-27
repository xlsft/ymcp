package views

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// daemonTimeout bounds `run -d` and `stop`, which wait for the runner.
const daemonTimeout = 90 * time.Second

// Pipelines lists the pipelines of one profile: whether their runner runs,
// how many runs sit in each column and which wait for input.
type Pipelines struct {
	base
	profile string
	loaded  bool
	err     error
	rows    []pipelineRow
	sel     int
	home    string
}

type pipelinesLoaded struct {
	to   int64
	rows []pipelineRow
	err  error
}

// NewPipelines makes the pipelines screen of a profile.
func NewPipelines(d *Deps, profile string) *Pipelines {
	home, _ := os.UserHomeDir()
	return &Pipelines{base: newBase(d), profile: profile, home: home}
}

// Init loads the pipelines.
func (m *Pipelines) Init() tea.Cmd { return m.load }

// Profile is the profile the screen shows.
func (m *Pipelines) Profile() string { return m.profile }

func (m *Pipelines) load() tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	rows, err := m.d.loadPipelines(ctx)
	out := rows[:0]
	for _, r := range rows {
		if r.p.Profile == m.profile {
			out = append(out, r)
		}
	}
	return pipelinesLoaded{to: m.id, rows: out, err: err}
}

func (m *Pipelines) selected() (pipelineRow, bool) {
	if len(m.rows) == 0 {
		return pipelineRow{}, false
	}
	return m.rows[m.sel], true
}

// Update handles keys, ticks and reloads.
func (m *Pipelines) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if done, cmd := m.common(msg); done {
		return m, cmd
	}
	switch msg := msg.(type) {
	case pipelinesLoaded:
		if msg.to != m.id {
			return m, nil
		}
		m.loaded, m.err = true, msg.err
		keep := ""
		if r, ok := m.selected(); ok {
			keep = r.p.Name
		}
		m.rows = msg.rows
		for i, r := range m.rows {
			if r.p.Name == keep {
				m.sel = i
			}
		}
		m.sel = clamp(m.sel, len(m.rows))
	case Tick:
		return m, m.load
	case tea.KeyMsg:
		return m, m.key(msg.String())
	}
	return m, nil
}

func (m *Pipelines) key(k string) tea.Cmd {
	switch k {
	case "q":
		return tea.Quit
	case "esc", "left", "h":
		return navigate(Back{})
	case "?":
		m.fullHelp = !m.fullHelp
	case "up", "k":
		m.sel = clamp(m.sel-1, len(m.rows))
	case "down", "j":
		m.sel = clamp(m.sel+1, len(m.rows))
	case "n":
		return navigate(OpenWizard{Profile: m.profile})
	case "i":
		return navigate(OpenInbox{})
	}
	r, ok := m.selected()
	if !ok {
		return nil
	}
	name := r.p.Name
	switch k {
	case "enter", "right", "l":
		return navigate(OpenBoard{Pipeline: name})
	case "e":
		p := r.p
		return navigate(OpenWizard{Profile: m.profile, Existing: &p})
	case "r":
		return startRunner(&m.base, name, r.runner)
	case "s":
		return stopRunner(&m.base, name, r.runner)
	case "d":
		if r.runner.state == runnerRunning {
			m.fail(fmt.Errorf("%s is running: stop it first (s)", name))
			return nil
		}
		m.confirm("Delete pipeline "+name+"? Its run history stays.", func() tea.Cmd {
			if err := m.d.DeletePipeline(name); err != nil {
				m.fail(err)
				return nil
			}
			m.say("Deleted pipeline " + name + ".")
			return m.load
		})
	}
	return nil
}

// startRunner starts a pipeline's runner in the background through the
// CLI, reporting the outcome on b's status line.
func startRunner(b *base, name string, r runnerInfo) tea.Cmd {
	switch {
	case b.d.Daemon == nil:
		b.fail(fmt.Errorf("starting runners is not available here; run `ymcp pipeline %s run -d`", name))
		return nil
	case r.state == runnerRunning:
		b.say(name + " is already running.")
		return nil
	}
	b.say("Starting " + name + "…")
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
		defer cancel()
		out, err := b.d.Daemon.Start(ctx, name)
		if err != nil {
			return b.report("", daemonErr("start "+name, out, err))
		}
		return b.report(lastLine(out, "Started "+name+"."), nil)
	}
}

// stopRunner stops a pipeline's runner through the CLI.
func stopRunner(b *base, name string, r runnerInfo) tea.Cmd {
	switch {
	case b.d.Daemon == nil:
		b.fail(fmt.Errorf("stopping runners is not available here; run `ymcp pipeline %s stop`", name))
		return nil
	case r.state == runnerStopped:
		b.say(name + " is not running.")
		return nil
	}
	b.say("Stopping " + name + "…")
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), daemonTimeout)
		defer cancel()
		out, err := b.d.Daemon.Stop(ctx, name)
		if err != nil {
			return b.report("", daemonErr("stop "+name, out, err))
		}
		return b.report(lastLine(out, "Stopped "+name+"."), nil)
	}
}

// daemonErr is the error of a CLI command, with the last line it printed.
func daemonErr(what, out string, err error) error {
	if line := lastLine(out, ""); line != "" {
		return fmt.Errorf("could not %s: %s", what, line)
	}
	return fmt.Errorf("could not %s: %w", what, err)
}

// lastLine is the last non-empty line of out, or def.
func lastLine(out, def string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "ymcp:")); l != "" {
			return l
		}
	}
	return def
}

// View renders the screen.
func (m *Pipelines) View() string {
	w := m.width
	waiting := 0
	for _, r := range m.rows {
		waiting += r.c.input
	}
	header := []string{spread(title("ymcp pipelines")+" "+theme.Bold.Render(m.profile), inputBadge(waiting), w), ""}
	help := []binding{
		{"enter", "board"}, {"n", "new"}, {"e", "edit"}, {"r", "run"}, {"s", "stop"},
		{"d", "delete"}, {"i", "inbox"}, {"esc", "back"}, {"q", "quit"},
	}
	var body []string
	switch {
	case !m.loaded:
		body = []string{theme.Dim.Render("  Loading…")}
	case len(m.rows) == 0:
		body = []string{"  No pipelines in " + m.profile + " yet.", "", "  Press " + theme.Key.Render("n") + " to create one."}
	default:
		body = m.table(w)
	}
	if m.err != nil {
		body = append(body, "", theme.Err.Render("  ! ")+oneLine(m.err.Error()))
	}
	return m.frame(header, body, help)
}

func (m *Pipelines) table(w int) []string {
	nameW := 4
	for _, r := range m.rows {
		nameW = max(nameW, len(r.p.Name))
	}
	nameW = min(nameW, max(w/4, 8))
	wide := w >= 72
	num := func(n int) string {
		if n == 0 {
			return theme.Dim.Render("·")
		}
		return fmt.Sprint(n)
	}
	var lines []string
	if wide {
		lines = append(lines, theme.Header.Render("  "+pad("NAME", nameW)+"  "+pad("RUNNER", 17)+"  "+pad("AGENT", 10)+
			"  "+padLeft("WORKING", 7)+"  "+padLeft("INPUT", 5)+"  "+padLeft("REVIEW", 6)+"  "+padLeft("DONE", 5)))
	}
	listRows := max(m.height-12, 3)
	top := scrollTo(0, m.sel, listRows)
	for i := top; i < len(m.rows) && i < top+listRows; i++ {
		r := m.rows[i]
		name := pad(r.p.Name, nameW)
		if i == m.sel {
			name = theme.Accent.Render(name)
		}
		var row string
		if wide {
			row = marker(i == m.sel) + name + "  " + pad(r.runner.label(), 17) + "  " + pad(r.p.Agent.Name, 10) +
				"  " + padLeft(num(r.c.working), 7) + "  " + padLeft(num(r.c.input), 5) + "  " + padLeft(num(r.c.review), 6) +
				"  " + padLeft(num(r.c.done), 5)
		} else {
			row = marker(i == m.sel) + name + "  " + r.runner.label()
		}
		lines = append(lines, spread(row, inputBadge(r.c.input), w))
	}
	if r, ok := m.selected(); ok {
		lines = append(lines, "")
		lines = append(lines, m.details(r, w)...)
	}
	return lines
}

// details describe the selected pipeline under the table.
func (m *Pipelines) details(r pipelineRow, w int) []string {
	p := r.p
	where := homeShort(p.Workspace.Dir, m.home)
	if p.Workspace.Host != "" {
		where = p.Workspace.Host + ":" + where
	}
	agent := p.Agent.Name
	if p.Agent.Model != "" {
		agent += " · " + p.Agent.Model
	}
	if p.Agent.Mode != "" {
		agent += " · " + p.Agent.Mode
	}
	runner := r.runner.label()
	switch r.runner.state {
	case runnerRunning:
		runner += theme.Dim.Render(fmt.Sprintf(" · pid %d on %s", r.runner.pid, r.runner.host))
	case runnerSilent:
		runner += theme.Dim.Render(fmt.Sprintf(" · pid %d on %s, last heartbeat %s ago", r.runner.pid, r.runner.host, short(m.d.Now().Sub(r.runner.beat))))
	}
	label := func(s string) string { return "    " + theme.Dim.Render(pad(s, 9)) }
	lines := []string{
		"  " + theme.Bold.Render(p.Name) + theme.Dim.Render(" · "+r.c.summary()),
		label("runner") + runner,
		label("source") + oneLine(p.Source.Query),
		label("repo") + where,
		label("publish") + strategyLabel(p.Strategy),
		label("agent") + agent,
	}
	if w < 50 {
		lines = lines[:2]
	}
	return lines
}

// strategyLabel says in words how finished work is published.
func strategyLabel(s spec.Strategy) string {
	label := map[string]string{
		"branch":          "push the branch",
		"github-pr":       "GitHub pull request",
		"github-issue-pr": "GitHub issue and pull request",
		"gitlab-mr":       "GitLab merge request",
	}[s.Kind]
	if label == "" {
		label = s.Kind
	}
	if s.Draft && s.Kind != "branch" {
		label += " (draft)"
	}
	return label
}

// pipelineByName finds a pipeline among the listed ones.
func pipelineByName(list []spec.Pipeline, name string) (spec.Pipeline, bool) {
	for _, p := range list {
		if p.Name == name {
			return p, true
		}
	}
	return spec.Pipeline{}, false
}
