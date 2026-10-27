// Package agenttest provides fake coding agents for tests: an ACP server
// built on the agent side of github.com/coder/acp-go-sdk and a fake
// claude printing stream-json, including real transcripts recorded from
// Claude Code. Both run by re-executing the test binary: call Main first
// in TestMain, then start an agent with the command and environment from
// Command.
//
// A Script says what the fake does on each turn of a session; turns are
// counted across resumes. The fakes keep their sessions and a log of every
// call in Script.State, which Calls reads back.
package agenttest

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// EnvVar carries the Script to the fake agent process.
const EnvVar = "YMCP_FAKE_AGENT"

//go:embed transcripts/*.jsonl
var transcripts embed.FS

// Script configures a fake agent.
type Script struct {
	// Kind is "acp" or "claude"; set by Command.
	Kind string `json:"kind"`
	// Flavor is the ACP agent imitated: "kimi" (default: forgets the mode
	// on resume, a reject fails the tool and the turn goes on), "opencode"
	// (session/cancel is ignored while a permission request is open, a
	// reject ends the turn with cancelled) or "plain" (no session/resume).
	Flavor string `json:"flavor,omitempty"`
	// State is a directory for sessions and the call log; required.
	State string `json:"state"`
	// Banner is printed to stdout before anything else; it is not JSON.
	Banner string `json:"banner,omitempty"`
	// Version is what --version prints; default "fake-agent 1.0.0".
	Version string `json:"version,omitempty"`
	// Turns[i] is the i-th turn of a session. Turns beyond the list
	// answer "ok".
	Turns [][]Step `json:"turns,omitempty"`
}

// Step is one thing the fake does in a turn.
type Step struct {
	Text    string `json:"text,omitempty"`
	Thought string `json:"thought,omitempty"`
	// Tool runs a tool call to completion.
	Tool *Tool `json:"tool,omitempty"`
	// Permission asks to run a tool (ACP) and runs it if allowed. The
	// answer is reported as text: "allowed <option>" or "rejected".
	Permission *Tool `json:"permission,omitempty"`
	// Sleep waits; a cancel ends the turn early (ACP), SIGINT kills the
	// fake claude.
	Sleep time.Duration `json:"sleep,omitempty"`
	// Echo answers "prompt=<text> history=<earlier prompts, |-separated>
	// mode=<mode> model=<model>".
	Echo bool `json:"echo,omitempty"`
	// Env answers "<NAME>=<value>" or "<NAME> unset".
	Env string `json:"env,omitempty"`
	// Stop is the ACP stop reason of the turn (default end_turn).
	Stop string `json:"stop,omitempty"`
	// Error fails the turn: a JSON-RPC error (ACP) or a result with
	// is_error and exit code 1 (claude).
	Error string `json:"error,omitempty"`
	// Exit ends the process at once with this code.
	Exit int `json:"exit,omitempty"`
	// Spawn starts a child process that sleeps and holds stdout; its pid
	// goes to State/child.pid.
	Spawn bool `json:"spawn,omitempty"`
	// Garbage is printed to stdout as is (not JSON).
	Garbage string `json:"garbage,omitempty"`
	// Stderr is printed to stderr.
	Stderr string `json:"stderr,omitempty"`
	// Transcript (claude) prints an embedded real transcript instead of
	// generated init and result lines: mcp_tool.jsonl, resumed.jsonl,
	// denied.jsonl, bad_model.jsonl, no_conversation.jsonl.
	Transcript string `json:"transcript,omitempty"`
	// Denied (claude) are reported in permission_denials of the result.
	Denied []Tool `json:"denied,omitempty"`
	// Usage (ACP) sends a usage_update.
	Usage bool `json:"usage,omitempty"`
	// MCP (ACP) calls a tool of the first MCP server of the session and
	// answers "<tool>: <result text>" or "mcp error: <error>".
	MCP *MCPCall `json:"mcp,omitempty"`
}

// MCPCall is a tool call to an MCP server over HTTP.
type MCPCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
}

