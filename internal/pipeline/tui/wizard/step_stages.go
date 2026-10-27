package wizard

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// stageDef describes one stage for the user.
type stageDef struct {
	name, key, about string
	enterLabel       string
	hasExit          bool
	matchLabel       string
}

var stageDefs = []stageDef{
	{
		name: "Working", key: "working",
		about:      "ymcp runs the command when it takes an issue, so it must take the issue out of the source query. The search recognises issues the agent is working on.",
		enterLabel: "Command when ymcp takes an issue",
		matchLabel: "Search for issues in work",
	},
	{
		name: "Review", key: "review",
		about:      "ymcp runs the command when the agent's work is pushed. The search recognises issues waiting for review.",
		enterLabel: "Command when the work is ready",
		matchLabel: "Search for issues in review",
	},
	{
		name: "Needs input", key: "needsInput",
		about:      "ymcp runs the command when the agent asks a question or a permission, and the exit command when someone answers.",
		enterLabel: "Command when the agent waits for you",
		hasExit:    true,
		matchLabel: "Search for issues waiting for you (optional)",
	},
	{
		name: "Done", key: "done",
		about:      "Optional, off by default. An issue that matches the search ends its run as done.",
		enterLabel: "Command when the work is merged",
		matchLabel: "Search for finished issues",
	},
	{
		name: "Closed", key: "closed",
		about:      "Humans close issues. An issue that matches the search ends its run as closed, e.g. State: {Won't fix}.",
		enterLabel: "Command (optional)",
		matchLabel: "Search for closed issues",
	},
}

// Focus slots of a stage.
const (
	slotTabs = iota
	slotEnter
	slotMatch
	slotExit
)

// cmdState is the live check of one command.
type cmdState struct {
	deb     debouncer
	pending bool
	done    bool
	on      string // issue it was checked on
	check   ytwatch.CommandCheck
	err     error
}

// matchState is the live result of a stage search. For the working
// stage, both are the issues that match the source query too.
type matchState struct {
	deb     debouncer
	pending bool
	done    bool
	issues  []ytwatch.Issue
	err     error
	both    []ytwatch.Issue
	bothErr error
	bothRan bool
}

type stageForm struct {
	def                stageDef
	enter, match, exit *assistField
	enterChk, exitChk  cmdState
	matches            matchState
}

type cmdMsg struct {
	id, seq int
	on      string
	check   ytwatch.CommandCheck
	err     error
}

type matchMsg struct {
	id, seq int
	issues  []ytwatch.Issue
	err     error
	both    []ytwatch.Issue
	bothErr error
	bothRan bool
}

type fieldsMsg struct {
	project string
	fields  []Field
	err     error
}

type tagsMsg struct {
	tags []string
	err  error
}

// stagesStep edits the five stages. Each command and search is checked as
// it is typed, and a picker builds them from the project's fields.
type stagesStep struct {
	sh    *shared
	forms []*stageForm
	sel   int
	focus int

	picker *fieldPicker

	fieldsProject string
	fields        []Field
	fieldsErr     error
	fieldsPending bool
	tags          []string
	tagsErr       error
	tagsState     int // 0 not loaded, 1 loading, 2 loaded

	checkedOn string // sample issue + profile the commands were last checked against
}

func newStagesStep(sh *shared) *stagesStep {
	s := &stagesStep{sh: sh}
	stages := stageList(sh.p)
	for i, d := range stageDefs {
		f := &stageForm{def: d}
		f.enter = newAssist(sh, d.enterLabel, "e.g. State In Progress", commandSuggest(sh))
		f.match = newAssist(sh, d.matchLabel, "e.g. State: {In Progress}", searchSuggest(sh))
		f.enter.set(stages[i].Enter)
		f.match.set(stages[i].Match)
		if d.hasExit {
			f.exit = newAssist(sh, "Command when someone answers", "e.g. untag needs-input", commandSuggest(sh))
			f.exit.set(stages[i].Exit)
		}
		f.enterChk.deb = debouncer{id: sh.newID()}
		f.exitChk.deb = debouncer{id: sh.newID()}
		f.matches.deb = debouncer{id: sh.newID()}
		s.forms = append(s.forms, f)
	}
	return s
}

