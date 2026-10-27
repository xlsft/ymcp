package wizard

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
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
// $EDITOR and previews them rendered on an issue of the source. It can
// save the current prompts as a preset of the user's.
type promptsStep struct {
	sh      *shared
	presets *pickList
	all     []spec.Preset // built-in, then the user's
	listErr error
	lang    *assistField
	focus   int
	confirm string // preset waiting for a second enter

	save   *saveForm // "save as preset", while open
	notice string    // what the last save did

	issueKey     string
	issue        *ytwatch.Issue
	issueErr     error
	issuePending bool
	editErr      error
}

// saveForm asks for the name and the one-line rationale of a new preset.
type saveForm struct {
	name, why textinput.Model
	field     int // 0 name, 1 rationale
	err       error
}

func newPromptsStep(sh *shared) *promptsStep {
	s := &promptsStep{sh: sh}
	s.reload()
	s.lang = newAssist(sh, "Language of public messages", spec.DefaultLanguage, staticSuggest(languages))
	s.lang.set(sh.p.Language)
	return s
}

// reload reads the presets again and puts the cursor on the pipeline's.
func (s *promptsStep) reload() {
	s.all, s.listErr = s.sh.deps.Presets.List()
	items := make([]pickItem, 0, len(s.all))
	for _, p := range s.all {
		items = append(items, pickItem{title: p.Name, value: p.Name})
	}
	s.presets = newPickList(items, false)
	s.presets.selectValue(s.sh.p.Prompts.Preset)
}

// lookup finds a preset, built-in or the user's, by name.
func (s *promptsStep) lookup(name string) (spec.Preset, bool) {
	for _, p := range s.all {
		if p.Name == name {
			return p, true
		}
	}
	return spec.Preset{}, false
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

func (s *promptsStep) leave() {
	s.lang.blur()
	s.save = nil
}

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

func (s *promptsStep) usePreset(name string) {
	pr, ok := s.lookup(name)
	if !ok {
		return
	}
	s.sh.p.Prompts = spec.Prompts{Preset: pr.Name, Start: pr.Start, Feedback: pr.Feedback, Answer: pr.Answer}
	s.confirm = ""
}

func (s *promptsStep) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	k := msg.String()
	s.notice = ""
	if s.save != nil {
		return s.saveKey(msg), true
	}
	if k == "s" && s.focus != promptLanguage {
		s.openSave()
		return nil, true
	}
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
			if pr, ok := s.lookup(s.sh.p.Prompts.Preset); ok {
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

// openSave opens the form that saves the current prompts as a preset.
func (s *promptsStep) openSave() {
	s.lang.blur()
	s.confirm = ""
	f := &saveForm{name: newInput("my-preset"), why: newInput("what these prompts are for (optional)")}
	f.name.CharLimit = 40
	f.name.Focus()
	s.save = f
}

// saveKey handles a key while the save form is open.
func (s *promptsStep) saveKey(msg tea.KeyMsg) tea.Cmd {
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

// checkNewName fails for an invalid name or one that is taken.
func (s *promptsStep) checkNewName() error {
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
func (s *promptsStep) savePreset() {
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
	s.notice = "Saved preset " + p.Name + "; the pipeline is now based on it"
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
	b.WriteString(label(promptPreset, "Preset") + "  " + s.presetRow(width-lenVisible("Preset  ")) + "\n")
	if it, ok := s.presets.selected(); ok && s.save == nil {
		if pr, ok := s.lookup(it.value); ok {
			about := pr.Rationale
			if !pr.Builtin {
				about = strings.TrimSpace("your preset · " + about)
			}
			b.WriteString(styleDim.Render(wrap(oneLine(about, 3*(width-2)), width-2)) + "\n")
		}
	}
	if s.listErr != nil {
		b.WriteString(markWarn + " " + styleDim.Render(oneLine(s.listErr.Error(), width-4)) + "\n")
	}
	if s.confirm != "" {
		b.WriteString(markWarn + " " + wrap("Your edits to "+strings.Join(s.modified(), ", ")+" will be replaced. Press enter again to use "+s.confirm+".", width-2) + "\n")
	}
	if f := s.save; f != nil {
		b.WriteString(styleFocus.Render("Save these prompts as a preset") + "\n")
		f.name.Width, f.why.Width = max(width-12, 10), max(width-12, 10)
		for i, row := range []struct{ l, v string }{{"Name ", f.name.View()}, {"About", f.why.View()}} {
			l := styleLabel.Render(row.l)
			if f.field == i {
				l = styleFocus.Render(row.l)
			}
			b.WriteString("  " + l + "  " + row.v + "\n")
		}
		if f.err != nil {
			b.WriteString(markErr + " " + oneLine(f.err.Error(), width-4) + "\n")
		}
	}
	if s.notice != "" {
		b.WriteString(markOK + " " + oneLine(s.notice, width-4) + "\n")
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

// presetRow lays the preset names out on one line of at most w cells: the
// built-in ones, then the user's after a bar, scrolled so that the one
// under the cursor shows.
func (s *promptsStep) presetRow(w int) string {
	items := s.presets.items
	if len(items) == 0 {
		return styleDim.Render("no presets")
	}
	cells := make([]string, len(items))
	firstUser := true
	for i, it := range items {
		n := " " + it.title + " "
		switch {
		case s.focus == promptPreset && s.save == nil && i == s.presets.cur:
			n = styleSel.Render(n)
		case it.value == s.sh.p.Prompts.Preset:
			n = styleFocus.Render("[" + it.title + "]")
		}
		if pr, ok := s.lookup(it.value); ok && !pr.Builtin && firstUser {
			firstUser = false
			if i > 0 {
				n = styleBorder.Render("│") + n
			}
		}
		cells[i] = n
	}
	total := 0
	for _, c := range cells {
		total += lenVisible(c)
	}
	if total <= w {
		return strings.Join(cells, "")
	}
	cur := min(max(s.presets.cur, 0), len(cells)-1)
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

func (s *promptsStep) help() string {
	if f := s.save; f != nil {
		if f.field == 0 {
			return keyHelp("enter", "next", "tab", "about", "esc", "cancel")
		}
		return keyHelp("enter", "save", "tab", "name", "esc", "cancel")
	}
	switch s.focus {
	case promptPreset:
		return keyHelp("←/→", "browse", "enter", "use preset", "s", "save as preset", "tab", "next")
	case promptStart, promptFeedback, promptAnswer:
		return keyHelp("enter", "edit in $EDITOR", "r", "reset to preset", "s", "save as preset", "tab", "next", "vars", "{{issue.id}} {{issue.summary}} {{issue.fields.X}} {{branch}} {{language}} | slug | trim N")
	case promptService:
		return keyHelp("space", "on/off", "enter", "continue", "s", "save as preset")
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
