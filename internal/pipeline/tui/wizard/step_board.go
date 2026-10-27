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
	enterDesc        string
	hasExit          bool
	matchLabel       string
	matchDesc        string
	// Placeholders: examples for this stage.
	enterHolder, matchHolder string
}

var stageDefs = []stageDef{
	{
		name: "Working", key: "working",
		about:       "issues the agent is working on",
		enterLabel:  "Command when ymcp takes one",
		enterDesc:   "Must take it out of the source.",
		matchLabel:  "Search for them",
		matchDesc:   "Recognises issues in work.",
		enterHolder: "e.g. State In Progress", matchHolder: "e.g. State: {In Progress}",
	},
	{
		name: "Review", key: "review",
		about:       "work pushed, waiting for a review",
		enterLabel:  "Command when work is ready",
		enterDesc:   "Runs once the work is pushed.",
		matchLabel:  "Search for them",
		matchDesc:   "Recognises issues in review.",
		enterHolder: "e.g. State Fixed", matchHolder: "e.g. State: Fixed",
	},
	{
		name: "Needs input", key: "needsInput",
		about:       "the agent asked something and waits for a person",
		enterLabel:  "Command when it asks",
		enterDesc:   "Runs when the agent asks.",
		hasExit:     true,
		matchLabel:  "Search for them",
		matchDesc:   "Optional; shows them on the board.",
		enterHolder: "e.g. tag needs-input", matchHolder: "e.g. tag: needs-input",
	},
	{
		name: "Done", key: "done",
		about:       "work merged: the run ends as done",
		enterLabel:  "Command when merged",
		enterDesc:   "Runs when the PR/MR is merged.",
		matchLabel:  "Search for them",
		matchDesc:   "A match ends the run as done.",
		enterHolder: "e.g. State Verified", matchHolder: "e.g. State: Verified",
	},
	{
		name: "Closed", key: "closed",
		about:       "people closed the issue: the run ends as closed",
		enterLabel:  "Command when closed",
		enterDesc:   "Optional.",
		matchLabel:  "Search for them",
		matchDesc:   "A match ends the run as closed.",
		enterHolder: "none", matchHolder: "e.g. State: {Won't fix}",
	},
}

// stageID is the id of a field of a stage: "working.enter".
func stageID(stage int, field string) string { return stageDefs[stage].key + "." + field }