// commandSuggest completes YouTrack commands against the sample issue.
func commandSuggest(sh *shared) suggestFunc {
	return func(text string, caret int) func(context.Context) ([]Suggestion, error) {
		yt, key := sh.conn.yt, sh.sample.key()
		if yt == nil || key == "" {
			return nil
		}
		return func(ctx context.Context) ([]Suggestion, error) { return yt.CommandAssist(ctx, text, caret, key) }
	}
}

func (s *stagesStep) title() string { return "Stages" }

func (s *stagesStep) enter() tea.Cmd {
	s.setFocus(s.focus)
	return s.recheck(false)
}

func (s *stagesStep) leave() {
	s.blurAll()
	s.picker = nil
}

// recheck validates every command and search of the enabled stages when
// the sample issue or the connection changed (or always with force).
func (s *stagesStep) recheck(force bool) tea.Cmd {
	if s.sh.conn.yt == nil {
		return nil
	}
	on := s.sh.conn.profile + "|" + s.sh.sample.key() + "|" + s.sh.sample.query
	if !force && on == s.checkedOn {
		return nil
	}
	s.checkedOn = on
	var cmds []tea.Cmd
	for i, f := range s.forms {
		if !stageList(s.sh.p)[i].On() {
			continue
		}
		cmds = append(cmds, s.validate(&f.enterChk, f.enter.value()), s.searchStage(f))
		if f.exit != nil {
			cmds = append(cmds, s.validate(&f.exitChk, f.exit.value()))
		}
	}
	return tea.Batch(cmds...)
}

// validate checks a command on the sample issue now.
func (s *stagesStep) validate(st *cmdState, command string) tea.Cmd {
	st.deb.seq++
	st.err, st.done, st.pending = nil, false, false
	yt, key := s.sh.conn.yt, s.sh.sample.key()
	if yt == nil || key == "" || strings.TrimSpace(command) == "" {
		return nil
	}
	st.pending = true
	id, seq, ctx := st.deb.id, st.deb.seq, s.sh.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		check, err := yt.Validate(ctx, command, key)
		return cmdMsg{id: id, seq: seq, on: key, check: check, err: err}
	}
}

// searchStage runs the stage search now.
func (s *stagesStep) searchStage(f *stageForm) tea.Cmd {
	st := &f.matches
	st.deb.seq++
	st.err, st.done, st.pending, st.issues = nil, false, false, nil
	yt, q := s.sh.conn.yt, f.match.value()
	if yt == nil || q == "" {
		return nil
	}
	st.pending = true
	id, seq, ctx := st.deb.id, st.deb.seq, s.sh.ctx
	src := ""
	if f == s.forms[0] {
		src = strings.TrimSpace(s.sh.p.Source.Query)
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		m := matchMsg{id: id, seq: seq}
		m.issues, m.err = yt.Search(ctx, q)
		if m.err == nil && src != "" {
			// An issue in work that still matches the source would be
			// taken again.
			m.both, m.bothErr = yt.Search(ctx, "("+src+") and ("+q+")")
			m.bothRan = true
		}
		return m
	}
}

func (s *stagesStep) slots() []int {
	if s.forms[s.sel].exit != nil {
		return []int{slotTabs, slotEnter, slotExit, slotMatch}
	}
	return []int{slotTabs, slotEnter, slotMatch}
}

func (s *stagesStep) field(slot int) *assistField {
	f := s.forms[s.sel]
	switch slot {
	case slotEnter:
		return f.enter
	case slotMatch:
		return f.match
	case slotExit:
		return f.exit
	}
	return nil
}

