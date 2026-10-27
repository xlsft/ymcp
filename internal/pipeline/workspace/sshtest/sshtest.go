// Package sshtest provides a fake ssh for tests. It runs by re-executing
// the test binary: call Main first in TestMain, then point an SSHHost at
// the program and environment from Command.
//
// The fake records every invocation and runs the remote command with the
// local sh, so the "remote host" is this machine. It understands what
// workspace.SSHHost uses: a master connection (ControlMaster=yes, a plain
// file stands in for the control socket), -O check, exit, forward and
// cancel, and a dedicated -N -R forward. A forward to local port P is
// reported as remote port P, since the remote host is this machine. As
// with sshd, the remote command leads a session of its own and its stdin
// ends when the fake ssh exits.
package sshtest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Environment of the fake.
const (
	// EnvLog is the file the fake appends its invocations to; setting it
	// makes the test binary act as ssh.
	EnvLog = "YMCP_FAKE_SSH"
	// EnvFail makes every connection fail like a refused key.
	EnvFail = "YMCP_FAKE_SSH_FAIL"
	// EnvPort is the remote port the fake reports for any forward.
	EnvPort = "YMCP_FAKE_SSH_PORT"
)

// Call is one recorded invocation.
type Call struct {
	Args    []string          `json:"args"`
	Alias   string            `json:"alias"`
	Options map[string]string `json:"options"` // -o Key=Value, by key
	Control string            `json:"control,omitempty"`
	Forward string            `json:"forward,omitempty"` // the -R spec
	N       bool              `json:"n,omitempty"`
	Command string            `json:"command,omitempty"`
}

// Command returns the program to use as ssh and the environment variables
// that make it the fake, logging to log.
func Command(log string) (program string, env map[string]string) {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	race := strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0")
	return exe, map[string]string{EnvLog: log, "GORACE": race}
}

// Calls reads the invocations logged to log.
func Calls(log string) ([]Call, error) {
	f, err := os.Open(log)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var calls []Call
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var c Call
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, err
		}
		calls = append(calls, c)
	}
	return calls, sc.Err()
}

// Main acts as ssh and exits when the process was started as the fake;
// otherwise it returns at once.
func Main() {
	log := os.Getenv(EnvLog)
	if log == "" {
		return
	}
	os.Exit(run(log, os.Args[1:]))
}

// Parse reads an ssh command line the way the fake does.
func Parse(args []string) Call {
	c := Call{Args: args, Options: map[string]string{}}
	var op string
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			break
		}
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a {
		case "-o":
			k, v, _ := strings.Cut(next(), "=")
			c.Options[k] = strings.Trim(v, `"`)
		case "-F", "-S", "-p", "-l", "-i", "-J":
			next()
		case "-O":
			op = next()
		case "-R":
			c.Forward = next()
		case "-N":
			c.N = true
		}
	}
	c.Control = op
	if i < len(args) {
		c.Alias = args[i]
		i++
	}
	if i < len(args) && args[i] == "--" {
		i++
	}
	c.Command = strings.Join(args[i:], " ")
	return c
}

var logMu sync.Mutex

func record(log string, c Call) {
	logMu.Lock()
	defer logMu.Unlock()
	b, _ := json.Marshal(c)
	f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

func run(log string, args []string) int {
	c := Parse(args)
	record(log, c)
	sock := c.Options["ControlPath"]
	if sock == "none" {
		sock = ""
	}
	if os.Getenv(EnvFail) != "" && c.Control == "" {
		fmt.Fprintln(os.Stderr, c.Alias+": Permission denied (publickey).")
		return 255
	}
	switch c.Control {
	case "check":
		if exists(sock) {
			fmt.Fprintln(os.Stderr, "Master running")
			return 0
		}
		fmt.Fprintln(os.Stderr, "Control socket connect: No such file or directory")
		return 255
	case "exit":
		if !exists(sock) {
			return 255
		}
		os.Remove(sock)
		fmt.Fprintln(os.Stderr, "Exit request sent.")
		return 0
	case "forward":
		if !exists(sock) {
			return 255
		}
		fmt.Println(remotePort(c.Forward))
		return 0
	case "cancel":
		return 0
	case "":
	default:
		fmt.Fprintln(os.Stderr, "unknown -O", c.Control)
		return 255
	}

	if c.Options["ControlMaster"] == "yes" {
		return runMaster(sock)
	}
	if c.N {
		if c.Forward != "" {
			fmt.Fprintln(os.Stderr, "Warning: Permanently added 'fake' (ED25519) to the list of known hosts.")
			_, port, _ := strings.Cut(c.Forward, ":127.0.0.1:")
			fmt.Fprintf(os.Stderr, "Allocated port %s for remote forward to 127.0.0.1:%s\r\n", remotePort(c.Forward), port)
		}
		select {} // until killed
	}
	if c.Command == "" {
		fmt.Fprintln(os.Stderr, "fake ssh: no command")
		return 255
	}
	// Like sshd: the command leads a session of its own, and its stdin
	// ends when the connection (this process) goes away.
	cmd := exec.Command("sh", "-c", c.Command)
	pr, pw, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake ssh:", err)
		return 255
	}
	go func() {
		io.Copy(pw, os.Stdin)
		pw.Close()
	}()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pr, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	newSession(cmd)
	err = cmd.Start()
	pr.Close()
	if err == nil {
		err = cmd.Wait()
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if code := ee.ExitCode(); code >= 0 {
				return code
			}
			return 255
		}
		fmt.Fprintln(os.Stderr, "fake ssh:", err)
		return 255
	}
	return 0
}

// remotePort is the port reported for a -R 0:127.0.0.1:P spec.
func remotePort(spec string) string {
	if p := os.Getenv(EnvPort); p != "" {
		return p
	}
	_, port, _ := strings.Cut(spec, ":127.0.0.1:")
	return port
}

// runMaster creates the control socket and stays until its stdin ends or
// the socket is removed (-O exit).
func runMaster(sock string) int {
	if sock == "" {
		fmt.Fprintln(os.Stderr, "fake ssh: master without ControlPath")
		return 255
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 255
	}
	if err := os.WriteFile(sock, []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 255
	}
	eof := make(chan struct{})
	go func() {
		io.Copy(io.Discard, os.Stdin)
		close(eof)
	}()
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-eof:
			os.Remove(sock)
			return 0
		case <-t.C:
			if !exists(sock) {
				return 0
			}
		}
	}
}

func exists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}
