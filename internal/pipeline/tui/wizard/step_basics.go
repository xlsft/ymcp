package wizard

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// sampleSize is how many matching issues the source query lists.
const sampleSize = 3

// basicsStep asks for the pipeline name, the YouTrack profile (signed in
// to as soon as it is picked) and the source query, with YouTrack's
// completions and a live count of what it matches.
type basicsStep struct {
	base
	name, query *assistField
	nameErr     string
	all         []*item

	deb     debouncer // the source search
	connDeb debouncer // signing in to a newly picked profile

	saved        *pickList
	savedOpen    bool
	savedErr     error
	savedPending bool
	savedDeb     debouncer
}

type sampleMsg struct {
	id, seq int
	query   string
	issues  []ytwatch.Issue
	err     error
}

type savedMsg struct {
	id   int
	list []SavedSearch
	err  error
}

func newBasicsStep(sh *shared) *basicsStep {
	s := &basicsStep{base: base{sh: sh}, deb: debouncer{id: sh.newID()}, connDeb: debouncer{id: sh.newID()}, savedDeb: debouncer{id: sh.newID()}}
	s.name = newAssist(sh, "e.g. backend", nil)
	s.name.set(sh.p.Name)
	s.query = newAssist(sh, "e.g. project: BE #Unresolved tag: ai", searchSuggest(sh))
	s.query.set(sh.p.Source.Query)
	s.checkName()

	s.all = []*item{
		{
			id: "name", label: "Pipeline name",
			desc: "Lowercase letters, digits, - and _; shown in the agent's messages.",
			ctl: newText(s.name, func() tea.Cmd {
				sh.p.Name = s.name.value()
				s.checkName()
				return nil
			}),
			problem: func() string {
				if s.nameErr != "" && (sh.p.Name != "" || s.fs.tried) {
					return s.nameErr
				}
				return ""
			},
		},
		{
			id: "profile", label: "YouTrack profile",
			desc: "The account ymcp works as; `ymcp setup` adds profiles.",
			ctl: &choiceCtl{
				opts: s.profileOptions,
				get:  func() string { return sh.p.Profile },
				set: func(v string) tea.Cmd {
					sh.p.Profile = v
					return s.connDeb.bump(sh.delay)
				},
			},
			hidden: func() bool { return len(sh.profiles) == 0 },
			below:  s.signInView,
		},
		{
			id: "noprofile",
			below: func(width int) string {
				if sh.profilesErr != nil {
					return errLine("can't read the YouTrack profiles: "+sh.profilesErr.Error(), width)
				}
				return errLine("No YouTrack profiles yet. Run `ymcp setup` to add one, then come back.", width)
			},
			hidden: func() bool { return len(sh.profiles) > 0 },
		},
		{
			id: "query", label: "Source query",
			desc: "The YouTrack search for issues the agent takes. Keep it narrow.",
			ctl: newText(s.query, func() tea.Cmd {
				sh.p.Source.Query = s.query.value()
				return s.deb.bump(sh.delay)
			}),
			problem: func() string {
				if s.fs.tried && strings.TrimSpace(sh.p.Source.Query) == "" {
					return "Type the query that finds issues for the agent."
				}
				return ""
			},
			below: func(width int) string { return sampleView(sh, width) },
		},
	}
	return s
}

func (s *basicsStep) title() string { return "Basics" }
func (s *basicsStep) intro() string {
	return "Name the pipeline, pick the YouTrack account and the issues to take."
}
func (s *basicsStep) items() []*item { return s.all }

// checkName validates the name: its form, and that no other pipeline has
// it.
func (s *basicsStep) checkName() {
	name := s.sh.p.Name
	s.nameErr = ""
	if err := spec.CheckName(name); err != nil {
		s.nameErr = err.Error()
		if name == "" {
			s.nameErr = "Give the pipeline a name."
		}
		return
	}
	if name == s.sh.orig {
		return
	}
	exists, err := s.sh.deps.Local.PipelineExists(name)
	switch {
	case err != nil:
		s.nameErr = err.Error()
	case exists:
		s.nameErr = fmt.Sprintf("a pipeline named %q already exists", name)
	}
}