func (s *stagesStep) blurAll() {
	for _, f := range s.forms {
		f.enter.blur()
		f.match.blur()
		if f.exit != nil {
			f.exit.blur()
		}
	}
}

func (s *stagesStep) setFocus(slot int) {
	s.blurAll()
	s.focus = slot
	if f := s.field(slot); f != nil {
		f.focus()
	}
}

// move goes to the next or previous slot; past the last slot of a stage
// it goes on to the next stage.
func (s *stagesStep) move(delta int) tea.Cmd {
	slots := s.slots()
	i := 0
	for j, sl := range slots {
		if sl == s.focus {
			i = j
		}
	}
	i += delta
	switch {
	case i >= len(slots):
		if s.sel == len(s.forms)-1 {
			s.setFocus(slotTabs)
			return next
		}
		s.sel++
		s.setFocus(slotTabs)
		return nil
	case i < 0:
		if s.sel == 0 {
			s.setFocus(slotTabs)
			return nil
		}
		s.sel--
		sl := s.slots()
		s.setFocus(sl[len(sl)-1])
		return nil
	}
	s.setFocus(slots[i])
	return nil
}

// store copies the fields of the selected stage into the draft.
func (s *stagesStep) store() {
	f, st := s.forms[s.sel], stageList(s.sh.p)[s.sel]
	st.Enter, st.Match = f.enter.value(), f.match.value()
	if f.exit != nil {
		st.Exit = f.exit.value()
	}
}

func (s *stagesStep) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	if s.picker != nil {
		return s.pickerKey(msg)
	}
	f := s.forms[s.sel]
	if msg.String() == "ctrl+f" {
		return s.openPicker(), true
	}
	if s.focus == slotTabs {
		switch msg.String() {
		case "left", "h":
			s.sel = (s.sel + len(s.forms) - 1) % len(s.forms)
			return nil, true
		case "right", "l":
			s.sel = (s.sel + 1) % len(s.forms)
			return nil, true
		case " ", "x":
			st := stageList(s.sh.p)[s.sel]
			on := !st.On()
			st.Enabled = &on
			if on {
				return tea.Batch(s.validate(&f.enterChk, f.enter.value()), s.searchStage(f)), true
			}
			return nil, true
		case "tab", "down", "enter":
			return s.move(1), true
		case "shift+tab", "up":
			return s.move(-1), true
		}
		return nil, false
	}
	in := s.field(s.focus)
	cmd, consumed, changed := in.key(msg)
	if changed {
		s.store()
		switch s.focus {
		case slotEnter:
			cmd = tea.Batch(cmd, f.enterChk.deb.bump(s.sh.delay))
		case slotExit:
			cmd = tea.Batch(cmd, f.exitChk.deb.bump(s.sh.delay))
		case slotMatch:
			cmd = tea.Batch(cmd, f.matches.deb.bump(s.sh.delay))
		}
	}
	if consumed {
		return cmd, true
	}
	switch msg.String() {
	case "tab", "down", "enter":
		return tea.Batch(cmd, s.move(1)), true
	case "shift+tab", "up":
		return tea.Batch(cmd, s.move(-1)), true
	}
	return cmd, false
}

