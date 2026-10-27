package wizard

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/tui/keys"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
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
	"kimi":   {"yolo", "auto", "default", "plan"},
}

var permissionModes = []option{
	{value: "ask", title: "ask", desc: "The run waits in Needs input until you decide."},
	{value: "allow", title: "allow", desc: "Every permission request is approved."},
	{value: "deny", title: "deny", desc: "Every request is rejected; the agent goes on without the tool."},
}

var languages = []string{
	"English", "Russian", "German", "French", "Spanish", "Portuguese", "Italian", "Dutch", "Polish",
	"Ukrainian", "Turkish", "Chinese", "Japanese", "Korean",
}

var promptNames = []string{"start", "feedback", "answer"}

var promptAbout = []string{
	"the first prompt of a run",
	"sent when review feedback arrives",
	"sent when someone answers the agent's question",
}

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

type issueMsg struct {
	key   string
	issue ytwatch.Issue
	err   error
}

type editedMsg struct {
	which int
	text  string
	err   error
}

// agentStep picks the agent (each provider checked on this machine), its
// mode, model and arguments, the permission policy and timeouts, and the
// prompts: a preset, the public language, edits in $EDITOR and a preview
// on an issue of the source. It can save the prompts as a preset.
type agentStep struct {
	base
	all      []*item
	advanced bool

	drafts  map[string]spec.Agent
	results map[string]*agentResult
	deb     debouncer

	command, mode, model, lang     *assistField
	silence, total, permissionWait *assistField
	args                           *areaCtl
	durErr                         map[string]string

	presets *presetCtl
	prompts *promptsCtl
	all_    []spec.Preset // built-in, then the user's
	listErr error
	save    *saveForm // "save as preset", while open
	notice  string    // what the last save did

	issueKey     string
	issue        *ytwatch.Issue
	issueErr     error
	issuePending bool
	editErr      error
}

