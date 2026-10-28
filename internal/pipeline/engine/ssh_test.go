package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
	"github.com/xlsft/ymcp/internal/pipeline/workspace/sshtest"
)

// fakeSSHHost is an ssh host "devbox" that runs the fake ssh: the remote
// side is this machine.
func fakeSSHHost(t *testing.T) (*workspace.SSHHost, string) {
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

// TestSSHRun runs a pipeline on an ssh host: git goes through ssh, the
// agent works in the host's worktree and reaches the MCP server through
// the forward.
func TestSSHRun(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Workspace.Host = "devbox" })
	host, log := fakeSSHHost(t)
	h.host = host
	is := h.yt.addIssue("Fix the login page", "Open")
	h.ag.script(func(t *fakeTurn) agent.Result {
		if _, err := t.Tool("report", map[string]any{"text": "Reached over ssh"}); err != nil {
			t.Say("report failed: " + err.Error())
		}
		writeFile(t, "login.go", "package login\n")
		t.Say("Fixed it.")
		return agent.Result{}
	})
	h.start()
	run := h.waitReview(1, is.Key)
	if run.Host != "devbox" {
		t.Errorf("run host = %q", run.Host)
	}
	if !contains(h.yt.agentComments(is.Key), "Reached over ssh") {
		t.Errorf("the MCP call did not arrive: %q", h.yt.agentComments(is.Key))
	}
	s0 := h.ag.startLog()[0]
	if !strings.HasSuffix(filepath.ToSlash(s0.RunDir), "/ymcp/runs/1") || s0.Dir != run.Worktree {
		t.Errorf("start options = %+v", s0)
	}
	if got := git(t, h.origin, "log", "-1", "--format=%s", run.Branch); got != is.Key+" Fixed it" {
		t.Errorf("origin log = %q", got)
	}
	cs, err := sshtest.Calls(log)
	if err != nil {
		t.Fatal(err)
	}
	var forwards, gits int
	for _, c := range cs {
		if c.Control == "forward" {
			forwards++
			if !strings.HasPrefix(c.Forward, "0:127.0.0.1:") || !strings.HasPrefix(s0.MCP[0].URL, "http://127.0.0.1:"+strings.TrimPrefix(c.Forward, "0:127.0.0.1:")+"/") {
				t.Errorf("forward %q, agent URL %q", c.Forward, s0.MCP[0].URL)
			}
		}
		if strings.Contains(c.Command, "'git'") || strings.Contains(c.Command, " git ") {
			gits++
		}
	}
	if forwards != 1 || gits < 5 {
		t.Errorf("%d forwards, %d git commands over ssh", forwards, gits)
	}
}

func TestCheckSSH(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Workspace.Host = "devbox" })
	host, _ := fakeSSHHost(t)
	h.yt.addIssue("Sample", "Open")
	o := h.options()
	o.Host = host
	byName := func(res []CheckResult) map[string]CheckResult {
		m := map[string]CheckResult{}
		for _, r := range res {
			if _, dup := m[r.Name]; !dup {
				m[r.Name] = r
			}
		}
		return m
	}
	res, err := CheckWith(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	got := byName(res)
	for _, name := range []string{checkSSHName, checkSSHGit, checkSSHForward, "workspace: git repository", "workspace: push access", "agent"} {
		if r, ok := got[name]; !ok || !r.OK {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if d := got[checkSSHName].Detail; !strings.HasPrefix(d, "connected to devbox (") {
		t.Errorf("ssh detail = %q", d)
	}
	if d := got["agent"].Detail; !strings.HasSuffix(d, " on devbox") {
		t.Errorf("agent detail = %q", d)
	}

	t.Setenv(sshtest.EnvFail, "1")
	host2, _ := fakeSSHHost(t)
	t.Setenv(sshtest.EnvFail, "1")
	o.Host = host2
	res, _ = CheckWith(context.Background(), o)
	got = byName(res)
	if r := got[checkSSHName]; r.OK || !strings.Contains(r.Detail, "Permission denied") {
		t.Errorf("ssh: %+v", r)
	}
	for _, name := range []string{"workspace", "agent"} {
		if r := got[name]; r.OK || !strings.Contains(r.Detail, "not reachable") {
			t.Errorf("%s: %+v", name, r)
		}
	}
}