func (s *stagesStep) msg(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case connMsg:
		s.checkedOn = ""
		s.fieldsProject, s.fields, s.fieldsErr = "", nil, nil
		s.tags, s.tagsState = nil, 0
		return nil
	case sampleMsg:
		// The source step has stored the new sample by now.
		s.checkedOn = ""
		return nil
	case cmdMsg:
		for _, f := range s.forms {
			for _, st := range []*cmdState{&f.enterChk, &f.exitChk} {
				if st.deb.current(m.id, m.seq) {
					st.pending, st.done, st.on, st.check, st.err = false, true, m.on, m.check, m.err
				}
			}
		}
		return nil
	case matchMsg:
		for _, f := range s.forms {
			if f.matches.deb.current(m.id, m.seq) {
				f.matches.pending, f.matches.done, f.matches.issues, f.matches.err = false, true, m.issues, m.err
				f.matches.both, f.matches.bothErr, f.matches.bothRan = m.both, m.bothErr, m.bothRan
			}
		}
		return nil
	case fieldsMsg:
		if m.project == s.fieldsProject {
			s.fieldsPending, s.fields, s.fieldsErr = false, m.fields, m.err
			if s.picker != nil && s.picker.phase == 0 {
				s.picker.setFields(s.fields)
				s.picker.note = ""
				if m.err != nil {
					s.picker.note = "can't list fields: " + m.err.Error()
				}
			}
		}
		return nil
	case tagsMsg:
		s.tagsState, s.tags, s.tagsErr = 2, m.tags, m.err
		if s.picker != nil && s.picker.field.Type == "tag" {
			s.picker.setValues(s.picker.field, s.tags)
		}
		return nil
	}
	var cmds []tea.Cmd
	for _, f := range s.forms {
		switch {
		case f.enterChk.deb.fired(msg):
			return s.validate(&f.enterChk, f.enter.value())
		case f.exitChk.deb.fired(msg):
			return s.validate(&f.exitChk, f.exit.value())
		case f.matches.deb.fired(msg):
			return s.searchStage(f)
		}
		cmds = append(cmds, f.enter.msg(msg), f.match.msg(msg))
		if f.exit != nil {
			cmds = append(cmds, f.exit.msg(msg))
		}
	}
	return tea.Batch(cmds...)
}

// stageMark is the status of one stage.
func (s *stagesStep) stageMark(i int) string {
	f, st := s.forms[i], stageList(s.sh.p)[i]
	if !st.On() {
		return markOff
	}
	marks := []string{cmdMark(f.enterChk, f.enter.value())}
	if f.exit != nil {
		marks = append(marks, cmdMark(f.exitChk, f.exit.value()))
	}
	switch {
	case f.matches.err != nil:
		marks = append(marks, markErr)
	case f.matches.pending:
		marks = append(marks, markPending)
	case len(f.matches.both) > 0:
		marks = append(marks, markWarn)
	}
	if (i == 0 || i == 1) && (f.enter.value() == "" || f.match.value() == "") {
		marks = append(marks, markErr)
	}
	if (i == 2 || i == 3) && f.enter.value() == "" {
		marks = append(marks, markErr)
	}
	for _, want := range []string{markErr, markPending, markWarn} {
		for _, m := range marks {
			if m == want {
				return want
			}
		}
	}
	return markOK
}

func cmdMark(st cmdState, command string) string {
	switch {
	case command == "":
		return markOK
	case st.pending:
		return markPending
	case st.err != nil:
		return markErr
	case !st.done:
		return markOK
	case st.check.State == ytwatch.Valid:
		return markOK
	case newTag(command, st.check) != "":
		return markWarn
	}
	return markErr
}

