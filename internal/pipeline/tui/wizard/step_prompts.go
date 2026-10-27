package wizard

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

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

// Focus slots of the prompts step.
const (
	promptPreset = iota
	promptLanguage
	promptStart
	promptFeedback
	promptAnswer
	promptService
	promptSlots
)

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

// promptsStep picks a preset, the public language, edits the prompts in
// $EDITOR and previews them rendered on an issue of the source.
type promptsStep struct {
	sh      *shared
	presets *pickList
	lang    *assistField
	focus   int
	confirm string // preset waiting for a second enter

	issueKey     string
	issue        *ytwatch.Issue
	issueErr     error
	issuePending bool
	editErr      error
}

func newPromptsStep(sh *shared) *promptsStep {
	s := &promptsStep{sh: sh}
	var items []pickItem
	for _, p := range spec.Presets() {
		items = append(items, pickItem{title: p.Name, value: p.Name})
	}
	s.presets = newPickList(items, false)
	s.presets.selectValue(sh.p.Prompts.Preset)
	s.lang = newAssist(sh, "Language of public messages", spec.DefaultLanguage, staticSuggest(languages))
	s.lang.set(sh.p.Language)
	return s
}

func (s *promptsStep) title() string { return "Prompts" }

func (s *promptsStep) enter() tea.Cmd {
	s.setFocus(s.focus)
	key := s.sh.sample.key()
	yt := s.sh.conn.yt
	if key == "" || yt == nil || key == s.issueKey {
		return nil
	}
	s.issueKey, s.issuePending, s.issue, s.issueErr = key, true, nil, nil
	ctx := s.sh.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		is, err := yt.Issue(ctx, key)
		return issueMsg{key: key, issue: is, err: err}
	}
}

func (s *promptsStep) leave() { s.lang.blur() }

func (s *promptsStep) setFocus(i int) {
	s.lang.blur()
	s.focus = i
	s.confirm = ""
	if i == promptLanguage {
		s.lang.focus()
	}
}

// texts are the three prompts, falling back to the preset.
func (s *promptsStep) texts() []string {
	r := s.sh.p.ResolvedPrompts()
	return []string{r.Start, r.Feedback, r.Answer}
}

