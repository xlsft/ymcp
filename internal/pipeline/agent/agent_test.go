package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/xlsft/ymcp/internal/pipeline/agent/agenttest"
)

func TestScrub(t *testing.T) {
	env := []string{
		"PATH=/usr/bin", "HOME=/home/u", "SSH_AUTH_SOCK=/tmp/agent", "HTTPS_PROXY=http://proxy:3128",
		"ANTHROPIC_API_KEY=sk-keep", "GIT_EDITOR=vim", "YMCP_PROFILE=work", "CLAUDE_CONFIG_DIR_NOT=x=y",
		"CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_SESSION_ID=abc", "CLAUDE_PID=42",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:9", "AI_AGENT=claude-code", "USE_STAGING_OAUTH=1",
		"MCP_CONNECTION_NONBLOCKING=1", "CODEX_SANDBOX=seatbelt", "OPENCODE=1",
		"GIT_DIR=/elsewhere/.git", "GIT_WORK_TREE=/elsewhere", "GIT_AUTHOR_EMAIL=me@example.com",
		"YOUTRACK_PASSWORD=secret", "YOUTRACK_URL=https://yt",
	}
	got := Scrub(env)
	want := []string{
		"PATH=/usr/bin", "HOME=/home/u", "SSH_AUTH_SOCK=/tmp/agent", "HTTPS_PROXY=http://proxy:3128",
		"ANTHROPIC_API_KEY=sk-keep", "GIT_EDITOR=vim", "YMCP_PROFILE=work",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Scrub =\n%q\nwant\n%q", got, want)
	}
}

func TestEnvironExtra(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://parent-proxy")
	t.Setenv("YMCP_TEST_KEEP", "parent")
	env := environ(map[string]string{"ANTHROPIC_BASE_URL": "http://mine", "YMCP_TEST_KEEP": "child", "EXTRA": "1"})
	count := func(prefix string) (n int, last string) {
		for _, kv := range env {
			if strings.HasPrefix(kv, prefix) {
				n, last = n+1, kv
			}
		}
		return
	}
	for kv, want := range map[string]string{"ANTHROPIC_BASE_URL=": "ANTHROPIC_BASE_URL=http://mine", "YMCP_TEST_KEEP=": "YMCP_TEST_KEEP=child", "EXTRA=": "EXTRA=1"} {
		if n, last := count(kv); n != 1 || last != want {
			t.Errorf("%s: %d entries, last %q; want one %q", kv, n, last, want)
		}
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		provider, command string
		args              []string
		name, wantCommand string
		wantArgs          []string
		wantErr           string
	}{
		{provider: "claude", name: "claude", wantCommand: "claude"},
		{provider: "kimi", args: []string{"--debug"}, name: "kimi", wantCommand: "kimi", wantArgs: []string{"acp", "--debug"}},
		{provider: "opencode", command: "/opt/oc", name: "opencode", wantCommand: "/opt/oc", wantArgs: []string{"acp"}},
		{provider: "acp", command: "npx", args: []string{"@agentclientprotocol/claude-agent-acp"}, name: "acp", wantCommand: "npx", wantArgs: []string{"@agentclientprotocol/claude-agent-acp"}},
		{provider: "acp", wantErr: "needs a command"},
		{provider: "codex", wantErr: "unknown provider"},
	}
	for _, tt := range tests {
		a, err := New(tt.provider, tt.command, tt.args)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("New(%q) error = %v, want %q", tt.provider, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("New(%q): %v", tt.provider, err)
		}
		if a.Name() != tt.name {
			t.Errorf("Name = %q, want %q", a.Name(), tt.name)
		}
		var command string
		var args []string
		switch a := a.(type) {
		case *claudeAgent:
			command, args = a.command, a.args
		case *acpAgent:
			command, args = a.command, a.args
		}
		if command != tt.wantCommand || !slices.Equal(args, tt.wantArgs) {
			t.Errorf("%s: command %q %q, want %q %q", tt.provider, command, args, tt.wantCommand, tt.wantArgs)
		}
	}
}

func TestCheck(t *testing.T) {
	command, env := agenttest.Command("acp", agenttest.Script{Version: "2.2.0\nextra"})
	for k, v := range env {
		t.Setenv(k, v) // Check runs without StartOptions.Env
	}
	tests := []struct {
		provider string
		args     []string
		settings []Setting
		warning  string // substring of the only warning; "" for none
	}{
		{provider: "claude", warning: "claude has no --permission-mode or --allowedTools"},
		{provider: "claude", args: []string{"--permission-mode", "acceptEdits"}},
		{provider: "claude", args: []string{"--allowedTools=Read,Edit"}},
		{provider: "claude", settings: []Setting{WithMode("acceptEdits")}},
		{provider: "claude", args: []string{"--allowedTools", "Read", "--resume", "x"}, warning: "--resume, which ymcp sets itself"},
		{provider: "kimi", settings: []Setting{WithMode("yolo")}, warning: `kimi in mode "yolo" never asks`},
		{provider: "kimi", settings: []Setting{WithMode("default")}},
		{provider: "opencode", warning: "opencode has no model set"},
		{provider: "opencode", settings: []Setting{WithModel("kimi-code-plan-global/k3")}},
		{provider: "acp"},
	}
	for _, tt := range tests {
		a, err := New(tt.provider, command, tt.args, tt.settings...)
		if err != nil {
			t.Fatal(err)
		}
		info, err := a.Check(context.Background())
		if err != nil {
			t.Fatalf("%s %q: Check: %v", tt.provider, tt.args, err)
		}
		if info.Path == "" || info.Version != "2.2.0" {
			t.Errorf("%s: info = %+v", tt.provider, info)
		}
		switch {
		case tt.warning == "" && len(info.Warnings) > 0:
			t.Errorf("%s %q: unexpected warnings %q", tt.provider, tt.args, info.Warnings)
		case tt.warning != "" && (len(info.Warnings) != 1 || !strings.Contains(info.Warnings[0], tt.warning)):
			t.Errorf("%s %q: warnings %q, want one with %q", tt.provider, tt.args, info.Warnings, tt.warning)
		}
	}

	a, _ := New("claude", "ymcp-no-such-agent", nil)
	if _, err := a.Check(context.Background()); err == nil {
		t.Error("Check of a missing binary succeeded")
	}
}

func TestEventQueueKeepsOrderAndCloses(t *testing.T) {
	q := newEventQueue()
	for i := range 1000 {
		q.emit(Event{Kind: EventText, Text: string(rune('a' + i%26))})
	}
	q.close()
	q.emit(Event{Kind: EventError}) // after close: dropped
	n := 0
	for e := range q.out {
		if want := string(rune('a' + n%26)); e.Text != want {
			t.Fatalf("event %d = %q, want %q", n, e.Text, want)
		}
		n++
	}
	if n != 1000 {
		t.Errorf("got %d events, want 1000", n)
	}
}

func TestClaudeMCPConfig(t *testing.T) {
	got := claudeMCPConfig([]MCPServer{{Name: "ymcp", URL: "http://127.0.0.1:1/p", Headers: map[string]string{"Authorization": "Bearer t"}}})
	var v map[string]map[string]map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatal(err)
	}
	s := v["mcpServers"]["ymcp"]
	if s["type"] != "http" || s["url"] != "http://127.0.0.1:1/p" || s["headers"].(map[string]any)["Authorization"] != "Bearer t" {
		t.Errorf("config = %s", got)
	}
}