func (s *stagesStep) view(width, height int) string {
	var b strings.Builder
	var tabs []string
	for i, d := range stageDefs {
		label := s.stageMark(i) + " " + d.name
		if i == s.sel {
			if s.focus == slotTabs {
				label = styleSel.Render(" " + d.name + " ")
			} else {
				label = styleFocus.Render("[" + d.name + "]")
			}
			label = s.stageMark(i) + label
		}
		tabs = append(tabs, label)
	}
	b.WriteString(strings.Join(tabs, "  ") + "\n\n")
	d, f, st := stageDefs[s.sel], s.forms[s.sel], stageList(s.sh.p)[s.sel]
	b.WriteString(styleBold.Render(d.name) + " " + styleDim.Render(wrap(d.about, width-lenRunes(d.name)-1)) + "\n")
	if !st.On() {
		b.WriteString("\n" + markOff + " This stage is off. Press space on the stage row to turn it on.\n")
		return b.String()
	}
	if s.picker != nil {
		b.WriteString("\n" + s.picker.view(width, height-6))
		return b.String()
	}
	if s.sh.conn.yt != nil && s.sh.sample.key() == "" {
		b.WriteString(markWarn + styleDim.Render(" commands are checked on an issue of the source; the source has none right now") + "\n")
	}
	b.WriteString("\n" + f.enter.view(width, s.focus == slotEnter) + "\n")
	b.WriteString(cmdView(f.enterChk, f.enter.value(), width) + "\n")
	if f.exit != nil {
		b.WriteString(f.exit.view(width, s.focus == slotExit) + "\n")
		b.WriteString(cmdView(f.exitChk, f.exit.value(), width) + "\n")
	}
	b.WriteString(f.match.view(width, s.focus == slotMatch) + "\n")
	b.WriteString(matchView(f.matches, f.match.value(), width) + "\n")
	b.WriteString(leavesView(f.matches, width))
	return b.String()
}

func cmdView(st cmdState, command string, width int) string {
	switch {
	case command == "":
		return styleDim.Render("    no command")
	case st.pending:
		return "    " + markPending + styleDim.Render(" checking…")
	case st.err != nil:
		return "    " + markErr + " " + oneLine(st.err.Error(), width-8)
	case !st.done:
		return ""
	case st.check.State == ytwatch.Valid:
		return "    " + markOK + " " + oneLine(st.check.Text, width-24) + styleDim.Render(" (on "+st.on+")")
	}
	if tag := newTag(command, st.check); tag != "" {
		return "    " + markWarn + " " + oneLine("tag "+tag+" doesn't exist yet; YouTrack creates it on first use of `tag "+tag+"`", width-8)
	}
	line := "    " + markErr + " " + string(st.check.State) + ": " + oneLine(st.check.Text, width-24)
	if len(st.check.Suggestions) > 0 {
		line += "\n      " + styleDim.Render(oneLine("try: "+strings.Join(st.check.Suggestions, ", "), width-8))
	}
	return line
}

func matchView(st matchState, query string, width int) string {
	switch {
	case query == "":
		return styleDim.Render("    no search")
	case st.pending:
		return "    " + markPending + styleDim.Render(" searching…")
	case st.err != nil:
		return "    " + markErr + " " + oneLine(st.err.Error(), width-8)
	case !st.done:
		return ""
	case len(st.issues) == 0:
		return "    " + markOK + styleDim.Render(" no issue is in this stage right now")
	}
	var parts []string
	for _, is := range st.issues[:min(3, len(st.issues))] {
		parts = append(parts, is.Key+" "+oneLine(is.Summary, 24))
	}
	return "    " + markOK + " " + oneLine(fmt.Sprintf("%d in this stage now: %s", len(st.issues), strings.Join(parts, " · ")), width-8)
}

// leavesView says whether issues in work drop out of the source.
func leavesView(st matchState, width int) string {
	switch {
	case !st.done || !st.bothRan:
		return ""
	case st.bothErr != nil:
		return "    " + markWarn + " " + oneLine("can't tell whether issues in work leave the source: "+st.bothErr.Error(), width-8) + "\n"
	case len(st.both) == 0:
		return "    " + markOK + styleDim.Render(" issues in work don't match the source query") + "\n"
	}
	var keys []string
	for _, is := range st.both[:min(3, len(st.both))] {
		keys = append(keys, is.Key)
	}
	return "    " + markWarn + " " + wrap(fmt.Sprintf("%d issues match both the source and this search (%s): the working command must take issues out of the source, or ymcp takes them again", len(st.both), strings.Join(keys, ", ")), width-8) + "\n"
}

