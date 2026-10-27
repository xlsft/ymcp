//go:build !windows

package proc

import (
	"errors"
	"syscall"
)

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
