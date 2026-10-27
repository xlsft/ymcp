package wizard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
)

// workspaceStep picks the repository with path completion and shows the
// result of workspace.Inspect line by line.
type workspaceStep struct {
	sh                                  *shared
	dir, base, branch, prefix, parallel *assistField
	focus                               int
	deb                                 debouncer
	checks                              []workspace.Check
	err                                 error
	pending                             bool
	checked                             string // dir the checks are for
}

type inspectMsg struct {
	id, seq int
	dir     string
	checks  []workspace.Check
	err     error
}

func newWorkspaceStep(sh *shared) *workspaceStep {
	s := &workspaceStep{sh: sh, deb: debouncer{id: sh.newID()}}
	w := sh.p.Workspace
	s.dir = newAssist(sh, "Repository directory", "~/code/backend", pathSuggest)
	s.dir.set(w.Dir)
	s.base = newAssist(sh, "Base branch (optional)", "empty: the default branch of origin", nil)
	s.base.set(w.Base)
	s.branch = newAssist(sh, "Branch name template", spec.DefaultBranch, nil)
	s.branch.set(w.Branch)
	s.prefix = newAssist(sh, "Commit message prefix", spec.DefaultCommitPrefix, nil)
	s.prefix.set(w.CommitPrefix)
	s.parallel = newAssist(sh, "Issues worked on at once", "1", staticSuggest([]string{"1", "2", "3", "4"}))
	s.parallel.set(strconv.Itoa(max(w.Parallel, 1)))
	return s
}

func (s *workspaceStep) fields() []*assistField {
	return []*assistField{s.dir, s.base, s.branch, s.prefix, s.parallel}
}

func (s *workspaceStep) title() string { return "Workspace" }

func (s *workspaceStep) enter() tea.Cmd {
	s.setFocus(s.focus)
	if s.sh.p.Workspace.Dir != s.checked && !s.pending {
		return s.inspect()
	}
	return nil
}

func (s *workspaceStep) leave() {
	for _, f := range s.fields() {
		f.blur()
	}
}

func (s *workspaceStep) setFocus(i int) {
	s.leave()
	s.focus = i
	s.fields()[i].focus()
}

// expandHome turns a leading ~ into the home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// normalizeDir is the directory as stored: home expanded and absolute,
// for this machine.
func normalizeDir(host, dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" || host != "" {
		return dir
	}
	dir = expandHome(dir)
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// pathSuggest completes local directories; git repositories are marked.
func pathSuggest(text string, _ int) func(context.Context) ([]Suggestion, error) {
	return func(context.Context) ([]Suggestion, error) {
		expanded := expandHome(text)
		dir, prefix := filepath.Split(expanded)
		if dir == "" {
			dir = "."
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, nil
		}
		keep := text[:len(text)-len(prefix)]
		var out []Suggestion
		for _, e := range entries {
			name := e.Name()
			isDir := e.IsDir()
			if e.Type()&os.ModeSymlink != 0 {
				if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
					isDir = st.IsDir()
				}
			}
			if !isDir || !strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
				continue
			}
			if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
				continue
			}
			sug := Suggestion{Option: keep + name + string(filepath.Separator), CompletionEnd: len([]rune(text))}
			if _, err := os.Stat(filepath.Join(dir, name, ".git")); err == nil {
				sug.Description = "git repository"
			}
			out = append(out, sug)
		}
		sort.SliceStable(out, func(i, j int) bool {
			return (out[i].Description != "") && (out[j].Description == "")
		})
		return out[:min(len(out), maxSuggestions)], nil
	}
}

func (s *workspaceStep) inspect() tea.Cmd {
	s.deb.seq++
	dir, host := s.sh.p.Workspace.Dir, s.sh.p.Workspace.Host
	s.checked, s.checks, s.err, s.pending = dir, nil, nil, false
	if dir == "" {
		return nil
	}
	s.pending = true
	id, seq, ctx, ins := s.deb.id, s.deb.seq, s.sh.ctx, s.sh.deps.Workspace
	return func() tea.Msg {
		if host == "" {
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				return inspectMsg{id: id, seq: seq, dir: dir, err: fmt.Errorf("no such directory: %s", dir)}
			}
		}
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		checks, err := ins.Inspect(ctx, host, dir)
		return inspectMsg{id: id, seq: seq, dir: dir, checks: checks, err: err}
	}
}

// store copies the fields into the draft.
func (s *workspaceStep) store() {
	w := &s.sh.p.Workspace
	w.Dir = normalizeDir(w.Host, s.dir.value())
	w.Base = s.base.value()
	w.Branch = s.branch.value()
	w.CommitPrefix = s.prefix.value()
	if n, err := strconv.Atoi(s.parallel.value()); err == nil {
		w.Parallel = n
	} else {
		w.Parallel = 0
	}
}

