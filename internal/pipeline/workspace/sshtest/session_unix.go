//go:build !windows

package sshtest

import (
	"os/exec"
	"syscall"
)

func newSession(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
