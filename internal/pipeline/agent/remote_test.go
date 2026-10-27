package agent

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/pipeline/agent/agenttest"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
	"github.com/xlsft/ymcp/internal/pipeline/workspace/sshtest"
)

func TestRemoteScript(t *testing.T) {
	s := remoteScript("/srv/it's", map[string]string{"B": "two words", "A": "$x"}, []string{"'kimi'", "acp"}, false)
	for _, want := range []string{
		`cd '/srv/it'\''s' || exit 127`,
		`printf 'ymcp-pgid %s\n' "$pg" >&2`,
		"exec env -u CLAUDECODE -u GIT_AUTHOR_DATE",
		"-u GIT_DIR",
		`'A=$x' 'B=two words' 'kimi' acp` + "\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "<&3") || strings.Contains(s, "</dev/null\n") {
		t.Errorf("unwatched script watches stdin:\n%s", s)
	}
	w := remoteScript("", nil, []string{"claude"}, true)
	if strings.Contains(w, "cd ") || !strings.Contains(w, `kill -s KILL -- "-$pg"`) || !strings.HasSuffix(w, " claude </dev/null\n") {
		t.Errorf("watched script:\n%s", w)
	}
}

// TestRemoteScriptRuns runs the script with sh, as sshd would.
func TestRemoteScriptRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	dir := t.TempDir()
	words := []string{"sh", "-c", workspace.Quote(`printf '%s|%s|%s|%s' "$(pwd -P)" "${GIT_DIR-unset}" "$V" "$(cat)"`)}
	cmd := exec.Command("sh", "-c", remoteScript(dir, map[string]string{"V": "a 'b'"}, words, false))
	cmd.Env = append(os.Environ(), "GIT_DIR=/elsewhere")
	cmd.Stdin = strings.NewReader("in")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	real, _ := filepath.EvalSymlinks(dir)
	if want := real + "|unset|a 'b'|in"; string(out) != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if !strings.HasPrefix(stderr.String(), pgidMarker) {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestPgidWriter(t *testing.T) {
	var out bytes.Buffer
	var pg string
	w := &pgidWriter{next: &out, set: func(s string) { pg = s }}
	for _, chunk := range []string{"Warning: added host\r\n", "ymcp-p", "gid 4242\nagent ", "says hi\n"} {
		w.Write([]byte(chunk))
	}
	if pg != "4242" || out.String() != "Warning: added host\r\nagent says hi\n" {
		t.Errorf("pg = %q, out = %q", pg, out.String())
	}

	out.Reset()
	pg = ""
	w = &pgidWriter{next: &out, set: func(s string) { pg = s }}
	w.Write(bytes.Repeat([]byte("x"), 5000))
	w.Write([]byte("tail"))
	if pg != "" || out.Len() != 5004 {
		t.Errorf("no marker: pg = %q, %d bytes", pg, out.Len())
	}
}

// sshHost is a workspace.SSHHost running the fake ssh.
func sshHost(t *testing.T) (*workspace.SSHHost, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh needs a POSIX sh")
	}
	log := filepath.Join(t.TempDir(), "ssh.jsonl")
	program, env := sshtest.Command(log)
	for k, v := range env {
		t.Setenv(k, v)
	}
	sockDir, err := os.MkdirTemp("", "ys")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	h := workspace.NewSSH("devbox", workspace.SSHOptions{Program: program, SocketDir: sockDir, ConnectTimeout: 5 * time.Second})
	t.Cleanup(func() { h.Close() })
	return h, log
}