func (s *workspaceStep) key(msg tea.KeyMsg) (tea.Cmd, bool) {
	f := s.fields()[s.focus]
	cmd, consumed, changed := f.key(msg)
	if changed {
		before := s.sh.p.Workspace.Dir
		s.store()
		if s.sh.p.Workspace.Dir != before {
			cmd = tea.Batch(cmd, s.deb.bump(s.sh.delay))
		}
	}
	if consumed {
		return cmd, true
	}
	n := len(s.fields())
	switch msg.String() {
	case "tab", "down", "enter":
		if s.focus == n-1 {
			return tea.Batch(cmd, next), true
		}
		s.setFocus(s.focus + 1)
		return cmd, true
	case "shift+tab", "up":
		s.setFocus(max(s.focus-1, 0))
		return cmd, true
	}
	return cmd, false
}

func (s *workspaceStep) msg(msg tea.Msg) tea.Cmd {
	if s.deb.fired(msg) {
		return s.inspect()
	}
	if m, ok := msg.(inspectMsg); ok {
		if s.deb.current(m.id, m.seq) {
			s.pending, s.checks, s.err = false, m.checks, m.err
		}
		return nil
	}
	var cmds []tea.Cmd
	for _, f := range s.fields() {
		cmds = append(cmds, f.msg(msg))
	}
	return tea.Batch(cmds...)
}

// vars are the template variables of the sample issue, for previews.
func (s *workspaceStep) preview(tmpl string) string {
	is := s.sh.sample.issues
	if len(is) == 0 || strings.TrimSpace(tmpl) == "" {
		return ""
	}
	out, err := text.Render(tmpl, text.Vars{"issue.id": is[0].Key, "issue.key": is[0].Key, "issue.summary": is[0].Summary, "issue.dbid": is[0].ID})
	if err != nil {
		return markErr + " " + err.Error()
	}
	return styleDim.Render("→ ") + styleCode.Render(out) + styleDim.Render(" for "+is[0].Key)
}

func (s *workspaceStep) view(width, _ int) string {
	var b strings.Builder
	host := "this machine"
	if h := s.sh.p.Workspace.Host; h != "" {
		host = "ssh host " + h
	}
	b.WriteString(wrap("The git repository the agent works in, on "+host+". Each run gets its own worktree and branch; ymcp pushes it.", width) + "\n\n")
	b.WriteString(s.dir.view(width, s.focus == 0) + "\n")
	switch {
	case s.sh.p.Workspace.Dir == "":
		b.WriteString(styleDim.Render("    type a path; tab completes directories") + "\n")
	case s.pending:
		b.WriteString("    " + markPending + " checking the repository…\n")
	case s.err != nil:
		b.WriteString("    " + markErr + " " + oneLine(s.err.Error(), width-8) + "\n")
	default:
		for _, c := range s.checks {
			mark := markOK
			if !c.OK {
				mark = markErr
			}
			b.WriteString("    " + mark + " " + oneLine(c.Name+": "+c.Detail, width-8) + "\n")
		}
	}
	b.WriteString(s.base.view(width, s.focus == 1) + "\n")
	b.WriteString(s.branch.view(width, s.focus == 2) + "\n")
	if p := s.preview(s.sh.p.Workspace.Branch); p != "" {
		b.WriteString("    " + p + "\n")
	}
	b.WriteString(s.prefix.view(width, s.focus == 3) + "\n")
	if p := s.preview(s.sh.p.Workspace.CommitPrefix); p != "" {
		b.WriteString("    " + p + "\n")
	}
	b.WriteString(s.parallel.view(width, s.focus == 4) + "\n")
	if s.sh.p.Workspace.Parallel < 1 {
		b.WriteString("    " + markErr + " a whole number of at least 1\n")
	}
	return b.String()
}

func (s *workspaceStep) help() string {
	if s.fields()[s.focus].showing() {
		return keyHelp("↑/↓", "choose", "tab", "complete", "esc", "hide")
	}
	return keyHelp("tab", "next field", "ctrl+space", "complete path", "shift+tab", "back")
}

func (s *workspaceStep) mark() string {
	switch {
	case s.sh.p.Workspace.Dir == "":
		return markOff
	case s.pending:
		return markPending
	case s.err != nil || s.sh.p.Workspace.Parallel < 1:
		return markErr
	}
	for _, c := range s.checks {
		if !c.OK {
			return markErr
		}
	}
	if len(s.checks) == 0 {
		return markOff
	}
	return markOK
}