func newAgentStep(sh *shared) *agentStep {
	s := &agentStep{
		base: base{sh: sh}, drafts: map[string]spec.Agent{}, results: map[string]*agentResult{},
		deb: debouncer{id: sh.newID()}, durErr: map[string]string{},
	}
	for _, p := range agent.Providers {
		s.drafts[p] = spec.Agent{Provider: p}
		s.results[p] = &agentResult{deb: debouncer{id: sh.newID()}}
	}
	if sh.p.Agent.Provider != "claude" {
		s.drafts["claude"] = claudeDefaults()
	}
	if sh.p.Agent.Provider != "kimi" {
		s.drafts["kimi"] = kimiDefaults()
	}
	s.drafts[sh.p.Agent.Provider] = sh.p.Agent
	s.command = newAssist(sh, "", nil)
	s.mode = newAssist(sh, "the agent's default", nil)
	s.model = newAssist(sh, "the agent's default", nil)
	s.args = newArea("none", 3)
	s.lang = newAssist(sh, spec.DefaultLanguage, staticSuggest(languages))
	s.lang.set(sh.p.Language)
	s.silence = newAssist(sh, fmtDuration(spec.DefaultSilence), staticSuggest([]string{"5m", "10m", "20m", "30m"}))
	s.total = newAssist(sh, fmtDuration(spec.DefaultTotal), staticSuggest([]string{"1h", "2h", "4h"}))
	s.permissionWait = newAssist(sh, fmtDuration(spec.DefaultPermission), staticSuggest([]string{"10m", "30m", "1h", "4h"}))
	t := sh.p.Timeouts
	s.silence.set(fmtDuration(t.Silence.Duration))
	s.total.set(fmtDuration(t.Total.Duration))
	s.permissionWait.set(fmtDuration(t.Permission.Duration))
	s.load()
	s.reload()
	s.presets = &presetCtl{s: s}
	s.presets.cur = s.presetIndex(sh.p.Prompts.Preset)
	s.prompts = &promptsCtl{s: s}

	t0 := sh.p.Timeouts
	s.advanced = (sh.p.Agent.Command != "" && sh.p.Agent.Provider != "acp") || sh.p.Agent.Mode != "" || sh.p.Agent.Model != "" ||
		!sh.p.ServiceEnabled() || t0.Silence.Duration != spec.DefaultSilence || t0.Total.Duration != spec.DefaultTotal ||
		t0.Permission.Duration != spec.DefaultPermission

	changed := func() tea.Cmd {
		s.store()
		return s.deb.bump(sh.delay)
	}
	s.args.onChange = changed
	adv := func() bool { return !s.advanced && len(s.durErr) == 0 }
	s.all = []*item{
		{
			id: "provider", label: "Agent",
			desc: "The coding agent that does the work, looked for on this machine.",
			ctl: &choiceCtl{
				segment: true,
				opts:    s.providerOptions,
				get:     s.provider,
				set: func(v string) tea.Cmd {
					s.switchTo(v)
					return s.check(v)
				},
			},
			below: s.warnings,
		},
		{
			id: "command", label: "Command",
			desc:   "The command that starts the ACP agent, with its arguments.",
			hidden: func() bool { return s.provider() != "acp" },
			ctl:    newText(s.command, changed),
			problem: func() string {
				if s.provider() == "acp" && s.command.value() == "" && s.fs.tried {
					return "An ACP agent needs the command that starts it."
				}
				return ""
			},
		},
		{
			id: "permissions", label: "Permission requests",
			desc: "What happens when the agent asks to use a tool.",
			ctl: &choiceCtl{
				segment: true,
				opts:    func() []option { return permissionModes },
				get:     func() string { return sh.p.Permissions },
				set:     func(v string) tea.Cmd { sh.p.Permissions = v; return nil },
			},
		},
		{
			id: "preset", label: "Prompt preset",
			descFn: s.presets.about,
			ctl:    s.presets,
			below:  s.presets.below,
		},
		{
			id: "prompts", label: "Prompts", labelNote: s.prompts.status,
			desc:  "enter edits the highlighted one in $EDITOR; s saves all as a preset.",
			ctl:   s.prompts,
			below: s.prompts.below,
		},
		{
			id: "advanced", inline: true,
			ctl: &disclosureCtl{label: "Advanced: mode, model, arguments, language, timeouts", open: &s.advanced, force: func() bool { return len(s.durErr) > 0 }, note: func() string {
				if len(s.durErr) > 0 {
					return markErr + styleErr.Render(" a field here is wrong")
				}
				return ""
			}},
		},
		{
			id: "mode", label: "Mode", hidden: adv,
			desc: "The agent's own mode, e.g. acceptEdits. Empty: its default.",
			ctl:  newText(s.mode, changed),
		},
		{
			id: "model", label: "Model", hidden: adv,
			desc: "Empty: the agent's default model.",
			ctl:  newText(s.model, changed),
		},
		{
			id: "args", label: "Extra arguments", hidden: adv,
			desc: "Passed to the agent as they are, one per line.",
			ctl:  s.args,
		},
		{
			id: "command2", label: "Command", hidden: func() bool { return adv() || s.provider() == "acp" },
			desc: "The agent's executable, when it isn't the usual one on the PATH.",
			ctl:  newText(s.command, changed),
		},
		{
			id: "language", label: "Language", hidden: adv,
			desc: "Of the agent's public messages: comments and PR texts.",
			ctl:  newText(s.lang, func() tea.Cmd { sh.p.Language = s.lang.value(); return nil }),
		},
		{
			id: "service", inline: true, hidden: adv,
			desc: "Tells the agent how to ask, commit and answer. Keep it on.",
			ctl: &toggleCtl{
				label: "Add ymcp's service instruction",
				get:   sh.p.ServiceEnabled,
				set:   func(v bool) tea.Cmd { sh.p.Service = &v; return nil },
			},
		},
		{
			id: "silence", label: "Silence timeout", hidden: adv,
			desc:    "No output from the agent this long: the run counts as stuck.",
			ctl:     newText(s.silence, func() tea.Cmd { s.storeTimeouts(); return nil }),
			problem: func() string { return s.durErr["silence"] },
		},
		{
			id: "total", label: "Turn timeout", hidden: adv,
			desc:    "The longest one turn of the agent may take.",
			ctl:     newText(s.total, func() tea.Cmd { s.storeTimeouts(); return nil }),
			problem: func() string { return s.durErr["total"] },
		},
		{
			id: "permissionWait", label: "Permission timeout", hidden: adv,
			desc:    "How long a permission request waits for a person.",
			ctl:     newText(s.permissionWait, func() tea.Cmd { s.storeTimeouts(); return nil }),
			problem: func() string { return s.durErr["permission"] },
		},
	}
	return s
}

