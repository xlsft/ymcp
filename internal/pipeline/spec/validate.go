package spec

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Reserved are names no pipeline may take: they are ymcp pipeline
// subcommands.
var Reserved = []string{"list", "new", "presets", "notify"}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// CheckName reports whether name can be a pipeline name: 1 to 40 lowercase
// letters, digits, dashes and underscores, starting with a letter or digit,
// and not reserved. The name is also the file name.
func CheckName(name string) error {
	if name == "" {
		return fmt.Errorf("pipeline name is empty")
	}
	if slices.Contains(Reserved, name) {
		return fmt.Errorf("pipeline name %q is reserved (reserved: %s)", name, strings.Join(Reserved, ", "))
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("pipeline name %q is invalid: use 1 to 40 lowercase letters, digits, \"-\" or \"_\", starting with a letter or digit", name)
	}
	return nil
}

// ValidationError lists everything wrong with a pipeline.
type ValidationError struct {
	Pipeline string
	Problems []string
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pipeline %q is invalid:", e.Pipeline)
	for _, p := range e.Problems {
		b.WriteString("\n  - " + p)
	}
	return b.String()
}

var (
	permissionModes = []string{"ask", "allow", "deny"}
	providers       = []string{"claude", "kimi", "opencode", "acp"}
	strategyKinds   = []string{"branch", "github-pr", "github-issue-pr", "gitlab-mr"}
)

// Validate checks the pipeline without touching the network or the disk:
// the name, required fields, known enum values and sane numbers. Unset
// optional fields are taken as their defaults. It returns a
// *ValidationError listing every problem.
func (p Pipeline) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if err := CheckName(p.Name); err != nil {
		add("%v", err)
	}
	p = p.WithDefaults()

	if strings.TrimSpace(p.Profile) == "" {
		add("profile is required: the YouTrack profile the pipeline works with")
	}
	if strings.TrimSpace(p.Source.Query) == "" {
		add("source.query is required: the YouTrack search that finds issues for the agent")
	}
	if p.Source.PollInterval.Duration < 0 {
		add("source.pollInterval must not be negative")
	}

	stage := func(name string, s Stage, needEnter, needMatch bool) {
		if !s.On() {
			return
		}
		if needEnter && strings.TrimSpace(s.Enter) == "" {
			add("stages.%s.enter is required: the YouTrack command that moves an issue into this stage (or set enabled to false)", name)
		}
		if needMatch && strings.TrimSpace(s.Match) == "" {
			add("stages.%s.match is required: the YouTrack search that recognises issues in this stage", name)
		}
	}
	stage("working", p.Stages.Working, true, true)
	stage("review", p.Stages.Review, true, true)
	stage("needsInput", p.Stages.NeedsInput, true, false)
	stage("done", p.Stages.Done, true, false)
	// Humans close issues; ymcp only recognises them by the match.
	stage("closed", p.Stages.Closed, false, false)

	w := p.Workspace
	if strings.TrimSpace(w.Dir) == "" {
		add("workspace.dir is required: the git repository the agent works in")
	}
	if strings.ContainsAny(w.Host, " \t\r\n") {
		add("workspace.host %q must be an ssh host alias without spaces", w.Host)
	}
	if w.Parallel < 1 {
		add("workspace.parallel must be at least 1, got %d", w.Parallel)
	}

	if !slices.Contains(providers, p.Agent.Provider) {
		add("agent.provider %q is unknown: use one of %s", p.Agent.Provider, strings.Join(providers, ", "))
	}
	if p.Agent.Provider == "acp" && strings.TrimSpace(p.Agent.Command) == "" {
		add("agent.command is required for the acp provider: the command that starts the ACP server")
	}
	for _, k := range slices.Sorted(maps.Keys(p.Agent.Env)) {
		if k == "" || strings.Contains(k, "=") {
			add("agent.env has an invalid variable name %q", k)
		}
	}

	if !slices.Contains(permissionModes, p.Permissions) {
		add("permissions %q is unknown: use one of %s", p.Permissions, strings.Join(permissionModes, ", "))
	}
	if !slices.Contains(strategyKinds, p.Strategy.Kind) {
		add("strategy.kind %q is unknown: use one of %s", p.Strategy.Kind, strings.Join(strategyKinds, ", "))
	}

	for _, d := range []struct {
		name string
		v    Duration
	}{
		{"timeouts.silence", p.Timeouts.Silence},
		{"timeouts.total", p.Timeouts.Total},
		{"timeouts.permission", p.Timeouts.Permission},
		{"feedback.debounce", p.Feedback.Debounce},
	} {
		if d.v.Duration < 0 {
			add("%s must not be negative", d.name)
		}
	}
	for _, l := range []struct {
		name string
		v    []string
	}{
		{"feedback.youtrack", p.Feedback.YouTrack},
		{"feedback.github", p.Feedback.GitHub},
		{"feedback.gitlab", p.Feedback.GitLab},
	} {
		if slices.ContainsFunc(l.v, func(s string) bool { return strings.TrimSpace(s) == "" }) {
			add("%s must not contain an empty login", l.name)
		}
	}

	problems = append(problems, p.Notifications.problems()...)

	pr := p.Prompts
	if pr.Preset != "" {
		if _, ok := LookupPreset(pr.Preset); !ok && (pr.Start == "" || pr.Feedback == "" || pr.Answer == "") {
			add("prompts.preset %q is not a built-in preset (%s) and prompts.start, feedback and answer are not all set", pr.Preset, strings.Join(presetNames(), ", "))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Pipeline: p.Name, Problems: problems}
}
