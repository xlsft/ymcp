package wizard

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

var strategyKinds = []pickItem{
	{title: "branch", value: "branch", desc: "push the branch; you open PRs/MRs yourself"},
	{title: "github-pr", value: "github-pr", desc: "push and open a GitHub pull request"},
	{title: "github-issue-pr", value: "github-issue-pr", desc: "also open a GitHub issue the pull request closes"},
	{title: "gitlab-mr", value: "gitlab-mr", desc: "push and open a GitLab merge request"},
}

type detectMsg struct {
	id, seq int
	det     Detection
	err     error
}

// strategyStep picks how finished work is published, suggested from the
// repository's origin when a StrategyDetector is wired.
type strategyStep struct {
	sh      *shared
	list    *pickList
	deb     debouncer
	det     *Detection
	err     error
	pending bool
	forDir  string
}

func newStrategyStep(sh *shared) *strategyStep {
	s := &strategyStep{sh: sh, list: newPickList(strategyKinds, false), deb: debouncer{id: sh.newID()}}
	s.list.selectValue(sh.p.Strategy.Kind)
	return s
}

func (s *strategyStep) title() string { return "Strategy" }

func (s *strategyStep) enter() tea.Cmd {
	s.list.selectValue(s.sh.p.Strategy.Kind)
	d := s.sh.deps.Strategy
	dir, host := s.sh.p.Workspace.Dir, s.sh.p.Workspace.Host
	if d == nil || dir == "" || dir == s.forDir {
		return nil
	}
	s.deb.seq++
	s.forDir, s.pending, s.det, s.err = dir, true, nil, nil
	id, seq, ctx := s.deb.id, s.deb.seq, s.sh.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		det, err := d.Detect(ctx, host, dir)
		return detectMsg{id: id, seq: seq, det: det, err: err}
	}
}

func (s *strategyStep) leave() {}

func (s *strategyStep) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case " ", "d":
		if s.sh.p.Strategy.Kind != "branch" {
			s.sh.p.Strategy.Draft = !s.sh.p.Strategy.Draft
		}
		return nil, true
	case "u":
		if s.det != nil && s.det.Kind != "" {
			s.sh.p.Strategy.Kind = s.det.Kind
			s.list.selectValue(s.det.Kind)
		}
		return nil, true
	case "enter", "tab":
		return next, true
	}
	if s.list.key(msg) {
		if it, ok := s.list.selected(); ok {
			s.sh.p.Strategy.Kind = it.value
		}
		return nil, true
	}
	return nil, false
}

func (s *strategyStep) msg(msg tea.Msg) tea.Cmd {
	if m, ok := msg.(detectMsg); ok && s.deb.current(m.id, m.seq) {
		s.pending = false
		s.err = m.err
		if m.err == nil {
			d := m.det
			s.det = &d
		}
	}
	return nil
}

// hosting is "github", "gitlab" or "" for a strategy kind.
func hosting(kind string) string {
	switch {
	case strings.HasPrefix(kind, "github"):
		return "github"
	case strings.HasPrefix(kind, "gitlab"):
		return "gitlab"
	}
	return ""
}

func (s *strategyStep) view(width, height int) string {
	var b strings.Builder
	b.WriteString(wrap("How ymcp publishes the agent's work once a turn ends.", width) + "\n\n")
	b.WriteString(s.list.view(width, len(strategyKinds)) + "\n\n")
	if s.sh.p.Strategy.Kind != "branch" {
		box := "[ ]"
		if s.sh.p.Strategy.Draft {
			box = "[x]"
		}
		b.WriteString(box + " open as a draft " + styleDim.Render("(space)") + "\n\n")
	}
	b.WriteString(styleLabel.Render("Detected from origin") + "\n")
	switch {
	case s.sh.deps.Strategy == nil:
		b.WriteString(markOff + styleDim.Render(" detection isn't available here; pick the strategy that matches your hosting") + "\n")
	case s.sh.p.Workspace.Dir == "":
		b.WriteString(markOff + styleDim.Render(" pick the repository in the Workspace step first") + "\n")
	case s.pending:
		b.WriteString(markPending + " reading origin…\n")
	case s.err != nil:
		b.WriteString(markErr + " " + wrap(s.err.Error(), width-2) + "\n")
	case s.det != nil:
		d := s.det
		repo := d.Repo
		if repo == "" {
			repo = "unknown hosting"
		}
		line := markOK + " " + repo
		if d.Kind != "" {
			line += styleDim.Render(" → suggests ") + styleCode.Render(d.Kind)
			if d.Kind != s.sh.p.Strategy.Kind {
				line += styleDim.Render("  (u to use it)")
			}
		}
		b.WriteString(line + "\n")
		if d.Auth != "" {
			mark := markOK
			if !d.AuthOK {
				mark = markErr
			}
			b.WriteString(mark + " " + oneLine(d.Auth, width-4) + "\n")
		}
		for _, n := range d.Notes {
			b.WriteString(markWarn + " " + oneLine(n, width-4) + "\n")
		}
		if h, want := hosting(s.sh.p.Strategy.Kind), hosting(d.Kind); h != "" && want != "" && h != want {
			b.WriteString(markWarn + " " + wrap("the strategy is for "+h+" but origin is on "+want, width-4) + "\n")
		}
	}
	return b.String()
}

func (s *strategyStep) help() string {
	return keyHelp("↑/↓", "strategy", "space", "draft", "u", "use detected", "enter", "continue")
}

func (s *strategyStep) mark() string {
	switch {
	case s.pending:
		return markPending
	case s.err != nil:
		return markWarn
	case s.det != nil && s.det.Kind != "" && hosting(s.det.Kind) != "" && hosting(s.sh.p.Strategy.Kind) != "" && hosting(s.det.Kind) != hosting(s.sh.p.Strategy.Kind):
		return markWarn
	case s.det != nil && s.det.Auth != "" && !s.det.AuthOK && s.sh.p.Strategy.Kind != "branch":
		return markErr
	}
	return markOK
}