func (s *agentStep) title() string { return "Agent" }
func (s *agentStep) intro() string {
	return "The coding agent, what it may do on its own, and the prompts it gets."
}
func (s *agentStep) items() []*item { return s.all }

func (s *agentStep) provider() string { return s.sh.p.Agent.Provider }

// load fills the inputs from the draft of the current provider.
func (s *agentStep) load() {
	a := s.sh.p.Agent
	s.command.set(a.Command)
	s.command.in.Placeholder = a.Provider
	if a.Provider == "acp" {
		s.command.in.Placeholder = "e.g. my-agent --acp"
	}
	s.mode.set(a.Mode)
	s.mode.src = nil
	if hints := modeHints[a.Provider]; hints != nil {
		s.mode.src = staticSuggest(hints)
	}
	s.model.set(a.Model)
	s.args.ta.SetValue(strings.Join(a.Args, "\n"))
}

// store copies the inputs into the draft.
func (s *agentStep) store() {
	a := &s.sh.p.Agent
	a.Command, a.Mode, a.Model = s.command.value(), s.mode.value(), s.model.value()
	a.Args = nil
	for _, l := range strings.Split(s.args.ta.Value(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			a.Args = append(a.Args, l)
		}
	}
	s.drafts[a.Provider] = *a
}

// storeTimeouts reads the three durations; an empty one is the default.
func (s *agentStep) storeTimeouts() {
	t := &s.sh.p.Timeouts
	for _, d := range []struct {
		key string
		f   *assistField
		v   *spec.Duration
		def time.Duration
	}{
		{"silence", s.silence, &t.Silence, spec.DefaultSilence},
		{"total", s.total, &t.Total, spec.DefaultTotal},
		{"permission", s.permissionWait, &t.Permission, spec.DefaultPermission},
	} {
		delete(s.durErr, d.key)
		v := d.f.value()
		if v == "" {
			d.v.Duration = d.def
			continue
		}
		dur, err := time.ParseDuration(v)
		if err != nil || dur <= 0 {
			s.durErr[d.key] = "Use a duration such as 30m, 2h or 90s."
			continue
		}
		d.v.Duration = dur
	}
}

// switchTo makes provider the pipeline's agent, keeping each provider's
// draft.
func (s *agentStep) switchTo(provider string) {
	s.drafts[s.provider()] = s.sh.p.Agent
	s.sh.p.Agent = s.drafts[provider]
	s.load()
}

func (s *agentStep) providerOptions() []option {
	var out []option
	for _, p := range agent.Providers {
		r := s.results[p]
		title, desc := p, providerAbout[p]
		switch {
		case r.pending:
			title, desc = p+" …", desc+" · checking"
		case r.err != nil:
			title, desc = p+" ✗", desc+" · "+r.err.Error()
		case r.done:
			title, desc = p+" ✓", desc+" · "+strings.TrimSpace(r.info.Version+" "+r.info.Path)
		}
		out = append(out, option{value: p, title: title, desc: desc})
	}
	return out
}

func (s *agentStep) warnings(width int) string {
	r := s.results[s.provider()]
	if !r.done || r.err != nil {
		return ""
	}
	var lines []string
	for _, w := range r.info.Warnings {
		lines = append(lines, markWarn+" "+oneLine(w, width-2))
	}
	return strings.Join(lines, "\n")
}

