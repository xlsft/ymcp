package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/workspace"
)

// Host is a machine the agent runs on instead of this one, reached through
// a local process: workspace.SSHHost. See WithHost.
type Host interface {
	// Name is the host as the user named it, e.g. an ssh alias.
	Name() string
	// Run runs argv in dir on the host and returns its output.
	Run(ctx context.Context, dir string, argv []string, stdin io.Reader) (stdout, stderr []byte, err error)
	// Command returns the local process that runs script, a POSIX shell
	// script, on the host with the process's stdin, stdout and stderr.
	Command(ctx context.Context, script string) (*exec.Cmd, error)
}

// WithHost runs the agent on h instead of this machine: Check looks for
// the binary there and sessions start there, in StartOptions.Dir on h.
// The agent gets the login environment of the host with StartOptions.Env
// set on top; nothing of ymcp's own environment goes there. nil means this
// machine.
func WithHost(h Host) Setting { return func(s *settings) { s.host = h } }

// remoteUnset are dropped from the host's environment: git state that
// would point the agent's git at another repository or override the
// worktree's identity, and the marker of an enclosing Claude Code session.
var remoteUnset = []string{
	"CLAUDECODE",
	"GIT_AUTHOR_DATE", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_NAME",
	"GIT_COMMITTER_DATE", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_NAME",
	"GIT_COMMON_DIR", "GIT_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_PREFIX", "GIT_WORK_TREE",
}

// pgidMarker starts the line the remote script prints to stderr with its
// process group, which Cancel and Close signal.
const pgidMarker = "ymcp-pgid "

// remoteScript is the shell script that runs the agent on a host: it
// changes to dir, reports its process group (sshd makes every session its
// own group) and execs the agent with env set by `env`. words is the agent's
// command line, already quoted.
//
// With watch, the agent's stdin is /dev/null and the script's stdin is
// watched instead: when it ends — ymcp closed the ssh session, or ssh or
// ymcp died — the whole group is killed. Without ssh's help nothing would
// stop it, as a session without a terminal gets no SIGHUP.
func remoteScript(dir string, env map[string]string, words []string, watch bool) string {
	var b strings.Builder
	if dir != "" {
		if strings.HasPrefix(dir, "-") {
			dir = "./" + dir
		}
		b.WriteString("cd " + workspace.Quote(dir) + " || exit 127\n")
	}
	b.WriteString(`pg=$(cut -d' ' -f5 /proc/$$/stat 2>/dev/null)` + "\n")
	b.WriteString(`[ -n "$pg" ] || pg=$(ps -o pgid= -p $$ 2>/dev/null | tr -d ' ')` + "\n")
	b.WriteString(`[ -n "$pg" ] || pg=$$` + "\n")
	b.WriteString(`printf '` + pgidMarker + `%s\n' "$pg" >&2` + "\n")
	if watch {
		b.WriteString("exec 3<&0\n")
		b.WriteString(`( cat <&3 >/dev/null 2>&1; kill -s KILL -- "-$pg" ) </dev/null >/dev/null 2>&1 &` + "\n")
		b.WriteString("exec 3<&-\n")
	}
	b.WriteString("exec env")
	for _, k := range remoteUnset {
		b.WriteString(" -u " + k)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(" " + workspace.Quote(k+"="+env[k]))
	}
	for _, w := range words {
		b.WriteString(" " + w)
	}
	if watch {
		b.WriteString(" </dev/null")
	}
	b.WriteString("\n")
	return b.String()
}

// started is an agent process: here, or the local ssh of a remote one.
type started struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser // the agent's stdin when asked for; for a watched remote agent, the watch
	stdout *os.File
	group  stopper
}

// launch starts argv as the agent in dir with env, here or on the host of
// s. With stdin the caller gets the agent's stdin; otherwise it reads
// /dev/null. On a host, argv[i] for every i in files is handed over in the
// file files[i] on the host rather than on the command line, which has to
// fit into one argument of the ssh session. onGroup, when set, is told the
// agent's process group (see StartOptions.OnGroup).
func (s settings) launch(ctx context.Context, dir string, env map[string]string, argv []string, files map[int]string, stdin bool, errs *tail, onGroup func(int)) (*started, error) {
	if s.host == nil {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = dir
		cmd.Env = environ(env)
		var in io.WriteCloser
		if stdin {
			var err error
			if in, err = cmd.StdinPipe(); err != nil {
				return nil, err
			}
		}
		stdout, g, err := startPiped(cmd, errs)
		if err != nil {
			if in != nil {
				in.Close()
			}
			return nil, err
		}
		if onGroup != nil {
			onGroup(cmd.Process.Pid) // it leads its own group (a job on Windows)
		}
		return &started{cmd: cmd, stdin: in, stdout: stdout, group: g}, nil
	}

	words := make([]string, len(argv))
	for i, a := range argv {
		if path, ok := files[i]; ok {
			if err := writeRemote(ctx, s.host, path, a); err != nil {
				return nil, err
			}
			words[i] = `"$(cat ` + workspace.Quote(path) + `)"`
			continue
		}
		words[i] = workspace.Quote(a)
	}
	cmd, err := s.host.Command(ctx, remoteScript(dir, env, words, !stdin))
	if err != nil {
		return nil, err
	}
	// The stdin of ssh is the agent's (ACP) or the watch (claude): either
	// way a pipe that stays open until the session ends.
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	g := &remoteGroup{host: s.host, onGroup: onGroup}
	stdout, local, err := startPiped(cmd, &pgidWriter{next: errs, set: g.setPG})
	if err != nil {
		in.Close()
		return nil, err
	}
	g.local = local
	g.stdin = in
	st := &started{cmd: cmd, stdout: stdout, group: g}
	if stdin {
		st.stdin = in
	}
	return st, nil
}

