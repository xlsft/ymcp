package wizard

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/tui/keys"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
)

// repoStep picks the repository (on this machine or an ssh host), shows
// what workspace.Inspect finds there, and how finished work is published,
// suggested from the repository's origin.
type repoStep struct {
	base
	host, dir, baseBr, branch, prefix, parallel *assistField
	titleTmpl                                   *assistField // strategy.title
	idName, idTag                               *assistField
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
	s.dir = newAssist(sh, "e.g. ~/code/backend", func(t string, _ int) func(context.Context) ([]Suggestion, error) {
		host, ins := sh.p.Workspace.Host, sh.deps.Workspace
		list := func(ctx context.Context, dir string) ([]workspace.Dir, error) { return ins.Dirs(ctx, host, dir) }
		if host != "" { // the path is on the host: / and its ~
			return dirSuggest(t, "/", false, list)
		}
		return dirSuggest(t, string(filepath.Separator), true, list)
	})
	s.dir.browse = true
	s.dir.set(w.Dir)
	s.baseBr = newAssist(sh, "the default branch of origin", nil)
	s.baseBr.set(w.Base)
	s.branch = newAssist(sh, spec.DefaultBranch, nil)
	s.branch.set(w.Branch)
	s.prefix = newAssist(sh, "none", nil)
	s.prefix.set(w.Prefix())
	s.titleTmpl = newAssist(sh, spec.DefaultTitle, nil)
	s.titleTmpl.set(cmp.Or(sh.p.Strategy.Title, spec.DefaultTitle))
	s.parallel = newAssist(sh, "1", staticSuggest([]string{"1", "2", "3", "4"}))
	s.parallel.set(strconv.Itoa(max(w.Parallel, 1)))
	s.idName = newAssist(sh, spec.DefaultIdentityName, nil)
	s.idName.set(w.Identity.Name)
	s.idTag = newAssist(sh, spec.DefaultIdentityTag, nil)
	s.idTag.set(w.Identity.Tag)
	s.advanced = w.Branch != spec.DefaultBranch || w.Prefix() != spec.DefaultCommitPrefix ||
		cmp.Or(sh.p.Strategy.Title, spec.DefaultTitle) != spec.DefaultTitle ||
		w.Host != "" || w.Base != "" || w.Parallel > 1 || w.Identity.Enabled

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
			desc:    "Put in front of the agent's commit messages; empty for none, e.g. for commitlint.",
			ctl:     newText(s.prefix, func() tea.Cmd { s.store(); return nil }),
			problem: func() string { return s.tmplErr(sh.p.Workspace.Prefix()) },
			below:   func(int) string { return s.preview(sh.p.Workspace.Prefix()) },
		},
		{
			id: "title", label: "PR/MR title template",
			hidden:  func() bool { return adv() || sh.p.Strategy.Kind == "branch" },
			desc:    "{{commit.subject}} is the branch's first commit, e.g. for a conventional title.",
			ctl:     newText(s.titleTmpl, func() tea.Cmd { s.store(); return nil }),
			problem: func() string { return s.tmplErr(sh.p.Strategy.Title) },
			below:   func(int) string { return s.preview(sh.p.Strategy.Title) },
		},
		{
			id: "identity", inline: true, hidden: adv,
			desc: "Off: the agent commits as the repository's user.name and user.email.",
			ctl: &toggleCtl{
				label: "Give the agent its own commit author",
				get:   func() bool { return sh.p.Workspace.Identity.Enabled },
				set:   func(v bool) tea.Cmd { sh.p.Workspace.Identity.Enabled = v; return nil },
			},
		},
		{
			id: "idName", label: "Author name template", hidden: func() bool { return adv() || !sh.p.Workspace.Identity.Enabled },
			desc:    "The agent's user.name; {{agent.name}}, {{pipeline.name}}, {{run.number}} and the issue's keys work.",
			ctl:     newText(s.idName, func() tea.Cmd { s.store(); return nil }),
			problem: func() string { return s.tmplErr(sh.p.Workspace.Identity.Name) },
			below:   func(int) string { return s.identityPreview(false) },
		},
		{
			id: "idTag", label: "Email tag template", hidden: func() bool { return adv() || !sh.p.Workspace.Identity.Enabled },
			desc:    "Goes after + in your user.email: you+<tag>@example.com. Lowercase letters, digits and _ are kept.",
			ctl:     newText(s.idTag, func() tea.Cmd { s.store(); return nil }),
			problem: func() string { return s.tmplErr(sh.p.Workspace.Identity.Tag) },
			below:   func(int) string { return s.identityPreview(true) },
		},
	}
	return s
}

// identityPreview renders the agent's author name, or its address, for run
// #1 on the sample issue.
func (s *repoStep) identityPreview(email bool) string {
	is := s.sh.sample.issues
	if len(is) == 0 {
		return ""
	}
	sample := is[0]
	if full := s.sh.full.issue; full != nil && full.Key == sample.Key {
		sample = *full
	}
	p := s.sh.p
	v := text.Vars{
		"issue.id": sample.Key, "issue.key": sample.Key, "issue.summary": sample.Summary, "issue.dbid": sample.ID,
		"agent.name": cmp.Or(p.Agent.Name, "Claude"), "pipeline.name": cmp.Or(p.Name, "pipeline"), "run.number": "1",
	}
	for k, f := range sample.Fields {
		v["issue.fields."+k] = f
	}
	name, err := text.Render(cmp.Or(p.Workspace.Identity.Name, spec.DefaultIdentityName), v)
	if err != nil {
		return ""
	}
	tag, err := text.Render(cmp.Or(p.Workspace.Identity.Tag, spec.DefaultIdentityTag), v)
	if err != nil {
		return ""
	}
	user := "you@example.com"
	for _, c := range s.checks {
		if c.Name == workspace.CheckUserEmail && c.OK {
			user = c.Detail
		}
	}
	id := workspace.AgentIdentity(user, name, tag)
	out := id.Name
	if email {
		out = id.Email
	}
	return styleDim.Render("preview: ") + styleCode.Render(out) + styleDim.Render("  (run #1 on "+sample.Key+")")
}