// newTag returns the tag of a `tag X` or `untag X` command that YouTrack
// refuses only because X does not exist yet, or "" (as the engine's check
// does).
func newTag(command string, check ytwatch.CommandCheck) string {
	verb, tag, ok := strings.Cut(strings.TrimSpace(command), " ")
	tag = strings.TrimSpace(tag)
	if !ok || tag == "" || (!strings.EqualFold(verb, "tag") && !strings.EqualFold(verb, "untag")) || check.Text == "" {
		return ""
	}
	for _, part := range strings.Split(check.Text, "; ") {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(part)), "tag expected") {
			return ""
		}
	}
	return strings.Trim(tag, "{}")
}

func (s *stagesStep) help() string {
	switch {
	case s.picker != nil:
		return s.picker.help()
	case s.focus == slotTabs:
		return keyHelp("←/→", "stage", "space", "on/off", "tab", "edit", "ctrl+f", "pick field & value")
	case s.field(s.focus) != nil && s.field(s.focus).showing():
		return keyHelp("↑/↓", "choose", "tab", "complete", "esc", "hide")
	}
	return keyHelp("tab", "next", "ctrl+f", "pick field & value", "ctrl+space", "completions", "shift+tab", "back")
}

func (s *stagesStep) mark() string {
	worst := markOK
	for i := range s.forms {
		switch m := s.stageMark(i); m {
		case markErr:
			return markErr
		case markPending:
			worst = markPending
		case markWarn:
			if worst == markOK {
				worst = markWarn
			}
		}
	}
	return worst
}

// fieldPicker builds a stage from a field and one of its values.
type fieldPicker struct {
	stage  int
	phase  int // 0 fields, 1 values
	fields *pickList
	values *pickList
	field  Field
	note   string
}

func (p *fieldPicker) setFields(fields []Field) {
	items := []pickItem{{title: "Tag", desc: "add a tag; the search matches it", value: "\x00tag"}}
	for _, f := range fields {
		items = append(items, pickItem{title: f.Name, desc: fmt.Sprintf("%s · %d values", f.Type, len(f.Values)), value: f.Name})
	}
	p.fields = newPickList(items, true)
	if len(fields) > 0 {
		p.fields.cur = 1 // the first field (State, when there is one)
	}
}

func (p *fieldPicker) setValues(f Field, values []string) {
	p.field = f
	var items []pickItem
	for _, v := range values {
		items = append(items, pickItem{title: v, value: v})
	}
	p.values = newPickList(items, true)
	if f.Type == "tag" {
		p.values.allowNew = "new tag (YouTrack creates it on first use)"
	}
}

func (s *stagesStep) openPicker() tea.Cmd {
	if !stageList(s.sh.p)[s.sel].On() {
		return nil
	}
	p := &fieldPicker{stage: s.sel}
	s.picker = p
	project := projectOf(s.sh.sample.key())
	p.setFields(nil)
	if s.sh.conn.yt == nil {
		p.note = "sign in to a profile to list fields; tags can still be typed"
		return nil
	}
	if project == "" {
		p.note = "the source has no issue, so its project's fields are unknown; tags can still be typed"
		return nil
	}
	if project == s.fieldsProject {
		p.setFields(s.fields)
		switch {
		case s.fieldsPending:
			p.note = "loading the fields of " + project + "…"
		case s.fieldsErr != nil:
			p.note = "can't list fields: " + s.fieldsErr.Error()
		}
		return nil
	}
	s.fieldsProject, s.fieldsPending = project, true
	p.note = "loading the fields of " + project + "…"
	yt, ctx := s.sh.conn.yt, s.sh.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		fields, err := yt.Fields(ctx, project)
		return fieldsMsg{project: project, fields: fields, err: err}
	}
}

