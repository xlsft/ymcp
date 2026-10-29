// Package spec describes a ymcp pipeline: the YouTrack source it watches,
// the stages an issue moves through, the workspace and agent that do the
// work, and the prompts the agent gets.
//
// A pipeline lives in one JSON file, <config dir>/pipelines/<name>.json,
// next to the profile config of internal/config (YMCP_CONFIG moves both).
// This package only reads, writes and checks those files; running them is
// the engine's job.
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/endpoint"
)

// Pipeline is the whole configuration of one pipeline.
type Pipeline struct {
	Name        string            `json:"name"`                  // the pipeline's name, also its file's
	Profile     string            `json:"profile"`               // the ymcp profile of the YouTrack instance
	Source      Source            `json:"source"`                // where issues come from
	Stages      Stages            `json:"stages"`                // the YouTrack commands and searches of each stage
	Workspace   Workspace         `json:"workspace"`             // where and on which branch the agent works
	Agent       Agent             `json:"agent"`                 // the coding agent
	Permissions string            `json:"permissions,omitempty"` // tool calls the agent wasn't allowed: ask (default), allow or deny
	Timeouts    Timeouts          `json:"timeouts"`              // limits of a run
	Strategy    Strategy          `json:"strategy"`              // how finished work is published
	Feedback    Feedback          `json:"feedback"`              // whose replies the agent reacts to
	Language    string            `json:"language,omitempty"`    // the language of everything the agent writes in public, default "English"
	Prompts     Prompts           `json:"prompts"`               // the prompt templates
	Messages    map[string]string `json:"messages,omitempty"`    // replacements of ymcp's own texts, by key
	Service     *bool             `json:"service,omitempty"`     // send ymcp's service instruction to the agent; on unless false
	// AutoCompact compacts a run's comments when it ends as done or
	// closed, as a /compact reply does: they are hidden and a short
	// summary is left.
	AutoCompact bool `json:"autoCompact,omitempty"`
	// Notifications narrows the machine's notification settings for this
	// pipeline; unset keeps them as they are.
	Notifications *Notifications `json:"notifications,omitempty"`
	// How the runner watches the CI of what the agent pushes.
	CI CI `json:"ci,omitzero"`
}

// CI is how the runner watches the CI of the agent's pushes: a failure
// gives the agent a turn with the failed jobs and the end of their logs,
// up to Fixes times in a row; then the run waits for a human.
type CI struct {
	Watch *bool `json:"watch,omitempty"` // watch the CI of each push; on unless false
	Fixes int   `json:"fixes,omitempty"` // turns in a row the agent gets for a failing CI, default 3
}

// Watched reports whether the runner watches the CI.
func (c CI) Watched() bool { return c.Watch == nil || *c.Watch }

// MaxFixes is how many turns in a row a failing CI gets.
func (c CI) MaxFixes() int {
	if c.Fixes <= 0 {
		return DefaultCIFixes
	}
	return c.Fixes
}

// Source is the YouTrack search the pipeline takes issues from.
type Source struct {
	Query        string   `json:"query"`                  // the YouTrack search of the issues to take
	PollInterval Duration `json:"pollInterval,omitempty"` // how often YouTrack, the code host and the CI are read, default 60s
}

// Stage is one step of an issue's life in the pipeline: the YouTrack
// command that moves an issue into it, the search that recognises issues
// in it and, for needsInput, the command that undoes Enter.
type Stage struct {
	Enter string `json:"enter,omitempty"` // the YouTrack command that moves an issue into the stage
	Match string `json:"match,omitempty"` // the YouTrack search that finds issues in the stage
	Exit  string `json:"exit,omitempty"`  // the command that leaves it (needsInput)
	// Whether the stage is used: on unless false, except done, which is off
	// unless true.
	Enabled *bool `json:"enabled,omitempty"`
}

// On reports whether the stage is in use.
func (s Stage) On() bool { return s.Enabled == nil || *s.Enabled }

// Stages are the five stages of a pipeline. Done is off by default.
type Stages struct {
	Working    Stage `json:"working"`
	Review     Stage `json:"review"`
	NeedsInput Stage `json:"needsInput"`
	Done       Stage `json:"done"`
	Closed     Stage `json:"closed"`
}