// boardStep edits the stages. Each command and search is checked as it
// is typed, and a picker builds them from the project's fields.
type boardStep struct {
	base
	forms []*stageForm
	all   []*item
	more  bool // Done and Closed are shown

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

func newBoardStep(sh *shared) *boardStep {
	s := &boardStep{base: base{sh: sh}}
	stages := stageList(sh.p)
	for i, d := range stageDefs {
		f := &stageForm{def: d}
		f.enter = newAssist(sh, d.enterHolder, commandSuggest(sh))
		f.match = newAssist(sh, d.matchHolder, searchSuggest(sh))
		f.enter.set(stages[i].Enter)
		f.match.set(stages[i].Match)
		if d.hasExit {
			f.exit = newAssist(sh, "e.g. untag needs-input", commandSuggest(sh))
			f.exit.set(stages[i].Exit)
		}
		f.enterChk.deb = debouncer{id: sh.newID()}
		f.exitChk.deb = debouncer{id: sh.newID()}
		f.matches.deb = debouncer{id: sh.newID()}
		s.forms = append(s.forms, f)
	}
	s.more = sh.p.Stages.Done.On() || sh.p.Stages.Closed.Enter != "" || sh.p.Stages.Closed.Match != ""
	s.build()
	return s
}

// build lays the form out: a section per stage, its command and search
// side by side. Working and Review show; the others are behind More
// stages.
func (s *boardStep) build() {
	sh := s.sh
	s.all = []*item{{
		id: "note",
		below: func(width int) string {
			switch {
			case sh.conn.yt == nil:
				return markOff + styleDim.Render(" sign in (Basics) to check the commands and searches")
			case sh.sample.key() == "":
				return markWarn + styleDim.Render(oneLine(" commands are checked on an issue of the source; it has none now", width-2))
			}
			return styleDim.Render(oneLine("Commands are checked on "+sh.sample.key()+" without changing it.", width))
		},
	}}
	for i, d := range stageDefs {
		i, d, f := i, d, s.forms[i]
		more := func() bool { return i >= 2 && !s.moreOpen() }
		on := func() bool { return stageList(sh.p)[i].On() }
		off := func() bool { return more() || !on() }
		if i == 2 {
			s.all = append(s.all, &item{
				id: "more", inline: true,
				ctl: &disclosureCtl{label: "More stages: needs input, merged, closed", open: &s.more, force: s.moreErr, note: func() string {
					if s.moreOpen() {
						return ""
					}
					for j := 2; j < len(stageDefs); j++ {
						if m := s.stageMark(j); m == markErr || m == markWarn {
							return m
						}
					}
					return ""
				}},
			})
		}
		if d.key == "done" {
			s.all = append(s.all, &item{
				id: "done", heading: d.name, headNote: d.about, hidden: more, inline: true,
				headMark: func() string { return s.stageMark(i) },
				desc:     "Off: a run ends when its issue is closed; on: also when merged.",
				ctl: &toggleCtl{
					label: "Use the Done stage",
					get:   on,
					set: func(v bool) tea.Cmd {
						stageList(sh.p)[i].Enabled = &v
						if v {
							return tea.Batch(s.validate(&f.enterChk, f.enter.value()), s.searchStage(f))
						}
						return nil
					},
				},
			})
		}
		enter := &item{
			id: stageID(i, "enter"), label: d.enterLabel, desc: d.enterDesc, hidden: off,
			ctl: newText(f.enter, func() tea.Cmd {
				s.store(i)
				return f.enterChk.deb.bump(sh.delay)
			}),
			problem: func() string {
				if i <= 3 && s.fs.tried && f.enter.value() == "" {
					return "Needs a command."
				}
				return ""
			},
			below: func(width int) string { return cmdView(f.enterChk, f.enter.value(), width) },
		}
		if d.key != "done" {
			enter.heading, enter.headNote = d.name, d.about
			enter.headMark = func() string { return s.stageMark(i) }
		}
		match := &item{
			id: stageID(i, "match"), label: d.matchLabel, desc: d.matchDesc, hidden: off, inRow: true,
			ctl: newText(f.match, func() tea.Cmd {
				s.store(i)
				return f.matches.deb.bump(sh.delay)
			}),
			problem: func() string {
				if i <= 1 && s.fs.tried && f.match.value() == "" {
					return "Needs a search."
				}
				return ""
			},
			below: func(width int) string {
				out := matchView(f.matches, f.match.value(), width)
				if l := leavesView(f.matches, width); l != "" {
					out += "\n" + l
				}
				return out
			},
		}
		if f.exit == nil {
			enter.right = match
			s.all = append(s.all, enter, match)
			continue
		}
		exit := &item{
			id: stageID(i, "exit"), label: "Command when answered", hidden: off, inRow: true,
			desc: "Undoes the command on the left.",
			ctl: newText(f.exit, func() tea.Cmd {
				s.store(i)
				return f.exitChk.deb.bump(sh.delay)
			}),
			below: func(width int) string { return cmdView(f.exitChk, f.exit.value(), width) },
		}
		enter.right = exit
		match.inRow = false
		s.all = append(s.all, enter, exit, match)
	}
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

func (s *boardStep) title() string { return "Board" }

// moreErr reports whether a stage under More stages lacks a required
// command.
func (s *boardStep) moreErr() bool {
	for i := 2; i <= 3; i++ {
		if stageList(s.sh.p)[i].On() && s.forms[i].enter.value() == "" {
			return true
		}
	}
	return false
}

// moreOpen reports whether the stages under More stages show.
func (s *boardStep) moreOpen() bool { return s.more || s.moreErr() }
func (s *boardStep) intro() string {
	return "The YouTrack command ymcp runs at each stage, and the search for it."
}
func (s *boardStep) items() []*item { return s.all }

func (s *boardStep) enter() tea.Cmd { return s.recheck(false) }

func (s *boardStep) leave() { s.picker = nil }

// recheck validates every command and search of the enabled stages when
// the sample issue or the connection changed (or always with force).
func (s *boardStep) recheck(force bool) tea.Cmd {
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

// store copies the fields of a stage into the draft.
func (s *boardStep) store(i int) {
	f, st := s.forms[i], stageList(s.sh.p)[i]
	st.Enter, st.Match = f.enter.value(), f.match.value()
	if f.exit != nil {
		st.Exit = f.exit.value()
	}
}

// stageOf is the stage of a field id, or -1.
func stageOf(id string) int {
	key, _, ok := strings.Cut(id, ".")
	if !ok {
		return -1
	}
	for i, d := range stageDefs {
		if d.key == key {
			return i
		}
	}
	return -1
}

func (s *boardStep) shortcut(msg tea.KeyMsg, focus string) (tea.Cmd, bool) {
	if msg.String() == "ctrl+f" {
		if i := stageOf(focus); i >= 0 {
			return s.openPicker(i), true
		}
		return nil, true
	}
	return nil, false
}

func (s *boardStep) extraHelp(focus string) string {
	if stageOf(focus) >= 0 {
		return keyHelp("ctrl+f", "pick field & value")
	}
	return ""
}

func (s *boardStep) overlay() overlay {
	if s.picker != nil {
		return pickerOverlay{s}
	}
	return nil
}

// pickerOverlay is the field picker over the board.
type pickerOverlay struct{ s *boardStep }

func (o pickerOverlay) key(msg tea.KeyMsg) tea.Cmd {
	cmd, _ := o.s.pickerKey(msg)
	return cmd
}

func (o pickerOverlay) view(width, height int) string {
	p := o.s.picker
	return styleSection.Render(stageDefs[p.stage].name+" stage") + "\n" + p.view(width, height-1)
}

func (o pickerOverlay) help() string { return o.s.picker.help() }

func (s *boardStep) msg(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case connMsg:
		s.checkedOn = ""
		s.fieldsProject, s.fields, s.fieldsErr = "", nil, nil
		s.tags, s.tagsState = nil, 0
		return nil
	case sampleMsg:
		// The basics step has stored the new sample by now.
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
func (s *boardStep) stageMark(i int) string {
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

func (s *boardStep) blockers() []problem {
	var out []problem
	for i, d := range stageDefs {
		f := s.forms[i]
		if !stageList(s.sh.p)[i].On() {
			continue
		}
		name := strings.ToLower(d.name)
		if i <= 3 && f.enter.value() == "" {
			out = append(out, problem{stageID(i, "enter"), "a " + name + " command"})
		}
		if i <= 1 && f.match.value() == "" {
			out = append(out, problem{stageID(i, "match"), "a " + name + " search"})
		}
	}
	return out
}

func (s *boardStep) mark() string {
	worst := markOK
	for i := range s.forms {
		switch m := s.stageMark(i); m {
		case markErr:
			return markBad
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

// validate checks a command on the sample issue now.
func (s *boardStep) validate(st *cmdState, command string) tea.Cmd {
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
func (s *boardStep) searchStage(f *stageForm) tea.Cmd {
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

func cmdView(st cmdState, command string, width int) string {
	switch {
	case command == "":
		return styleDim.Render("no command")
	case st.pending:
		return markPending + styleDim.Render(" checking…")
	case st.err != nil:
		return markErr + " " + oneLine(st.err.Error(), width-2)
	case !st.done:
		return ""
	case st.check.State == ytwatch.Valid:
		on := " · " + st.on
		return markOK + " " + oneLine(st.check.Text, max(width-2-lenRunes(on), 8)) + styleDim.Render(on)
	}
	if tag := newTag(command, st.check); tag != "" {
		return markWarn + " " + oneLine("tag "+tag+" is new; YouTrack creates it on first use", width-2)
	}
	line := markErr + " " + oneLine(string(st.check.State)+": "+st.check.Text, width-2)
	if len(st.check.Suggestions) > 0 {
		line += "\n  " + styleDim.Render(oneLine("try: "+strings.Join(st.check.Suggestions, ", "), width-2))
	}
	return line
}

func matchView(st matchState, query string, width int) string {
	switch {
	case query == "":
		return styleDim.Render("no search")
	case st.pending:
		return markPending + styleDim.Render(" searching…")
	case st.err != nil:
		return markErr + " " + oneLine(st.err.Error(), width-2)
	case !st.done:
		return ""
	case len(st.issues) == 0:
		return markOK + styleDim.Render(" none in this stage now")
	}
	var keys []string
	for _, is := range st.issues[:min(3, len(st.issues))] {
		keys = append(keys, is.Key)
	}
	return markOK + " " + oneLine(fmt.Sprintf("%d in this stage now: %s", len(st.issues), strings.Join(keys, ", ")), width-2)
}

// leavesView says whether issues in work drop out of the source.
func leavesView(st matchState, width int) string {
	switch {
	case !st.done || !st.bothRan:
		return ""
	case st.bothErr != nil:
		return markWarn + " " + oneLine("can't tell if they leave the source: "+st.bothErr.Error(), width-2)
	case len(st.both) == 0:
		return markOK + " " + styleDim.Render(oneLine("they leave the source query", width-2))
	}
	var keys []string
	for _, is := range st.both[:min(3, len(st.both))] {
		keys = append(keys, is.Key)
	}
	return markWarn + " " + oneLine(fmt.Sprintf("%d also match the source (%s)", len(st.both), strings.Join(keys, ", ")), width-2) +
		"\n  " + styleWarn.Render(oneLine("ymcp would take them again", width-2))
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

func (s *boardStep) openPicker(stage int) tea.Cmd {
	if !stageList(s.sh.p)[stage].On() {
		return nil
	}
	p := &fieldPicker{stage: stage}
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

func (s *boardStep) pickerKey(msg tea.KeyMsg) (tea.Cmd, bool) {
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
func (s *boardStep) applyPick(f Field, value string, add bool) tea.Cmd {
	command, query := clause(f, value)
	stage := s.picker.stage
	form := s.forms[stage]
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
	s.store(stage)
	s.picker = nil
	s.fs.focus = stageID(stage, "enter")
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