func (s *agentStep) enter() tea.Cmd {
	var cmds []tea.Cmd
	for _, p := range agent.Providers {
		if r := s.results[p]; !r.done && !r.pending {
			cmds = append(cmds, s.check(p))
		}
	}
	key := s.sh.sample.key()
	yt := s.sh.conn.yt
	if key != "" && yt != nil && key != s.issueKey {
		s.issueKey, s.issuePending, s.issue, s.issueErr = key, true, nil, nil
		ctx := s.sh.ctx
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(ctx, requestTimeout)
			defer cancel()
			is, err := yt.Issue(ctx, key)
			return issueMsg{key: key, issue: is, err: err}
		})
	}
	return tea.Batch(cmds...)
}

func (s *agentStep) leave() { s.save = nil }

// check runs agent.Check for a provider with its draft.
func (s *agentStep) check(provider string) tea.Cmd {
	r := s.results[provider]
	r.deb.seq++
	r.pending = true
	a := s.drafts[provider]
	if provider == s.provider() {
		a = s.sh.p.Agent
	}
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

func (s *agentStep) shortcut(msg tea.KeyMsg, focus string) (tea.Cmd, bool) {
	s.notice = ""
	if keys.Of(msg) == "s" && (focus == "preset" || focus == "prompts") {
		s.openSave()
		return nil, true
	}
	return nil, false
}

func (s *agentStep) extraHelp(focus string) string {
	if focus == "preset" || focus == "prompts" {
		return keyHelp("s", "save as preset")
	}
	return ""
}

func (s *agentStep) overlay() overlay {
	if s.save != nil {
		return saveOverlay{s}
	}
	return nil
}

func (s *agentStep) msg(msg tea.Msg) tea.Cmd {
	if s.deb.fired(msg) {
		return s.check(s.provider())
	}
	switch m := msg.(type) {
	case agentMsg:
		for _, r := range s.results {
			if r.deb.current(m.id, m.seq) {
				r.pending, r.done, r.info, r.err = false, true, m.info, m.err
			}
		}
		return nil
	case issueMsg:
		if m.key == s.issueKey {
			s.issuePending, s.issueErr = false, m.err
			if m.err == nil {
				is := m.issue
				s.issue = &is
			}
		}
		return nil
	case editedMsg:
		s.editErr = m.err
		if m.err == nil {
			if strings.TrimSpace(m.text) == "" {
				s.editErr = fmt.Errorf("the %s prompt is empty; kept the previous text", promptNames[m.which])
			} else {
				s.setText(m.which, m.text)
			}
		}
		return nil
	}
	var cmds []tea.Cmd
	for _, f := range []*assistField{s.command, s.mode, s.model, s.lang, s.silence, s.total, s.permissionWait} {
		cmds = append(cmds, f.msg(msg))
	}
	return tea.Batch(cmds...)
}

func (s *agentStep) blockers() []problem {
	var out []problem
	if s.provider() == "acp" && s.command.value() == "" {
		out = append(out, problem{"command", "the command of the ACP agent"})
	}
	for _, d := range []struct{ key, id string }{{"silence", "silence"}, {"total", "total"}, {"permission", "permissionWait"}} {
		if s.durErr[d.key] != "" {
			out = append(out, problem{d.id, "a valid " + d.key + " timeout"})
		}
	}
	for i, t := range s.texts() {
		if _, err := text.Render(t, nil); err != nil {
			out = append(out, problem{"prompts", "a valid " + promptNames[i] + " prompt"})
		}
	}
	return out
}

func (s *agentStep) mark() string {
	r := s.results[s.provider()]
	switch {
	case len(s.blockers()) > 0 || r.err != nil:
		return markBad
	case r.pending:
		return markPending
	case r.done && len(r.info.Warnings) > 0:
		return markWarn
	case s.sh.p.Language == "":
		return markWarn
	case r.done:
		return markOK
	}
	return markOff
}

// The prompts.

// reload reads the presets again.
func (s *agentStep) reload() {
	s.all_, s.listErr = s.sh.deps.Presets.List()
}

// lookup finds a preset, built-in or the user's, by name.
func (s *agentStep) lookup(name string) (spec.Preset, bool) {
	for _, p := range s.all_ {
		if p.Name == name {
			return p, true
		}
	}
	return spec.Preset{}, false
}

func (s *agentStep) presetIndex(name string) int {
	for i, p := range s.all_ {
		if p.Name == name {
			return i
		}
	}
	return 0
}

// texts are the three prompts, falling back to the preset.
func (s *agentStep) texts() []string {
	pr := s.sh.p.Prompts
	out := []string{pr.Start, pr.Feedback, pr.Answer}
	if base, ok := s.lookup(pr.Preset); ok {
		for i, t := range []string{base.Start, base.Feedback, base.Answer} {
			if out[i] == "" {
				out[i] = t
			}
		}
	}
	return out
}

func (s *agentStep) setText(which int, t string) {
	pr := &s.sh.p.Prompts
	switch which {
	case 0:
		pr.Start = t
	case 1:
		pr.Feedback = t
	case 2:
		pr.Answer = t
	}
}

// modified lists the prompts that differ from the preset.
func (s *agentStep) modified() []string {
	pr, ok := s.lookup(s.sh.p.Prompts.Preset)
	if !ok {
		return nil
	}
	var out []string
	for i, t := range s.texts() {
		if strings.TrimSpace(t) != strings.TrimSpace([]string{pr.Start, pr.Feedback, pr.Answer}[i]) {
			out = append(out, promptNames[i])
		}
	}
	return out
}

func (s *agentStep) usePreset(name string) {
	pr, ok := s.lookup(name)
	if !ok {
		return
	}
	s.sh.p.Prompts = spec.Prompts{Preset: pr.Name, Start: pr.Start, Feedback: pr.Feedback, Answer: pr.Answer}
	s.presets.confirm = ""
}

// edit opens a prompt in the editor.
func (s *agentStep) edit(which int) tea.Cmd {
	s.editErr = nil
	f, err := os.CreateTemp("", "ymcp-"+promptNames[which]+"-*.md")
	if err != nil {
		s.editErr = err
		return nil
	}
	path := f.Name()
	_, err = f.WriteString(s.texts()[which] + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		s.editErr = err
		return nil
	}
	return tea.ExecProcess(s.sh.deps.Editor.Command(path), func(err error) tea.Msg { return readEdited(which, path, err) })
}

// readEdited reads the edited prompt back and removes the file.
func readEdited(which int, path string, runErr error) tea.Msg {
	defer os.Remove(path)
	if runErr != nil {
		return editedMsg{which: which, err: fmt.Errorf("editor: %w", runErr)}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return editedMsg{which: which, err: err}
	}
	return editedMsg{which: which, text: strings.TrimRight(string(data), " \t\r\n")}
}

// vars are the template variables for the preview: the sample issue plus
// stand-ins for what only a real run knows.
func (s *agentStep) vars() text.Vars {
	p := s.sh.p
	is := ytwatch.Issue{Key: "ABC-1", Summary: "Example issue", URL: "https://youtrack.example/issue/ABC-1"}
	if s.issue != nil {
		is = *s.issue
	}
	v := text.Vars{
		"issue.id": is.Key, "issue.key": is.Key, "issue.dbid": is.ID, "issue.summary": is.Summary,
		"issue.description": is.Description, "issue.url": is.URL,
		"comments": "(the issue's comments)", "run.number": "1", "pipeline.name": p.Name,
		"language": p.Language, "pr.url": "none", "answer": "Use ISO 8601 dates.",
	}
	for k, f := range is.Fields {
		v["issue.fields."+k] = f
	}
	branch, _ := text.Render(p.Workspace.Branch, v)
	prefix, _ := text.Render(p.Workspace.CommitPrefix, v)
	v["branch"], v["commit.prefix"] = branch, prefix
	v["base"] = p.Workspace.Base
	if v["base"] == "" {
		v["base"] = "main"
	}
	who := s.sh.conn.login
	if who == "" {
		who = "reviewer"
	}
	v["feedback"] = text.RenderFeedback([]text.FeedbackItem{{N: 1, Source: "youtrack", Author: who, URL: is.URL, Body: "Please also handle an empty list."}})
	return v
}

// presetCtl browses the presets on one line; enter bases the prompts on
// the one under the cursor, asking first when that drops edits.
type presetCtl struct {
	s       *agentStep
	cur     int
	confirm string // preset waiting for a second enter
	focused bool
}

func (c *presetCtl) focus() { c.focused = true }
func (c *presetCtl) blur() {
	c.focused = false
	c.confirm = ""
	c.cur = c.s.presetIndex(c.s.sh.p.Prompts.Preset)
}

func (c *presetCtl) help() string {
	return keyHelp("←/→", "browse", "enter", "use preset")
}

func (c *presetCtl) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	s := c.s
	n := len(s.all_)
	switch msg.String() {
	case "left":
		c.cur = max(c.cur-1, 0)
		c.confirm = ""
		return nil, true
	case "right":
		c.cur = min(c.cur+1, max(n-1, 0))
		c.confirm = ""
		return nil, true
	case "enter", " ":
		if c.cur >= n {
			return nil, true
		}
		name := s.all_[c.cur].Name
		if name == s.sh.p.Prompts.Preset && len(s.modified()) == 0 {
			return nil, msg.String() == " "
		}
		if len(s.modified()) > 0 && c.confirm != name {
			c.confirm = name
			return nil, true
		}
		s.usePreset(name)
		return nil, true
	}
	return nil, false
}

