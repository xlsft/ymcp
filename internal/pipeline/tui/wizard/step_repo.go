package wizard

import (
	"bufio"
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

var strategyKinds = []option{
	{value: "branch", title: "branch", desc: "push the branch; you open pull or merge requests yourself"},
	{value: "github-pr", title: "github-pr", desc: "push and open a GitHub pull request"},
	{value: "github-issue-pr", title: "github-issue-pr", desc: "also open a GitHub issue the pull request closes"},
	{value: "gitlab-mr", title: "gitlab-mr", desc: "push and open a GitLab merge request"},
}

// repoStep picks the repository (on this machine or an ssh host), shows
// what workspace.Inspect finds there, and how finished work is published,
// suggested from the repository's origin.
type repoStep struct {
	base
	host, dir, baseBr, branch, prefix, parallel *assistField
	all                                         []*item
	advanced                                    bool

	deb     debouncer
	checks  []workspace.Check
	err     error
	pending bool
	checked string // host and dir the checks are for

	det        *Detection
	detErr     error
	detPending bool
	touched    bool // the user chose the strategy; detection doesn't change it
}

type inspectMsg struct {
	id, seq int
	checks  []workspace.Check
	err     error
}

type detectMsg struct {
	id, seq int
	det     Detection
	err     error
}

func newRepoStep(sh *shared) *repoStep {
	s := &repoStep{base: base{sh: sh}, deb: debouncer{id: sh.newID()}, touched: sh.orig != ""}
	w := sh.p.Workspace
	s.host = newAssist(sh, "this machine", sshSuggest)
	s.host.set(w.Host)
	s.dir = newAssist(sh, "e.g. ~/code/backend", func(t string, c int) func(context.Context) ([]Suggestion, error) {
		if sh.p.Workspace.Host != "" {
			return nil // the path is on the host
		}
		return pathSuggest(t, c)
	})
	s.dir.set(w.Dir)
	s.baseBr = newAssist(sh, "the default branch of origin", nil)
	s.baseBr.set(w.Base)
	s.branch = newAssist(sh, spec.DefaultBranch, nil)
	s.branch.set(w.Branch)
	s.prefix = newAssist(sh, spec.DefaultCommitPrefix, nil)
	s.prefix.set(w.CommitPrefix)
	s.parallel = newAssist(sh, "1", staticSuggest([]string{"1", "2", "3", "4"}))
	s.parallel.set(strconv.Itoa(max(w.Parallel, 1)))
	s.advanced = w.Branch != spec.DefaultBranch || w.CommitPrefix != spec.DefaultCommitPrefix ||
		w.Host != "" || w.Base != "" || w.Parallel > 1

	store := func() tea.Cmd {
		before := s.where()
		s.store()
		if s.where() != before {
			return s.deb.bump(sh.delay)
		}
		return nil
	}
	adv := func() bool { return !s.advOpen() }
	s.all = []*item{
		{
			id: "dir", label: "Repository directory",
			descFn: func() string {
				where := "on this machine"
				if h := sh.p.Workspace.Host; h != "" {
					where = "on " + h + " (Advanced)"
				}
				return "The git repository the agent works in, " + where + "."
			},
			ctl: newText(s.dir, store),
			problem: func() string {
				if s.fs.tried && sh.p.Workspace.Dir == "" {
					return "Type the path of the repository."
				}
				return ""
			},
			below: s.checksView,
		},
		{
			id: "strategy", label: "Publishing",
			desc: "What ymcp does with finished work; suggested from origin.",
			ctl: &choiceCtl{
				opts: s.strategyOptions,
				get:  func() string { return sh.p.Strategy.Kind },
				set: func(v string) tea.Cmd {
					sh.p.Strategy.Kind = v
					s.touched = true
					return nil
				},
			},
			below: s.detectView,
		},
		{
			id: "draft", inline: true,
			desc:   "Reviewers see a draft until someone marks it ready.",
			hidden: func() bool { return sh.p.Strategy.Kind == "branch" },
			ctl: &toggleCtl{
				label: "Open pull and merge requests as drafts",
				get:   func() bool { return sh.p.Strategy.Draft },
				set:   func(v bool) tea.Cmd { sh.p.Strategy.Draft = v; return nil },
			},
		},
		{
			id: "advanced", inline: true,
			ctl: &disclosureCtl{label: "Advanced: host, base branch, issues at once, names", open: &s.advanced, force: s.advErr, note: func() string {
				if s.advErr() {
					return markErr + styleErr.Render(" a field here is wrong")
				}
				return ""
			}},
		},
		{
			id: "host", label: "Host", hidden: adv,
			desc: "Empty for this machine, or an ssh host alias from ~/.ssh/config.",
			ctl:  newText(s.host, store),
			problem: func() string {
				if strings.ContainsAny(s.host.value(), " \t") {
					return "An ssh host alias has no spaces."
				}
				return ""
			},
		},
		{
			id: "base", label: "Base branch", hidden: adv,
			desc: "Where new work starts and PRs go. Empty: the default branch.",
			ctl:  newText(s.baseBr, func() tea.Cmd { s.store(); return nil }),
		},
		{
			id: "parallel", label: "Issues at once", hidden: adv,
			desc: "How many issues the agent works on at a time, each in a worktree.",
			ctl:  newText(s.parallel, func() tea.Cmd { s.store(); return nil }),
			problem: func() string {
				if sh.p.Workspace.Parallel < 1 {
					return "Type a whole number of at least 1."
				}
				return ""
			},
		},
		{
			id: "branch", label: "Branch name template", hidden: adv,
			desc:    "The branch of each run, e.g. {{issue.id}}-{{issue.summary | slug}}.",
			ctl:     newText(s.branch, func() tea.Cmd { s.store(); return nil }),
			problem: func() string { return s.tmplErr(sh.p.Workspace.Branch) },
			below:   func(int) string { return s.preview(sh.p.Workspace.Branch) },
		},
		{
			id: "prefix", label: "Commit message prefix", hidden: adv,
			desc:    "Put in front of the agent's commit messages.",
			ctl:     newText(s.prefix, func() tea.Cmd { s.store(); return nil }),
			problem: func() string { return s.tmplErr(sh.p.Workspace.CommitPrefix) },
			below:   func(int) string { return s.preview(sh.p.Workspace.CommitPrefix) },
		},
	}
	return s
}

func (s *repoStep) title() string { return "Repository" }

// advErr reports whether a field under Advanced is wrong.
func (s *repoStep) advErr() bool {
	w := s.sh.p.Workspace
	return strings.ContainsAny(w.Host, " \t") || w.Parallel < 1 || s.tmplErr(w.Branch) != "" || s.tmplErr(w.CommitPrefix) != ""
}

// advOpen reports whether the Advanced fields show.
func (s *repoStep) advOpen() bool { return s.advanced || s.advErr() }
func (s *repoStep) intro() string {
	return "The git repository the agent works in, and how its work is published."
}
func (s *repoStep) items() []*item { return s.all }

// where is the host and directory the checks run on.
func (s *repoStep) where() string { return s.sh.p.Workspace.Host + "|" + s.sh.p.Workspace.Dir }

func (s *repoStep) enter() tea.Cmd {
	if s.where() != s.checked && !s.pending {
		return s.inspect()
	}
	return nil
}

func (s *repoStep) leave() {}

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

// sshSuggest completes host aliases from ~/.ssh/config.
func sshSuggest(text string, _ int) func(context.Context) ([]Suggestion, error) {
	return func(context.Context) ([]Suggestion, error) {
		t := strings.ToLower(strings.TrimSpace(text))
		var out []Suggestion
		for _, h := range sshHosts(expandHome("~/.ssh/config")) {
			if strings.HasPrefix(strings.ToLower(h), t) && h != text {
				out = append(out, Suggestion{Option: h, Description: "ssh host", CompletionEnd: len([]rune(text))})
			}
		}
		return out[:min(len(out), maxSuggestions)], nil
	}
}

// sshHosts reads the Host aliases of an ssh config file, without
// patterns.
func sshHosts(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || !strings.EqualFold(fields[0], "host") {
			continue
		}
		for _, h := range fields[1:] {
			if !strings.ContainsAny(h, "*?!") && !contains(out, h) {
				out = append(out, h)
			}
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// inspect checks the repository and reads its origin now.
func (s *repoStep) inspect() tea.Cmd {
	s.deb.seq++
	dir, host := s.sh.p.Workspace.Dir, s.sh.p.Workspace.Host
	s.checked, s.checks, s.err, s.pending = s.where(), nil, nil, false
	s.det, s.detErr, s.detPending = nil, nil, false
	if dir == "" {
		return nil
	}
	s.pending = true
	id, seq, ctx, ins := s.deb.id, s.deb.seq, s.sh.ctx, s.sh.deps.Workspace
	cmds := []tea.Cmd{func() tea.Msg {
		if host == "" {
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				return inspectMsg{id: id, seq: seq, err: fmt.Errorf("no such directory: %s", dir)}
			}
		}
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		checks, err := ins.Inspect(ctx, host, dir)
		return inspectMsg{id: id, seq: seq, checks: checks, err: err}
	}}
	if d := s.sh.deps.Strategy; d != nil {
		s.detPending = true
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(ctx, requestTimeout)
			defer cancel()
			det, err := d.Detect(ctx, host, dir)
			return detectMsg{id: id, seq: seq, det: det, err: err}
		})
	}
	return tea.Batch(cmds...)
}

