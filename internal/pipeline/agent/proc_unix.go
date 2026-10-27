//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// setGroup makes the command lead a new process group, so the agent and
// everything it starts can be signalled together.
func setGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// procGroup is the process group of a started agent.
type procGroup struct{ pid int }

func attach(cmd *exec.Cmd) *procGroup { return &procGroup{pid: cmd.Process.Pid} }

// interrupt sends SIGINT to the whole group.
func (g *procGroup) interrupt() { _ = syscall.Kill(-g.pid, syscall.SIGINT) }

// kill sends SIGKILL to the whole group.
func (g *procGroup) kill() { _ = syscall.Kill(-g.pid, syscall.SIGKILL) }

func (g *procGroup) release() {}