func (c *presetCtl) view(width int, focused bool) string { return c.row(width, focused) }

// row lays the preset names out on one line of at most w cells: the
// built-in ones, then the user's after a bar, scrolled so that the one
// under the cursor shows.
func (c *presetCtl) row(w int, focused bool) string {
	s := c.s
	if len(s.all_) == 0 {
		return styleDim.Render("no presets")
	}
	cells := make([]string, len(s.all_))
	firstUser := true
	for i, pr := range s.all_ {
		n := " " + pr.Name + " "
		switch {
		case focused && i == c.cur:
			n = styleSel.Render(n)
		case pr.Name == s.sh.p.Prompts.Preset:
			n = styleFocus.Render("[" + pr.Name + "]")
		}
		if !pr.Builtin && firstUser {
			firstUser = false
			if i > 0 {
				n = styleBorder.Render("│") + n
			}
		}
		cells[i] = n
	}
	total := 0
	for _, x := range cells {
		total += lenVisible(x)
	}
	if total <= w {
		return strings.Join(cells, "")
	}
	cur := min(max(c.cur, 0), len(cells)-1)
	lo, hi := cur, cur+1
	used, room := lenVisible(cells[cur]), w-4 // "‹ " and " ›"
	for grew := true; grew; {
		grew = false
		if hi < len(cells) && used+lenVisible(cells[hi]) <= room {
			used += lenVisible(cells[hi])
			hi++
			grew = true
		}
		if lo > 0 && used+lenVisible(cells[lo-1]) <= room {
			lo--
			used += lenVisible(cells[lo])
			grew = true
		}
	}
	out := strings.Join(cells[lo:hi], "")
	if lo > 0 {
		out = styleDim.Render("‹ ") + out
	}
	if hi < len(cells) {
		out += styleDim.Render(" ›")
	}
	return out
}