func (s *basicsStep) profileOptions() []option {
	var out []option
	for _, c := range s.sh.profiles {
		desc := c.URL
		if c.Login != "" {
			desc += " (" + c.Login + ")"
		}
		out = append(out, option{value: c.Name, title: c.Title(), desc: desc})
	}
	return out
}

func (s *basicsStep) signInView(width int) string {
	c := s.sh.conn
	switch {
	case c.profile == "":
		return ""
	case c.pending:
		return markPending + styleDim.Render(" signing in to "+c.profile+"…")
	case c.yt != nil:
		return markOK + " signed in to " + c.yt.BaseURL() + " as " + styleBold.Render(c.login)
	case c.err != nil:
		return errLine("can't sign in with "+c.profile+": "+c.err.Error(), width) + "\n" +
			styleDim.Render("  fix the profile with `ymcp setup`, then press ctrl+r to try again")
	}
	return ""
}

// searchSuggest completes YouTrack search queries.
func searchSuggest(sh *shared) suggestFunc {
	return func(text string, caret int) func(context.Context) ([]Suggestion, error) {
		yt := sh.conn.yt
		if yt == nil {
			return nil
		}
		return func(ctx context.Context) ([]Suggestion, error) { return yt.SearchAssist(ctx, text, caret) }
	}
}

func (s *basicsStep) enter() tea.Cmd {
	if s.sh.sample.query != s.sh.p.Source.Query || (!s.sh.sample.done && !s.sh.sample.pending) {
		return s.search()
	}
	return nil
}

func (s *basicsStep) leave() { s.savedOpen = false }

// search runs the source query now.
func (s *basicsStep) search() tea.Cmd {
	yt, q := s.sh.conn.yt, strings.TrimSpace(s.sh.p.Source.Query)
	s.deb.seq++
	if yt == nil || q == "" {
		s.sh.sample = sample{query: s.sh.p.Source.Query}
		return nil
	}
	s.sh.sample.pending = true
	id, seq, ctx := s.deb.id, s.deb.seq, s.sh.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		issues, err := yt.Search(ctx, q)
		return sampleMsg{id: id, seq: seq, query: q, issues: issues, err: err}
	}
}

func (s *basicsStep) shortcut(msg tea.KeyMsg, _ string) (tea.Cmd, bool) {
	switch msg.String() {
	case "ctrl+r":
		s.sh.conn = connection{}
		return s.sh.connect(s.sh.p.Profile), true
	case "ctrl+s":
		s.savedOpen = true
		s.fs.focus = "query"
		if s.saved == nil && !s.savedPending {
			return s.loadSaved(), true
		}
		return nil, true
	}
	return nil, false
}

func (s *basicsStep) extraHelp(focus string) string {
	if focus == "query" && !s.query.showing() {
		return keyHelp("ctrl+s", "saved searches")
	}
	if focus == "profile" && s.sh.conn.err != nil {
		return keyHelp("ctrl+r", "sign in again")
	}
	return ""
}

func (s *basicsStep) overlay() overlay {
	if s.savedOpen {
		return savedOverlay{s}
	}
	return nil
}

// savedOverlay picks one of the user's saved searches as the query.
type savedOverlay struct{ s *basicsStep }

func (o savedOverlay) key(msg tea.KeyMsg) tea.Cmd {
	s := o.s
	switch msg.String() {
	case "esc", "ctrl+s":
		s.savedOpen = false
		return nil
	case "enter", "tab":
		if s.saved == nil {
			return nil
		}
		if it, ok := s.saved.selected(); ok {
			s.query.set(it.value)
			s.sh.p.Source.Query = it.value
			s.savedOpen = false
			return s.search()
		}
		return nil
	}
	if s.saved != nil {
		s.saved.key(msg)
	}
	return nil
}

func (o savedOverlay) view(width, height int) string {
	s := o.s
	var b strings.Builder
	b.WriteString(styleLabel.Render("Saved searches") + "\n" + styleDim.Render("Pick one of your saved searches as the source query.") + "\n")
	switch {
	case s.savedPending:
		b.WriteString(markPending + " loading…")
	case s.savedErr != nil:
		b.WriteString(errLine(s.savedErr.Error(), width))
	case s.saved != nil:
		b.WriteString(s.saved.view(width, height-2))
	}
	return b.String()
}