// Workspace says where and on which branch the agent works.
type Workspace struct {
	Host         string  `json:"host,omitempty"`         // an ssh host alias, or empty for this machine
	Dir          string  `json:"dir"`                    // the git repository on the host
	Base         string  `json:"base,omitempty"`         // the branch to start from; empty for origin's default branch
	Branch       string  `json:"branch"`                 // template of a run's branch, default "{{issue.id}}-{{issue.summary | slug}}"
	CommitPrefix *string `json:"commitPrefix,omitempty"` // template put in front of the agent's commit messages, default "{{issue.id}}"; "" for none
	Worktrees    string  `json:"worktrees,omitempty"`    // where the runs' worktrees go, ~ being the home directory on the host; empty for <user cache dir>/ymcp/worktrees/<pipeline> on the host
	Parallel     int     `json:"parallel"`               // how many issues are worked on at once, default 1
	KeepWorktree bool    `json:"keepWorktree,omitempty"` // keep a run's worktree after it ends
	// A git author of the agent's own in each worktree. Off by default: the
	// agent commits as the repository's user.name and user.email.
	Identity Identity `json:"identity,omitzero"`
}

// Prefix is the template put in front of the agent's commit messages:
// the default when unset, "" for none.
func (w Workspace) Prefix() string {
	if w.CommitPrefix == nil {
		return DefaultCommitPrefix
	}
	return *w.CommitPrefix
}

// Identity is the git author the agent commits as when it has its own.
// The address is the repository's user.email with a tag after "+":
// dev@example.com becomes dev+<tag>@example.com.
type Identity struct {
	Enabled bool   `json:"enabled,omitempty"` // commit as the agent's own author; off by default
	Name    string `json:"name,omitempty"`    // template of the author's name, default "{{agent.name}} · {{pipeline.name}} #{{run.number}}"
	Tag     string `json:"tag,omitempty"`     // template of what goes after "+" in the address, default "agent_{{pipeline.name}}_{{run.number}}"
}

// Agent picks and configures the coding agent.
type Agent struct {
	Provider string            `json:"provider"`           // one of claude, kimi, opencode, acp, anthropic or openai
	Name     string            `json:"name,omitempty"`     // the agent's name in messages; default from the provider
	Command  string            `json:"command,omitempty"`  // the executable or its full path; required for acp
	Model    string            `json:"model,omitempty"`    // the model, as the agent names it; required for anthropic and openai unless the preset has one
	Mode     string            `json:"mode,omitempty"`     // the agent's mode, e.g. a claude permission mode
	Args     []string          `json:"args,omitempty"`     // more command-line arguments of the agent
	Env      map[string]string `json:"env,omitempty"`      // environment variables of the agent
	Endpoint *Endpoint         `json:"endpoint,omitempty"` // the model API of the anthropic and openai providers
}

// Endpoint is the model API Claude Code works with for the anthropic and
// openai providers. ymcp keeps the API key and passes the agent's requests
// on, translating them for an OpenAI-compatible API.
type Endpoint struct {
	Preset string `json:"preset,omitempty"` // a known service: openrouter, kimi-coding, zai, deepseek, minimax or ollama
	URL    string `json:"url,omitempty"`    // the API's base URL; default from the preset
	KeyEnv string `json:"keyEnv,omitempty"` // the variable with the API key, read before the keychain; default from the preset
}

// UsesEndpoint reports whether the agent works with a model API of the
// pipeline's: the anthropic and openai providers.
func (a Agent) UsesEndpoint() bool {
	return a.Provider == endpoint.Anthropic || a.Provider == endpoint.OpenAI
}

// IsClaude reports whether the agent is Claude Code: the claude provider
// and the ones with an endpoint.
func (a Agent) IsClaude() bool { return a.Provider == "claude" || a.UsesEndpoint() }

// ResolvedEndpoint is the agent's endpoint with the URL and key variable
// of its preset filled in; zero when the agent has none.
func (a Agent) ResolvedEndpoint() Endpoint {
	if a.Endpoint == nil {
		return Endpoint{}
	}
	e := *a.Endpoint
	if p, ok := endpoint.Find(e.Preset); ok {
		if strings.TrimSpace(e.URL) == "" {
			e.URL = p.URL(a.Provider)
		}
		if e.KeyEnv == "" {
			e.KeyEnv = p.KeyEnv
		}
	}
	e.URL = strings.TrimRight(strings.TrimSpace(e.URL), "/")
	return e
}

// Upstream is the API the agent's requests go to, with its key from the
// variable or the keychain (see endpoint.Key). A service that needs no key
// (a preset without a key variable) gets none when there is none.
func (a Agent) Upstream() (endpoint.Upstream, error) {
	e := a.ResolvedEndpoint()
	up := endpoint.Upstream{Protocol: a.Provider, URL: e.URL}
	if err := endpoint.CheckURL(e.URL); err != nil {
		return up, fmt.Errorf("agent.endpoint.url: %w", err)
	}
	key, _, err := endpoint.Key(e.URL, e.KeyEnv)
	if err != nil {
		if p, ok := endpoint.Find(e.Preset); ok && p.KeyEnv == "" && errors.Is(err, endpoint.ErrNoKey) {
			return up, nil
		}
		return up, err
	}
	up.Key = key
	return up, nil
}