// about is the rationale of the preset under the cursor, on one line.
func (c *presetCtl) about() string {
	s := c.s
	if c.cur >= len(s.all_) {
		return "Ready-made prompts for a kind of work."
	}
	pr := s.all_[c.cur]
	about := pr.Rationale
	if !pr.Builtin {
		about = strings.TrimSpace("your preset · " + about)
	}
	if about == "" {
		return "Ready-made prompts for a kind of work."
	}
	return about
}

// below says what the last action did or is about to do.
func (c *presetCtl) below(width int) string {
	s := c.s
	var lines []string
	if s.listErr != nil {
		lines = append(lines, markWarn+" "+styleDim.Render(oneLine(s.listErr.Error(), width-2)))
	}
	if c.confirm != "" {
		lines = append(lines, markWarn+" "+oneLine("Your edits to "+strings.Join(s.modified(), ", ")+" will be replaced. enter again uses "+c.confirm+".", width-2))
	}
	if s.notice != "" {
		lines = append(lines, markOK+" "+oneLine(s.notice, width-2))
	}
	return strings.Join(lines, "\n")
}

// promptsCtl lists the three prompts; enter edits the highlighted one in
// $EDITOR, r resets it to the preset. The highlighted one is previewed on
// an issue of the source.
type promptsCtl struct {
	s       *agentStep
	cur     int
	focused bool
}

