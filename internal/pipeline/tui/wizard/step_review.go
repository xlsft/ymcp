package wizard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/engine"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/tui/keys"
)

type checkMsg struct {
	id, seq int
	results []engine.CheckResult
	err     error
}

// reviewStep sums the pipeline up and runs the full check; the buttons
// under it save.
type reviewStep struct {
	base
	all     []*item
	deb     debouncer
	results []engine.CheckResult
	err     error
	pending bool
	checked string // JSON of the pipeline the results are for
}

func newReviewStep(sh *shared) *reviewStep {
	s := &reviewStep{base: base{sh: sh}, deb: debouncer{id: sh.newID()}}
	s.all = []*item{
		{id: "summary", heading: "Summary", below: s.summary},
		{
			id: "invalid", heading: "Fix these before saving",
			hidden: func() bool { return s.final().Validate() == nil },
			below:  s.invalid,
		},
		{
			id: "checks", heading: "Checks", headNote: "c runs them again",
			headMark: func() string {
				if s.pending {
					return markPending
				}
				return ""
			},
			hidden: func() bool { return s.final().Validate() != nil },
			below:  s.checkList,
		},
	}
	return s
}

func (s *reviewStep) title() string { return "Review" }
func (s *reviewStep) intro() string {
	return "The pipeline at a glance and the full check; failures don't block."
}
func (s *reviewStep) items() []*item { return s.all }

// final is the pipeline as it will be saved.
func (s *reviewStep) final() spec.Pipeline { return clonePipeline(*s.sh.p).WithDefaults() }

func (s *reviewStep) enter() tea.Cmd {
	s.fs.focus = btnSave
	p := s.final()
	if p.Validate() != nil {
		return nil
	}
	data, _ := json.Marshal(p)
	if string(data) == s.checked && (s.pending || s.results != nil || s.err != nil) {
		return nil
	}
	return s.check()
}

func (s *reviewStep) check() tea.Cmd {
	p := s.final()
	data, _ := json.Marshal(p)
	s.checked = string(data)
	s.deb.seq++
	s.pending, s.results, s.err = true, nil, nil
	id, seq, ctx, ch := s.deb.id, s.deb.seq, s.sh.ctx, s.sh.deps.Checker
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 2*requestTimeout)
		defer cancel()
		res, err := ch.Check(ctx, p)
		return checkMsg{id: id, seq: seq, results: res, err: err}
	}
}

func (s *reviewStep) leave() {}

func (s *reviewStep) shortcut(msg tea.KeyMsg, _ string) (tea.Cmd, bool) {
	switch keys.Of(msg) {
	case "c":
		return s.check(), true
	}
	return nil, false
}

func (s *reviewStep) extraHelp(string) string {
	return keyHelp("c", "check again")
}

func (s *reviewStep) msg(msg tea.Msg) tea.Cmd {
	if m, ok := msg.(checkMsg); ok && s.deb.current(m.id, m.seq) {
		s.pending, s.results, s.err = false, m.results, m.err
	}
	return nil
}

func (s *reviewStep) summary(width int) string {
	p := s.final()
	var b []string
	row := func(k, v string) {
		b = append(b, styleDim.Render(fmt.Sprintf("%-11s", k))+" "+oneLine(v, width-12))
	}
	row("Pipeline", p.Name+" · profile "+p.Profile)
	src := p.Source.Query
	if smp := s.sh.sample; smp.done && smp.err == nil {
		src += fmt.Sprintf("  (%d issues now)", len(smp.issues))
	}
	row("Source", src)
	var stages []string
	for i, st := range stageList(&p) {
		if st.On() && (st.Enter != "" || st.Match != "") {
			stages = append(stages, strings.ToLower(stageDefs[i].name)+" ("+orDash(st.Enter)+")")
		}
	}
	row("Board", strings.Join(stages, " · "))
	where := orDash(p.Workspace.Dir)
	if p.Workspace.Host != "" {
		where = p.Workspace.Host + ":" + where
	}
	rest := fmt.Sprintf(" · %d at once", p.Workspace.Parallel)
	if room := width - 12 - lenRunes(rest); lenRunes(where) > room && room > 10 {
		// A path matters at its end.
		r := []rune(where)
		where = "…" + string(r[len(r)-room+1:])
	}
	row("Repository", where+rest)
	st := p.Strategy.Kind
	if p.Strategy.Draft {
		st += " (draft)"
	}
	row("Publishing", st)
	ag := p.Agent.Provider
	if p.Agent.Mode != "" {
		ag += " · mode " + p.Agent.Mode
	}
	if p.Agent.Model != "" {
		ag += " · model " + p.Agent.Model
	}
	row("Agent", ag+" · permissions "+p.Permissions)
	row("Prompts", p.Prompts.Preset+" · "+p.Language)
	fb := "youtrack " + orDash(strings.Join(p.Feedback.YouTrack, ", ")) + " · github " + orDash(strings.Join(p.Feedback.GitHub, ", ")) + " · gitlab " + orDash(strings.Join(p.Feedback.GitLab, ", "))
	if p.AutoCompact {
		fb += " · compact finished runs"
	}
	row("Feedback", fb)
	notify := "the machine's settings"
	if n := p.Notifications; n != nil {
		notify = "providers " + orDash(strings.Join(n.Providers, ", ")) + " · events " + orDash(strings.Join(n.Events, ", "))
	}
	row("Alerts", notify)
	return strings.Join(b, "\n")
}

