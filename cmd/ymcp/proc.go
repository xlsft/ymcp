package main

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// insideApp reports whether ymcp runs under the macOS app named app, e.g.
// in a terminal of Claude Desktop, where quitting the app would end ymcp.
func insideApp(app string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	pid := os.Getppid()
	for range 64 {
		if pid <= 1 {
			return false
		}
		out, err := exec.Command("ps", "-o", "ppid=,comm=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return false
		}
		ppid, comm, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
		if strings.Contains(comm, "/"+app+".app/") {
			return true
		}
		if pid, err = strconv.Atoi(ppid); err != nil {
			return false
		}
	}
	return false
}
