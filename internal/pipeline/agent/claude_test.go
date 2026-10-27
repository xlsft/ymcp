package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent/agenttest"
)

// argValue is the value after flag in args.
func argValue(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

func TestClaudeTranscriptAndResume(t *testing.T) {
	script := agenttest.Script{Turns: [][]agenttest.Step{
		{{Transcript: "mcp_tool.jsonl"}},
		{{Transcript: "resumed.jsonl"}},
	}}
	opts := StartOptions{
		Model: "sonnet", Mode: "acceptEdits", SystemPrompt: "SERVICE",
		AllowedTools: []string{"mcp__ymcp__request_input", "mcp__ymcp__report"},
		MCP:          []MCPServer{{Name: "ymcp", URL: "http://127.0.0.1:1/work", Headers: map[string]string{"Authorization": "Bearer tok"}}},
		Args:         []string{"--max-turns", "5"},
	}
	s, state := fake(t, "claude", script, opts)
	id := s.ID()
	if len(id) != 36 {
		t.Fatalf("ID = %q, want a UUID", id)
	}

	res := s.prompt(t, "-Remember PELICAN, then tell me who I am.")
	if res.StopReason != StopEndTurn || res.IsError || res.Text != "admin\n\n(Noted: PELICAN.)" {
		t.Errorf("turn 1 = %+v", res)
	}
	if e := s.wait(t, EventSession, nil); e.SessionID != id {
		t.Errorf("session event %q, want %q", e.SessionID, id)
	}
	call := s.wait(t, EventToolCall, func(e Event) bool { return e.Tool.Title == "mcp__ymcp__get_current_user" })
	if call.Tool.ID != "toolu_0142kooWzvjxAHhHgXNTiyjC" || call.Tool.Status != "in_progress" {
		t.Errorf("tool call = %+v", call.Tool)
	}
	upd := s.wait(t, EventToolUpdate, func(e Event) bool { return e.Tool.ID == call.Tool.ID })
	if upd.Tool.Status != "completed" || upd.Tool.Title != "mcp__ymcp__get_current_user" {
		t.Errorf("tool update = %+v", upd.Tool)
	}
	s.wait(t, EventText, func(e Event) bool { return strings.Contains(e.Text, "PELICAN") })
	s.wait(t, EventUsage, func(e Event) bool { return strings.Contains(e.Text, "cost=$0.1151") })

	res = s.prompt(t, "What word?")
	if res.Text != "PELICAN" {
		t.Errorf("turn 2 = %+v", res)
	}

	runs := calls(t, state, "claude")
	if len(runs) != 2 {
		t.Fatalf("%d claude runs, want 2", len(runs))
	}
	first, second := runs[0], runs[1]
	if p, _ := argValue(first.Args, "-p"); p != " -Remember PELICAN, then tell me who I am." {
		t.Errorf("-p %q", p)
	}
	for flag, want := range map[string]string{
		"--output-format": "stream-json", "--session-id": id, "--model": "sonnet", "--permission-mode": "acceptEdits",
		"--append-system-prompt": "SERVICE", "--allowedTools": "mcp__ymcp__request_input,mcp__ymcp__report", "--max-turns": "5",
	} {
		if got, _ := argValue(first.Args, flag); got != want {
			t.Errorf("first run %s = %q, want %q (args %q)", flag, got, want, first.Args)
		}
	}
	if !slices.Contains(first.Args, "--verbose") || !slices.Contains(first.Args, "--strict-mcp-config") {
		t.Errorf("first run args %q", first.Args)
	}
	if !first.StdinEOF {
		t.Error("stdin of claude is not /dev/null")
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type, URL string
			Headers   map[string]string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(first.MCPConfig), &cfg); err != nil {
		t.Fatalf("mcp config %q: %v", first.MCPConfig, err)
	}
	if y := cfg.MCPServers["ymcp"]; y.Type != "http" || y.URL != "http://127.0.0.1:1/work" || y.Headers["Authorization"] != "Bearer tok" {
		t.Errorf("mcp config = %s", first.MCPConfig)
	}
	if got, _ := argValue(second.Args, "--resume"); got != id || slices.Contains(second.Args, "--session-id") {
		t.Errorf("second run args %q, want --resume %s", second.Args, id)
	}
}

func TestClaudeDenied(t *testing.T) {
	s, _ := fake(t, "claude", agenttest.Script{Turns: [][]agenttest.Step{{{Transcript: "denied.jsonl"}}}}, StartOptions{})
	res := s.prompt(t, "Create hello.txt")
	if res.IsError || res.StopReason != StopEndTurn || len(res.Denied) != 5 {
		t.Fatalf("result = %+v", res)
	}
	d := res.Denied[4]
	if d.Title != "Write" || d.Kind != "edit" || d.Status != "denied" || d.ID != "toolu_01T6NdCLD5opowNNKxErpJDL" || !strings.Contains(string(d.Input), "hello.txt") {
		t.Errorf("denial = %+v", d)
	}
	upd := s.wait(t, EventToolUpdate, func(e Event) bool { return e.Tool.ID == d.ID })
	if upd.Tool.Status != "failed" {
		t.Errorf("denied call update = %+v", upd.Tool)
	}

	s2, _ := fake(t, "claude", agenttest.Script{Turns: [][]agenttest.Step{{
		{Denied: []agenttest.Tool{{Title: "mcp__ymcp__get_current_user"}}},
		{Text: "I need permission."},
	}}}, StartOptions{})
	res = s2.prompt(t, "who am I")
	if len(res.Denied) != 1 || res.Denied[0].Title != "mcp__ymcp__get_current_user" || res.Text != "I need permission." {
		t.Errorf("result = %+v", res)
	}
}

func TestClaudeErrors(t *testing.T) {
	tests := []struct {
		name    string
		steps   []agenttest.Step
		wantErr string // in Result.Err
		fails   bool   // Prompt returns an error too
	}{
		{name: "unknown model, success with is_error", steps: []agenttest.Step{{Transcript: "bad_model.jsonl"}, {Exit: 1}},
			wantErr: "There's an issue with the selected model (no-such-model-xyz)"},
		{name: "sign-in failure", steps: []agenttest.Step{{Error: "Invalid API key · Please run /login"}}, wantErr: "Invalid API key"},
		{name: "crash without result", steps: []agenttest.Step{{Text: "half"}, {Stderr: "panic: boom"}, {Exit: 3}}, wantErr: "panic: boom", fails: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := fake(t, "claude", agenttest.Script{Turns: [][]agenttest.Step{tt.steps}}, StartOptions{})
			res, err := s.Prompt(context.Background(), "hi")
			if (err != nil) != tt.fails {
				t.Errorf("err = %v, want failure %v", err, tt.fails)
			}
			if !res.IsError || res.StopReason != StopError || !strings.Contains(res.Err, tt.wantErr) {
				t.Errorf("result = %+v, want error with %q", res, tt.wantErr)
			}
			if !tt.fails {
				s.wait(t, EventError, nil)
			}
		})
	}
}

