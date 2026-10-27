package wizard

import (
	"context"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

var providerAbout = map[string]string{
	"claude":   "Claude Code CLI",
	"kimi":     "Kimi CLI over ACP",
	"opencode": "opencode over ACP",
	"acp":      "another ACP agent",
}

// modeHints are the known modes per provider; others take free text.
var modeHints = map[string][]string{
	"claude": {"acceptEdits", "default", "plan", "dontAsk", "bypassPermissions"},
}

var permissionModes = []struct{ name, about string }{
	{"ask", "a tool call the agent may not make on its own puts the run in Needs Input until you decide"},
	{"allow", "every permission request is approved"},
	{"deny", "every permission request is rejected; the agent goes on without the tool"},
}

// Focus slots of the agent step.
const (
	agentProvider = iota
	agentCommand
	agentMode
	agentModel
	agentArgs
	agentPerms
	agentSlots
)

type agentResult struct {
	deb     debouncer
	pending bool
	done    bool
	info    agent.Info
	err     error
}

type agentMsg struct {
	id, seq int
	info    agent.Info
	err     error
}

// agentStep picks the provider (each checked on this machine), its mode,
// model and arguments, and the permission policy.
type agentStep struct {
	sh      *shared
	drafts  map[string]spec.Agent
	results map[string]*agentResult
	focus   int

	command, mode, model *assistField
	args                 textarea.Model
	deb                  debouncer
}

func newAgentStep(sh *shared) *agentStep {
	s := &agentStep{sh: sh, drafts: map[string]spec.Agent{}, results: map[string]*agentResult{}, deb: debouncer{id: sh.newID()}}
	for _, p := range agent.Providers {
		s.drafts[p] = spec.Agent{Provider: p}
		s.results[p] = &agentResult{deb: debouncer{id: sh.newID()}}
	}
	if sh.p.Agent.Provider != "claude" {
		s.drafts["claude"] = claudeDefaults()
	}
	s.drafts[sh.p.Agent.Provider] = sh.p.Agent
	s.command = newAssist(sh, "Command", "", nil)
	s.mode = newAssist(sh, "Mode", "the agent's default", nil)
	s.model = newAssist(sh, "Model", "the agent's default", nil)
	s.args = textarea.New()
	s.args.ShowLineNumbers = false
	s.args.Prompt = "  "
	s.args.Placeholder = "one argument per line"
	s.args.Cursor.SetMode(cursor.CursorStatic)
	s.args.SetHeight(4)
	s.load()
	return s
}

func (s *agentStep) provider() string { return s.sh.p.Agent.Provider }

// load fills the inputs from the draft of the current provider.
func (s *agentStep) load() {
	a := s.sh.p.Agent
	s.command.set(a.Command)
	s.command.in.Placeholder = a.Provider
	if a.Provider == "acp" {
		s.command.in.Placeholder = "required: the command that starts the ACP server"
	}
	s.mode.set(a.Mode)
	s.mode.src = staticSuggest(modeHints[a.Provider])
	if modeHints[a.Provider] == nil {
		s.mode.src = nil
	}
	s.model.set(a.Model)
	s.args.SetValue(strings.Join(a.Args, "\n"))
}

// store copies the inputs into the draft.
func (s *agentStep) store() {
	a := &s.sh.p.Agent
	a.Command, a.Mode, a.Model = s.command.value(), s.mode.value(), s.model.value()
	a.Args = nil
	for _, l := range strings.Split(s.args.Value(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			a.Args = append(a.Args, l)
		}
	}
	s.drafts[a.Provider] = *a
}

func (s *agentStep) title() string { return "Agent" }

func (s *agentStep) enter() tea.Cmd {
	s.setFocus(s.focus)
	var cmds []tea.Cmd
	for _, p := range agent.Providers {
		if r := s.results[p]; !r.done && !r.pending {
			cmds = append(cmds, s.check(p))
		}
	}
	return tea.Batch(cmds...)
}

func (s *agentStep) leave() {
	s.command.blur()
	s.mode.blur()
	s.model.blur()
	s.args.Blur()
}

func (s *agentStep) setFocus(i int) {
	s.leave()
	s.focus = i
	switch i {
	case agentCommand:
		s.command.focus()
	case agentMode:
		s.mode.focus()
	case agentModel:
		s.model.focus()
	case agentArgs:
		s.args.Focus()
	}
}

// check runs agent.Check for a provider with its draft.
func (s *agentStep) check(provider string) tea.Cmd {
	r := s.results[provider]
	r.deb.seq++
	r.pending = true
	a := s.drafts[provider]
	if a.Provider == "acp" && a.Command == "" {
		r.pending, r.done, r.err, r.info = false, true, errNoCommand, agent.Info{}
		return nil
	}
	id, seq, ctx, ch := r.deb.id, r.deb.seq, s.sh.ctx, s.sh.deps.Agents
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		info, err := ch.Check(ctx, a)
		return agentMsg{id: id, seq: seq, info: info, err: err}
	}
}

