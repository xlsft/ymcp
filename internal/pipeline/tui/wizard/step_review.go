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
)

type checkMsg struct {
	id, seq int
	results []engine.CheckResult
	err     error
}

// reviewStep sums the pipeline up, runs the full check and saves.
type reviewStep struct {
	sh      *shared
	deb     debouncer
	results []engine.CheckResult
	err     error
	pending bool
	checked string // JSON of the pipeline the results are for
	btn     int    // 0 save, 1 save & run
}

func newReviewStep(sh *shared) *reviewStep {
	return &reviewStep{sh: sh, deb: debouncer{id: sh.newID()}}
}

func (s *reviewStep) title() string { return "Review & save" }

// final is the pipeline as it will be saved.
func (s *reviewStep) final() spec.Pipeline { return clonePipeline(*s.sh.p).WithDefaults() }

func (s *reviewStep) enter() tea.Cmd {
	p := s.final()
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

func (s *reviewStep) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "left", "h", "shift+tab":
		s.btn = 0
		return nil, true
	case "right", "l", "tab":
		s.btn = 1
		return nil, true
	case "c":
		return s.check(), true
	case "s":
		return s.done(false), true
	case "r":
		return s.done(true), true
	case "enter":
		return s.done(s.btn == 1), true
	}
	return nil, false
}

// done ends the wizard, unless the pipeline is invalid.
func (s *reviewStep) done(run bool) tea.Cmd {
	p := s.final()
	if p.Validate() != nil {
		return nil
	}
	return func() tea.Msg { return Done{Pipeline: p, RunNow: run} }
}

func (s *reviewStep) msg(msg tea.Msg) tea.Cmd {
	if m, ok := msg.(checkMsg); ok && s.deb.current(m.id, m.seq) {
		s.pending, s.results, s.err = false, m.results, m.err
	}
	return nil
}

func (s *reviewStep) view(width, height int) string {
	p := s.final()
	var b strings.Builder
	row := func(k, v string) {
		b.WriteString(styleDim.Render(fmt.Sprintf("%-10s", k)) + " " + oneLine(v, width-12) + "\n")
	}
	row("Pipeline", p.Name+" · profile "+p.Profile)
	src := p.Source.Query
	if smp := s.sh.sample; smp.done && smp.err == nil {
		src += fmt.Sprintf("  (%d issues now)", len(smp.issues))
	}
	row("Source", src)
	for i, st := range stageList(&p) {
		name := strings.ToLower(stageDefs[i].name)
		switch {
		case !st.On():
			row("", name+": off")
		default:
			v := name + ": " + orDash(st.Enter) + " · match " + orDash(st.Match)
			if st.Exit != "" {
				v += " · exit " + st.Exit
			}
			k := ""
			if i == 0 {
				k = "Stages"
			}
			row(k, v)
		}
	}
	row("Workspace", orDash(p.Workspace.Dir)+fmt.Sprintf(" · parallel %d · branch %s", p.Workspace.Parallel, p.Workspace.Branch))
	ag := p.Agent.Provider
	if p.Agent.Mode != "" {
		ag += " · mode " + p.Agent.Mode
	}
	if p.Agent.Model != "" {
		ag += " · model " + p.Agent.Model
	}
	row("Agent", ag+" · permissions "+p.Permissions)
	st := p.Strategy.Kind
	if p.Strategy.Draft {
		st += " (draft)"
	}
	row("Strategy", st)
	row("Feedback", "youtrack "+orDash(strings.Join(p.Feedback.YouTrack, ", "))+" · github "+orDash(strings.Join(p.Feedback.GitHub, ", "))+" · gitlab "+orDash(strings.Join(p.Feedback.GitLab, ", ")))
	row("Prompts", p.Prompts.Preset+" · "+p.Language)
	b.WriteString("\n")

	var ve *spec.ValidationError
	if err := p.Validate(); errors.As(err, &ve) {
		b.WriteString(markErr + " " + styleBold.Render("Fix these before saving:") + "\n")
		for _, pr := range ve.Problems {
			b.WriteString("  " + oneLine(pr, width-2) + "\n")
		}
		return b.String()
	}

	b.WriteString(styleLabel.Render("Check") + styleDim.Render("  (c runs it again)") + "\n")
	used := strings.Count(b.String(), "\n")
	room := max(height-used-3, 3)
	switch {
	case s.pending:
		b.WriteString(markPending + " checking YouTrack, the repository and the agent…\n")
	case s.err != nil:
		b.WriteString(markErr + " " + oneLine(s.err.Error(), width-4) + "\n")
	default:
		b.WriteString(checkList(s.results, width, room))
	}
	b.WriteString("\n")
	save, run := " Save ", " Save & run in background "
	if s.btn == 0 {
		save = styleSel.Render(save)
		run = styleBorder.Render("[") + run + styleBorder.Render("]")
	} else {
		run = styleSel.Render(run)
		save = styleBorder.Render("[") + save + styleBorder.Render("]")
	}
	b.WriteString(save + "  " + run)
	if failed := countFailed(s.results); failed > 0 {
		b.WriteString("  " + styleWarn.Render(fmt.Sprintf("%d checks failed; you can still save and fix them later", failed)))
	}
	return b.String()
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

// checkList shows failures first, then warnings, then the passed checks,
// in at most room lines.
func checkList(rs []engine.CheckResult, width, room int) string {
	var bad, warn, ok []engine.CheckResult
	for _, r := range rs {
		switch {
		case !r.OK:
			bad = append(bad, r)
		case r.Warning:
			warn = append(warn, r)
		default:
			ok = append(ok, r)
		}
	}
	var lines []string
	for _, r := range bad {
		lines = append(lines, markErr+" "+oneLine(r.Name+": "+r.Detail, width-2))
	}
	for _, r := range warn {
		lines = append(lines, markWarn+" "+oneLine(r.Name+": "+r.Detail, width-2))
	}
	for _, r := range ok {
		lines = append(lines, markOK+" "+oneLine(r.Name+": "+r.Detail, width-2))
	}
	if len(lines) > room {
		hidden := len(lines) - room + 1
		lines = append(lines[:room-1], styleDim.Render(fmt.Sprintf("  … %d more passed", hidden)))
	}
	return strings.Join(lines, "\n") + "\n"
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func (s *reviewStep) help() string {
	return keyHelp("enter", "confirm", "←/→", "choose", "s", "save", "r", "save & run", "c", "check again")
}

func (s *reviewStep) mark() string {
	switch {
	case s.final().Validate() != nil:
		return markErr
	case s.pending:
		return markPending
	case s.err != nil || countFailed(s.results) > 0:
		return markErr
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