func TestClaudeUnknownSessionIsErrNoResume(t *testing.T) {
	s, _ := fake(t, "claude", agenttest.Script{}, StartOptions{SessionID: "0b0e7a52-1111-4a2b-9c3d-123456789abc"})
	res, err := s.Prompt(context.Background(), "hi")
	if !errors.Is(err, ErrNoResume) || !res.IsError {
		t.Errorf("Prompt = %+v, %v; want ErrNoResume", res, err)
	}
}

func TestClaudeSkipsNonJSONLines(t *testing.T) {
	s, _ := fake(t, "claude", agenttest.Script{Banner: "Warning: update available", Turns: [][]agenttest.Step{{
		{Garbage: "[debug] something odd"}, {Text: "fine"}, {Garbage: "{not json either"},
	}}}, StartOptions{})
	if res := s.prompt(t, "hi"); res.Text != "fine" || res.IsError {
		t.Errorf("result = %+v", res)
	}
}

func TestClaudeEnvIsScrubbed(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "sdk-ts")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:9")
	s, state := fake(t, "claude", agenttest.Script{Turns: [][]agenttest.Step{{
		{Env: "CLAUDECODE"}, {Env: "ANTHROPIC_BASE_URL"}, {Env: "EXTRA"},
	}}}, StartOptions{Env: map[string]string{"EXTRA": "yes"}})
	res := s.prompt(t, "env")
	if want := "CLAUDECODE unset\n\nANTHROPIC_BASE_URL unset\n\nEXTRA=yes"; res.Text != want {
		t.Errorf("text = %q, want %q", res.Text, want)
	}
	for _, kv := range calls(t, state, "claude")[0].Env {
		if strings.HasPrefix(kv, "CLAUDE") {
			t.Errorf("leaked %s", kv)
		}
	}
}