// store copies the fields into the draft.
func (s *repoStep) store() {
	w := &s.sh.p.Workspace
	w.Host = s.host.value()
	if w.Host == "local" {
		w.Host = ""
	}
	w.Dir = normalizeDir(w.Host, s.dir.value())
	w.Base = s.baseBr.value()
	w.Branch = s.branch.value()
	w.CommitPrefix = s.prefix.value()
	if n, err := strconv.Atoi(s.parallel.value()); err == nil {
		w.Parallel = n
	} else {
		w.Parallel = 0
	}
}

func (s *repoStep) shortcut(msg tea.KeyMsg, focus string) (tea.Cmd, bool) {
	if focus == "strategy" && msg.String() == "u" && s.det != nil && s.det.Kind != "" {
		s.sh.p.Strategy.Kind = s.det.Kind
		s.touched = true
		return nil, true
	}
	return nil, false
}

func (s *repoStep) extraHelp(focus string) string {
	if focus == "strategy" && s.det != nil && s.det.Kind != "" && s.det.Kind != s.sh.p.Strategy.Kind {
		return keyHelp("u", "use "+s.det.Kind)
	}
	return ""
}

func (s *repoStep) msg(msg tea.Msg) tea.Cmd {
	if s.deb.fired(msg) {
		return s.inspect()
	}
	switch m := msg.(type) {
	case inspectMsg:
		if s.deb.current(m.id, m.seq) {
			s.pending, s.checks, s.err = false, m.checks, m.err
			for _, c := range m.checks {
				if c.Name == workspace.CheckDefaultBranch && c.OK && c.Detail != "" {
					s.baseBr.in.Placeholder = "the default branch of origin (" + c.Detail + ")"
				}
			}
		}
		return nil
	case detectMsg:
		if s.deb.current(m.id, m.seq) {
			s.detPending, s.detErr = false, m.err
			if m.err == nil {
				d := m.det
				s.det = &d
				if !s.touched && d.Kind != "" {
					s.sh.p.Strategy.Kind = d.Kind
				}
			}
		}
		return nil
	}
	var cmds []tea.Cmd
	for _, f := range []*assistField{s.host, s.dir, s.baseBr, s.branch, s.prefix, s.parallel} {
		cmds = append(cmds, f.msg(msg))
	}
	return tea.Batch(cmds...)
}