type constError string

func (e constError) Error() string { return string(e) }

const errNoCommand = constError("set Command to start it")

func (s *agentStep) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch s.focus {
	case agentProvider:
		i := slices.Index(agent.Providers, s.provider())
		switch msg.String() {
		case "up", "k":
			if i > 0 {
				s.switchTo(agent.Providers[i-1])
			}
			return nil, true
		case "down", "j":
			if i < len(agent.Providers)-1 {
				s.switchTo(agent.Providers[i+1])
			}
			return nil, true
		case "tab", "enter":
			s.setFocus(agentCommand)
			return nil, true
		case "shift+tab":
			return nil, true
		}
		return nil, false
	case agentPerms:
		switch msg.String() {
		case "left", "right", " ", "h", "l":
			i := slices.IndexFunc(permissionModes, func(m struct{ name, about string }) bool { return m.name == s.sh.p.Permissions })
			d := 1
			if msg.String() == "left" || msg.String() == "h" {
				d = len(permissionModes) - 1
			}
			s.sh.p.Permissions = permissionModes[(max(i, 0)+d)%len(permissionModes)].name
			return nil, true
		case "tab", "enter", "down":
			return next, true
		case "shift+tab", "up":
			s.setFocus(agentArgs)
			return nil, true
		}
		return nil, false
	case agentArgs:
		switch msg.String() {
		case "tab":
			s.setFocus(agentPerms)
			return nil, true
		case "shift+tab":
			s.setFocus(agentModel)
			return nil, true
		case "esc":
			return nil, false
		}
		before := s.args.Value()
		var cmd tea.Cmd
		s.args, cmd = s.args.Update(msg)
		if s.args.Value() != before {
			s.store()
			cmd = tea.Batch(cmd, s.deb.bump(s.sh.delay))
		}
		return cmd, true
	}
	f := map[int]*assistField{agentCommand: s.command, agentMode: s.mode, agentModel: s.model}[s.focus]
	cmd, consumed, changed := f.key(msg)
	if changed {
		s.store()
		cmd = tea.Batch(cmd, s.deb.bump(s.sh.delay))
	}
	if consumed {
		return cmd, true
	}
	switch msg.String() {
	case "tab", "down", "enter":
		s.setFocus(s.focus + 1)
		return cmd, true
	case "shift+tab", "up":
		s.setFocus(s.focus - 1)
		return cmd, true
	}
	return cmd, false
}

// switchTo makes provider the pipeline's agent, keeping each provider's
// draft.
func (s *agentStep) switchTo(provider string) {
	s.drafts[s.provider()] = s.sh.p.Agent
	s.sh.p.Agent = s.drafts[provider]
	s.load()
}