// remoteFake starts a fake agent through the fake ssh.
func remoteFake(t *testing.T, kind string, script agenttest.Script, opts StartOptions) (*recorder, string, string) {
	t.Helper()
	h, log := sshHost(t)
	if script.State == "" {
		script.State = t.TempDir()
	}
	command, env := agenttest.Command(kind, script)
	a, err := New(kind, command, nil, WithHost(h))
	if err != nil {
		t.Fatal(err)
	}
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.Env == nil {
		opts.Env = map[string]string{}
	}
	for k, v := range env {
		opts.Env[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := a.Start(ctx, opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r := record(s)
	t.Cleanup(func() { s.Close() })
	return r, script.State, log
}

func agentCommands(t *testing.T, log string) []string {
	t.Helper()
	cs, err := sshtest.Calls(log)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range cs {
		if strings.Contains(c.Command, "ymcp-pgid") {
			out = append(out, c.Command)
		}
	}
	return out
}

func TestRemoteACP(t *testing.T) {
	dir := t.TempDir()
	r, state, log := remoteFake(t, "acp", agenttest.Script{Turns: [][]agenttest.Step{
		{{Echo: true}},
		{{Env: "YMCP_REMOTE_VAR"}},
		{{Spawn: true}, {Sleep: time.Minute}},
	}}, StartOptions{Dir: dir, Env: map[string]string{"YMCP_REMOTE_VAR": "it's here"}, MCP: []MCPServer{{Name: "ymcp", URL: "http://127.0.0.1:1/p"}}})
	if res := r.prompt(t, "hello 'there'"); !strings.Contains(res.Text, "prompt=hello 'there'") {
		t.Errorf("turn 1 = %q", res.Text)
	}
	if res := r.prompt(t, "env"); res.Text != "YMCP_REMOTE_VAR=it's here" {
		t.Errorf("turn 2 = %q", res.Text)
	}
	news := calls(t, state, "session/new")
	if len(news) != 1 || params(t, news[0])["cwd"] != dir {
		t.Errorf("session/new = %+v", news)
	}
	cmds := agentCommands(t, log)
	if len(cmds) != 1 || !strings.Contains(cmds[0], "YMCP_REMOTE_VAR") || strings.Contains(cmds[0], "<&3") {
		t.Errorf("agent ssh commands = %q", cmds)
	}

	// A running turn with a child: Close ends both on the host.
	go r.Prompt(context.Background(), "sleep")
	child := waitChild(t, state)
	r.Close()
	gone(t, child)
}

func TestRemoteClaude(t *testing.T) {
	runDir := t.TempDir()
	prompt := "-starts like a flag; it's long: " + strings.Repeat("x", 300)
	r, state, log := remoteFake(t, "claude", agenttest.Script{Turns: [][]agenttest.Step{{{Text: "done"}}}}, StartOptions{
		RunDir:       runDir,
		Env:          map[string]string{"YMCP_REMOTE_VAR": "1"},
		SystemPrompt: "be 'nice'",
		MCP:          []MCPServer{{Name: "ymcp", URL: "http://127.0.0.1:9/p", Headers: map[string]string{"Authorization": "Bearer t"}}},
	})
	if res := r.prompt(t, prompt); res.Text != "done" {
		t.Fatalf("result = %+v", res)
	}
	cs := calls(t, state, "claude")
	if len(cs) != 1 {
		t.Fatalf("claude calls = %+v", cs)
	}
	c := cs[0]
	if !c.StdinEOF {
		t.Error("claude's stdin is not at EOF")
	}
	if got := argAfter(c.Args, "-p"); got != " "+prompt {
		t.Errorf("prompt = %q", got)
	}
	if got := argAfter(c.Args, "--append-system-prompt"); got != "be 'nice'" {
		t.Errorf("system prompt = %q", got)
	}
	if mc := argAfter(c.Args, "--mcp-config"); mc != runDir+"/mcp.json" || !strings.Contains(c.MCPConfig, "Bearer t") {
		t.Errorf("mcp config %q = %q", mc, c.MCPConfig)
	}
	for _, cmd := range agentCommands(t, log) {
		if strings.Contains(cmd, "xxxxxxxx") || strings.Contains(cmd, "nice") {
			t.Errorf("the prompts went on the ssh command line: %q", cmd)
		}
		if !strings.Contains(cmd, "<&3") {
			t.Errorf("claude's session is not watched: %q", cmd)
		}
	}
	r.Close()
	left, _ := os.ReadDir(runDir)
	if len(left) != 0 {
		t.Errorf("files left in the run dir: %v", left)
	}
}

func argAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func TestRemoteClaudeTempDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	r, state, _ := remoteFake(t, "claude", agenttest.Script{}, StartOptions{MCP: []MCPServer{{Name: "ymcp", URL: "http://127.0.0.1:9/p"}}})
	r.prompt(t, "hi")
	mc := argAfter(calls(t, state, "claude")[0].Args, "--mcp-config")
	if !strings.HasPrefix(mc, tmp+"/ymcp-claude.") {
		t.Errorf("mcp config at %q", mc)
	}
	r.Close()
	left, _ := filepath.Glob(filepath.Join(tmp, "ymcp-claude.*"))
	if len(left) != 0 {
		t.Errorf("left in TMPDIR: %v", left)
	}
}

func TestRemoteClaudeCancel(t *testing.T) {
	r, state, log := remoteFake(t, "claude", agenttest.Script{Turns: [][]agenttest.Step{{{Spawn: true}, {Sleep: time.Minute}}}}, StartOptions{})
	done := make(chan Result, 1)
	go func() {
		res, _ := r.Prompt(context.Background(), "sleep")
		done <- res
	}()
	child := waitChild(t, state)
	if err := r.Cancel(); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res.StopReason != StopCancelled {
			t.Errorf("stop reason = %q", res.StopReason)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Cancel did not end the turn")
	}
	gone(t, child)
	cs, _ := sshtest.Calls(log)
	interrupted := false
	for _, c := range cs {
		interrupted = interrupted || (strings.Contains(c.Command, "kill -s") && strings.Contains(c.Command, " INT "))
	}
	if !interrupted {
		t.Error("no SIGINT was sent to the group on the host")
	}
}

// TestRemoteClaudeDisconnect: when ssh dies, the watch on the host kills
// the agent and what it started.
func TestRemoteClaudeDisconnect(t *testing.T) {
	r, state, _ := remoteFake(t, "claude", agenttest.Script{Turns: [][]agenttest.Step{{{Spawn: true}, {Sleep: time.Minute}}}}, StartOptions{})
	go r.Prompt(context.Background(), "sleep")
	child := waitChild(t, state)
	s := r.Session.(*claudeSession)
	s.mu.Lock()
	g := s.group.(*remoteGroup)
	s.mu.Unlock()
	g.local.kill() // only ssh
	gone(t, child)
}

func TestRemoteCheck(t *testing.T) {
	h, _ := sshHost(t)
	command := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(command, []byte("#!/bin/sh\necho '2.1.0 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a, _ := New("claude", command, nil, WithHost(h), WithMode("acceptEdits"))
	info, err := a.Check(context.Background())
	if err != nil || info.Path != command || info.Version != "2.1.0 (Claude Code)" {
		t.Errorf("Check = %+v, %v", info, err)
	}
	a, _ = New("kimi", "no-such-agent-binary", nil, WithHost(h))
	if _, err := a.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "not found on devbox") {
		t.Errorf("missing binary: %v", err)
	}
}

// pingServer is an MCP server on this machine with one tool, ping, that
// answers "pong <auth header>".
func pingServer(t *testing.T) (port int) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "ping", Version: "1"}, nil)
	type in struct{}
	mcp.AddTool(srv, &mcp.Tool{Name: "ping"}, func(_ context.Context, req *mcp.CallToolRequest, _ in) (*mcp.CallToolResult, any, error) {
		auth := ""
		if req.Extra != nil && req.Extra.Header != nil {
			auth = req.Extra.Header.Get("Authorization")
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong " + auth}}}, nil, nil
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)}
	go hs.Serve(l)
	t.Cleanup(func() { hs.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

// TestRemoteACPCallsMCP: an agent on the host reaches the MCP server here
// through the forward.
func TestRemoteACPCallsMCP(t *testing.T) {
	h, _ := sshHost(t)
	local := pingServer(t)
	ctx := context.Background()
	fw, err := h.Forward(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	defer fw.Close()
	port, err := fw.Port(ctx)
	if err != nil {
		t.Fatal(err)
	}
	script := agenttest.Script{State: t.TempDir(), Turns: [][]agenttest.Step{{{MCP: &agenttest.MCPCall{Tool: "ping"}}}}}
	command, env := agenttest.Command("acp", script)
	a, _ := New("acp", command, nil, WithHost(h))
	s, err := a.Start(ctx, StartOptions{Dir: t.TempDir(), Env: env, MCP: []MCPServer{{
		Name: "ymcp", URL: fmt.Sprintf("http://127.0.0.1:%d/p", port), Headers: map[string]string{"Authorization": "Bearer run-token"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	r := record(s)
	defer r.Close()
	if res := r.prompt(t, "call"); res.Text != "ping: pong Bearer run-token" {
		t.Errorf("result = %q", res.Text)
	}
}
