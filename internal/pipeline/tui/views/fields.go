package views

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/pipeline/tui/keys"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// The fields grid of the card's top bar.
const (
	// gridCols is the most chips a row of the grid holds.
	gridCols = 4
	// chipMax is the widest a field's chip gets; a longer value is cut.
	chipMax = 32
	// chipGap is the space between two columns of the grid.
	chipGap = 2
)

// keyFields are the YouTrack fields that come first among the chips and
// in the Fields menu, in this order; the rest follow by name.
var keyFields = []string{"State", "Type", "Priority"}

// fieldOrder compares two field names: keyFields first, in their order,
// then the rest by name, case ignored.
func fieldOrder(a, b string) int {
	rank := func(s string) int {
		if i := slices.Index(keyFields, s); i >= 0 {
			return i
		}
		return len(keyFields)
	}
	if c := cmp.Compare(rank(a), rank(b)); c != 0 {
		return c
	}
	return cmp.Or(cmp.Compare(strings.ToLower(a), strings.ToLower(b)), cmp.Compare(a, b))
}

// fieldChips are the issue's fields that have a value, as chips, in
// fieldOrder.
func fieldChips(fields map[string]string) []string {
	names := make([]string, 0, len(fields))
	for name, v := range fields {
		if strings.TrimSpace(v) != "" {
			names = append(names, name)
		}
	}
	slices.SortFunc(names, fieldOrder)
	chips := make([]string, len(names))
	for i, name := range names {
		chips[i] = chipStyle.Render(cut(name+": "+oneLine(fields[name]), chipMax-2))
	}
	return chips
}

// chipGrid lays groups of chips out in rows of at most gridCols, each
// group starting on a new row, in columns as wide as their widest chip so
// they line up: the most columns that fit w cells. It keeps at most n
// rows, the last ending with "+N more" when chips were left out; all
// reports whether every chip shows whole. Every row is padded to the
// width of the grid.
func chipGrid(groups [][]string, w, n int) (lines []string, all bool) {
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	if total == 0 {
		return nil, true
	}
	if n <= 0 {
		return nil, false
	}
	var rows [][]string
	var widths []int
	for cols := gridCols; cols >= 1; cols-- {
		rows = rows[:0]
		for _, g := range groups {
			for i := 0; i < len(g); i += cols {
				rows = append(rows, g[i:min(i+cols, len(g))])
			}
		}
		widths = make([]int, cols)
		for _, r := range rows {
			for c, chip := range r {
				widths[c] = max(widths[c], ansi.StringWidth(chip))
			}
		}
		if gridWidth(widths) <= w {
			break
		}
	}
	all = len(rows) <= n && gridWidth(widths) <= w
	if len(rows) > n {
		hidden := 0
		for _, r := range rows[n:] {
			hidden += len(r)
		}
		last := slices.Clone(rows[n-1])
		if len(last) == len(widths) { // no room left: the last chip gives way
			last = last[:len(last)-1]
			hidden++
		}
		more := theme.Dim.Render(fmt.Sprintf("+%d more", hidden))
		c := len(last)
		widths[c] = max(widths[c], ansi.StringWidth(more))
		rows = append(rows[:n-1:n-1], append(last, more))
	}
	gw := min(gridWidth(widths), w)
	for _, r := range rows {
		var b strings.Builder
		for c, chip := range r {
			if c > 0 {
				b.WriteString(strings.Repeat(" ", chipGap))
			}
			b.WriteString(pad(chip, widths[c]))
		}
		lines = append(lines, pad(b.String(), gw))
	}
	return lines, all
}

// gridWidth is the width of a grid with columns of widths.
func gridWidth(widths []int) int {
	w := 0
	for _, cw := range widths {
		w += cw
	}
	return w + chipGap*max(len(widths)-1, 0)
}