func (s *reviewStep) invalid(width int) string {
	var ve *spec.ValidationError
	if err := s.final().Validate(); errors.As(err, &ve) {
		var lines []string
		for _, pr := range ve.Problems {
			lines = append(lines, errLine(pr, width))
		}
		return strings.Join(lines, "\n")
	} else if err != nil {
		return errLine(err.Error(), width)
	}
	return ""
}

// checkSection is the step a check result belongs to.
func checkSection(name string) string {
	switch {
	case name == "pipeline":
		return "Config"
	case name == "YouTrack" || name == "source query":
		return "Basics"
	case strings.HasPrefix(name, "stages.") || strings.HasSuffix(name, " command") || strings.Contains(name, "leaves the source"):
		return "Board"
	case strings.HasPrefix(name, "workspace") || strings.Contains(name, "strategy") ||
		strings.Contains(name, "github") || strings.Contains(name, "gitlab") || strings.Contains(name, "token"):
		return "Repository"
	case strings.HasPrefix(name, "agent"):
		return "Agent"
	}
	return "Other"
}

// checkList shows the results, failures first, then warnings, each with
// the step it belongs to.
func (s *reviewStep) checkList(width int) string {
	switch {
	case s.pending:
		return markPending + styleDim.Render(" checking YouTrack, the repository and the agent…")
	case s.err != nil:
		return errLine(s.err.Error(), width)
	case s.results == nil:
		return styleDim.Render("not checked yet")
	}
	var lines []string
	for _, pass := range []int{0, 1, 2} {
		for _, r := range s.results {
			rank, mark := 2, markOK
			switch {
			case !r.OK:
				rank, mark = 0, markErr
			case r.Warning:
				rank, mark = 1, markWarn
			}
			if rank == pass {
				sec := styleDim.Render(fmt.Sprintf("%-11s", checkSection(r.Name)))
				lines = append(lines, sec+" "+mark+" "+oneLine(r.Name+": "+r.Detail, width-14))
			}
		}
	}
	if failed := countFailed(s.results); failed > 0 {
		lines = append(lines, styleWarn.Render(fmt.Sprintf("%d checks failed; you can still save and fix them later", failed)))
	}
	return strings.Join(lines, "\n")
}

func countFailed(rs []engine.CheckResult) int {
	n := 0
	for _, r := range rs {
		if !r.OK {
			n++
		}
	}
	return n
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func (s *reviewStep) blockers() []problem {
	var ve *spec.ValidationError
	if err := s.final().Validate(); errors.As(err, &ve) {
		var out []problem
		for _, pr := range ve.Problems {
			out = append(out, problem{"invalid", pr})
		}
		return out
	} else if err != nil {
		return []problem{{"invalid", err.Error()}}
	}
	return nil
}

func (s *reviewStep) mark() string {
	switch {
	case s.final().Validate() != nil:
		return markBad
	case s.pending:
		return markPending
	case s.err != nil || countFailed(s.results) > 0:
		return markBad
	}
	for _, r := range s.results {
		if r.Warning {
			return markWarn
		}
	}
	if s.results == nil {
		return markOff
	}
	return markOK
}