func (c *promptsCtl) focus() { c.focused = true }
func (c *promptsCtl) blur()  { c.focused = false }

func (c *promptsCtl) help() string {
	return keyHelp("↑/↓", "prompt", "enter", "edit in $EDITOR", "r", "reset to preset")
}

func (c *promptsCtl) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	s := c.s
	switch keys.Of(msg) {
	case "up":
		c.cur = max(c.cur-1, 0)
		return nil, true
	case "down":
		c.cur = min(c.cur+1, len(promptNames)-1)
		return nil, true
	case "enter", "e":
		return s.edit(c.cur), true
	case "r":
		s.setText(c.cur, "")
		if pr, ok := s.lookup(s.sh.p.Prompts.Preset); ok {
			s.setText(c.cur, []string{pr.Start, pr.Feedback, pr.Answer}[c.cur])
		}
		return nil, true
	}
	return nil, false
}

func (c *promptsCtl) view(width int, focused bool) string {
	s := c.s
	var lines []string
	texts := s.texts()
	for i, n := range promptNames {
		rest := fmt.Sprintf(" %3d lines  %s", strings.Count(texts[i], "\n")+1, promptAbout[i])
		name := " " + padRight(n, 9)
		switch {
		case focused && i == c.cur:
			name = styleSel.Render(name)
		case i == c.cur:
			name = styleBold.Render(name)
		}
		lines = append(lines, name+styleDim.Render(cut(rest, max(width-11, 8))))
	}
	return strings.Join(lines, "\n")
}

// status says which preset the prompts are based on and which differ.
func (c *promptsCtl) status() string {
	s := c.s
	out := "based on " + s.sh.p.Prompts.Preset
	if mod := s.modified(); len(mod) > 0 {
		out += ", modified: " + strings.Join(mod, ", ")
	}
	return out
}

// below is the preview of the highlighted prompt while the prompts have
// the focus.
func (c *promptsCtl) below(width int) string {
	s := c.s
	var lines []string
	if s.editErr != nil {
		lines = append(lines, errLine(s.editErr.Error(), width))
	}
	if !c.focused {
		return strings.Join(lines, "\n")
	}
	on := "an example issue"
	switch {
	case s.issue != nil:
		on = s.issue.Key
	case s.issuePending:
		on = "an example issue (loading " + s.issueKey + "…)"
	case s.issueErr != nil:
		on = "an example issue (" + oneLine(s.issueErr.Error(), 40) + ")"
	}
	lines = append(lines, styleLabel.Render("Preview of "+promptNames[c.cur])+styleDim.Render(" on "+on))
	rendered, err := text.Render(s.texts()[c.cur], s.vars())
	if err != nil {
		return strings.Join(append(lines, errLine(err.Error(), width)), "\n")
	}
	const room = 6
	pl := strings.Split(rendered, "\n")
	for i, l := range pl {
		if i == room-1 && len(pl) > room {
			lines = append(lines, styleDim.Render(fmt.Sprintf("  … %d more lines", len(pl)-i)))
			break
		}
		lines = append(lines, styleBorder.Render("│ ")+oneLine(l, width-3))
	}
	return strings.Join(lines, "\n")
}

// saveForm asks for the name and the one-line rationale of a new preset.
type saveForm struct {
	name, why textinput.Model
	field     int // 0 name, 1 rationale
	err       error
}

// openSave opens the form that saves the current prompts as a preset.
func (s *agentStep) openSave() {
	s.presets.confirm = ""
	f := &saveForm{name: newInput("e.g. my-preset"), why: newInput("what these prompts are for (optional)")}
	f.name.CharLimit = 40
	f.name.Focus()
	s.save = f
}

