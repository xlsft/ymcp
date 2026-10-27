package workspace_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/agent/agenttest"
	"github.com/xlsft/ymcp/internal/pipeline/engine"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// The integration test runs against a real sshd in Docker. It is off
// unless YMCP_SSH_IT=1. YMCP_SSH_IT_DIR keeps the key, the ssh config and
// the fake agent in that directory instead of a temporary one.
const itImage = "ymcp-ssh-it:1"

const itDockerfile = `FROM debian:13
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server git && rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd
`

// itSetup prepares the container as root and starts sshd: a user dev with
// the test key and a git identity, a bare origin with one commit and a
// clone of it.
const itSetup = `set -e
useradd -m -s /bin/bash dev
mkdir -p /home/dev/.ssh
printf '%s\n' "$PUBKEY" > /home/dev/.ssh/authorized_keys
chmod 700 /home/dev/.ssh
chmod 600 /home/dev/.ssh/authorized_keys
chown -R dev:dev /home/dev/.ssh
mkdir -p /srv
chown dev:dev /srv
su dev -c 'set -e
git config --global user.email dev@example.com
git config --global user.name Dev
git init -q --bare -b main /srv/origin.git
git clone -q /srv/origin.git /home/dev/repo 2>/dev/null
cd /home/dev/repo
echo hello > README.md
git add README.md
git commit -qm init
git push -q origin main'
ssh-keygen -A
exec /usr/sbin/sshd -D -e
`

func docker(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// itHost starts the container and returns the ssh host "it" pointing at it,
// with the directory of the test files.
func itHost(t *testing.T) (*workspace.SSHHost, string, string) {
	t.Helper()
	if os.Getenv("YMCP_SSH_IT") != "1" {
		t.Skip("set YMCP_SSH_IT=1 to run the ssh integration test (needs Docker)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	dir := os.Getenv("YMCP_SSH_IT_DIR")
	if dir == "" {
		dir = t.TempDir()
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if _, err := os.Stat(key); err != nil {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "ymcp-it", "-f", key).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v\n%s", err, out)
		}
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}

	docker(t, itDockerfile, "build", "-q", "-t", itImage, "-")
	b := make([]byte, 4)
	rand.Read(b)
	name := "ymcp-ssh-it-" + hex.EncodeToString(b)
	docker(t, "", "run", "-d", "--rm", "--name", name, "-e", "PUBKEY="+strings.TrimSpace(string(pub)), "-p", "127.0.0.1::22", itImage, "sh", "-c", itSetup)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	addr := docker(t, "", "port", name, "22")
	_, port, _ := strings.Cut(strings.Split(addr, "\n")[0], ":")

	cfg := filepath.Join(dir, "ssh_config")
	conf := fmt.Sprintf(`Host it
  HostName 127.0.0.1
  Port %s
  User dev
  IdentityFile %s
  IdentitiesOnly yes
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
`, port, key)
	if err := os.WriteFile(cfg, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workspace.EnvSSHConfig, cfg)

	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err := exec.Command("ssh", "-F", cfg, "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", "it", "test -d /home/dev/repo/.git && echo ready").CombinedOutput()
		if err == nil && strings.Contains(string(out), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sshd did not come up: %v\n%s\n%s", err, out, docker(t, "", "logs", name))
		}
		time.Sleep(500 * time.Millisecond)
	}
	h := workspace.SSH("it")
	t.Cleanup(func() { h.Close() })
	return h, dir, name
}

// itAgent cross-compiles the agent package's test binary, which turns into
// the fake agent when YMCP_FAKE_AGENT is set, and installs it in the
// container.
func itAgent(t *testing.T, dir, container string) string {
	t.Helper()
	arch := docker(t, "", "version", "--format", "{{.Server.Arch}}")
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	}
	bin := filepath.Join(dir, "fake-agent-linux-"+arch)
	cmd := exec.Command("go", "test", "-c", "-o", bin, "github.com/xlsft/ymcp/internal/pipeline/agent")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the fake agent: %v\n%s", err, out)
	}
	const remote = "/usr/local/bin/fake-agent"
	docker(t, "", "cp", bin, container+":"+remote)
	docker(t, "", "exec", container, "chmod", "755", remote)
	return remote
}