func (o savedOverlay) help() string {
	return keyHelp("type", "filter", "↑/↓", "choose", "enter", "use this query", "esc", "close")
}

func (s *basicsStep) loadSaved() tea.Cmd {
	yt := s.sh.conn.yt
	if yt == nil {
		s.savedErr = fmt.Errorf("sign in to a profile first")
		return nil
	}
	s.savedPending = true
	id, ctx := s.savedDeb.id, s.sh.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		list, err := yt.SavedSearches(ctx)
		return savedMsg{id: id, list: list, err: err}
	}
}

func (s *basicsStep) msg(msg tea.Msg) tea.Cmd {
	if s.deb.fired(msg) {
		return s.search()
	}
	if s.connDeb.fired(msg) {
		return s.sh.connect(s.sh.p.Profile)
	}
	switch m := msg.(type) {
	case connMsg:
		s.saved, s.savedErr = nil, nil
		if m.err == nil && m.profile == s.sh.p.Profile {
			return s.search()
		}
		return nil
	case sampleMsg:
		if s.deb.current(m.id, m.seq) {
			s.sh.sample = sample{query: s.sh.p.Source.Query, issues: m.issues, err: m.err, done: true}
		}
		return nil
	case savedMsg:
		if m.id == s.savedDeb.id {
			s.savedPending = false
			s.savedErr = m.err
			var items []pickItem
			for _, q := range m.list {
				items = append(items, pickItem{title: q.Name, desc: q.Query, value: q.Query})
			}
			s.saved = newPickList(items, true)
		}
		return nil
	}
	return tea.Batch(s.name.msg(msg), s.query.msg(msg))
}

// sampleView tells how many issues the source matches and lists some.
func sampleView(sh *shared, width int) string {
	smp := sh.sample
	switch {
	case sh.conn.yt == nil:
		return markOff + styleDim.Render(" sign in to a profile to see matching issues")
	case strings.TrimSpace(sh.p.Source.Query) == "":
		return markOff + styleDim.Render(" type a query to see matching issues")
	case smp.pending:
		return markPending + styleDim.Render(" searching…")
	case smp.err != nil:
		return errLine(smp.err.Error(), width)
	case !smp.done:
		return ""
	case len(smp.issues) == 0:
		return markWarn + " no issue matches right now"
	}
	var b strings.Builder
	n := len(smp.issues)
	word := "issues match"
	if n == 1 {
		word = "issue matches"
	}
	b.WriteString(markOK + " " + styleBold.Render(fmt.Sprint(n)) + " " + word + " right now")
	for _, is := range smp.issues[:min(sampleSize, n)] {
		b.WriteString("\n  " + styleCode.Render(fmt.Sprintf("%-9s", is.Key)) + " " + oneLine(is.Summary, width-14))
	}
	if n > sampleSize {
		b.WriteString("\n" + styleDim.Render(fmt.Sprintf("  … and %d more", n-sampleSize)))
	}
	return b.String()
}

func (s *basicsStep) blockers() []problem {
	var out []problem
	switch {
	case s.sh.p.Name == "":
		out = append(out, problem{"name", "a pipeline name"})
	case s.nameErr != "":
		out = append(out, problem{"name", "a valid pipeline name"})
	}
	if s.sh.p.Profile == "" {
		out = append(out, problem{"profile", "a YouTrack profile"})
	}
	if strings.TrimSpace(s.sh.p.Source.Query) == "" {
		out = append(out, problem{"query", "a source query"})
	}
	return out
}

func (s *basicsStep) mark() string {
	smp := s.sh.sample
	switch {
	case len(s.blockers()) > 0 || s.sh.conn.err != nil || smp.err != nil:
		return markBad
	case s.sh.conn.pending || smp.pending:
		return markPending
	case smp.done && len(smp.issues) == 0:
		return markWarn
	case s.sh.conn.yt != nil && smp.done:
		return markOK
	}
	return markOff
}