// fieldEditor is the card's Fields menu: the fields of the issue's
// project with their values; enter on one lists the values it can take,
// or opens a box to type one in.
type fieldEditor struct {
	open    bool
	loaded  bool // the project's fields have been read
	loading bool
	err     error // why they could not be read
	project string
	fields  []Field
	sel     int // the field under the cursor

	field  *Field // the field being changed; nil on the list of fields
	filter string // what was typed to narrow its values
	vsel   int    // the value under the cursor
	typing bool   // a typed field: the input box is open
	input  textinput.Model

	busy string // the field being changed in YouTrack, "" when none
}

// fieldsLoaded is the answer to the first opening of the Fields menu.
type fieldsLoaded struct {
	to      int64
	project string
	fields  []Field
	err     error
}

// fieldApplied is the outcome of a field change: the command YouTrack
// ran, and for a backlog issue whether the source query still finds it.
type fieldApplied struct {
	to      int64
	change  fieldChange
	err     error
	left    bool  // a backlog issue the source query no longer finds
	leftErr error // why that could not be told
}

// fieldChange is one change of a field: a value set, added or removed.
type fieldChange struct {
	field  string
	value  string
	multi  bool
	remove bool
}

// command is the change as a YouTrack command: "Priority Major",
// "Stage {In Progress}", "add flow frontend".
func (c fieldChange) command() string {
	cmd := c.field + " " + commandValue(c.value)
	switch {
	case c.multi && c.remove:
		return "remove " + cmd
	case c.multi:
		return "add " + cmd
	}
	return cmd
}

// done says what the change did.
func (c fieldChange) done(key string) string {
	switch {
	case c.multi && c.remove:
		return fmt.Sprintf("Removed %s from %s of %s.", c.value, c.field, key)
	case c.multi:
		return fmt.Sprintf("Added %s to %s of %s.", c.value, c.field, key)
	}
	return fmt.Sprintf("Set %s of %s to %s.", c.field, key, c.value)
}

// commandValue is a value as a command spells it: in braces when it has
// spaces or commas.
func commandValue(v string) string {
	if strings.ContainsFunc(v, func(r rune) bool { return unicode.IsSpace(r) || r == ',' }) {
		return "{" + v + "}"
	}
	return v
}

// pickTypes are the field types whose values are picked from a list;
// typedTypes those typed in.
var (
	pickTypes  = []string{"enum", "state", "version", "build", "owned", "user"}
	typedTypes = []string{"integer", "float", "string", "period"}
)

// editable says why the card cannot change a field, "" when it can.
func editable(f Field) string {
	switch {
	case slices.Contains(typedTypes, f.Type) && !f.Multi:
		return ""
	case f.Type == "user", slices.Contains(pickTypes, f.Type) && len(f.Values) > 0:
		return ""
	case slices.Contains(pickTypes, f.Type):
		return f.Name + " has no values to pick from."
	}
	return f.Name + " can't be changed here; o opens the issue."
}

// typeHint is what the input box of a typed field says it takes.
func typeHint(f Field) string {
	switch f.Type {
	case "integer":
		return "A whole number"
	case "float":
		return "A number"
	case "period":
		return "A period, e.g. 1w 2d 4h"
	}
	return "New value"
}

// project is the short name of the project of the card's issue: the
// prefix of its readable id (BE of BE-131).
func (m *Run) project() string {
	key := cmp.Or(m.issue.Key, m.run.IssueKey)
	p, _, _ := strings.Cut(key, "-")
	return p
}

// current is a field's value on the issue: its values, one for a single
// value field, none when it is empty.
func (m *Run) current(name string) []string {
	var out []string
	for v := range strings.SplitSeq(m.issue.Fields[name], ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// openFields opens the Fields menu, reading the project's fields the
// first time.
func (m *Run) openFields() tea.Cmd {
	if m.d.Fields == nil || !m.loaded {
		return nil
	}
	fe := &m.fe
	fe.open, fe.field, fe.typing = true, nil, false
	project := m.project()
	if fe.loading || (fe.loaded && fe.err == nil && fe.project == project) {
		return nil
	}
	fe.loading, fe.err = true, nil
	ed, profile, to := m.d.Fields, m.run.Profile, m.id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), ytTimeout)
		defer cancel()
		fs, err := ed.Fields(ctx, profile, project)
		return fieldsLoaded{to: to, project: project, fields: fs, err: err}
	}
}