func (s *agentStep) msg(msg tea.Msg) tea.Cmd {
	if s.deb.fired(msg) {
		return s.check(s.provider())
	}
	if m, ok := msg.(agentMsg); ok {
		for _, r := range s.results {
			if r.deb.current(m.id, m.seq) {
				r.pending, r.done, r.info, r.err = false, true, m.info, m.err
			}
		}
		return nil
	}
	return tea.Batch(s.command.msg(msg), s.mode.msg(msg), s.model.msg(msg))
}

func (s *agentStep) view(width, _ int) string {
	var b strings.Builder
	label := styleLabel.Render
	if s.focus == agentProvider {
		label = styleFocus.Render
	}
	b.WriteString(label("Provider") + styleDim.Render("  checked on this machine") + "\n")
	for _, p := range agent.Providers {
		r := s.results[p]
		status := ""
		switch {
		case r.pending:
			status = markPending + styleDim.Render(" checking")
		case r.err != nil:
			status = markErr + " " + styleDim.Render(oneLine(r.err.Error(), width-38))
		case r.done:
			v := oneLine(r.info.Version, 24)
			status = markOK + " " + v + " " + styleDim.Render(oneLine(r.info.Path, max(width-39-lenRunes(v), 8)))
		}
		name := p
		if p == s.provider() {
			name = styleFocus.Render("›") + styleSel.Render(p+" ")
		} else {
			name = " " + p + " "
		}
		b.WriteString("  " + padRight(name, 10) + " " + padRight(styleDim.Render(oneLine(providerAbout[p], 20)), 21) + " " + status + "\n")
	}
	if r := s.results[s.provider()]; r.done && r.err == nil {
		for _, w := range r.info.Warnings {
			b.WriteString("  " + markWarn + " " + oneLine(w, width-6) + "\n")
		}
	}
	b.WriteString(s.command.view(width, s.focus == agentCommand) + "\n")
	b.WriteString(s.mode.view(width, s.focus == agentMode) + "\n")
	b.WriteString(s.model.view(width, s.focus == agentModel) + "\n")
	argLabel := styleLabel.Render("Extra arguments")
	if s.focus == agentArgs {
		argLabel = styleFocus.Render("Extra arguments")
	}
	b.WriteString(argLabel + styleDim.Render("  one per line, passed to the agent as they are") + "\n")
	s.args.SetWidth(max(width-4, 20))
	if s.focus != agentArgs && len(s.sh.p.Agent.Args) > 0 {
		b.WriteString("  " + styleCode.Render(oneLine(strings.Join(s.sh.p.Agent.Args, " "), width-4)) + "\n")
	} else {
		b.WriteString(s.args.View() + "\n")
	}
	permLabel := styleLabel.Render("Permissions")
	if s.focus == agentPerms {
		permLabel = styleFocus.Render("Permissions")
	}
	var opts []string
	about := ""
	for _, m := range permissionModes {
		if m.name == s.sh.p.Permissions {
			opts = append(opts, styleFocus.Render("›")+styleSel.Render(m.name+" "))
			about = m.about
		} else {
			opts = append(opts, " "+m.name+" ")
		}
	}
	b.WriteString(permLabel + "  " + strings.Join(opts, " ") + "\n")
	b.WriteString(styleDim.Render(wrap(about, width-2)) + "\n")
	return b.String()
}

func padRight(s string, n int) string {
	if w := lenVisible(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func (s *agentStep) help() string {
	switch s.focus {
	case agentProvider:
		return keyHelp("↑/↓", "provider", "tab", "settings", "ctrl+n", "next step")
	case agentPerms:
		return keyHelp("←/→", "change", "enter", "continue", "shift+tab", "back")
	case agentArgs:
		return keyHelp("enter", "new line", "tab", "next", "shift+tab", "back")
	}
	return keyHelp("tab", "next", "shift+tab", "back", "ctrl+space", "suggestions")
}

func (s *agentStep) mark() string {
	r := s.results[s.provider()]
	switch {
	case r.pending:
		return markPending
	case r.err != nil:
		return markErr
	case r.done && len(r.info.Warnings) > 0:
		return markWarn
	case r.done:
		return markOK
	}
	return markOff
}
