package agent

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// setGroup starts the command in a new process group, so a Ctrl+C in
// ymcp's console doesn't reach it directly.
func setGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// procGroup is a job object holding a started agent and its children.
// Without one (assignment can fail on old systems) only the agent itself
// is killed.
type procGroup struct {
	proc *os.Process
	job  windows.Handle
}

func attach(cmd *exec.Cmd) *procGroup {
	g := &procGroup{proc: cmd.Process}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return g
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return g
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return g
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return g
	}
	g.job = job
	return g
}

// interrupt kills the tree: Windows has no SIGINT for a process group
// whose console ymcp doesn't share.
func (g *procGroup) interrupt() { g.kill() }

func (g *procGroup) kill() {
	if g.job != 0 {
		_ = windows.TerminateJobObject(g.job, 1)
	}
	_ = g.proc.Kill()
}

func (g *procGroup) release() {
	if g.job != 0 {
		windows.CloseHandle(g.job)
		g.job = 0
	}
}
