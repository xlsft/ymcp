//go:build !windows

package agent

import "syscall"

// alive reports whether a process exists and is not a zombie.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	var ws syscall.WaitStatus
	// Our own children can't be zombies here: the fake agents are their
	// parents. A process reparented to init is reaped by it.
	_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	return syscall.Kill(pid, 0) == nil
}