// fieldsRead takes the project's fields in, in fieldOrder.
func (m *Run) fieldsRead(msg fieldsLoaded) {
	fe := &m.fe
	fe.loading, fe.loaded, fe.err, fe.project = false, true, msg.err, msg.project
	if msg.err != nil {
		m.fail(fmt.Errorf("could not read the fields of %s: %w", msg.project, msg.err))
		return
	}
	fe.fields = slices.Clone(msg.fields)
	slices.SortStableFunc(fe.fields, func(a, b Field) int { return fieldOrder(a.Name, b.Name) })
	fe.sel = clamp(fe.sel, len(fe.fields))
}

// fieldValue is one entry of a field's list of values.
type fieldValue struct {
	label, right string
	value        string // what the command sets
	current      bool   // the issue has it now
}

// values are what the field being changed can take, narrowed by the
// filter: the bundle's values, or for a user field those it holds now and
// the people found, else the login being typed.
func (m *Run) values() []fieldValue {
	f := m.fe.field
	if f == nil {
		return nil
	}
	cur := m.current(f.Name)
	q := strings.ToLower(strings.TrimSpace(m.fe.filter))
	match := func(s string) bool { return strings.Contains(strings.ToLower(s), q) }
	var out []fieldValue
	if f.Type != "user" {
		for _, v := range f.Values {
			if match(v) {
				out = append(out, fieldValue{label: v, value: v, current: slices.Contains(cur, v)})
			}
		}
		return out
	}
	var found []Person
	if m.d.People != nil {
		found = m.peopleFor(m.fe.filter)
	}
	seen := map[string]bool{}
	add := func(p Person) {
		if p.Login == "" || seen[p.Login] || !match(p.Login+" "+p.FullName+" "+p.Email) {
			return
		}
		seen[p.Login] = true
		out = append(out, fieldValue{label: cmp.Or(p.FullName, p.Login), right: p.Login, value: p.Login, current: slices.Contains(cur, p.Login)})
	}
	for _, login := range cur { // first, by name when YouTrack found them
		p := Person{Login: login}
		if i := slices.IndexFunc(found, func(f Person) bool { return f.Login == login }); i >= 0 {
			p = found[i]
		}
		add(p)
	}
	for _, p := range found {
		add(p)
	}
	if login := strings.TrimSpace(m.fe.filter); len(out) == 0 && login != "" && !strings.ContainsAny(login, " ,") {
		out = append(out, fieldValue{label: login, right: "login", value: login}) // nobody found: as typed
	}
	return out
}

// askFieldPeople looks up the people a user field can take for the
// filter, once per query, as askPeople does for mentions.
func (m *Run) askFieldPeople() tea.Cmd {
	if f := m.fe.field; f == nil || f.Type != "user" || m.d.People == nil {
		return nil
	}
	return m.lookUpPeople(m.fe.filter)
}

// fieldKey handles a key while the Fields menu is open.
func (m *Run) fieldKey(msg tea.KeyMsg) tea.Cmd {
	fe := &m.fe
	switch {
	case fe.typing:
		return m.inputKey(msg)
	case fe.field != nil:
		return m.valueKey(msg)
	}
	n := len(fe.fields)
	switch keys.Of(msg) {
	case "esc", "q", "e":
		fe.open = false
	case "up", "k":
		fe.sel = clamp(fe.sel-1, n)
	case "down", "j":
		fe.sel = clamp(fe.sel+1, n)
	case "home", "g":
		fe.sel = 0
	case "end", "G":
		fe.sel = max(n-1, 0)
	case "enter", " ", "right", "l":
		if !fe.loaded || n == 0 {
			return nil
		}
		f := fe.fields[clamp(fe.sel, n)]
		if why := editable(f); why != "" {
			m.fail(errors.New(why))
			return nil
		}
		fe.field, fe.filter, fe.vsel = &f, "", 0
		if slices.Contains(typedTypes, f.Type) {
			return m.startInput(f)
		}
		if i := slices.IndexFunc(m.values(), func(v fieldValue) bool { return v.current }); i >= 0 {
			fe.vsel = i
		}
		return m.askFieldPeople()
	}
	return nil
}