// Timeouts bound a run. Zero values mean the defaults on Load.
type Timeouts struct {
	Silence    Duration `json:"silence"`    // an agent silent this long is stuck, default 10m
	Total      Duration `json:"total"`      // the longest one turn of the agent may take, default 2h
	Permission Duration `json:"permission"` // how long a permission request waits for a person, default 30m
}

// Strategy is how finished work is published.
type Strategy struct {
	Kind  string `json:"kind"`            // one of branch (default), github-pr, github-issue-pr or gitlab-mr
	Draft bool   `json:"draft,omitempty"` // open pull and merge requests as drafts
	// Tick a GitLab merge request's "Squash commits" and "Delete source
	// branch" options.
	Squash       bool `json:"squash,omitempty"`
	DeleteBranch bool `json:"deleteBranch,omitempty"` // see squash
	// Describe asks the agent for the PR/MR's title and description in a
	// turn of their own after each push (prompts.describe), and updates
	// the PR/MR with them unless a person edited it. On unless false.
	Describe *bool `json:"describe,omitempty"`
	// Template of the PR/MR title, default "{{issue.key}} {{issue.summary}}";
	// {{commit.subject}} is the subject of the branch's first commit.
	Title string `json:"title,omitempty"`
}

// Options names the strategy's options that apply to its kind, for
// people: draft, squash, delete branch.
func (s Strategy) Options() []string {
	var out []string
	if s.Draft && s.Kind != "branch" && s.Kind != "" {
		out = append(out, "draft")
	}
	if s.Kind == "gitlab-mr" {
		if s.Squash {
			out = append(out, "squash")
		}
		if s.DeleteBranch {
			out = append(out, "delete branch")
		}
	}
	return out
}

// Feedback says whose messages the agent reacts to and how long to wait for
// more of them before starting a turn.
type Feedback struct {
	Debounce Duration `json:"debounce"`           // how long to wait for more replies before a turn, default 60s
	YouTrack []string `json:"youtrack,omitempty"` // YouTrack logins besides yours whose comments are feedback
	GitHub   []string `json:"github,omitempty"`   // GitHub logins besides yours
	GitLab   []string `json:"gitlab,omitempty"`   // GitLab usernames besides yours
}

// Prompts are the three prompt templates. Preset names the preset, built-in
// or user, they were copied from; an empty text falls back to that preset
// (see Pipeline.ResolvedPrompts).
type Prompts struct {
	Preset   string `json:"preset,omitempty"`   // the preset an empty prompt comes from, default "fix"
	Start    string `json:"start,omitempty"`    // template of a run's first turn
	Feedback string `json:"feedback,omitempty"` // template of a turn on replies
	Answer   string `json:"answer,omitempty"`   // template of a turn that answers the agent's question
	// Template of the turn that asks for the PR/MR's title and description
	// after a push; {{change.noun}} is "pull request" or "merge request".
	Describe string `json:"describe,omitempty"`
}

// Duration is a time.Duration that is written in JSON as a string such as
// "90s", "10m" or "2h".
type Duration struct{ time.Duration }

// MarshalJSON writes the duration in its shortest whole unit; zero is "".
func (d Duration) MarshalJSON() ([]byte, error) {
	var s string
	switch {
	case d.Duration == 0:
	case d.Duration%time.Hour == 0:
		s = fmt.Sprintf("%dh", d.Duration/time.Hour)
	case d.Duration%time.Minute == 0:
		s = fmt.Sprintf("%dm", d.Duration/time.Minute)
	case d.Duration%time.Second == 0:
		s = fmt.Sprintf("%ds", d.Duration/time.Second)
	default:
		s = d.Duration.String()
	}
	return json.Marshal(s)
}

// UnmarshalJSON reads a string such as "90s" or "1h30m"; "" and null are zero.
func (d *Duration) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("a duration must be a string such as \"90s\", \"10m\" or \"2h\": %s", data)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: use a form like \"90s\", \"10m\" or \"2h\"", s)
	}
	d.Duration = v
	return nil
}

