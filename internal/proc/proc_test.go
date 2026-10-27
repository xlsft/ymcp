package proc

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

func TestAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("this process is not alive")
	}
	if Alive(0) || Alive(-1) {
		t.Error("a pid that is not positive is alive")
	}
	exe := "true"
	args := []string{}
	if runtime.GOOS == "windows" {
		exe, args = "cmd", []string{"/c", "exit"}
	}
	cmd := exec.Command(exe, args...)
	if err := cmd.Run(); err != nil {
		t.Skip(err)
	}
	if Alive(cmd.Process.Pid) {
		t.Errorf("the exited pid %d is alive", cmd.Process.Pid)
	}
}