func (s *repoStep) title() string { return "Repository" }

// advErr reports whether a field under Advanced is wrong.
func (s *repoStep) advErr() bool {
	w := s.sh.p.Workspace
	return strings.ContainsAny(w.Host, " \t") || w.Parallel < 1 || s.tmplErr(w.Branch) != "" || s.tmplErr(w.Prefix()) != "" ||
		s.tmplErr(s.sh.p.Strategy.Title) != "" || s.tmplErr(w.Identity.Name) != "" || s.tmplErr(w.Identity.Tag) != ""
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
	load := s.sh.loadIssue() // the previews may use the issue's fields
	if s.where() != s.checked && !s.pending {
		return tea.Batch(load, s.inspect())
	}
	return load
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

// dirSuggest completes the repository path with the directories in the
// one typed so far, sep being the path separator: of this machine when
// local (~ is expanded here), else "/" of the host (~ is the host's).
// Each completion ends in sep, so taking one lists what is inside it.
// Repositories come first; in a directory, ".." goes up.
func dirSuggest(text, sep string, local bool, list func(ctx context.Context, dir string) ([]workspace.Dir, error)) func(context.Context) ([]Suggestion, error) {
	return func(ctx context.Context) ([]Suggestion, error) {
		base := text
		if base == "" || base == "~" { // the home directory itself
			base = "~" + sep
		}
		i := strings.LastIndex(base, sep)
		parent, prefix := base[:i+1], base[i+1:]
		dir := parent
		if local {
			if dir = expandHome(parent); dir == "" {
				dir = "."
			}
		}
		dirs, err := list(ctx, dir)
		if err != nil {
			return nil, err
		}
		end := len([]rune(text))
		var repos, plain []Suggestion
		for _, d := range dirs {
			if !strings.HasPrefix(strings.ToLower(d.Name), strings.ToLower(prefix)) {
				continue
			}
			if strings.HasPrefix(d.Name, ".") && !strings.HasPrefix(prefix, ".") {
				continue
			}
			s := Suggestion{Option: parent + d.Name + sep, Label: d.Name + sep, CompletionEnd: end}
			if d.Git {
				s.Description = "git repository"
				repos = append(repos, s)
			} else {
				plain = append(plain, s)
			}
		}
		out := append(repos, plain...)
		if up, ok := parentDir(parent, sep, local); ok && prefix == "" {
			out = append([]Suggestion{{Option: up, Label: "..", Description: "parent directory", CompletionEnd: end}}, out...)
		}
		return out, nil
	}
}

// parentDir is the directory above dir (which ends in sep), if there is
// one to show: not above the root, a drive or a relative path, nor above
// the home directory of a host.
func parentDir(dir, sep string, local bool) (string, bool) {
	p := strings.TrimSuffix(dir, sep)
	if p == "~" {
		if !local {
			return "", false
		}
		p = strings.TrimSuffix(expandHome(dir), sep)
	}
	i := strings.LastIndex(p, sep)
	if p == "" || i < 0 {
		return "", false
	}
	return p[:i+1], true
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
	prefix := s.prefix.value()
	w.CommitPrefix = &prefix
	s.sh.p.Strategy.Title = s.titleTmpl.value()
	w.Identity.Name, w.Identity.Tag = s.idName.value(), s.idTag.value()
	if n, err := strconv.Atoi(s.parallel.value()); err == nil {
		w.Parallel = n
	} else {
		w.Parallel = 0
	}
}

func (s *repoStep) shortcut(msg tea.KeyMsg, focus string) (tea.Cmd, bool) {
	if focus == "strategy" && keys.Of(msg) == "u" && s.det != nil && s.det.Kind != "" {
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
	for _, f := range []*assistField{s.host, s.dir, s.baseBr, s.branch, s.prefix, s.titleTmpl, s.parallel} {
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
	out := make([]option, len(spec.StrategyKinds))
	for i, k := range spec.StrategyKinds {
		out[i] = option{value: k.Name, title: k.Name, desc: k.About}
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
	sample := is[0]
	if full := s.sh.full.issue; full != nil && full.Key == sample.Key {
		sample = *full
	}
	v := text.Vars{"issue.id": sample.Key, "issue.key": sample.Key, "issue.summary": sample.Summary, "issue.dbid": sample.ID,
		"commit.subject": "fix: the first commit's subject"}
	for k, f := range sample.Fields {
		v["issue.fields."+k] = f
	}
	out, err := text.Render(tmpl, v)
	if err != nil {
		return ""
	}
	note := "  (for " + sample.Key + ")"
	if s.sh.full.pending && strings.Contains(tmpl, "issue.fields.") {
		note = "  (for " + sample.Key + ", loading its fields…)"
	}
	return styleDim.Render("preview: ") + styleCode.Render(out) + styleDim.Render(note)
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
	for _, t := range []struct{ id, v, what string }{{"branch", w.Branch, "a valid branch template"}, {"prefix", w.Prefix(), "a valid commit prefix"}, {"title", s.sh.p.Strategy.Title, "a valid PR/MR title template"}} {
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
