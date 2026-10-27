package engine

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// CheckResult is the outcome of one check. A warning is OK but worth a
// look; only a result that is not OK fails the pipeline.
type CheckResult struct {
	Name    string
	OK      bool
	Detail  string
	Warning bool
}

// Check verifies a pipeline without running it: the config, the profile's
// sign-in, the source query, every stage command (validated against an
// issue of the source when there is one), the repository, the code host of
// the strategy (remote, token, account) and the agent, and whether the
// working command takes an issue out of the source. The
// error is set only when ctx ends.
func Check(ctx context.Context, p spec.Pipeline) ([]CheckResult, error) {
	return CheckWith(ctx, Options{Pipeline: p})
}

// CheckWith is Check with the seams of opts (Connect, NewAgent, Host,
// NewStrategy); opts.Pipeline is the pipeline checked.
func CheckWith(ctx context.Context, opts Options) ([]CheckResult, error) {
	opts = withDefaults(opts)
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	p := opts.Pipeline.WithDefaults()
	var out []CheckResult
	add := func(name string, ok bool, detail string) {
		out = append(out, CheckResult{Name: name, OK: ok, Detail: detail})
	}
	warn := func(name, detail string) {
		out = append(out, CheckResult{Name: name, OK: true, Warning: true, Detail: detail})
	}

	if err := p.Validate(); err != nil {
		add("pipeline", false, err.Error())
	} else {
		add("pipeline", true, "the configuration is valid")
	}

	// YouTrack.
	var watch *ytwatch.Watcher
	yt, _, err := opts.Connect(ctx, p.Profile, logger)
	if err == nil {
		watch = ytwatch.New(yt)
		var login string
		if login, err = watch.Me(ctx); err == nil {
			add("YouTrack", true, "signed in to "+yt.BaseURL()+" as "+login)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		add("YouTrack", false, err.Error())
		watch = nil
	}
	if watch != nil {
		checkYouTrack(ctx, p, watch, add, warn)
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
	}

	// The repository.
	if host, err := hostFor(p, opts); err != nil {
		add("workspace", false, err.Error())
	} else if strings.TrimSpace(p.Workspace.Dir) != "" {
		checks, err := workspace.Inspect(ctx, host, p.Workspace.Dir)
		if err != nil {
			return out, err
		}
		for _, c := range checks {
			add("workspace: "+c.Name, c.OK, c.Detail)
		}
		checkWorktrees(ctx, p, host, add, warn)
		checkStrategy(ctx, p, host, opts, add)
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
	}

	// The agent.
	ag, err := opts.NewAgent(p.Agent, logger)
	if err != nil {
		add("agent", false, err.Error())
		return out, nil
	}
	info, err := ag.Check(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		add("agent", false, err.Error())
		return out, nil
	}
	detail := strings.TrimSpace(info.Path + " " + info.Version)
	if detail == "" {
		detail = ag.Name()
	}
	add("agent", true, detail)
	for _, w := range info.Warnings {
		warn("agent", w)
	}
	return out, nil
}

// checkWorktrees says where the run worktrees go and warns about a root
// inside a .git directory, where Claude Code refuses to edit files.
func checkWorktrees(ctx context.Context, p spec.Pipeline, host workspace.Host, add func(string, bool, string), warn func(string, string)) {
	const name = "workspace: worktrees"
	root := strings.TrimSpace(p.Workspace.Worktrees)
	if root == "" {
		var err error
		if root, err = workspace.DefaultRoot(ctx, host, p.Name); err != nil {
			add(name, false, err.Error())
			return
		}
	}
	if slices.Contains(strings.FieldsFunc(root, func(r rune) bool { return r == '/' || r == '\\' }), ".git") {
		warn(name, root+" is inside a .git directory: Claude Code refuses to edit files there")
		return
	}
	add(name, true, root)
}

// checkYouTrack checks the source query and the stage commands.
func checkYouTrack(ctx context.Context, p spec.Pipeline, watch *ytwatch.Watcher, add func(string, bool, string), warn func(string, string)) {
	issues, err := watch.Search(ctx, p.Source.Query)
	var sample string
	switch {
	case err != nil && maybeNewTag(err, p.Source.Query):
		warn("source query", "YouTrack can't parse it yet; if it names a tag nobody has used, that tag doesn't exist: "+
			"create it or tag an issue with it ("+err.Error()+")")
	case err != nil:
		add("source query", false, err.Error())
	case len(issues) == 0:
		warn("source query", "no issue matches it right now")
	default:
		keys := make([]string, 0, 5)
		for _, is := range issues[:min(5, len(issues))] {
			keys = append(keys, is.Key)
		}
		sample = issues[0].Key
		add("source query", true, fmt.Sprintf("%d issues match: %s", len(issues), strings.Join(keys, ", ")))
	}

	type command struct{ name, cmd string }
	var commands []command
	for _, s := range []struct {
		name string
		st   spec.Stage
	}{
		{"working", p.Stages.Working}, {"review", p.Stages.Review}, {"needsInput", p.Stages.NeedsInput},
		{"done", p.Stages.Done}, {"closed", p.Stages.Closed},
	} {
		if !s.st.On() {
			continue
		}
		if s.st.Enter != "" {
			commands = append(commands, command{"stages." + s.name + ".enter", s.st.Enter})
		}
		if s.st.Exit != "" {
			commands = append(commands, command{"stages." + s.name + ".exit", s.st.Exit})
		}
	}
	for _, c := range commands {
		if ctx.Err() != nil {
			return
		}
		name := c.name + " command"
		if sample == "" {
			warn(name, "not validated: the source has no issue to try `"+c.cmd+"` on")
			continue
		}
		check, err := watch.Validate(ctx, c.cmd, sample)
		tag := newTag(c.cmd, check)
		switch {
		case err != nil:
			add(name, false, err.Error())
		case check.State == ytwatch.Valid:
			add(name, true, fmt.Sprintf("`%s` on %s: %s", c.cmd, sample, check.Text))
		case tag != "":
			if strings.EqualFold(strings.Fields(c.cmd)[0], "untag") {
				warn(name, fmt.Sprintf("tag %s doesn't exist yet; `%s` works once it does (YouTrack creates it on first use of `tag %s`)", tag, c.cmd, tag))
			} else {
				warn(name, fmt.Sprintf("tag %s doesn't exist yet; YouTrack creates it on first use", tag))
			}
		default:
			d := fmt.Sprintf("`%s` is %s on %s: %s", c.cmd, check.State, sample, check.Text)
			if len(check.Suggestions) > 0 {
				d += " (suggestions: " + strings.Join(check.Suggestions, ", ") + ")"
			}
			add(name, false, d)
		}
	}

	// Does the working command take an issue out of the source? An issue
	// in the working stage that still matches the source would be
	// captured again; YouTrack can only tell for issues already there.
	const name = "working leaves the source"
	switch {
	case !p.Stages.Working.On() || p.Stages.Working.Match == "":
		warn(name, "can't tell: stages.working.match is not set")
	default:
		both, err := watch.Search(ctx, "("+p.Source.Query+") and ("+p.Stages.Working.Match+")")
		switch {
		case err != nil:
			warn(name, "can't tell: "+err.Error())
		case len(both) > 0:
			keys := make([]string, 0, 5)
			for _, is := range both[:min(5, len(both))] {
				keys = append(keys, is.Key)
			}
			warn(name, fmt.Sprintf("%d issues match both the source query and stages.working.match (%s): the working command may leave issues in the source", len(both), strings.Join(keys, ", ")))
		default:
			add(name, true, "no issue matches both the source query and stages.working.match")
		}
	}
}

// newTag returns the tag of a `tag X` or `untag X` command that YouTrack
// refuses only because X does not exist yet ("tag expected: X"), or "".
// The tag command creates a missing tag, so such a command is fine.
func newTag(command string, check ytwatch.CommandCheck) string {
	verb, tag, ok := strings.Cut(strings.TrimSpace(command), " ")
	tag = strings.TrimSpace(tag)
	if !ok || tag == "" || (!strings.EqualFold(verb, "tag") && !strings.EqualFold(verb, "untag")) {
		return ""
	}
	for _, part := range strings.Split(check.Text, "; ") {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(part)), "tag expected") {
			return ""
		}
	}
	return strings.Trim(tag, "{}")
}

// maybeNewTag reports whether err is YouTrack failing to parse a query that
// names a tag: an unknown tag is a parse error, not an empty result.
func maybeNewTag(err error, query string) bool {
	if !strings.Contains(err.Error(), "parse search query") {
		return false
	}
	q := strings.ToLower(query)
	return strings.Contains(q, "tag:") || strings.Contains(q, "#")
}