func TestClaudeCancelKillsProcessGroup(t *testing.T) {
	for _, viaContext := range []bool{false, true} {
		s, state := fake(t, "claude", agenttest.Script{Turns: [][]agenttest.Step{{
			{Spawn: true}, {Text: "working"}, {Sleep: time.Minute},
		}}}, StartOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		type out struct {
			res Result
			err error
		}
		done := make(chan out, 1)
		go func() {
			res, err := s.Prompt(ctx, "work")
			done <- out{res, err}
		}()
		s.wait(t, EventText, nil)
		child := waitChild(t, state)
		start := time.Now()
		if viaContext {
			cancel()
		} else if err := s.Cancel(); err != nil {
			t.Fatal(err)
		}
		select {
		case o := <-done:
			if o.err != nil || o.res.StopReason != StopCancelled || o.res.Text != "working" {
				t.Errorf("context %v: Prompt = %+v, %v", viaContext, o.res, o.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Prompt did not return after Cancel")
		}
		if d := time.Since(start); d > killGrace+2*time.Second {
			t.Errorf("cancel took %v", d)
		}
		gone(t, child)
		cancel()
		// The session stays usable: the next turn resumes it.
		if res := s.prompt(t, "again"); res.Text != "ok" {
			t.Errorf("after cancel: %+v", res)
		}
		if args := calls(t, state, "claude")[1].Args; !slices.Contains(args, "--resume") {
			t.Errorf("after cancel args %q", args)
		}
	}
}

func TestClaudeCloseTwice(t *testing.T) {
	s, _ := fake(t, "claude", agenttest.Script{}, StartOptions{MCP: []MCPServer{{Name: "ymcp", URL: "http://x"}}})
	sess := s.Session.(*claudeSession)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("events not closed")
	}
	if _, err := s.Prompt(context.Background(), "x"); err == nil {
		t.Error("Prompt after Close succeeded")
	}
	if err := s.Answer("1", "allow"); err == nil {
		t.Error("Answer on claude succeeded")
	}
	if _, err := os.Stat(sess.tmp); sess.tmp == "" || !os.IsNotExist(err) {
		t.Errorf("mcp config dir %q left behind (%v)", sess.tmp, err)
	}
}

func TestClaudeCrashWithChildHoldingOutput(t *testing.T) {
	s, state := fake(t, "claude", agenttest.Script{Turns: [][]agenttest.Step{{
		{Spawn: true}, {Stderr: "fatal: oops"}, {Exit: 3},
	}}}, StartOptions{})
	done := make(chan error, 1)
	go func() {
		_, err := s.Prompt(context.Background(), "x")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "fatal: oops") || !strings.Contains(err.Error(), "exit status 3") {
			t.Errorf("err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Prompt hangs while a child holds stdout")
	}
	gone(t, waitChild(t, state))
}

func TestRedactArgs(t *testing.T) {
	got := redactArgs([]string{"-p", "fix it", "--verbose", "--append-system-prompt", "секрет", "--model", "sonnet"})
	want := []string{"-p", "<6 chars>", "--verbose", "--append-system-prompt", "<6 chars>", "--model", "sonnet"}
	if !slices.Equal(got, want) {
		t.Errorf("redactArgs = %q, want %q", got, want)
	}
}