// valueKey handles a key on a field's list of values: typing narrows it.
func (m *Run) valueKey(msg tea.KeyMsg) tea.Cmd {
	fe := &m.fe
	items := m.values()
	switch msg.Type {
	case tea.KeyEsc, tea.KeyLeft:
		fe.field, fe.filter = nil, ""
		return nil
	case tea.KeyUp:
		fe.vsel = clamp(fe.vsel-1, len(items))
		return nil
	case tea.KeyDown:
		fe.vsel = clamp(fe.vsel+1, len(items))
		return nil
	case tea.KeyEnter:
		if len(items) == 0 {
			return nil
		}
		return m.pick(items[clamp(fe.vsel, len(items))])
	case tea.KeyBackspace:
		if r := []rune(fe.filter); len(r) > 0 {
			fe.filter, fe.vsel = string(r[:len(r)-1]), 0
		}
		return m.askFieldPeople()
	case tea.KeySpace:
		fe.filter, fe.vsel = fe.filter+" ", 0
		return m.askFieldPeople()
	case tea.KeyRunes:
		fe.filter, fe.vsel = fe.filter+string(msg.Runes), 0
		return m.askFieldPeople()
	}
	return nil
}

// pick sets the value chosen on a field's list; on a field with several
// values it adds the value, or removes it when the issue has it.
func (m *Run) pick(v fieldValue) tea.Cmd {
	f := *m.fe.field
	if !f.Multi && v.current {
		m.say(fmt.Sprintf("%s is %s already.", f.Name, v.value))
		m.fe.field, m.fe.filter = nil, ""
		return nil
	}
	c := fieldChange{field: f.Name, value: v.value, multi: f.Multi, remove: f.Multi && v.current}
	if !f.Multi { // back to the list of fields
		m.fe.field, m.fe.filter = nil, ""
	}
	return m.apply(c)
}

// startInput opens the box of a typed field, with its value.
func (m *Run) startInput(f Field) tea.Cmd {
	in := textinput.New()
	in.Prompt = theme.Accent.Render("›") + " "
	in.CharLimit = 200
	in.SetValue(m.issue.Fields[f.Name])
	in.CursorEnd()
	m.fe.input, m.fe.typing = in, true
	return m.fe.input.Focus()
}

// inputKey handles a key in the box of a typed field.
func (m *Run) inputKey(msg tea.KeyMsg) tea.Cmd {
	fe := &m.fe
	switch msg.String() {
	case "esc":
		fe.typing, fe.field = false, nil
		return nil
	case "enter":
		v := strings.TrimSpace(fe.input.Value())
		if v == "" {
			return nil
		}
		f := *fe.field
		fe.typing, fe.field = false, nil
		if v == strings.TrimSpace(m.issue.Fields[f.Name]) {
			m.say(fmt.Sprintf("%s is %s already.", f.Name, v))
			return nil
		}
		return m.apply(fieldChange{field: f.Name, value: v})
	}
	var cmd tea.Cmd
	fe.input, cmd = fe.input.Update(msg)
	return cmd
}

// apply runs a field change in YouTrack as the profile's user. For a
// backlog issue it then asks whether the source query still finds it.
func (m *Run) apply(c fieldChange) tea.Cmd {
	if m.fe.busy != "" {
		m.fail(fmt.Errorf("still changing %s; wait a moment", m.fe.busy))
		return nil
	}
	m.fe.busy = c.field
	m.say(fmt.Sprintf("Changing %s of %s…", c.field, m.run.IssueKey))
	ed, profile, ref, key, to := m.d.Fields, m.run.Profile, issueRef(m.run), m.run.IssueKey, m.id
	query := ""
	if m.backlog {
		query = m.source
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), ytTimeout)
		defer cancel()
		msg := fieldApplied{to: to, change: c}
		if msg.err = ed.Apply(ctx, profile, ref, c.command()); msg.err != nil || strings.TrimSpace(query) == "" {
			return msg
		}
		found, err := ed.Matches(ctx, profile, key, query)
		msg.left, msg.leftErr = err == nil && !found, err
		return msg
	}
}

