package wizard

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// sampleSize is how many matching issues the source step lists.
const sampleSize = 10

// sourceStep edits the source query with YouTrack's completions, and
// shows how many issues it matches and which.
type sourceStep struct {
	sh    *shared
	query *assistField
	deb   debouncer

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

func newSourceStep(sh *shared) *sourceStep {
	s := &sourceStep{sh: sh, deb: debouncer{id: sh.newID()}, savedDeb: debouncer{id: sh.newID()}}
	s.query = newAssist(sh, "Source query", "project: BE #Unresolved tag: ai", searchSuggest(sh))
	s.query.set(sh.p.Source.Query)
	return s
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

func (s *sourceStep) title() string { return "Source" }

func (s *sourceStep) enter() tea.Cmd {
	s.query.focus()
	if s.sh.sample.query != s.sh.p.Source.Query || (!s.sh.sample.done && !s.sh.sample.pending) {
		return s.search()
	}
	return nil
}

func (s *sourceStep) leave() { s.query.blur(); s.savedOpen = false }

// search runs the source query now.
func (s *sourceStep) search() tea.Cmd {
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

func (s *sourceStep) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	if s.savedOpen {
		switch msg.String() {
		case "esc", "ctrl+s":
			s.savedOpen = false
			return nil, true
		case "enter", "tab":
			if it, ok := s.saved.selected(); ok {
				s.query.set(it.value)
				s.sh.p.Source.Query = it.value
				s.savedOpen = false
				return s.search(), true
			}
			return nil, true
		}
		s.saved.key(msg)
		return nil, true
	}
	if msg.String() == "ctrl+s" {
		s.savedOpen = true
		if s.saved == nil && !s.savedPending {
			return s.loadSaved(), true
		}
		return nil, true
	}
	cmd, consumed, changed := s.query.key(msg)
	if changed {
		s.sh.p.Source.Query = s.query.value()
		cmd = tea.Batch(cmd, s.deb.bump(s.sh.delay))
	}
	if !consumed && msg.String() == "enter" {
		return next, true
	}
	return cmd, consumed
}

func (s *sourceStep) loadSaved() tea.Cmd {
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

func (s *sourceStep) msg(msg tea.Msg) tea.Cmd {
	if s.deb.fired(msg) {
		return s.search()
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
	return s.query.msg(msg)
}

func (s *sourceStep) view(width, height int) string {
	var b strings.Builder
	b.WriteString(wrap("Which issues should the agent take? Type a YouTrack search; completions come from YouTrack as you type. An issue leaves the source when the working command runs on it.", width) + "\n\n")
	if s.savedOpen {
		b.WriteString(styleLabel.Render("Saved searches") + "\n")
		switch {
		case s.savedPending:
			b.WriteString(markPending + " loading…")
		case s.savedErr != nil:
			b.WriteString(markErr + " " + s.savedErr.Error())
		case s.saved != nil:
			b.WriteString(s.saved.view(width, height-6))
		}
		return b.String()
	}
	b.WriteString(s.query.view(width, true) + "\n\n")
	b.WriteString(sampleView(s.sh, width))
	b.WriteString("\n" + styleDim.Render(wrap("Tip: keep the source narrow, e.g. a tag only issues meant for the agent carry (tag: ai), and #Unresolved.", width)))
	return b.String()
}

// sampleView tells how many issues the source matches and lists some.
func sampleView(sh *shared, width int) string {
	smp := sh.sample
	switch {
	case sh.conn.yt == nil:
		return markOff + styleDim.Render(" sign in to a profile to see matching issues") + "\n"
	case strings.TrimSpace(sh.p.Source.Query) == "":
		return markOff + styleDim.Render(" type a query to see matching issues") + "\n"
	case smp.pending:
		return markPending + " searching…\n"
	case smp.err != nil:
		return markErr + " " + wrap(smp.err.Error(), width-2) + "\n"
	case !smp.done:
		return ""
	case len(smp.issues) == 0:
		return markWarn + " no issue matches right now\n"
	}
	var b strings.Builder
	n := len(smp.issues)
	word := "issues match"
	if n == 1 {
		word = "issue matches"
	}
	b.WriteString(markOK + " " + styleBold.Render(fmt.Sprint(n)) + " " + word + " right now\n")
	for _, is := range smp.issues[:min(sampleSize, n)] {
		b.WriteString("  " + styleCode.Render(fmt.Sprintf("%-9s", is.Key)) + " " + oneLine(is.Summary, width-14) + "\n")
	}
	if n > sampleSize {
		b.WriteString(styleDim.Render(fmt.Sprintf("  … and %d more", n-sampleSize)) + "\n")
	}
	return b.String()
}

func (s *sourceStep) help() string {
	if s.savedOpen {
		return keyHelp("↑/↓", "choose", "enter", "use this query", "esc", "back")
	}
	if s.query.showing() {
		return keyHelp("↑/↓", "choose completion", "tab", "complete", "esc", "hide")
	}
	return keyHelp("ctrl+space", "completions", "ctrl+s", "saved searches", "enter", "continue")
}

func (s *sourceStep) mark() string {
	smp := s.sh.sample
	switch {
	case strings.TrimSpace(s.sh.p.Source.Query) == "":
		return markOff
	case smp.pending:
		return markPending
	case smp.err != nil:
		return markErr
	case smp.done && len(smp.issues) == 0:
		return markWarn
	case smp.done:
		return markOK
	}
	return markOff
}
