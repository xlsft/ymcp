//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the child in a session of its own, so closing the
// terminal or the parent's process group does not reach it.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// terminateProcess asks the process to stop with SIGTERM.
func terminateProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// killProcess kills the process, and its whole group when it leads one (a
// detached runner does), which takes the agents it started along.
func killProcess(pid int) error {
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		return syscall.Kill(-pid, syscall.SIGKILL)
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