func (s *stagesStep) pickerKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	p := s.picker
	switch msg.String() {
	case "esc":
		if p.phase == 1 {
			p.phase = 0
			return nil, true
		}
		s.picker = nil
		return nil, true
	case "enter", "tab":
		if p.phase == 0 {
			it, ok := p.fields.selected()
			if !ok {
				return nil, true
			}
			if it.value == "\x00tag" {
				p.phase = 1
				p.setValues(Field{Name: "tag", Type: "tag"}, s.tags)
				if s.tagsState == 0 && s.sh.conn.yt != nil {
					s.tagsState = 1
					yt, ctx := s.sh.conn.yt, s.sh.ctx
					return func() tea.Msg {
						ctx, cancel := context.WithTimeout(ctx, requestTimeout)
						defer cancel()
						tags, err := yt.Tags(ctx)
						return tagsMsg{tags: tags, err: err}
					}, true
				}
				return nil, true
			}
			for _, f := range s.fields {
				if f.Name == it.value {
					p.phase = 1
					p.setValues(f, f.Values)
				}
			}
			return nil, true
		}
		it, ok := p.values.selected()
		if !ok {
			return nil, true
		}
		return s.applyPick(p.field, it.value, msg.String() == "tab"), true
	}
	if p.phase == 0 {
		p.fields.key(msg)
	} else {
		p.values.key(msg)
	}
	return nil, true
}

// applyPick writes the command and search for field = value into the
// stage, replacing what is there or adding to it.
func (s *stagesStep) applyPick(f Field, value string, add bool) tea.Cmd {
	command, query := clause(f, value)
	form := s.forms[s.sel]
	join := func(old, part, sep string) string {
		if !add || strings.TrimSpace(old) == "" {
			return part
		}
		return strings.TrimSpace(old) + sep + part
	}
	form.enter.set(join(form.enter.value(), command, " "))
	form.match.set(join(form.match.value(), query, " "))
	if form.exit != nil && f.Type == "tag" {
		form.exit.set(join(form.exit.value(), "untag "+braced(value), " "))
	}
	s.store()
	s.picker = nil
	s.setFocus(slotEnter)
	cmds := []tea.Cmd{s.validate(&form.enterChk, form.enter.value()), s.searchStage(form)}
	if form.exit != nil {
		cmds = append(cmds, s.validate(&form.exitChk, form.exit.value()))
	}
	return tea.Batch(cmds...)
}

// clause is the command and the search for a field value.
func clause(f Field, value string) (command, query string) {
	switch f.Type {
	case "tag":
		return "tag " + braced(value), "tag: " + braced(value)
	case "user":
		return f.Name + " " + value, braced(f.Name) + ": " + value
	}
	return f.Name + " " + value, braced(f.Name) + ": " + braced(value)
}

// braced wraps a name with spaces or punctuation in {} for YouTrack.
func braced(s string) string {
	if strings.ContainsAny(s, " ,:{}()#") {
		return "{" + s + "}"
	}
	return s
}

func (p *fieldPicker) view(width, height int) string {
	var b strings.Builder
	if p.phase == 0 {
		b.WriteString(styleLabel.Render("Pick a field") + styleDim.Render("  then a value; ymcp writes the command and the search for you") + "\n")
		if p.note != "" {
			b.WriteString(markWarn + " " + styleDim.Render(p.note) + "\n")
		}
		b.WriteString(p.fields.view(width, height-2))
		return b.String()
	}
	b.WriteString(styleLabel.Render("Pick a value of "+p.field.Name) + "\n")
	b.WriteString(p.values.view(width, height-4))
	if it, ok := p.values.selected(); ok {
		c, q := clause(p.field, it.value)
		b.WriteString("\n\n" + styleDim.Render("command ") + styleCode.Render(c) + styleDim.Render("   search ") + styleCode.Render(q))
	}
	return b.String()
}

func (p *fieldPicker) help() string {
	if p.phase == 0 {
		return keyHelp("type", "filter", "↑/↓", "choose", "enter", "values", "esc", "close")
	}
	return keyHelp("enter", "replace the stage", "tab", "add to it", "esc", "back to fields")
}