// itPing is an MCP server here with one tool, ping, answering "pong
// <Authorization header>".
func itPing(t *testing.T) int {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "ping", Version: "1"}, nil)
	type in struct{}
	mcp.AddTool(srv, &mcp.Tool{Name: "ping"}, func(_ context.Context, req *mcp.CallToolRequest, _ in) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong " + req.Extra.Header.Get("Authorization")}}}, nil, nil
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

func remoteOut(t *testing.T, h *workspace.SSHHost, argv ...string) string {
	t.Helper()
	out, stderr, err := h.Run(context.Background(), "", argv, nil)
	if err != nil {
		t.Fatalf("%v on the host: %v: %s", argv, err, stderr)
	}
	return strings.TrimSpace(string(out))
}

func fakeEnv(t *testing.T, kind string, s agenttest.Script) map[string]string {
	t.Helper()
	s.Kind = kind
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{agenttest.EnvVar: string(b)}
}

func TestSSHIntegration(t *testing.T) {
	h, dir, container := itHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	const repo = "/home/dev/repo"

	// The git side.
	checks, err := workspace.Inspect(ctx, h, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if !c.OK {
			t.Errorf("check %s: %s", c.Name, c.Detail)
		}
	}
	root, err := workspace.DefaultRoot(ctx, h, "backend")
	if err != nil || root != "/home/dev/.cache/ymcp/worktrees/backend" {
		t.Fatalf("DefaultRoot = %q, %v", root, err)
	}
	wt, err := workspace.Create(ctx, h, repo, root, "IT-1-fix", "", workspace.AgentIdentity("dev@example.com", "backend", 1, "Fake"))
	if err != nil {
		t.Fatal(err)
	}
	runDir, err := workspace.RunDir(ctx, h, repo, 1)
	if err != nil || runDir != "/home/dev/repo/.git/ymcp/runs/1" {
		t.Fatalf("RunDir = %q, %v", runDir, err)
	}

	// The agent, on the host, reaching the MCP server here.
	bin := itAgent(t, dir, container)
	local := itPing(t)
	fw, err := h.Forward(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	port, err := fw.Port(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MCP server 127.0.0.1:%d here is 127.0.0.1:%d on the host", local, port)
	mcpURL := fmt.Sprintf("http://127.0.0.1:%d/test", port)
	state := "/tmp/fake-state"
	remoteOut(t, h, "mkdir", "-p", state)

	a, err := agent.New("acp", bin, nil, agent.WithHost(h))
	if err != nil {
		t.Fatal(err)
	}
	env := fakeEnv(t, "acp", agenttest.Script{State: state, Turns: [][]agenttest.Step{
		{{MCP: &agenttest.MCPCall{Tool: "ping"}}},
		{{Env: "YMCP_IT"}, {Text: " "}, {Env: "GIT_DIR"}},
		{{Text: "editing"}, {Spawn: true}, {Sleep: time.Minute}},
	}})
	env["YMCP_IT"] = "it's set"
	s, err := a.Start(ctx, agent.StartOptions{Dir: wt.Path, RunDir: runDir, Env: env, MCP: []agent.MCPServer{{
		Name: "ymcp", URL: mcpURL, Headers: map[string]string{"Authorization": "Bearer run-token"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range s.Events() {
		}
	}()
	res, err := s.Prompt(ctx, "call ping")
	if err != nil || res.Text != "ping: pong Bearer run-token" {
		t.Errorf("MCP turn = %q, %v", res.Text, err)
	}
	res, err = s.Prompt(ctx, "env")
	if err != nil || res.Text != "YMCP_IT=it's set GIT_DIR unset" {
		t.Errorf("env turn = %q, %v", res.Text, err)
	}
	// A turn with a child: Close ends both on the host.
	go s.Prompt(ctx, "sleep")
	var child string
	for i := 0; i < 100 && child == ""; i++ {
		time.Sleep(100 * time.Millisecond)
		child, _, _ = runQuiet(h, "cat", state+"/child.pid")
	}
	if child == "" {
		t.Fatal("the fake agent's child never started")
	}
	s.Close()
	waitGone(t, h, child)

	// claude, with the prompts and the MCP config in the run directory.
	c, err := agent.New("claude", bin, nil, agent.WithHost(h))
	if err != nil {
		t.Fatal(err)
	}
	cstate := "/tmp/fake-claude"
	remoteOut(t, h, "mkdir", "-p", cstate)
	prompt := strings.Repeat("p", 100<<10)
	system := strings.Repeat("s", 60<<10)
	cs, err := c.Start(ctx, agent.StartOptions{Dir: wt.Path, RunDir: runDir, SystemPrompt: system,
		Env: fakeEnv(t, "claude", agenttest.Script{State: cstate, Turns: [][]agenttest.Step{{{Text: "done"}}, {{Spawn: true}, {Sleep: time.Minute}}}}),
		MCP: []agent.MCPServer{{Name: "ymcp", URL: mcpURL}}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range cs.Events() {
		}
	}()
	if res, err := cs.Prompt(ctx, prompt); err != nil || res.Text != "done" {
		t.Errorf("claude turn = %+v, %v", res, err)
	}
	var calls []agenttest.Call
	for _, line := range strings.Split(remoteOut(t, h, "cat", cstate+"/calls.jsonl"), "\n") {
		var call agenttest.Call
		if json.Unmarshal([]byte(line), &call) == nil {
			calls = append(calls, call)
		}
	}
	if len(calls) != 1 || calls[0].Dir != wt.Path || !strings.Contains(calls[0].MCPConfig, mcpURL) || !calls[0].StdinEOF {
		t.Errorf("claude calls = %d: %+v", len(calls), calls)
	} else if args := calls[0].Args; len(args) < 2 || args[1] != prompt {
		t.Errorf("claude got a prompt of %d bytes", len(args[1]))
	}
	// Cancel interrupts the group on the host.
	done := make(chan agent.Result, 1)
	go func() {
		r, _ := cs.Prompt(ctx, "sleep")
		done <- r
	}()
	child = ""
	for i := 0; i < 100 && child == ""; i++ {
		time.Sleep(100 * time.Millisecond)
		child, _, _ = runQuiet(h, "cat", cstate+"/child.pid")
	}
	if child == "" {
		t.Fatal("the fake claude's child never started")
	}
	cs.Cancel()
	select {
	case r := <-done:
		if r.StopReason != agent.StopCancelled {
			t.Errorf("cancelled turn = %+v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Cancel did not end the turn")
	}
	waitGone(t, h, child)
	cs.Close()
	if left := remoteOut(t, h, "ls", "-A", runDir); left != "" {
		t.Errorf("left in the run dir: %q", left)
	}

	// ymcp commits and pushes from the host.
	remoteOut(t, h, "sh", "-c", "echo work > '"+wt.Path+"/work.txt'")
	if ok, err := workspace.CommitLeftovers(ctx, h, wt, "IT-1 work"); !ok || err != nil {
		t.Fatalf("CommitLeftovers = %v, %v", ok, err)
	}
	if err := workspace.Push(ctx, h, wt); err != nil {
		t.Fatal(err)
	}
	if got := remoteOut(t, h, "git", "--git-dir=/srv/origin.git", "log", "-1", "--format=%s|%ae", "IT-1-fix"); got != "IT-1 work|dev+agent_backend_1@example.com" {
		t.Errorf("origin = %q", got)
	}
	if err := workspace.Remove(ctx, h, repo, wt); err != nil {
		t.Fatal(err)
	}
	fw.Close()

	// The pipeline check: ssh, git, the forward probe, the repository and
	// the agent on the host. YouTrack is not there, which only fails its
	// own check.
	p := spec.Pipeline{
		Name: "backend", Profile: "it",
		Source:    spec.Source{Query: "#Unresolved"},
		Workspace: spec.Workspace{Host: "it", Dir: repo},
		Agent:     spec.Agent{Provider: "acp", Command: bin},
	}
	results, err := engine.CheckWith(ctx, engine.Options{Pipeline: p,
		Connect: func(context.Context, string, *slog.Logger) (*youtrack.Client, config.Config, error) {
			return nil, config.Config{}, fmt.Errorf("no YouTrack in this test")
		}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]engine.CheckResult{}
	for _, r := range results {
		if _, dup := got[r.Name]; !dup {
			got[r.Name] = r
		}
		t.Logf("check %s ok=%v warn=%v: %s", r.Name, r.OK, r.Warning, r.Detail)
	}
	for _, name := range []string{"ssh", "ssh: git", "ssh: MCP forward", "workspace: git repository", "workspace: push access", "workspace: worktrees", "agent"} {
		if r := got[name]; !r.OK {
			t.Errorf("check %s: %+v", name, r)
		}
	}
}

func runQuiet(h *workspace.SSHHost, argv ...string) (string, string, error) {
	out, stderr, err := h.Run(context.Background(), "", argv, nil)
	return strings.TrimSpace(string(out)), string(stderr), err
}

// waitGone waits until pid no longer runs on the host.
func waitGone(t *testing.T, h *workspace.SSHHost, pid string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		// A zombie still has a /proc entry; its state is Z.
		out, _, _ := runQuiet(h, "sh", "-c", `cut -d' ' -f3 /proc/$1/stat 2>/dev/null`, "sh", pid)
		if out == "" || out == "Z" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %s still runs on the host (state %s)", pid, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestSSHIntegrationWithoutMultiplexing takes the Windows path: every
// command connects by itself and a dedicated `ssh -N -R` holds the forward,
// its port read from ssh's stderr.
func TestSSHIntegrationWithoutMultiplexing(t *testing.T) {
	_, dir, container := itHost(t)
	off := false
	h := workspace.NewSSH("it", workspace.SSHOptions{Multiplex: &off})
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	checks, err := workspace.Inspect(ctx, h, "/home/dev/repo")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if !c.OK {
			t.Errorf("check %s: %s", c.Name, c.Detail)
		}
	}
	bin := itAgent(t, dir, container)
	fw, err := h.Forward(ctx, itPing(t))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := fw.Port(ctx)
	a, _ := agent.New("acp", bin, nil, agent.WithHost(h))
	s, err := a.Start(ctx, agent.StartOptions{Dir: "/home/dev/repo",
		Env: fakeEnv(t, "acp", agenttest.Script{State: "/tmp/fake-nomux", Turns: [][]agenttest.Step{{{MCP: &agenttest.MCPCall{Tool: "ping"}}}}}),
		MCP: []agent.MCPServer{{Name: "ymcp", URL: fmt.Sprintf("http://127.0.0.1:%d/test", port), Headers: map[string]string{"Authorization": "Bearer t"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	go func() {
		for range s.Events() {
		}
	}()
	if res, err := s.Prompt(ctx, "ping"); err != nil || res.Text != "ping: pong Bearer t" {
		t.Errorf("MCP turn = %q, %v", res.Text, err)
	}
}