// applied takes the outcome of a field change in: the issue shows the
// change at once and is read again; a backlog issue the source query no
// longer finds closes the card and leaves the board's backlog.
func (m *Run) applied(msg fieldApplied) tea.Cmd {
	m.fe.busy = ""
	c, key := msg.change, m.run.IssueKey
	if msg.err != nil {
		m.fail(fmt.Errorf("could not change %s of %s: %w", c.field, key, msg.err))
		return nil
	}
	m.issue.Fields = m.changed(c)
	if msg.left {
		m.fe.open = false
		return tea.Batch(navigate(Back{}), navigate(BacklogLeft{Pipeline: m.pipeline, Issue: key}))
	}
	if msg.leftErr != nil {
		m.fail(fmt.Errorf("changed %s of %s, but could not tell whether the source query still finds it: %w", c.field, key, msg.leftErr))
	} else {
		m.say(c.done(key))
	}
	return m.loadIssue(true)
}

// changed is the issue's fields with a change made, until YouTrack's copy
// is read again.
func (m *Run) changed(c fieldChange) map[string]string {
	out := make(map[string]string, len(m.issue.Fields)+1)
	for k, v := range m.issue.Fields {
		out[k] = v
	}
	if !c.multi {
		out[c.field] = c.value
		return out
	}
	cur := m.current(c.field)
	if c.remove {
		cur = slices.DeleteFunc(cur, func(v string) bool { return v == c.value })
	} else if !slices.Contains(cur, c.value) {
		cur = append(cur, c.value)
	}
	out[c.field] = strings.Join(cur, ", ")
	return out
}

// fieldsHelp is the help line while the Fields menu is open.
func (m *Run) fieldsHelp() []binding {
	fe := m.fe
	switch {
	case fe.typing:
		return []binding{{"enter", "set"}, {"esc", "back"}}
	case fe.field != nil && fe.field.Multi:
		return []binding{{"type", "filter"}, {"↑↓", "choose"}, {"enter", "add/remove"}, {"esc", "back"}}
	case fe.field != nil:
		return []binding{{"type", "filter"}, {"↑↓", "choose"}, {"enter", "set"}, {"esc", "back"}}
	}
	return []binding{{"↑↓", "choose"}, {"enter", "change"}, {"esc", "close"}}
}

// fieldsBox is the Fields menu, drawn like the pipelines' menu, at most
// maxW cells wide and maxH lines tall.
func (m *Run) fieldsBox(maxW, maxH int) []string {
	fe := &m.fe
	rows := max(maxH-2, 1) // inside the frame
	switch {
	case fe.typing:
		return m.inputBox(maxW)
	case fe.field != nil:
		return m.valuesBox(maxW, rows)
	}
	title := theme.Bold.Render("Fields")
	if fe.project != "" {
		title += theme.Dim.Render(" · " + fe.project)
	}
	var lines []string
	inner := menuMinWidth
	switch {
	case !fe.loaded || fe.loading:
		lines = []string{theme.Dim.Render("Loading fields…")}
	case fe.err != nil:
		inner = max(min(48, maxW-4), 8)
		lines = wrap(theme.Err.Render("✗ ")+"Could not read the fields: "+oneLine(fe.err.Error()), inner)
	case len(fe.fields) == 0:
		lines = []string{theme.Dim.Render("The project has no fields.")}
	default:
		labelW, valueW := 0, 0
		for _, f := range fe.fields {
			labelW = max(labelW, ansi.StringWidth(f.Name))
			valueW = max(valueW, min(ansi.StringWidth(m.issue.Fields[f.Name]), chipMax))
		}
		inner = max(2+labelW+3+max(valueW, 1), menuMinWidth)
		inner = min(inner, max(maxW-4, 8))
		why := ""
		top := scrollTop(fe.sel, len(fe.fields), rows)
		for i, f := range fe.fields[top:min(top+rows, len(fe.fields))] {
			i += top
			label, value := f.Name, theme.Dim.Render("—")
			if v := oneLine(m.issue.Fields[f.Name]); v != "" {
				value = cut(v, chipMax)
			}
			switch {
			case editable(f) != "":
				label = theme.Dim.Render(label)
			case i == fe.sel:
				label = theme.Bold.Render(label)
			}
			if i == fe.sel {
				why = editable(f)
			}
			lines = append(lines, spread(marker(i == fe.sel)+label, value, inner))
		}
		if why != "" && len(lines)+2 <= rows {
			lines = append(lines, "")
			for _, l := range wrap(why, inner) {
				lines = append(lines, theme.Warn.Render(l))
			}
		}
	}
	return box(lines, inner+4, solidEdges, theme.Accent, title, m.busyBadge())
}

