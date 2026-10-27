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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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
	// IsDir reports whether path is an existing directory on the host. A
	// path that does not exist is (false, nil).
	IsDir(ctx context.Context, path string) (bool, error)
	// CacheDir is the per-user cache directory on the host: os.UserCacheDir
	// for this machine; an ssh host would answer ~/.cache.
	CacheDir(ctx context.Context) (string, error)
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
	if dir != "" {
		// exec would blame the program for a missing directory.
		if ok, err := isDir(dir); err != nil || !ok {
			if err == nil {
				err = fmt.Errorf("directory %s does not exist", dir)
			}
			return nil, nil, err
		}
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

func (localHost) IsDir(_ context.Context, path string) (bool, error) { return isDir(path) }

func isDir(path string) (bool, error) {
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	return fi.IsDir(), nil
}

func (localHost) CacheDir(context.Context) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(dir), nil
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

// run executes git with args in dir and returns trimmed stdout. A git
// command that only reads or sets config is run again when git crashed
// (see crashed).
func run(ctx context.Context, h Host, dir string, args ...string) (string, error) {
	argv := append([]string{"git"}, args...)
	stdout, stderr, err := h.Run(ctx, dir, argv, nil)
	for try := 1; try < 3 && crashed(err) && rerunnable(args) && ctx.Err() == nil; try++ {
		stdout, stderr, err = h.Run(ctx, dir, argv, nil)
	}
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

// crashed reports whether git died of a crash of its own, a segfault or a
// bus error, which git on a busy macOS machine now and then does.
func crashed(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	s := ee.Error()
	return s == "signal: segmentation fault" || s == "signal: bus error"
}

// rerunnable reports whether a git command can simply run again after a
// crash: it reads, fetches or sets config, and changes nothing else.
func rerunnable(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "config", "rev-parse", "rev-list", "fetch", "status", "log", "diff", "show-ref", "for-each-ref",
		"symbolic-ref", "check-ref-format", "ls-remote", "merge-base", "remote":
		return true
	case "worktree":
		return len(args) > 1 && args[1] == "list"
	}
	return false
}

// reason is the human-readable cause of a failed git call, for check details.
func reason(err error) string {
	var ce *cmdError
	if errors.As(err, &ce) && ce.msg != "" {
		return ce.msg
	}
	return err.Error()
}