// checksView sums up workspace.Inspect: the passed checks on one line,
// each failed one on its own.
func (s *repoStep) checksView(width int) string {
	switch {
	case s.sh.p.Workspace.Dir == "":
		if s.sh.p.Workspace.Host == "" {
			return styleDim.Render("type a path; ↓ picks a directory from the completions")
		}
		return ""
	case s.pending:
		return markPending + styleDim.Render(" checking the repository…")
	case s.err != nil:
		return errLine(s.err.Error(), width)
	}
	var ok, lines []string
	for _, c := range s.checks {
		if !c.OK {
			lines = append(lines, markErr+" "+oneLine(c.Name+": "+c.Detail, width-2))
			continue
		}
		switch c.Name {
		case workspace.CheckDefaultBranch:
			ok = append(ok, "default branch "+c.Detail)
		case workspace.CheckOrigin:
			ok = append(ok, "origin "+c.Detail)
		case workspace.CheckPush:
			ok = append(ok, "push "+c.Detail)
		default:
			ok = append(ok, c.Name)
		}
	}
	if len(ok) > 0 {
		lines = append([]string{markOK + " " + oneLine(strings.Join(ok, " · "), width-2)}, lines...)
	}
	return strings.Join(lines, "\n")
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

func (s *repoStep) strategyOptions() []option {
	out := make([]option, len(strategyKinds))
	copy(out, strategyKinds)
	for i := range out {
		if s.det != nil && s.det.Kind == out[i].value {
			out[i].status = func(int) string { return styleOK.Render("detected") }
		}
	}
	return out
}

// detectView says what origin is and whether there is a token for it.
func (s *repoStep) detectView(width int) string {
	var lines []string
	switch {
	case s.sh.deps.Strategy == nil:
		lines = append(lines, markOff+styleDim.Render(" detection isn't available here; pick the one that matches your hosting"))
	case s.sh.p.Workspace.Dir == "":
		lines = append(lines, markOff+styleDim.Render(" ymcp suggests one once it knows the repository"))
	case s.detPending:
		lines = append(lines, markPending+styleDim.Render(" reading origin…"))
	case s.detErr != nil:
		lines = append(lines, markWarn+" "+oneLine("can't read origin: "+s.detErr.Error(), width-2))
	case s.det != nil:
		d := s.det
		repo := d.Repo
		if repo == "" {
			repo = "unknown hosting"
		}
		line := markOK + " origin: " + repo
		if d.Kind != "" {
			line += styleDim.Render(" → suggests ") + styleCode.Render(d.Kind)
		}
		lines = append(lines, line)
		if d.Auth != "" && s.sh.p.Strategy.Kind != "branch" {
			mark := markOK
			if !d.AuthOK {
				mark = markErr
			}
			lines = append(lines, mark+" "+oneLine(d.Auth, width-2))
		}
		for _, n := range d.Notes {
			lines = append(lines, markWarn+" "+oneLine(n, width-2))
		}
		if h, want := hosting(s.sh.p.Strategy.Kind), hosting(d.Kind); h != "" && want != "" && h != want {
			lines = append(lines, markWarn+" "+wrap("the strategy is for "+h+" but origin is on "+want, width-2))
		}
	}
	return strings.Join(lines, "\n")
}

// tmplErr is the error of a branch or prefix template.
func (s *repoStep) tmplErr(tmpl string) string {
	if _, err := text.Render(tmpl, text.Vars{"issue.id": "X-1", "issue.key": "X-1", "issue.summary": "x", "issue.dbid": "1"}); err != nil {
		return err.Error()
	}
	return ""
}

// preview renders a template on the sample issue.
func (s *repoStep) preview(tmpl string) string {
	is := s.sh.sample.issues
	if len(is) == 0 || strings.TrimSpace(tmpl) == "" {
		return ""
	}
	out, err := text.Render(tmpl, text.Vars{"issue.id": is[0].Key, "issue.key": is[0].Key, "issue.summary": is[0].Summary, "issue.dbid": is[0].ID})
	if err != nil {
		return ""
	}
	return styleDim.Render("preview: ") + styleCode.Render(out) + styleDim.Render("  (for "+is[0].Key+")")
}

func (s *repoStep) blockers() []problem {
	var out []problem
	w := s.sh.p.Workspace
	if strings.ContainsAny(w.Host, " \t") {
		out = append(out, problem{"host", "a host alias without spaces"})
	}
	if w.Dir == "" {
		out = append(out, problem{"dir", "a repository directory"})
	}
	if w.Parallel < 1 {
		out = append(out, problem{"parallel", "issues at once of at least 1"})
	}
	for _, t := range []struct{ id, v, what string }{{"branch", w.Branch, "a valid branch template"}, {"prefix", w.CommitPrefix, "a valid commit prefix"}} {
		if s.tmplErr(t.v) != "" {
			out = append(out, problem{t.id, t.what})
		}
	}
	return out
}

func (s *repoStep) mark() string {
	switch {
	case len(s.blockers()) > 0 || s.err != nil:
		return markBad
	case s.pending || s.detPending:
		return markPending
	}
	for _, c := range s.checks {
		if !c.OK {
			return markBad
		}
	}
	if s.det != nil && s.det.Auth != "" && !s.det.AuthOK && s.sh.p.Strategy.Kind != "branch" {
		return markBad
	}
	if s.det != nil && hosting(s.det.Kind) != "" && hosting(s.sh.p.Strategy.Kind) != "" && hosting(s.det.Kind) != hosting(s.sh.p.Strategy.Kind) {
		return markWarn
	}
	if len(s.checks) == 0 {
		return markOff
	}
	return markOK
}