func (s *promptsStep) setText(which int, t string) {
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
func (s *promptsStep) modified() []string {
	pr, ok := spec.LookupPreset(s.sh.p.Prompts.Preset)
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

func (s *promptsStep) usePreset(name string) {
	pr, ok := spec.LookupPreset(name)
	if !ok {
		return
	}
	s.sh.p.Prompts = spec.Prompts{Preset: pr.Name, Start: pr.Start, Feedback: pr.Feedback, Answer: pr.Answer}
	s.confirm = ""
}

func (s *promptsStep) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	k := msg.String()
	switch s.focus {
	case promptPreset:
		switch k {
		case "enter", " ":
			it, ok := s.presets.selected()
			if !ok {
				return nil, true
			}
			if it.value == s.sh.p.Prompts.Preset && len(s.modified()) == 0 {
				s.setFocus(promptLanguage)
				return nil, true
			}
			if len(s.modified()) > 0 && s.confirm != it.value {
				s.confirm = it.value
				return nil, true
			}
			s.usePreset(it.value)
			return nil, true
		case "tab", "down":
			s.setFocus(promptLanguage)
			return nil, true
		case "shift+tab", "up":
			return nil, true
		case "left", "h":
			s.presets.cur = max(s.presets.cur-1, 0)
			s.confirm = ""
			return nil, true
		case "right", "l":
			s.presets.cur = min(s.presets.cur+1, len(s.presets.items)-1)
			s.confirm = ""
			return nil, true
		}
		return nil, false
	case promptLanguage:
		cmd, consumed, changed := s.lang.key(msg)
		if changed {
			s.sh.p.Language = s.lang.value()
		}
		if consumed {
			return cmd, true
		}
	case promptStart, promptFeedback, promptAnswer:
		switch k {
		case "enter", "e":
			return s.edit(s.focus - promptStart), true
		case "r":
			// Back to the preset's text.
			s.setText(s.focus-promptStart, "")
			if pr, ok := spec.LookupPreset(s.sh.p.Prompts.Preset); ok {
				s.setText(s.focus-promptStart, []string{pr.Start, pr.Feedback, pr.Answer}[s.focus-promptStart])
			}
			return nil, true
		}
	case promptService:
		switch k {
		case " ", "x":
			on := !s.sh.p.ServiceEnabled()
			s.sh.p.Service = &on
			return nil, true
		case "enter":
			return next, true
		}
	}
	switch k {
	case "tab", "down":
		if s.focus == promptSlots-1 {
			return next, true
		}
		s.setFocus(s.focus + 1)
		return nil, true
	case "enter":
		s.setFocus(s.focus + 1)
		return nil, true
	case "shift+tab", "up":
		s.setFocus(max(s.focus-1, 0))
		return nil, true
	}
	return nil, false
}

// edit opens a prompt in the editor.
func (s *promptsStep) edit(which int) tea.Cmd {
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

func (s *promptsStep) msg(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
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
	return s.lang.msg(msg)
}

// vars are the template variables for the preview: the sample issue plus
// stand-ins for what only a real run knows.
func (s *promptsStep) vars() text.Vars {
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

func (s *promptsStep) view(width, height int) string {
	var b strings.Builder
	label := func(slot int, l string) string {
		if s.focus == slot {
			return styleFocus.Render(l)
		}
		return styleLabel.Render(l)
	}
	var names []string
	for _, it := range s.presets.items {
		n := " " + it.title + " "
		switch {
		case s.focus == promptPreset && it.value == s.presets.items[s.presets.cur].value:
			n = styleSel.Render(n)
		case it.value == s.sh.p.Prompts.Preset:
			n = styleFocus.Render("[" + it.title + "]")
		}
		names = append(names, n)
	}
	b.WriteString(label(promptPreset, "Preset") + "  " + strings.Join(names, "") + "\n")
	if it, ok := s.presets.selected(); ok {
		if pr, ok := spec.LookupPreset(it.value); ok {
			b.WriteString(styleDim.Render(wrap(oneLine(pr.Rationale, 3*(width-2)), width-2)) + "\n")
		}
	}
	if s.confirm != "" {
		b.WriteString(markWarn + " " + wrap("Your edits to "+strings.Join(s.modified(), ", ")+" will be replaced. Press enter again to use "+s.confirm+".", width-2) + "\n")
	}
	b.WriteString(s.lang.view(width, s.focus == promptLanguage) + "\n")

	status := "based on " + s.sh.p.Prompts.Preset
	if mod := s.modified(); len(mod) > 0 {
		status += ", modified: " + strings.Join(mod, ", ")
	}
	b.WriteString(styleLabel.Render("Prompts") + "  " + styleDim.Render(status) + "\n")
	texts := s.texts()
	for i, n := range promptNames {
		line := fmt.Sprintf("%-9s %3d lines  %s", n, strings.Count(texts[i], "\n")+1, promptAbout[i])
		if s.focus == promptStart+i {
			b.WriteString(styleFocus.Render("› ") + styleSel.Render(" "+n+" ") + styleDim.Render(strings.TrimPrefix(line, n)) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	box := "[x]"
	if !s.sh.p.ServiceEnabled() {
		box = "[ ]"
	}
	b.WriteString(label(promptService, box+" ymcp's service instruction") + styleDim.Render(" (how to ask, commit and answer; keep it on)") + "\n")
	if s.editErr != nil {
		b.WriteString(markErr + " " + oneLine(s.editErr.Error(), width-4) + "\n")
	}

	which := 0
	if s.focus >= promptStart && s.focus <= promptAnswer {
		which = s.focus - promptStart
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
	b.WriteString("\n" + styleLabel.Render("Preview of "+promptNames[which]) + styleDim.Render(" on "+on) + "\n")
	used := strings.Count(b.String(), "\n")
	rendered, err := text.Render(texts[which], s.vars())
	if err != nil {
		b.WriteString(markErr + " " + oneLine(err.Error(), width-4) + "\n")
		return b.String()
	}
	lines := strings.Split(rendered, "\n")
	room := max(height-used-1, 3)
	for i, l := range lines {
		if i == room-1 && len(lines) > room {
			b.WriteString(styleDim.Render(fmt.Sprintf("  … %d more lines", len(lines)-i)))
			break
		}
		b.WriteString(styleBorder.Render("│ ") + oneLine(l, width-3) + "\n")
	}
	return b.String()
}

func (s *promptsStep) help() string {
	switch s.focus {
	case promptPreset:
		return keyHelp("←/→", "browse", "enter", "use preset", "tab", "next")
	case promptStart, promptFeedback, promptAnswer:
		return keyHelp("enter", "edit in $EDITOR", "r", "reset to preset", "tab", "next", "vars", "{{issue.id}} {{issue.summary}} {{issue.fields.X}} {{branch}} {{language}} | slug | trim N")
	case promptService:
		return keyHelp("space", "on/off", "enter", "continue")
	}
	return keyHelp("tab", "next", "shift+tab", "back")
}

func (s *promptsStep) mark() string {
	for _, t := range s.texts() {
		if _, err := text.Render(t, nil); err != nil {
			return markErr
		}
	}
	if s.sh.p.Language == "" {
		return markWarn
	}
	return markOK
}
