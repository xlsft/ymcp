package spec

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/xlsft/ymcp/internal/pipeline/endpoint"
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

// Choice is an allowed value of an enum field and what it means.
type Choice struct{ Name, About string }

// Providers are the values of agent.provider, in the order the wizard
// offers them.
var Providers = []Choice{
	{"claude", "Claude Code CLI"},
	{"kimi", "Kimi CLI over ACP"},
	{"opencode", "opencode over ACP"},
	{"acp", "another ACP agent"},
	{"anthropic", "Claude Code on an Anthropic-compatible API"},
	{"openai", "Claude Code on an OpenAI-compatible API"},
}

// StrategyKinds are the values of strategy.kind, in the order the wizard
// offers them.
var StrategyKinds = []Choice{
	{"branch", "push the branch; you open pull or merge requests yourself"},
	{"github-pr", "push and open a GitHub pull request"},
	{"github-issue-pr", "also open a GitHub issue the pull request closes"},
	{"gitlab-mr", "push and open a GitLab merge request"},
}

var (
	permissionModes = []string{"ask", "allow", "deny"}
	providers       = names(Providers)
	strategyKinds   = names(StrategyKinds)
)

func endpointPresetNames() []string {
	out := make([]string, len(endpoint.Presets))
	for i, p := range endpoint.Presets {
		out[i] = p.Name
	}
	return out
}

func names(cs []Choice) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

// Validate checks the pipeline without touching the network: the name,
// required fields, known enum values and sane numbers. The only file it
// reads is that of a user preset named by prompts.preset. Unset
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
	if p.Agent.UsesEndpoint() {
		e := p.Agent.ResolvedEndpoint()
		switch {
		case p.Agent.Endpoint == nil || (p.Agent.Endpoint.Preset == "" && strings.TrimSpace(p.Agent.Endpoint.URL) == ""):
			add("agent.endpoint is required for the %s provider: a preset or the API's URL", p.Agent.Provider)
		case p.Agent.Endpoint.Preset != "" && !slices.Contains(endpointPresetNames(), p.Agent.Endpoint.Preset):
			add("agent.endpoint.preset %q is unknown: use one of %s", p.Agent.Endpoint.Preset, strings.Join(endpointPresetNames(), ", "))
		default:
			if err := endpoint.CheckURL(e.URL); err != nil {
				add("agent.endpoint.url: %v", err)
			}
		}
		if p.Agent.Endpoint != nil && strings.ContainsAny(p.Agent.Endpoint.KeyEnv, "= \t") {
			add("agent.endpoint.keyEnv %q is not a variable name", p.Agent.Endpoint.KeyEnv)
		}
		if strings.TrimSpace(p.Agent.Model) == "" {
			add("agent.model is required for the %s provider: the model's name at the API", p.Agent.Provider)
		}
	} else if p.Agent.Endpoint != nil {
		add("agent.endpoint is only for the anthropic and openai providers")
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
			add("prompts.preset %q is neither a built-in preset (%s) nor one of yours, and prompts.start, feedback and answer are not all set", pr.Preset, strings.Join(presetNames(), ", "))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Pipeline: p.Name, Problems: problems}
}