func (f *saveForm) setField(i int) {
	f.field = i
	f.name.Blur()
	f.why.Blur()
	if i == 0 {
		f.name.Focus()
	} else {
		f.why.Focus()
	}
}

// saveOverlay is the "save as preset" form over the agent step.
type saveOverlay struct{ s *agentStep }

func (o saveOverlay) key(msg tea.KeyMsg) tea.Cmd {
	s := o.s
	f := s.save
	switch msg.String() {
	case "esc":
		s.save = nil
		return nil
	case "tab", "shift+tab", "up", "down":
		f.setField(1 - f.field)
		return nil
	case "enter":
		if f.field == 0 {
			if f.err = s.checkNewName(); f.err == nil {
				f.setField(1)
			}
			return nil
		}
		s.savePreset()
		return nil
	}
	var cmd tea.Cmd
	if f.field == 0 {
		f.name, cmd = f.name.Update(msg)
	} else {
		f.why, cmd = f.why.Update(msg)
	}
	f.err = nil
	return cmd
}

func (o saveOverlay) view(width, _ int) string {
	f := o.s.save
	var b strings.Builder
	b.WriteString(styleSection.Render("Save these prompts as a preset") + "\n")
	b.WriteString(dimLines(wrap("The three prompts as they are now become a preset of yours, and the pipeline is based on it.", width)) + "\n\n")
	for i, row := range []struct {
		l, d string
		in   *textinput.Model
	}{
		{"Name", "Lowercase letters, digits, - and _; not the name of another preset.", &f.name},
		{"About", "One line on what these prompts are for, shown when picking a preset.", &f.why},
	} {
		l := styleLabel.Render(row.l)
		if f.field == i {
			l = styleFocus.Render(row.l)
		}
		row.in.Width = max(boxWidth(width)-5, 5)
		b.WriteString(l + "\n" + styleDim.Render(row.d) + "\n" + inputBox(row.in.View(), width, f.field == i, i == 0 && f.err != nil) + "\n")
		if i == 0 && f.err != nil {
			b.WriteString(errLine(f.err.Error(), width) + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (o saveOverlay) help() string {
	if o.s.save.field == 0 {
		return keyHelp("enter", "next", "tab", "about", "esc", "cancel")
	}
	return keyHelp("enter", "save", "tab", "name", "esc", "cancel")
}

// checkNewName fails for an invalid name or one that is taken.
func (s *agentStep) checkNewName() error {
	name := strings.TrimSpace(s.save.name.Value())
	if err := spec.CheckPresetName(name); err != nil {
		return err
	}
	if _, ok := s.lookup(name); ok {
		return presetTaken(name)
	}
	return nil
}

// savePreset saves the current prompts as a new preset of the user's and
// bases the pipeline on it. An error stays in the form.
func (s *agentStep) savePreset() {
	f := s.save
	if f.err = s.checkNewName(); f.err != nil {
		f.setField(0)
		return
	}
	t := s.texts()
	p := spec.Preset{
		Name:      strings.TrimSpace(f.name.Value()),
		Rationale: strings.TrimSpace(f.why.Value()),
		Start:     t[0], Feedback: t[1], Answer: t[2],
	}
	if f.err = p.Validate(); f.err != nil {
		return
	}
	if f.err = s.sh.deps.Presets.Save(p); f.err != nil {
		return
	}
	s.sh.p.Prompts = spec.Prompts{Preset: p.Name, Start: p.Start, Feedback: p.Feedback, Answer: p.Answer}
	s.save = nil
	s.reload()
	s.presets.cur = s.presetIndex(p.Name)
	s.notice = "Saved preset " + p.Name + "; the pipeline is now based on it"
}

func fmtDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d%time.Hour == 0:
		return strings.TrimSuffix(d.String(), "0m0s")
	case d%time.Minute == 0:
		return strings.TrimSuffix(d.String(), "0s")
	}
	return d.String()
}