// Tool is a tool call of a Step.
type Tool struct {
	ID    string         `json:"id,omitempty"`
	Title string         `json:"title,omitempty"`
	Kind  string         `json:"kind,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

// Command returns the command and environment that start the fake agent
// of kind "acp" or "claude" running s. Pass env as StartOptions.Env.
func Command(kind string, s Script) (command string, env map[string]string) {
	s.Kind = kind
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	b, _ := json.Marshal(s)
	// Race-enabled binaries otherwise sleep a second before exiting.
	race := strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0")
	return exe, map[string]string{EnvVar: string(b), "GORACE": race}
}

// Main runs the fake agent and exits when the process was started as one;
// otherwise it returns at once.
func Main() {
	v := os.Getenv(EnvVar)
	if v == "" {
		return
	}
	var s Script
	if err := json.Unmarshal([]byte(v), &s); err != nil {
		fmt.Fprintln(os.Stderr, "agenttest:", err)
		os.Exit(2)
	}
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		if s.Version == "" {
			s.Version = "fake-agent 1.0.0"
		}
		fmt.Println(s.Version)
		os.Exit(0)
	}
	switch s.Kind {
	case "acp":
		os.Exit(runACP(s))
	case "claude":
		os.Exit(runClaude(s, os.Args[1:]))
	case "sleep":
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "agenttest: unknown kind %q\n", s.Kind)
	os.Exit(2)
}

// Call is one recorded call to a fake agent.
type Call struct {
	// Method is the ACP method, or "claude" for a run of the fake claude.
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	// For "claude":
	Args      []string `json:"args,omitempty"`
	Dir       string   `json:"dir,omitempty"`
	StdinEOF  bool     `json:"stdinEOF,omitempty"`
	MCPConfig string   `json:"mcpConfig,omitempty"`
	Env       []string `json:"env,omitempty"`
}

// Calls reads the call log of the fakes run with this state directory.
func Calls(state string) ([]Call, error) {
	f, err := os.Open(filepath.Join(state, "calls.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var calls []Call
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var c Call
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, err
		}
		calls = append(calls, c)
	}
	return calls, sc.Err()
}

// ChildPID is the pid a Spawn step recorded, or 0.
func ChildPID(state string) int {
	b, err := os.ReadFile(filepath.Join(state, "child.pid"))
	if err != nil {
		return 0
	}
	var pid int
	fmt.Sscan(strings.TrimSpace(string(b)), &pid)
	return pid
}

var logMu sync.Mutex

func record(state string, c Call) {
	logMu.Lock()
	defer logMu.Unlock()
	b, _ := json.Marshal(c)
	f, err := os.OpenFile(filepath.Join(state, "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// session is a fake session kept on disk, so a resume in a new process
// finds it.
type session struct {
	ID      string   `json:"id"`
	Cwd     string   `json:"cwd"`
	Mode    string   `json:"mode"`
	Model   string   `json:"model"`
	Prompts []string `json:"prompts"`
}

func sessionPath(state, id string) string {
	return filepath.Join(state, "sessions", filepath.Base(id)+".json")
}

func loadSession(state, id string) (*session, bool) {
	b, err := os.ReadFile(sessionPath(state, id))
	if err != nil {
		return nil, false
	}
	var s session
	if json.Unmarshal(b, &s) != nil {
		return nil, false
	}
	return &s, true
}

func (s *session) save(state string) {
	os.MkdirAll(filepath.Join(state, "sessions"), 0o700)
	b, _ := json.Marshal(s)
	os.WriteFile(sessionPath(state, s.ID), b, 0o600)
}

func (s *session) echo(prompt string) string {
	return fmt.Sprintf("prompt=%s history=%s mode=%s model=%s", prompt, strings.Join(s.Prompts, "|"), s.Mode, s.Model)
}

func envText(name string) string {
	if v, ok := os.LookupEnv(name); ok {
		return name + "=" + v
	}
	return name + " unset"
}

// spawn starts a sleeping child that shares our stdout and records its
// pid.
func spawn(state string) {
	exe, _ := os.Executable()
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), EnvVar+`={"kind":"sleep"}`)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "agenttest: spawn:", err)
		return
	}
	os.WriteFile(filepath.Join(state, "child.pid"), []byte(fmt.Sprint(cmd.Process.Pid)), 0o600)
}

func turn(s Script, n int) []Step {
	if n < len(s.Turns) {
		return s.Turns[n]
	}
	return []Step{{Text: "ok"}}
}
