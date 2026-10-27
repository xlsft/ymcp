//go:build !windows

package engine

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/workspace/sshtest"
)

// sleeper starts a process that leads its own group, like an agent.
func sleeper(t *testing.T) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	return cmd, done
}

// TestOrphanedAgentIsKilledOnStart: an agent group an earlier runner left
// in the store is killed when the next runner starts, unless the group id
// now belongs to a process that started at another time.
func TestOrphanedAgentIsKilledOnStart(t *testing.T) {
	h := newHarness(t, nil)
	machine, _ := os.Hostname()
	orphan, orphanDone := sleeper(t)
	other, otherDone := sleeper(t)
	now := time.Now().UnixMilli()
	killed := putGroup(t, h, "YPC-1", agentGroup{PGID: orphan.Process.Pid, Host: "local", Machine: machine, At: now})
	// Recorded an hour ago: the id was reused by the process started now.
	reused := putGroup(t, h, "YPC-2", agentGroup{PGID: other.Process.Pid, Host: "local", Machine: machine, At: now - time.Hour.Milliseconds()})
	elsewhere := putGroup(t, h, "YPC-3", agentGroup{PGID: other.Process.Pid, Host: "local", Machine: "another-machine", At: now})
	gone := putGroup(t, h, "YPC-4", agentGroup{PGID: 999999, Host: "local", Machine: machine, At: now})

	h.start()
	select {
	case <-orphanDone:
	case <-time.After(waitTimeout):
		t.Fatal("the orphaned agent was not killed")
	}
	h.waitFor("the records cleared", func() bool {
		for _, run := range []int64{killed.ID, reused.ID, elsewhere.ID, gone.ID} {
			if v, _, _ := h.st.RunState(context.Background(), run, stateAgentGroup); v != "" {
				return false
			}
		}
		return true
	})
	if len(h.events(killed.ID, "agent.orphan_killed")) != 1 {
		t.Errorf("events = %+v", h.events(killed.ID, ""))
	}
	select {
	case err := <-otherDone:
		t.Errorf("a process that only shares the group id was killed: %v", err)
	default:
	}
	if len(h.events(reused.ID, "agent.orphan_killed"))+len(h.events(elsewhere.ID, "agent.orphan_killed")) != 0 {
		t.Error("a group that is not the recorded one was reported killed")
	}
}

// TestOrphanedAgentOnSSHHostIsKilled: the group of an agent on an ssh host
// is checked and killed there, through the host.
func TestOrphanedAgentOnSSHHostIsKilled(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Workspace.Host = "devbox" })
	host, log := fakeSSHHost(t)
	h.host = host
	orphan, orphanDone := sleeper(t)
	run := putGroup(t, h, "YPC-1", agentGroup{PGID: orphan.Process.Pid, Host: "devbox", Machine: "laptop", At: time.Now().UnixMilli()})
	h.start()
	select {
	case <-orphanDone:
	case <-time.After(waitTimeout):
		t.Fatal("the orphaned agent was not killed")
	}
	h.waitFor("the record cleared", func() bool {
		v, _, _ := h.st.RunState(context.Background(), run.ID, stateAgentGroup)
		return v == ""
	})
	cs, err := sshtest.Calls(log)
	if err != nil {
		t.Fatal(err)
	}
	var kills int
	for _, c := range cs {
		if strings.Contains(c.Command, "kill -s KILL") {
			kills++
		}
	}
	if kills != 1 {
		t.Errorf("%d kills over ssh", kills)
	}
}
