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

	"github.com/xlsft/ymcp/internal/config"
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

	profiles    []config.Config
	profilesErr error

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

// step is one page of the wizard: a form.
type step interface {
	title() string
	// intro says in a sentence what the step is about.
	intro() string
	// enter is called when the step is shown.
	enter() tea.Cmd
	// leave is called when another step is shown.
	leave()
	// items are the fields of the form, hidden ones included.
	items() []*item
	// msg handles every message but keys, shown or not.
	msg(msg tea.Msg) tea.Cmd
	// overlay is the picker shown over the form, or nil.
	overlay() overlay
	// shortcut handles a key of the step before the focused field does;
	// focus is the id of the focused field or button.
	shortcut(msg tea.KeyMsg, focus string) (cmd tea.Cmd, consumed bool)
	// blockers are the errors that keep Next from leaving the step.
	blockers() []problem
	// mark is the status mark in the step list.
	mark() string
	form() *formState
}

// problem is an error of a field (id) that blocks Next.
type problem struct{ id, text string }

// overlay is a picker over the form; it gets every key until it closes.
type overlay interface {
	key(msg tea.KeyMsg) tea.Cmd
	view(width, height int) string
	help() string
}

// base gives a step the parts most steps share.
type base struct {
	sh *shared
	fs formState
}

func (b *base) form() *formState                            { return &b.fs }
func (b *base) overlay() overlay                            { return nil }
func (b *base) shortcut(tea.KeyMsg, string) (tea.Cmd, bool) { return nil, false }

// Buttons of the row under the form.
const (
	btnBack = "btn:back"
	btnNext = "btn:next"
	btnSave = "btn:save"
	btnRun  = "btn:run"
)