// writeRemote writes content to path on h, readable only by its owner.
func writeRemote(ctx context.Context, h Host, path, content string) error {
	_, stderr, err := h.Run(ctx, "", []string{"sh", "-c", `umask 077 && cat > "$1"`, "sh", path}, strings.NewReader(content))
	if err != nil {
		return fmt.Errorf("writing %s on %s: %w: %s", path, h.Name(), err, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// stopper ends a started agent: its process group here, or the ssh
// process and the process group on the host.
type stopper interface {
	interrupt()
	kill()
	// exited says the process has exited; a later kill only cleans up.
	exited()
	release()
}

func (g *procGroup) exited() {}

// remoteGroup is an agent on a host: the local ssh and the remote group.
type remoteGroup struct {
	host    Host
	local   *procGroup
	stdin   io.Closer
	onGroup func(int)

	mu   sync.Mutex
	pg   string
	done bool
}

func (g *remoteGroup) setPG(pg string) {
	g.mu.Lock()
	g.pg = pg
	g.mu.Unlock()
	if n, err := strconv.Atoi(pg); err == nil && n > 1 && g.onGroup != nil {
		g.onGroup(n)
	}
}

// signal sends sig to the remote group; false when it could not.
func (g *remoteGroup) signal(sig string) bool {
	g.mu.Lock()
	pg, done := g.pg, g.done
	g.mu.Unlock()
	if pg == "" || done {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := g.host.Run(ctx, "", []string{"sh", "-c", `kill -s "$1" -- "-$2"`, "sh", sig, pg}, nil)
	return err == nil
}

// interrupt sends SIGINT to the agent's group on the host. If that is not
// possible, ssh itself is interrupted, which ends the session.
func (g *remoteGroup) interrupt() {
	go func() {
		if !g.signal("INT") {
			g.local.interrupt()
		}
	}()
}

// kill kills the agent's group on the host while it runs, then ssh.
func (g *remoteGroup) kill() {
	g.signal("KILL")
	g.stdin.Close()
	g.local.kill()
}

func (g *remoteGroup) exited() {
	g.mu.Lock()
	g.done = true
	g.mu.Unlock()
}

func (g *remoteGroup) release() { g.local.release() }

// pgidWriter passes stderr on, taking out the line with the remote process
// group.
type pgidWriter struct {
	next  io.Writer
	set   func(string)
	buf   []byte
	found bool
	lines int
}

func (w *pgidWriter) Write(p []byte) (int, error) {
	if w.found {
		return w.next.Write(p)
	}
	w.buf = append(w.buf, p...)
	for !w.found {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			if len(w.buf) > 4<<10 {
				w.found = true
			}
			break
		}
		line := w.buf[:i+1]
		if pg, ok := strings.CutPrefix(strings.TrimRight(string(line), "\r\n"), pgidMarker); ok {
			w.set(strings.TrimSpace(pg))
			w.found = true
		} else if _, err := w.next.Write(line); err != nil {
			return len(p), err
		}
		w.buf = w.buf[i+1:]
		// ssh's own warnings come first; the marker is the script's
		// first line.
		if w.lines++; w.lines > 50 {
			w.found = true
		}
	}
	if w.found && len(w.buf) > 0 {
		rest := w.buf
		w.buf = nil
		if _, err := w.next.Write(rest); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// end passes on what is left of a stream that ended before the marker.
func (w *pgidWriter) end() {
	if len(w.buf) > 0 {
		_, _ = w.next.Write(w.buf)
		w.buf = nil
	}
	if e, ok := w.next.(ender); ok {
		e.end()
	}
}

// checkRemoteBinary finds command on the host and reads its version.
func checkRemoteBinary(ctx context.Context, h Host, command string) (Info, error) {
	out, stderr, err := h.Run(ctx, "", []string{"sh", "-c", `command -v "$1"`, "sh", command}, nil)
	path := strings.TrimSpace(firstLine(string(out)))
	if err != nil || path == "" {
		if ctx.Err() != nil {
			return Info{}, ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 255 {
			return Info{}, fmt.Errorf("agent: ssh %s: %s", h.Name(), strings.TrimSpace(string(stderr)))
		}
		return Info{}, fmt.Errorf("agent: %s is not found on %s in the PATH of a non-interactive ssh session; install it there or set agent.command to its full path", command, h.Name())
	}
	info := Info{Path: path}
	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, stderr, err = h.Run(vctx, "", []string{path, "--version"}, nil)
	if err != nil {
		msg := "could not read the version: " + err.Error()
		if s := strings.TrimSpace(string(stderr)); s != "" {
			msg += ": " + s
		}
		info.Warnings = append(info.Warnings, msg)
		return info, nil
	}
	info.Version = strings.TrimSpace(firstLine(string(out)))
	return info, nil
}

// firstLine is the first non-empty line of s.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