// busyBadge marks the menu while a change is on its way to YouTrack.
func (m *Run) busyBadge() string {
	if m.fe.busy == "" {
		return ""
	}
	return theme.Accent.Render(spinner[m.frameN%len(spinner)]) + theme.Dim.Render(" saving")
}

// valuesBox is the list of values of the field being changed, under the
// filter line.
func (m *Run) valuesBox(maxW, rows int) []string {
	fe := &m.fe
	f := fe.field
	items := m.values()
	fe.vsel = clamp(fe.vsel, len(items))
	labelW, rightW := 0, 0
	for _, it := range f.Values {
		labelW = max(labelW, ansi.StringWidth(it))
	}
	for _, it := range items {
		labelW = max(labelW, ansi.StringWidth(it.label))
		rightW = max(rightW, ansi.StringWidth(it.right)+2) // and the check
	}
	inner := min(max(2+labelW+3+rightW, ansi.StringWidth(f.Name)+12, menuMinWidth), max(maxW-4, 8))
	filter := theme.Dim.Render("type to filter")
	if fe.filter != "" {
		filter = fe.filter + theme.Accent.Render("▏")
	}
	lines := []string{cut(theme.Accent.Render("/")+" "+filter, inner), ""}
	room := max(rows-len(lines), 1)
	if len(items) == 0 {
		what := "No values match."
		if f.Type == "user" && fe.filter == "" {
			what = "Type a name or a login."
		}
		lines = append(lines, theme.Dim.Render(what))
	}
	top := scrollTop(fe.vsel, len(items), room)
	for i, it := range items[top:min(top+room, len(items))] {
		i += top
		label := it.label
		if i == fe.vsel {
			label = theme.Bold.Render(label)
		}
		right := theme.Dim.Render(it.right)
		if it.current {
			right = strings.TrimSpace(right + " " + theme.OK.Render("✓"))
		}
		lines = append(lines, spread(marker(i == fe.vsel)+label, right, inner))
	}
	title := theme.Bold.Render(f.Name)
	if f.Multi {
		title += theme.Dim.Render(" · add or remove")
	}
	return box(lines, inner+4, solidEdges, theme.Accent, title, m.busyBadge())
}

// inputBox is the box of a typed field.
func (m *Run) inputBox(maxW int) []string {
	f := m.fe.field
	inner := min(44, max(maxW-4, 10))
	m.fe.input.Width = max(inner-3, 1)
	lines := []string{theme.Dim.Render(typeHint(*f)), m.fe.input.View()}
	return box(lines, inner+4, solidEdges, theme.Accent, theme.Bold.Render(f.Name), m.busyBadge())
}

// scrollTop is the first of n entries to show in rows lines so that sel
// shows, roughly in the middle once the list scrolls.
func scrollTop(sel, n, rows int) int {
	if n <= rows {
		return 0
	}
	return min(max(sel-rows/2, 0), n-rows)
}
