package workspace

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach starts a long-lived ssh in a new process group, so a Ctrl+C in
// ymcp's console doesn't end the connection while ymcp shuts down.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}