// Model is the wizard. Make it with New.
type Model struct {
	sh      *shared
	steps   []step
	cur     int
	confirm bool // asking whether to discard the draft
	// seen marks the steps shown so far; a new pipeline shows no status
	// for steps the user hasn't reached.
	seen []bool
	// initial is the draft as the wizard started, to tell whether
	// leaving loses anything.
	initial string
	// blocked is why the last Next or Save did not go on.
	blocked string
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
	sh.profiles, sh.profilesErr = deps.Local.Profiles()
	if p.Profile == "" && len(sh.profiles) > 0 {
		p.Profile = sh.profiles[0].Name
		for _, c := range sh.profiles {
			if c.Name == config.DefaultProfile {
				p.Profile = c.Name
			}
		}
	}
	m := &Model{sh: sh}
	m.steps = []step{
		newBasicsStep(sh),
		newBoardStep(sh),
		newRepoStep(sh),
		newAgentStep(sh),
		newPeopleStep(sh),
		newReviewStep(sh),
	}
	m.seen = make([]bool, len(m.steps))
	m.seen[0] = true
	if existing != nil {
		// Show what is wrong with a saved pipeline at once.
		for _, s := range m.steps {
			s.form().tried = true
		}
	}
	m.initial = m.snapshot()
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

// snapshot is the draft as JSON.
func (m *Model) snapshot() string {
	data, _ := json.Marshal(m.sh.p)
	return string(data)
}

// dirty reports whether the draft differs from what the wizard started
// with.
func (m *Model) dirty() bool { return m.snapshot() != m.initial }

// Init starts the first step and signs in to the profile.
func (m *Model) Init() tea.Cmd {
	cmd := m.steps[0].enter()
	m.syncFocus()
	return tea.Batch(cmd, m.sh.connect(m.sh.p.Profile))
}

// Update handles a message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.sh.width, m.sh.height = msg.Width, msg.Height
		return m, m.broadcast(msg)
	case tea.KeyMsg:
		cmd := m.key(msg)
		m.syncFocus()
		return m, cmd
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

func cancelled() tea.Msg { return Cancelled{} }

func (m *Model) last() bool { return m.cur == len(m.steps)-1 }

// buttons are the buttons under the current step, left to right.
func (m *Model) buttons() []string {
	var out []string
	if m.cur > 0 {
		out = append(out, btnBack)
	}
	if m.last() {
		return append(out, btnSave, btnRun)
	}
	return append(out, btnNext)
}

// primary is the button enter on the last field leads to.
func (m *Model) primary() string {
	if m.last() {
		return btnSave
	}
	return btnNext
}

// focusables are the ids that take the focus, in tab order.
func (m *Model) focusables() []string {
	var ids []string
	for _, it := range m.steps[m.cur].items() {
		if it.focusable() {
			ids = append(ids, it.id)
		}
	}
	return append(ids, m.buttons()...)
}

// focusID is the focused field or button; the first one when the focused
// field went away.
func (m *Model) focusID() string {
	fs := m.steps[m.cur].form()
	ids := m.focusables()
	for _, id := range ids {
		if id == fs.focus {
			return id
		}
	}
	fs.focus = ids[0]
	return fs.focus
}

func (m *Model) item(id string) *item {
	for _, it := range m.steps[m.cur].items() {
		if it.id == id && it.ctl != nil {
			return it
		}
	}
	return nil
}

// syncFocus focuses the focused control and blurs the others. A section
// forced open by an error stays open once the error is fixed, so the
// field being edited doesn't vanish.
func (m *Model) syncFocus() {
	for _, it := range m.steps[m.cur].items() {
		if d, ok := it.ctl.(*disclosureCtl); ok && d.force != nil && d.force() {
			*d.open = true
		}
	}
	focus := m.focusID()
	for _, it := range m.steps[m.cur].items() {
		if it.ctl == nil {
			continue
		}
		if it.id == focus && it.visible() {
			it.ctl.focus()
		} else {
			it.ctl.blur()
		}
	}
}

// move moves the focus by delta, round the form.
func (m *Model) move(delta int) {
	ids := m.focusables()
	focus := m.focusID()
	i := 0
	for j, id := range ids {
		if id == focus {
			i = j
		}
	}
	m.steps[m.cur].form().focus = ids[(i+delta+len(ids))%len(ids)]
}

// forward is enter in a field: the next field, or the primary button
// after the last field. It never changes the step.
func (m *Model) forward() {
	ids := m.focusables()
	focus := m.focusID()
	for j, id := range ids {
		if id == focus && j+1 < len(ids) {
			to := ids[j+1]
			if isButton(to) {
				to = m.primary()
			}
			m.steps[m.cur].form().focus = to
			return
		}
	}
}

func isButton(id string) bool { return strings.HasPrefix(id, "btn:") }

func (m *Model) key(msg tea.KeyMsg) tea.Cmd {
	k := msg.String()
	if k == "ctrl+c" {
		return cancelled
	}
	if m.confirm {
		switch k {
		case "y", "Y":
			return cancelled
		case "n", "N", "esc":
			m.confirm = false
		}
		return nil
	}
	st := m.steps[m.cur]
	if o := st.overlay(); o != nil {
		return o.key(msg)
	}
	switch k {
	case "ctrl+n":
		return m.next()
	case "ctrl+p":
		return m.back()
	}
	if strings.HasPrefix(k, "alt+") && len(k) == 5 && k[4] >= '1' && k[4] <= '9' {
		return m.jump(int(k[4] - '1'))
	}
	focus := m.focusID()
	if cmd, ok := st.shortcut(msg, focus); ok {
		return cmd
	}
	if isButton(focus) {
		switch k {
		case "enter", " ":
			return m.press(focus)
		case "left":
			m.move(-1)
			return nil
		case "right":
			if focus != m.buttons()[len(m.buttons())-1] {
				m.move(1)
			}
			return nil
		}
	} else if it := m.item(focus); it != nil {
		cmd, consumed := it.ctl.key(msg)
		if consumed {
			return cmd
		}
		if c := m.formKey(k); c != nil {
			return tea.Batch(cmd, c)
		}
		return cmd
	}
	return m.formKey(k)
}

// formKey handles the keys no field used.
func (m *Model) formKey(k string) tea.Cmd {
	switch k {
	case "tab":
		m.move(1)
	case "shift+tab":
		m.move(-1)
	case "enter":
		m.forward()
	case "esc":
		if !m.dirty() {
			return cancelled
		}
		m.confirm = true
	}
	return nil
}

// press activates a button.
func (m *Model) press(id string) tea.Cmd {
	switch id {
	case btnBack:
		return m.back()
	case btnNext:
		return m.next()
	case btnSave:
		return m.save(false)
	case btnRun:
		return m.save(true)
	}
	return nil
}

// next goes on to the next step unless the current one has errors.
func (m *Model) next() tea.Cmd {
	if m.last() {
		return nil
	}
	if !m.mayLeave() {
		return nil
	}
	return m.goTo(m.cur + 1)
}

// mayLeave reports whether the current step has no blocking errors; when
// it has, it says why at the buttons and focuses the first wrong field.
func (m *Model) mayLeave() bool {
	st := m.steps[m.cur]
	probs := st.blockers()
	if len(probs) == 0 {
		return true
	}
	st.form().tried = true
	var texts []string
	for _, p := range probs {
		texts = append(texts, p.text)
	}
	m.blocked = "needs: " + strings.Join(texts, "; ")
	for _, it := range st.items() {
		if it.id == probs[0].id && it.focusable() {
			st.form().focus = it.id
		}
	}
	return false
}

// back goes to the previous step; it always may.
func (m *Model) back() tea.Cmd {
	if m.cur == 0 {
		return nil
	}
	return m.goTo(m.cur - 1)
}

// jump goes to step i from the step list: back always, forward like Next.
func (m *Model) jump(i int) tea.Cmd {
	if i < 0 || i >= len(m.steps) || i == m.cur {
		return nil
	}
	if i > m.cur && !m.mayLeave() {
		return nil
	}
	return m.goTo(i)
}

// save ends the wizard with the pipeline, unless it is invalid.
func (m *Model) save(run bool) tea.Cmd {
	p := clonePipeline(*m.sh.p).WithDefaults()
	if err := p.Validate(); err != nil {
		m.blocked = "the pipeline isn't valid yet; see the list above"
		return nil
	}
	return func() tea.Msg { return Done{Pipeline: p, RunNow: run} }
}

func (m *Model) goTo(i int) tea.Cmd {
	m.steps[m.cur].leave()
	for _, it := range m.steps[m.cur].items() {
		if it.ctl != nil {
			it.ctl.blur()
		}
	}
	m.cur = i
	m.seen[i] = true
	m.blocked = ""
	cmd := m.steps[i].enter()
	m.syncFocus()
	return cmd
}

// sidebarWidth is the width of the step list.
const sidebarWidth = 22

// View draws the wizard.
func (m *Model) View() string {
	w, h := max(m.sh.width, 60), max(m.sh.height, 16)
	st := m.steps[m.cur]
	focus := m.focusID()

	bodyH := h - 3
	// The content column keeps a margin on both sides.
	cw := w - sidebarWidth - 3 - 2
	heading := styleBold.Render(st.title()) + styleDim.Render(fmt.Sprintf("  step %d of %d", m.cur+1, len(m.steps)))
	intro := []string{styleDim.Render(oneLine(st.intro(), cw))}
	viewH := bodyH - 4 - len(intro)
	var formView string
	if o := st.overlay(); o != nil {
		formView = o.view(cw, viewH)
	} else {
		formView = renderForm(st.items(), focus, cw, viewH, st.form())
	}
	content := heading + "\n" + strings.Join(intro, "\n") + "\n\n" + fit(formView, cw, viewH)
	content = lipgloss.NewStyle().Height(bodyH - 1).Render(fit(content, cw, bodyH-1))
	content += "\n" + m.buttonRow(focus, cw, st.overlay() == nil)

	sideView := lipgloss.NewStyle().Width(sidebarWidth).Height(bodyH).Render(fit(m.sidebar(), sidebarWidth, bodyH))
	sep := styleBorder.Render(strings.TrimRight(strings.Repeat("│\n", bodyH), "\n"))
	body := lipgloss.JoinHorizontal(lipgloss.Top, sideView, sep, "  ", content)

	return fit(m.header(), w, 1) + "\n\n" + fit(body, w, bodyH) + "\n" + fit(m.help(focus), w, 1)
}

func (m *Model) header() string {
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
	return head
}

// sidebar lists the steps with their marks, the current one highlighted.
func (m *Model) sidebar() string {
	lines := []string{styleDim.Render(" Steps")}
	for i, s := range m.steps {
		mark := s.mark()
		if !m.seen[i] && m.sh.orig == "" {
			mark = markOff
		}
		label := fmt.Sprintf("%d %s", i+1, s.title())
		if i == m.cur {
			label = styleSel.Render(" " + padRight(label, sidebarWidth-5) + " ")
		} else {
			label = " " + label
		}
		lines = append(lines, " "+mark+label)
	}
	lines = append(lines, "", styleDim.Render(" ✓ ready  ! check"), styleDim.Render(" ○ not done yet"), "", styleDim.Render(" alt+1…6 go to step"))
	return strings.Join(lines, "\n")
}

// buttonRow draws the buttons and, when Next is blocked, why.
func (m *Model) buttonRow(focus string, width int, active bool) string {
	label := map[string]string{btnBack: "‹ Back", btnNext: "Next ›", btnSave: "Save", btnRun: "Save & run"}
	probs := m.steps[m.cur].blockers()
	var cells []string
	if m.cur == 0 {
		cells = append(cells, styleButtonDisabled.Render(label[btnBack]))
	}
	for _, b := range m.buttons() {
		switch {
		case active && b == focus:
			cells = append(cells, styleButtonFocus.Render(label[b]))
		case b != btnBack && len(probs) > 0:
			cells = append(cells, styleButtonDisabled.Render(label[b]))
		default:
			cells = append(cells, styleButton.Render(label[b]))
		}
	}
	row := strings.Join(cells, "  ")
	switch {
	case m.blocked != "" && len(probs) > 0:
		row += "  " + styleErr.Render("✗ "+oneLine(m.blocked, max(width-lenVisible(row)-4, 10)))
	case len(probs) > 0:
		var texts []string
		for _, p := range probs {
			texts = append(texts, p.text)
		}
		row += "  " + styleDim.Render(oneLine("needs: "+strings.Join(texts, "; "), max(width-lenVisible(row)-2, 10)))
	}
	return row
}

// help is the key help of the focused field plus the keys of the form.
func (m *Model) help(focus string) string {
	if m.confirm {
		return styleWarn.Render("Discard your changes?") + "  " + keyHelp("y", "discard", "n", "keep editing")
	}
	if o := m.steps[m.cur].overlay(); o != nil {
		return o.help()
	}
	var parts []string
	if isButton(focus) {
		parts = append(parts, keyHelp("enter", "press", "←/→", "button"))
	} else if it := m.item(focus); it != nil {
		if h := it.ctl.help(); h != "" {
			parts = append(parts, h)
		}
	}
	if h, ok := m.steps[m.cur].(interface{ extraHelp(string) string }); ok {
		if e := h.extraHelp(focus); e != "" {
			parts = append(parts, e)
		}
	}
	parts = append(parts, keyHelp("tab", "next field", "ctrl+n/p", "next/back step", "esc", "cancel"))
	return strings.Join(parts, "   ")
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
