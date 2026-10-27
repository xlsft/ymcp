// Package proc answers questions about processes on this machine.
package proc

// Alive reports whether a process with the pid exists on this machine. A
// pid that is not positive is never alive.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return alive(pid)
}
