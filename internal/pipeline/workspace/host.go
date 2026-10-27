// Package workspace is the git side of a ymcp pipeline run: it checks that a
// directory can host agent runs, creates one worktree per run with its own
// commit identity, commits whatever an agent left behind, and pushes the
// branch.
//
// Every git command goes through a Host, so the same code can later drive a
// remote machine over SSH. Commands are plain argv vectors; no shell is
// involved on the way.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// Host runs commands on the machine that holds the code.
type Host interface {
	// Name is "local" or an ssh host alias.
	Name() string
	// Run executes argv (argv[0] is the program) in dir and returns what it
	// wrote to stdout and stderr. A non-zero exit is returned as an error
	// together with the captured output. An empty dir means the host's
	// default directory; a nil stdin means no input.
	Run(ctx context.Context, dir string, argv []string, stdin io.Reader) (stdout, stderr []byte, err error)
}

// dirMaker is implemented by hosts that can create directories without
// spawning a process.
type dirMaker interface {
	MkdirAll(ctx context.Context, path string) error
}

type localHost struct{}

// Local returns the Host that runs commands on this machine with os/exec.
// git is told never to prompt on a terminal and to speak the C locale, so its
// output stays parsable.
func Local() Host { return localHost{} }

func (localHost) Name() string { return "local" }

func (localHost) Run(ctx context.Context, dir string, argv []string, stdin io.Reader) ([]byte, []byte, error) {
	if len(argv) == 0 {
		return nil, nil, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Stdin = stdin
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func (localHost) MkdirAll(_ context.Context, path string) error {
	return os.MkdirAll(path, 0o755)
}

// cmdError is a failed command with the message git printed.
type cmdError struct {
	cmd string
	msg string
	err error
}

func (e *cmdError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("%s: %v", e.cmd, e.err)
	}
	return fmt.Sprintf("%s: %v: %s", e.cmd, e.err, e.msg)
}

func (e *cmdError) Unwrap() error { return e.err }

// run executes git with args in dir and returns trimmed stdout.
func run(ctx context.Context, h Host, dir string, args ...string) (string, error) {
	argv := append([]string{"git"}, args...)
	stdout, stderr, err := h.Run(ctx, dir, argv, nil)
	out := trim(string(stdout))
	if err == nil {
		return out, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return out, cerr
	}
	msg := trim(string(stderr))
	if msg == "" {
		msg = out
	}
	return out, &cmdError{cmd: shorten("git "+join(args), 120), msg: shorten(msg, 600), err: err}
}

// reason is the human-readable cause of a failed git call, for check details.
func reason(err error) string {
	var ce *cmdError
	if errors.As(err, &ce) && ce.msg != "" {
		return ce.msg
	}
	return err.Error()
}
