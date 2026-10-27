// Package wizard is the pipeline wizard of the ymcp TUI: a step-by-step
// form that creates or edits a pipeline. Every choice comes with live
// help from YouTrack and the machine: how many issues a query matches and
// which, completions of queries, commands and logins, command checks,
// repository and agent checks, a preview of the prompts on a real issue,
// and finally the full pipeline check.
//
// The wizard never writes files. It ends with a Done message carrying the
// pipeline (the receiver saves it, renames the old file when an edited
// pipeline got a new name, and starts the runner when RunNow is set) or
// with Cancelled.
package wizard

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// Done is sent when the user saves the pipeline. The receiver writes it
// with spec.Save and, when RunNow is set, starts its runner in the
// background.
type Done struct {
	Pipeline spec.Pipeline
	RunNow   bool
}

// Cancelled is sent when the user leaves the wizard without saving.
type Cancelled struct{}

// debounceDelay is how long typing must pause before the wizard asks
// YouTrack or the machine about it.
const debounceDelay = 350 * time.Millisecond

// shared is the state every step sees: the draft pipeline and what is
// known about its YouTrack.
type shared struct {
	ctx   context.Context
	deps  Deps
	p     *spec.Pipeline
	orig  string // name of the edited pipeline; "" for a new one
	delay time.Duration
	ids   int

	conn   connection
	sample sample
	width  int
	height int
}

func (sh *shared) newID() int {
	sh.ids++
	return sh.ids
}

// connection is the YouTrack sign-in of the chosen profile.
type connection struct {
	profile string
	yt      YouTrack
	login   string
	err     error
	pending bool
}

// sample is what the source query matches right now.
type sample struct {
	query   string
	issues  []ytwatch.Issue
	err     error
	pending bool
	done    bool
}

// key is the issue the wizard validates commands and renders prompts on.
func (s sample) key() string {
	if len(s.issues) == 0 {
		return ""
	}
	return s.issues[0].Key
}

type connMsg struct {
	profile string
	yt      YouTrack
	login   string
	err     error
}

// connect signs in to the profile's YouTrack unless that is done or under way.
func (sh *shared) connect(profile string) tea.Cmd {
	if profile == "" || (sh.conn.profile == profile && (sh.conn.pending || sh.conn.yt != nil)) {
		return nil
	}
	sh.conn = connection{profile: profile, pending: true}
	ctx, f := sh.ctx, sh.deps.YouTrack
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		yt, err := f.Connect(ctx, profile)
		var login string
		if err == nil {
			login, err = yt.Me(ctx)
		}
		if err != nil {
			yt = nil
		}
		return connMsg{profile: profile, yt: yt, login: login, err: err}
	}
}

// step is one page of the wizard.
type step interface {
	title() string
	// enter is called when the step is shown.
	enter() tea.Cmd
	// leave is called when another step is shown.
	leave()
	// key handles a key while the step is shown; consumed is false for
	// keys the wizard may use (esc).
	key(msg tea.KeyMsg) (cmd tea.Cmd, consumed bool)
	// msg handles every other message, shown or not.
	msg(msg tea.Msg) tea.Cmd
	view(width, height int) string
	help() string
	// mark is the status mark in the step list.
	mark() string
}

// navMsg moves to another step: by delta, or to index when to >= 0.
type navMsg struct{ delta, to int }

func next() tea.Msg { return navMsg{delta: 1, to: -1} }

// Model is the wizard. Make it with New.
type Model struct {
	sh      *shared
	steps   []step
	cur     int
	confirm bool // asking whether to discard the draft
	// seen marks the steps shown so far; a new pipeline shows no status
	// for steps the user hasn't reached.
	seen []bool
}

// New returns the wizard for a new pipeline (existing nil) or for editing
// existing, starting from its values.
func New(ctx context.Context, deps Deps, existing *spec.Pipeline) tea.Model {
	sh := &shared{ctx: ctx, deps: deps, delay: debounceDelay, width: 100, height: 30}
	var p spec.Pipeline
	if existing != nil {
		p = clonePipeline(*existing).WithDefaults()
		sh.orig = existing.Name
	} else {
		p = newPipeline()
	}
	sh.p = &p
	m := &Model{sh: sh}
	m.steps = []step{
		newNameStep(sh),
		newSourceStep(sh),
		newStagesStep(sh),
		newWorkspaceStep(sh),
		newAgentStep(sh),
		newStrategyStep(sh),
		newFeedbackStep(sh),
		newPromptsStep(sh),
		newNotifyStep(sh),
		newReviewStep(sh),
	}
	m.seen = make([]bool, len(m.steps))
	m.seen[0] = true
	return m
}

// newPipeline is the draft of a new pipeline: stage commands that work in
// YouTrack's default workflow, a claude agent that may edit and commit,
// the default preset.
func newPipeline() spec.Pipeline {
	p := spec.Pipeline{
		Stages: spec.Stages{
			Working:    spec.Stage{Enter: "State In Progress", Match: "State: {In Progress}"},
			Review:     spec.Stage{Enter: "State Fixed", Match: "State: Fixed"},
			NeedsInput: spec.Stage{Enter: "tag needs-input", Exit: "untag needs-input", Match: "tag: needs-input"},
		},
		Agent:   claudeDefaults(),
		Prompts: spec.Prompts{Preset: spec.DefaultPreset},
	}.WithDefaults()
	if pr, ok := spec.LookupPreset(spec.DefaultPreset); ok {
		p.Prompts.Start, p.Prompts.Feedback, p.Prompts.Answer = pr.Start, pr.Feedback, pr.Answer
	}
	return p
}