// Defaults of the optional settings.
const (
	DefaultPollInterval = 60 * time.Second
	DefaultSilence      = 10 * time.Minute
	DefaultTotal        = 2 * time.Hour
	DefaultPermission   = 30 * time.Minute
	DefaultDebounce     = 60 * time.Second

	DefaultBranch       = "{{issue.id}}-{{issue.summary | slug}}"
	DefaultTitle        = "{{issue.key}} {{issue.summary}}"
	DefaultCommitPrefix = "{{issue.id}}"
	DefaultCIFixes      = 3
	DefaultIdentityName = "{{agent.name}} · {{pipeline.name}} #{{run.number}}"
	DefaultIdentityTag  = "agent_{{pipeline.name}}_{{run.number}}"
	DefaultLanguage     = "English"
	DefaultPreset       = "fix"
	DefaultProvider     = "claude"
	DefaultStrategy     = "branch"
	DefaultPermissions  = "ask"
)

// WithDefaults returns p with every unset optional field filled in. Load,
// Save and Validate apply it, so callers can rely on the defaults.
func (p Pipeline) WithDefaults() Pipeline {
	if p.Source.PollInterval.Duration == 0 {
		p.Source.PollInterval.Duration = DefaultPollInterval
	}
	if p.Stages.Done.Enabled == nil {
		off := false
		p.Stages.Done.Enabled = &off
	}
	if p.Workspace.Branch == "" {
		p.Workspace.Branch = DefaultBranch
	}
	if p.Workspace.CommitPrefix == nil {
		prefix := DefaultCommitPrefix
		p.Workspace.CommitPrefix = &prefix
	}
	if p.Strategy.Title == "" {
		p.Strategy.Title = DefaultTitle
	}
	if p.Workspace.Parallel == 0 {
		p.Workspace.Parallel = 1
	}
	if p.Agent.Provider == "" {
		p.Agent.Provider = DefaultProvider
	}
	if p.Agent.UsesEndpoint() && p.Agent.Endpoint != nil {
		if pr, ok := endpoint.Find(p.Agent.Endpoint.Preset); ok {
			if p.Agent.Model == "" {
				p.Agent.Model = pr.Model
			}
			if p.Agent.Name == "" {
				p.Agent.Name = pr.Title
			}
		}
	}
	if p.Agent.Name == "" {
		p.Agent.Name = strings.ToUpper(p.Agent.Provider[:1]) + p.Agent.Provider[1:]
		if p.Agent.UsesEndpoint() {
			p.Agent.Name = "Agent"
		}
	}
	if p.Permissions == "" {
		p.Permissions = DefaultPermissions
	}
	if p.Timeouts.Silence.Duration == 0 {
		p.Timeouts.Silence.Duration = DefaultSilence
	}
	if p.Timeouts.Total.Duration == 0 {
		p.Timeouts.Total.Duration = DefaultTotal
	}
	if p.Timeouts.Permission.Duration == 0 {
		p.Timeouts.Permission.Duration = DefaultPermission
	}
	if p.Strategy.Kind == "" {
		p.Strategy.Kind = DefaultStrategy
	}
	if p.Feedback.Debounce.Duration == 0 {
		p.Feedback.Debounce.Duration = DefaultDebounce
	}
	if p.Language == "" {
		p.Language = DefaultLanguage
	}
	if p.Prompts.Preset == "" {
		p.Prompts.Preset = DefaultPreset
	}
	return p
}

// ServiceEnabled reports whether the service instruction is sent.
func (p Pipeline) ServiceEnabled() bool { return p.Service == nil || *p.Service }

// ResolvedPrompts returns the prompts with every empty text taken from the
// pipeline's preset (the default preset when none is named). A text that
// cannot be resolved stays empty.
func (p Pipeline) ResolvedPrompts() Prompts {
	out := p.Prompts
	name := out.Preset
	if name == "" {
		name = DefaultPreset
	}
	if pr, ok := LookupPreset(name); ok {
		if out.Start == "" {
			out.Start = pr.Start
		}
		if out.Feedback == "" {
			out.Feedback = pr.Feedback
		}
		if out.Answer == "" {
			out.Answer = pr.Answer
		}
	}
	if out.Describe == "" {
		out.Describe = DefaultDescribe
	}
	return out
}

// Described reports whether the agent writes the PR/MR's title and
// description in a turn of their own.
func (s Strategy) Described() bool { return s.Describe == nil || *s.Describe }

// DefaultDescribe is the prompt of the turn that asks for the PR/MR's
// title and description. {{change.noun}} is "pull request" or "merge
// request", {{change.action}} "opens" or "updates".
const DefaultDescribe = "Your work on branch {{branch}} is pushed, and ymcp {{change.action}} its {{change.noun}}. " +
	"Write the {{change.noun}}'s title and description for the branch as it is now and pass them to the ymcp tool set_change. " +
	"Write them in {{language}}, the way this repository's {{change.noun}}s are written: the title on one line, the description " +
	"what changed and why, for a reviewer. Change, commit and push nothing, and call no other tool."
