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
	"fmt"
	"strings"
	"time"
)

// Pipeline is the whole configuration of one pipeline.
type Pipeline struct {
	Name        string            `json:"name"`
	Profile     string            `json:"profile"`
	Source      Source            `json:"source"`
	Stages      Stages            `json:"stages"`
	Workspace   Workspace         `json:"workspace"`
	Agent       Agent             `json:"agent"`
	Permissions string            `json:"permissions,omitempty"` // ask (default) | allow | deny
	Timeouts    Timeouts          `json:"timeouts"`
	Strategy    Strategy          `json:"strategy"`
	Feedback    Feedback          `json:"feedback"`
	Language    string            `json:"language,omitempty"` // default "English"
	Prompts     Prompts           `json:"prompts"`
	Messages    map[string]string `json:"messages,omitempty"` // overrides of ymcp's own texts
	Service     *bool             `json:"service,omitempty"`  // service instruction on (nil = true)
	// AutoCompact compacts a run's comments when it ends as done or
	// closed, as a /compact reply does: they are hidden and a short
	// summary is left.
	AutoCompact bool `json:"autoCompact,omitempty"`
	// Notifications narrows the machine's notification settings for this
	// pipeline; nil keeps them as they are.
	Notifications *Notifications `json:"notifications,omitempty"`
}

// Source is the YouTrack search the pipeline takes issues from.
type Source struct {
	Query        string   `json:"query"`
	PollInterval Duration `json:"pollInterval,omitempty"` // default 60s
}

// Stage is one step of an issue's life in the pipeline: the YouTrack
// command that moves an issue into it, the search that recognises issues
// in it and, for needsInput, the command that undoes Enter.
type Stage struct {
	Enter string `json:"enter,omitempty"`
	Match string `json:"match,omitempty"`
	Exit  string `json:"exit,omitempty"`
	// Enabled is nil for "on", except for Done, which is off unless set to
	// true (WithDefaults writes false for it).
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
	Host         string `json:"host,omitempty"`      // empty = local; otherwise an ssh host alias
	Dir          string `json:"dir"`                 // the repository
	Base         string `json:"base,omitempty"`      // empty = the default branch of origin
	Branch       string `json:"branch"`              // template, default "{{issue.id}}-{{issue.summary | slug}}"
	CommitPrefix string `json:"commitPrefix"`        // template, default "{{issue.id}}"
	Worktrees    string `json:"worktrees,omitempty"` // empty = <user cache dir>/ymcp/worktrees/<pipeline name> on the host
	Parallel     int    `json:"parallel"`            // default 1
	KeepWorktree bool   `json:"keepWorktree,omitempty"`
	// Identity gives the agent a git author of its own in each worktree.
	// Off by default: the agent commits as the repository's user.name and
	// user.email.
	Identity Identity `json:"identity,omitzero"`
}

// Identity is the git author the agent commits as when it has its own.
// The address is the repository's user.email with a tag after "+":
// dev@example.com becomes dev+<tag>@example.com.
type Identity struct {
	Enabled bool   `json:"enabled,omitempty"`
	Name    string `json:"name,omitempty"` // template, default DefaultIdentityName
	Tag     string `json:"tag,omitempty"`  // template, default DefaultIdentityTag
}

// Agent picks and configures the coding agent.
type Agent struct {
	Provider string            `json:"provider"` // claude | kimi | opencode | acp
	Name     string            `json:"name,omitempty"`
	Command  string            `json:"command,omitempty"` // the executable; required for acp
	Model    string            `json:"model,omitempty"`
	Mode     string            `json:"mode,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

// Timeouts bound a run. Zero values mean the defaults on Load.
type Timeouts struct {
	Silence    Duration `json:"silence"`    // no agent output for this long: stuck, default 10m
	Total      Duration `json:"total"`      // one invocation, default 2h
	Permission Duration `json:"permission"` // waiting for a human to decide, default 30m
}

// Strategy is how finished work is published.
type Strategy struct {
	Kind  string `json:"kind"` // branch | github-pr | github-issue-pr | gitlab-mr
	Draft bool   `json:"draft,omitempty"`
}

// Feedback says whose messages the agent reacts to and how long to wait for
// more of them before starting a turn.
type Feedback struct {
	Debounce Duration `json:"debounce"` // default 60s
	YouTrack []string `json:"youtrack,omitempty"`
	GitHub   []string `json:"github,omitempty"`
	GitLab   []string `json:"gitlab,omitempty"`
}

// Prompts are the three prompt templates. Preset names the preset, built-in
// or user, they were copied from; an empty text falls back to that preset
// (see Pipeline.ResolvedPrompts).
type Prompts struct {
	Preset   string `json:"preset,omitempty"`
	Start    string `json:"start,omitempty"`
	Feedback string `json:"feedback,omitempty"`
	Answer   string `json:"answer,omitempty"`
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
	DefaultCommitPrefix = "{{issue.id}}"
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
	if p.Workspace.CommitPrefix == "" {
		p.Workspace.CommitPrefix = DefaultCommitPrefix
	}
	if p.Workspace.Parallel == 0 {
		p.Workspace.Parallel = 1
	}
	if p.Agent.Provider == "" {
		p.Agent.Provider = DefaultProvider
	}
	if p.Agent.Name == "" {
		p.Agent.Name = strings.ToUpper(p.Agent.Provider[:1]) + p.Agent.Provider[1:]
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
	return out
}
