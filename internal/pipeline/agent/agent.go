// Package agent runs coding agents for ymcp pipelines.
//
// Two adapters sit behind one small interface. Claude Code runs as one
// `claude -p` process per turn that prints stream-json, with the session
// carried over by --session-id and --resume. Kimi, opencode and any other
// agent speaking the Agent Client Protocol run as one long-lived ACP server
// per session, driven through github.com/coder/acp-go-sdk.
//
// Agents run in their own process group (a job object on Windows), so
// Cancel and Close also stop whatever the agent started, and with an
// environment scrubbed of the variables of an enclosing agent session.
// With WithHost they run on another machine through ssh instead.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// Kind is the kind of an Event.
type Kind int

// Event kinds.
const (
	EventText       Kind = iota // a chunk of the agent's answer
	EventThought                // a chunk of the agent's reasoning
	EventToolCall               // a tool call started
	EventToolUpdate             // a tool call changed status or finished
	EventPermission             // the agent asks to run a tool; answer with Session.Answer
	EventUsage                  // token or cost accounting
	EventSession                // the session ID became known
	EventError                  // the agent reported an error
)

func (k Kind) String() string {
	switch k {
	case EventText:
		return "text"
	case EventThought:
		return "thought"
	case EventToolCall:
		return "tool_call"
	case EventToolUpdate:
		return "tool_update"
	case EventPermission:
		return "permission"
	case EventUsage:
		return "usage"
	case EventSession:
		return "session"
	case EventError:
		return "error"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Event is one thing the agent did while running a turn.
type Event struct {
	Kind       Kind
	Text       string
	Tool       *ToolCall   // for tool events
	Permission *Permission // for EventPermission; answer via Session.Answer
	SessionID  string      // for EventSession
	Raw        json.RawMessage
}

// ToolCall is a tool invocation by the agent. Status is pending,
// in_progress, completed, failed, or denied (claude's permission_denials).
type ToolCall struct {
	ID, Title, Kind, Status string
	Input                   json.RawMessage
}

// Permission is a pending request of the agent to run a tool.
type Permission struct {
	RequestID string
	Tool      ToolCall
	Options   []Option
}

// Option is one answer to a Permission. Kind is allow_once, allow_always,
// reject_once or reject_always.
type Option struct{ ID, Name, Kind string }

// MCPServer is an MCP server over HTTP given to the agent. ymcp's own
// server is always named "ymcp".
type MCPServer struct {
	Name, URL string
	Headers   map[string]string
}

// StartOptions configure a session.
type StartOptions struct {
	// Dir is the working directory, on the agent's host.
	Dir string
	// RunDir is a directory of ymcp's own for the run, outside the
	// worktree, on the agent's host. On a remote host claude's MCP config
	// and prompts are written there; when it is empty they go to a
	// temporary directory.
	RunDir string
	// Env is merged onto the scrubbed environment of ymcp (see Scrub), or
	// onto the login environment of a remote host (see WithHost).
	Env         map[string]string
	Model, Mode string
	// Args are appended to the agent's command line after the adapter's
	// own arguments and the ones given to New.
	Args []string
	MCP  []MCPServer
	// SessionID resumes this session when set; adapters that can't resume
	// return an error wrapping ErrNoResume.
	SessionID string
	// AllowedTools are added to claude's --allowedTools; the caller always
	// includes the pipeline tools. Ignored by ACP agents.
	AllowedTools []string
	// SystemPrompt is the service instruction: claude gets it with
	// --append-system-prompt on every turn, ACP agents in front of the
	// first prompt of a new session.
	SystemPrompt string
}

// Agent is a configured coding agent.
type Agent interface {
	Name() string
	// Check finds the binary, reads its version and lists warnings about
	// the configuration, e.g. claude without a permission mode.
	Check(ctx context.Context) (Info, error)
	Start(ctx context.Context, opts StartOptions) (Session, error)
}

// Info is what Check found.
type Info struct {
	Path, Version string
	Warnings      []string
}

// Session is a conversation with an agent.
//
// Events must be drained by the caller; the channel is closed by Close.
// Prompt runs one turn at a time.
type Session interface {
	// ID is the agent's session ID, known after Start (claude: the
	// --session-id ymcp chose; ACP: from session/new or the resumed one).
	ID() string
	// Prompt runs one turn and streams to Events while it runs. An error
	// means the turn could not run or the agent broke the protocol; an
	// error the agent reported comes back in Result with IsError set.
	// Cancelling ctx cancels the turn like Cancel.
	Prompt(ctx context.Context, text string) (Result, error)
	Events() <-chan Event
	// Answer replies to a permission request with one of its options.
	Answer(requestID, optionID string) error
	// Cancel stops the running turn: session/cancel for ACP, SIGINT and
	// then a kill of the process group for claude.
	Cancel() error
	// Close ends the agent's processes; it is safe to call twice.
	Close() error
}

// Result is the outcome of a turn. StopReason is end_turn, cancelled,
// max_tokens, refusal or error. Denied lists the tool calls the agent was
// not allowed to make.
type Result struct {
	StopReason, Text string
	Denied           []ToolCall
	IsError          bool
	Err              string
}

// Stop reasons.
const (
	StopEndTurn   = "end_turn"
	StopCancelled = "cancelled"
	StopMaxTokens = "max_tokens"
	StopRefusal   = "refusal"
	StopError     = "error"
)

// ErrNoResume means the session can't be resumed: the agent doesn't
// support it or doesn't know the session. Start a new one instead.
var ErrNoResume = errors.New("agent can't resume the session")

// Providers lists the agent providers New accepts.
var Providers = []string{"claude", "kimi", "opencode", "acp"}

// Setting configures an Agent made by New.
type Setting func(*settings)

type settings struct {
	mode, model string
	logger      *slog.Logger
	host        Host // nil: this machine
}

// WithMode tells Check the mode the pipeline uses, for its warnings.
func WithMode(mode string) Setting { return func(s *settings) { s.mode = mode } }

// WithModel tells Check the model the pipeline uses, for its warnings.
func WithModel(model string) Setting { return func(s *settings) { s.model = model } }

// WithLogger sets the logger for protocol diagnostics; the default
// discards them.
func WithLogger(l *slog.Logger) Setting { return func(s *settings) { s.logger = l } }

// New returns the agent of a provider: "claude", "kimi", "opencode", or
// "acp" for any other ACP agent, started by command and args. command
// defaults to the provider's binary and is required for "acp". kimi and
// opencode get their "acp" subcommand in front of args.
func New(provider, command string, args []string, opts ...Setting) (Agent, error) {
	s := settings{}
	for _, o := range opts {
		o(&s)
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	args = append([]string(nil), args...)
	switch provider {
	case "claude":
		if command == "" {
			command = "claude"
		}
		return &claudeAgent{command: command, args: args, settings: s}, nil
	case "kimi", "opencode":
		if command == "" {
			command = provider
		}
		return &acpAgent{provider: provider, command: command, args: append([]string{"acp"}, args...), settings: s}, nil
	case "acp":
		if command == "" {
			return nil, errors.New("agent: provider acp needs a command")
		}
		return &acpAgent{provider: provider, command: command, args: args, settings: s}, nil
	}
	return nil, fmt.Errorf("agent: unknown provider %q (want one of claude, kimi, opencode, acp)", provider)
}
