//go:build !windows

package workspace

import (
	"os/exec"
	"syscall"
)

// detach puts a long-lived ssh in its own process group, so a Ctrl+C in
// ymcp's terminal doesn't end the connection while ymcp shuts down.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