// claudeDefaults let a new claude agent work unattended, running every tool
// without asking, like `ymcp pipeline new`.
func claudeDefaults() spec.Agent {
	return spec.Agent{
		Provider: "claude",
		Args:     []string{"--dangerously-skip-permissions"},
	}
}

func clonePipeline(p spec.Pipeline) spec.Pipeline {
	// A JSON round trip copies every slice, map and pointer.
	data, err := json.Marshal(p)
	if err != nil {
		return p
	}
	var out spec.Pipeline
	if json.Unmarshal(data, &out) != nil {
		return p
	}
	out.Messages = maps.Clone(p.Messages)
	return out
}

// Init starts the first step and signs in to the profile of an edited
// pipeline.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.steps[0].enter(), m.sh.connect(m.sh.p.Profile))
}

// Update handles a message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.sh.width, m.sh.height = msg.Width, msg.Height
		return m, m.broadcast(msg)
	case tea.KeyMsg:
		return m, m.key(msg)
	case navMsg:
		to := msg.to
		if to < 0 {
			to = m.cur + msg.delta
		}
		return m, m.goTo(to)
	case connMsg:
		if msg.profile == m.sh.p.Profile {
			m.sh.conn = connection{profile: msg.profile, yt: msg.yt, login: msg.login, err: msg.err}
			m.sh.sample = sample{}
		}
		return m, m.broadcast(msg)
	}
	return m, m.broadcast(msg)
}

func (m *Model) broadcast(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	for _, s := range m.steps {
		cmds = append(cmds, s.msg(msg))
	}
	return tea.Batch(cmds...)
}

func (m *Model) key(msg tea.KeyMsg) tea.Cmd {
	if msg.String() == "ctrl+c" {
		return func() tea.Msg { return Cancelled{} }
	}
	if m.confirm {
		switch msg.String() {
		case "y", "Y":
			return func() tea.Msg { return Cancelled{} }
		case "n", "N", "esc":
			m.confirm = false
		}
		return nil
	}
	switch msg.String() {
	case "ctrl+n", "pgdown":
		return m.goTo(m.cur + 1)
	case "ctrl+p", "pgup":
		return m.goTo(m.cur - 1)
	}
	cmd, consumed := m.steps[m.cur].key(msg)
	if consumed {
		return cmd
	}
	if msg.String() == "esc" {
		m.confirm = true
	}
	return cmd
}

func (m *Model) goTo(i int) tea.Cmd {
	if i < 0 || i >= len(m.steps) || i == m.cur {
		return nil
	}
	m.steps[m.cur].leave()
	m.cur = i
	m.seen[i] = true
	return m.steps[i].enter()
}

// sidebarWidth is the width of the step list.
const sidebarWidth = 22

// View draws the wizard.
func (m *Model) View() string {
	w, h := max(m.sh.width, 60), max(m.sh.height, 16)
	title := "New pipeline"
	if m.sh.orig != "" {
		title = "Edit pipeline " + m.sh.orig
	}
	head := styleTitle.Render(title)
	if p := m.sh.p; p.Name != "" || p.Profile != "" {
		head += " " + styleDim.Render(strings.Trim(p.Name+" · "+p.Profile, " ·"))
	}
	switch c := m.sh.conn; {
	case c.pending:
		head += "  " + markPending + styleDim.Render(" signing in")
	case c.yt != nil:
		head += "  " + markOK + styleDim.Render(" "+c.login+" @ "+c.yt.BaseURL())
	case c.err != nil:
		head += "  " + markErr + styleDim.Render(" not signed in")
	}

	bodyH := h - 4
	var side []string
	for i, s := range m.steps {
		mark := s.mark()
		if !m.seen[i] && m.sh.orig == "" {
			mark = markOff
		}
		label := fmt.Sprintf("%2d %s %s", i+1, mark, s.title())
		if i == m.cur {
			label = styleFocus.Render("▸") + label
		} else {
			label = " " + label
		}
		side = append(side, label)
	}
	side = append(side, "", styleDim.Render(" ctrl+n next"), styleDim.Render(" ctrl+p back"), styleDim.Render(" esc   cancel"))
	sideView := lipgloss.NewStyle().Width(sidebarWidth).Height(bodyH).Render(fit(strings.Join(side, "\n"), sidebarWidth, bodyH))
	sep := styleBorder.Render(strings.TrimRight(strings.Repeat("│\n", bodyH), "\n"))
	cw := w - sidebarWidth - 3
	content := fit(m.steps[m.cur].view(cw, bodyH), cw, bodyH)
	body := lipgloss.JoinHorizontal(lipgloss.Top, sideView, sep, " ", content)

	help := m.steps[m.cur].help()
	if m.confirm {
		help = styleWarn.Render("Discard this draft?") + "  " + keyHelp("y", "discard", "n", "keep editing")
	}
	return fit(head, w, 1) + "\n\n" + body + "\n\n" + fit(help, w, 1)
}

// stageList gives the stages of a pipeline by index, in display order.
func stageList(p *spec.Pipeline) []*spec.Stage {
	return []*spec.Stage{&p.Stages.Working, &p.Stages.Review, &p.Stages.NeedsInput, &p.Stages.Done, &p.Stages.Closed}
}

// projectOf is the project short name of a readable issue id: YPC of YPC-12.
func projectOf(key string) string {
	i := strings.LastIndex(key, "-")
	if i <= 0 {
		return ""
	}
	return key[:i]
}
