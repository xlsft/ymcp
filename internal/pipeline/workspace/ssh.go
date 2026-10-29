package workspace

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Environment variables read by SSH.
const (
	// EnvSSH names the ssh program; default "ssh" from PATH.
	EnvSSH = "YMCP_SSH"
	// EnvSSHConfig is an ssh config file passed with -F instead of
	// ~/.ssh/config.
	EnvSSHConfig = "YMCP_SSH_CONFIG"
)

// SSHOptions configure an SSHHost. The zero value is the default.
type SSHOptions struct {
	// Program is the ssh binary. Default: $YMCP_SSH, else "ssh".
	Program string
	// ConfigFile is passed with -F. Default: $YMCP_SSH_CONFIG, else none
	// (ssh reads ~/.ssh/config).
	ConfigFile string
	// Multiplex says whether commands share one master connection. Default:
	// yes, except on Windows, where OpenSSH can't multiplex.
	Multiplex *bool
	// SocketDir holds the master's control socket. Default:
	// <user cache dir>/ymcp/ssh.
	SocketDir string
	// ConnectTimeout bounds connecting and authenticating. Default 15s.
	ConnectTimeout time.Duration
}

// SSHHost is a Host reached with the system ssh, so ~/.ssh/config,
// ssh-agent, ProxyJump and hardware keys work as they do in a terminal. The
// remote side is assumed to have a POSIX shell.
//
// On macOS and Linux every command goes through one master connection that
// the host opens on first use and Close ends. On Windows, where OpenSSH can't
// multiplex, every command opens its own connection.
//
// Its methods are safe for concurrent use.
type SSHHost struct {
	alias   string
	program string
	config  string
	mux     bool
	sockDir string
	timeout time.Duration
	id      string // makes the control socket unique to this host value

	mu       sync.Mutex
	m        *master
	gen      int // incremented for every master started
	closed   bool
	cache    string
	forwards map[*SSHForward]bool
}

// SSH returns the host of an ssh alias (or user@host) with the default
// options.
func SSH(alias string) *SSHHost { return NewSSH(alias, SSHOptions{}) }

// NewSSH returns the host of an ssh alias with options o.
func NewSSH(alias string, o SSHOptions) *SSHHost {
	h := &SSHHost{alias: strings.TrimSpace(alias), program: o.Program, config: o.ConfigFile, sockDir: o.SocketDir, timeout: o.ConnectTimeout, forwards: map[*SSHForward]bool{}}
	if h.program == "" {
		h.program = os.Getenv(EnvSSH)
	}
	if h.program == "" {
		h.program = "ssh"
	}
	if h.config == "" {
		h.config = os.Getenv(EnvSSHConfig)
	}
	h.mux = runtime.GOOS != "windows"
	if o.Multiplex != nil {
		h.mux = *o.Multiplex
	}
	if h.timeout <= 0 {
		h.timeout = 15 * time.Second
	}
	h.id = randHex(8)
	return h
}

// Name is the ssh alias.
func (h *SSHHost) Name() string { return h.alias }

func (h *SSHHost) checkAlias() error {
	switch {
	case h.alias == "":
		return errors.New("ssh host is empty")
	case strings.HasPrefix(h.alias, "-"):
		return fmt.Errorf("ssh host %q must not start with -", h.alias)
	case strings.ContainsAny(h.alias, " \t\r\n"):
		return fmt.Errorf("ssh host %q must not contain spaces", h.alias)
	}
	return nil
}

// baseArgs are the options of every ssh invocation.
func (h *SSHHost) baseArgs() []string {
	var a []string
	if h.config != "" {
		a = append(a, "-F", h.config)
	}
	return append(a,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout="+strconv.Itoa(int(h.timeout/time.Second)),
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	)
}

// clientArgs are the options of a command that uses the master connection
// when there is one and connects by itself otherwise.
func (h *SSHHost) clientArgs() []string {
	a := h.baseArgs()
	if h.mux {
		return append(a, "-o", "ControlMaster=no", "-o", "ControlPath="+sshPathOption(h.socket()))
	}
	return append(a, "-o", "ControlMaster=no", "-o", "ControlPath=none")
}

// socket is the control socket of the master connection.
func (h *SSHHost) socket() string {
	dir := h.sockDir
	if dir == "" {
		if c, err := os.UserCacheDir(); err == nil {
			dir = filepath.Join(c, "ymcp", "ssh")
		} else {
			dir = filepath.Join(os.TempDir(), "ymcp-ssh")
		}
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s", h.alias, os.Getpid(), h.id)))
	name := hex.EncodeToString(sum[:8])
	p := filepath.Join(dir, name)
	// Unix sockets have a path limit of 104 bytes on macOS.
	if len(p) > 100 {
		p = filepath.Join(os.TempDir(), "ymcp-"+name)
	}
	return p
}

// sshPathOption quotes a path for an -o option: ssh splits option values
// on spaces and expands % tokens.
func sshPathOption(p string) string {
	p = strings.ReplaceAll(p, "%", "%%")
	if strings.ContainsAny(p, " \t") {
		return `"` + p + `"`
	}
	return p
}

// remote turns argv in dir into the command line ssh hands to the login
// shell of the host: `sh -c '<script>'`, so the script is read by a POSIX
// shell whatever the login shell is.
func remote(script string) string { return "sh -c " + Quote(script) }

// cmd builds an ssh invocation: options, the alias and the remote command.
// ctx ends the process; long-lived ones get context.Background.
func (h *SSHHost) cmd(ctx context.Context, opts []string, command string) *exec.Cmd {
	args := append(opts, "-T", h.alias)
	if command != "" {
		args = append(args, "--", command)
	}
	return exec.CommandContext(ctx, h.program, args...)
}

// Run executes argv in dir on the host. git is told never to prompt and to
// speak the C locale, as on Local.
func (h *SSHHost) Run(ctx context.Context, dir string, argv []string, stdin io.Reader) ([]byte, []byte, error) {
	if len(argv) == 0 {
		return nil, nil, errors.New("empty command")
	}
	if err := h.ensureMaster(ctx); err != nil {
		return nil, nil, err
	}
	script := cdScript(dir) + "exec env LC_ALL=C GIT_TERMINAL_PROMPT=0 " + QuoteArgs(argv)
	cmd := h.cmd(ctx, h.clientArgs(), remote(script))
	cmd.Stdin = stdin
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// Command returns the local ssh process that runs script, a POSIX shell
// script, on the host. Its stdin, stdout and stderr are those of the
// remote script; the caller sets them and starts it. Ending the process
// closes the remote session but does not signal the remote processes: the
// script has to see to that itself.
func (h *SSHHost) Command(ctx context.Context, script string) (*exec.Cmd, error) {
	if err := h.ensureMaster(ctx); err != nil {
		return nil, err
	}
	return h.cmd(context.Background(), h.clientArgs(), remote(script)), nil
}

// IsDir runs `test -d` on the host.
func (h *SSHHost) IsDir(ctx context.Context, path string) (bool, error) {
	_, stderr, err := h.Run(ctx, "", []string{"test", "-d", path}, nil)
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 && ctx.Err() == nil {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, sshError(h.alias, err, stderr)
}

// CacheDir is ${XDG_CACHE_HOME:-$HOME/.cache} on the host.
func (h *SSHHost) CacheDir(ctx context.Context) (string, error) {
	h.mu.Lock()
	c := h.cache
	h.mu.Unlock()
	if c != "" {
		return c, nil
	}
	out, stderr, err := h.Run(ctx, "", []string{"sh", "-c", `printf '%s' "${XDG_CACHE_HOME:-$HOME/.cache}"`}, nil)
	if err != nil {
		return "", sshError(h.alias, err, stderr)
	}
	c = strings.TrimRight(trim(string(out)), "/")
	if !strings.HasPrefix(c, "/") {
		return "", fmt.Errorf("unexpected cache directory %q on %s", c, h.alias)
	}
	h.mu.Lock()
	h.cache = c
	h.mu.Unlock()
	return c, nil
}

// HomeDir is $HOME on the host.
func (h *SSHHost) HomeDir(ctx context.Context) (string, error) {
	out, stderr, err := h.Run(ctx, "", []string{"sh", "-c", `printf '%s' "$HOME"`}, nil)
	if err != nil {
		return "", sshError(h.alias, err, stderr)
	}
	home := strings.TrimRight(trim(string(out)), "/")
	if !strings.HasPrefix(home, "/") {
		return "", fmt.Errorf("unexpected home directory %q on %s", home, h.alias)
	}
	return home, nil
}

// Close ends the master connection and every forward. A closed host can't
// be used again.
func (h *SSHHost) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	m := h.m
	h.m = nil
	fws := make([]*SSHForward, 0, len(h.forwards))
	for f := range h.forwards {
		fws = append(fws, f)
	}
	h.mu.Unlock()
	for _, f := range fws {
		f.stop()
	}
	if m != nil {
		h.exitMaster(m)
	}
	return nil
}

// sshError describes a failed ssh command with what it printed.
func sshError(alias string, err error, stderr []byte) error {
	if msg := trim(string(stderr)); msg != "" {
		return fmt.Errorf("ssh %s: %w: %s", alias, err, shorten(msg, 600))
	}
	return fmt.Errorf("ssh %s: %w", alias, err)
}

// master is a running master connection.
type master struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	gen    int
	done   chan struct{} // closed when the process has exited
	stderr *lockedBuffer
}

func (m *master) alive() bool {
	select {
	case <-m.done:
		return false
	default:
		return true
	}
}

// ensureMaster starts the master connection unless it is running. It
// returns once the control socket accepts clients.
func (h *SSHHost) ensureMaster(ctx context.Context) error {
	if err := h.checkAlias(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return fmt.Errorf("ssh %s: the connection is closed", h.alias)
	}
	if !h.mux || (h.m != nil && h.m.alive()) {
		return nil
	}
	sock := h.socket()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return fmt.Errorf("ssh %s: control socket: %w", h.alias, err)
	}
	// The master runs `cat` on the host: when ymcp dies, its stdin closes,
	// cat ends and so does the master, instead of lingering.
	opts := append(h.baseArgs(), "-o", "ControlMaster=yes", "-o", "ControlPersist=no",
		"-o", "ControlPath="+sshPathOption(sock), "-o", "ClearAllForwardings=yes")
	cmd := h.cmd(context.Background(), opts, "cat")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	m := &master{cmd: cmd, stdin: stdin, done: make(chan struct{}), stderr: &lockedBuffer{}}
	cmd.Stdout = io.Discard
	cmd.Stderr = m.stderr
	cmd.WaitDelay = 2 * time.Second
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ssh %s: %w", h.alias, err)
	}
	go func() {
		_ = cmd.Wait()
		close(m.done)
	}()

	deadline := time.NewTimer(h.timeout + 15*time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		select {
		case <-m.done:
			return sshError(h.alias, errors.New("connection failed"), m.stderr.Bytes())
		case <-ctx.Done():
			h.exitMaster(m)
			return ctx.Err()
		case <-deadline.C:
			h.exitMaster(m)
			return sshError(h.alias, errors.New("timed out connecting"), m.stderr.Bytes())
		case <-tick.C:
		}
	}
	h.gen++
	m.gen = h.gen
	h.m = m
	return nil
}

// exitMaster asks the master to exit and makes sure it does.
func (h *SSHHost) exitMaster(m *master) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if m.alive() {
		c := h.cmd(ctx, append(h.baseArgs(), "-o", "ControlPath="+sshPathOption(h.socket()), "-O", "exit"), "")
		_ = c.Run()
	}
	m.stdin.Close()
	select {
	case <-m.done:
	case <-time.After(3 * time.Second):
		_ = m.cmd.Process.Kill()
		<-m.done
	}
}

// generation is the number of the running master, 0 when there is none.
func (h *SSHHost) generation() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil || !h.m.alive() {
		return 0
	}
	return h.m.gen
}

// SSHForward makes a port on this machine's loopback reachable on the
// host's loopback (ssh -R 0:127.0.0.1:P), for the agent's MCP server.
type SSHForward struct {
	h     *SSHHost
	local int

	mu     sync.Mutex
	port   int
	gen    int      // master generation the forward was made on (mux)
	proc   *process // the dedicated ssh -N process (no mux)
	closed bool
}

// Forward makes 127.0.0.1:local reachable on the host and returns the
// forward; its Port is the port on the host. The host allocates the port.
// With a master connection the forward is added to it (ssh -O forward);
// otherwise a dedicated `ssh -N -R` process holds it.
func (h *SSHHost) Forward(ctx context.Context, local int) (*SSHForward, error) {
	f := &SSHForward{h: h, local: local}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, fmt.Errorf("ssh %s: the connection is closed", h.alias)
	}
	h.forwards[f] = true
	h.mu.Unlock()
	if _, err := f.Port(ctx); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (f *SSHForward) spec() string { return "0:127.0.0.1:" + strconv.Itoa(f.local) }

// Port returns the port on the host, setting the forward up again first if
// it was lost (the master connection or the dedicated process died); the
// port may then change.
func (f *SSHForward) Port(ctx context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, errors.New("the forward is closed")
	}
	h := f.h
	if h.mux {
		if err := h.ensureMaster(ctx); err != nil {
			return 0, err
		}
		gen := h.generation()
		if f.port != 0 && f.gen == gen {
			return f.port, nil
		}
		cctx, cancel := context.WithTimeout(ctx, h.timeout+15*time.Second)
		defer cancel()
		c := h.cmd(cctx, append(h.baseArgs(), "-o", "ControlPath="+sshPathOption(h.socket()), "-O", "forward", "-R", f.spec()), "")
		var stdout, stderr bytes.Buffer
		c.Stdout, c.Stderr = &stdout, &stderr
		if err := c.Run(); err != nil {
			return 0, sshError(h.alias, fmt.Errorf("forwarding port %d: %w", f.local, err), stderr.Bytes())
		}
		port, err := parseForwardPort(stdout.String())
		if err != nil {
			return 0, sshError(h.alias, err, stderr.Bytes())
		}
		f.port, f.gen = port, gen
		return port, nil
	}

	if f.proc != nil && f.proc.alive() {
		return f.port, nil
	}
	if err := h.checkAlias(); err != nil {
		return 0, err
	}
	opts := append(h.baseArgs(), "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ExitOnForwardFailure=yes", "-N", "-R", f.spec())
	c := h.cmd(context.Background(), opts, "")
	// Never -q or LogLevel=ERROR: the port is in an info message.
	stderr, err := c.StderrPipe()
	if err != nil {
		return 0, err
	}
	detach(c)
	if err := c.Start(); err != nil {
		return 0, fmt.Errorf("ssh %s: %w", h.alias, err)
	}
	p := &process{cmd: c, done: make(chan struct{}), out: &lockedBuffer{}}
	found := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		reported := false
		for sc.Scan() {
			line := sc.Text()
			if !reported {
				if port, ok := parseAllocated(line, f.local); ok {
					reported = true
					found <- port
					continue
				}
			}
			p.out.Write([]byte(line + "\n"))
		}
		_ = c.Wait()
		close(p.done)
	}()
	timer := time.NewTimer(h.timeout + 15*time.Second)
	defer timer.Stop()
	select {
	case port := <-found:
		f.proc, f.port = p, port
		return port, nil
	case <-p.done:
		return 0, sshError(h.alias, fmt.Errorf("forwarding port %d failed", f.local), p.out.Bytes())
	case <-ctx.Done():
		p.kill()
		return 0, ctx.Err()
	case <-timer.C:
		p.kill()
		return 0, sshError(h.alias, fmt.Errorf("forwarding port %d: no port was allocated", f.local), p.out.Bytes())
	}
}

// Close removes the forward.
func (f *SSHForward) Close() error {
	f.h.mu.Lock()
	delete(f.h.forwards, f)
	f.h.mu.Unlock()
	f.stop()
	return nil
}

func (f *SSHForward) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	if f.proc != nil {
		f.proc.kill()
		return
	}
	h := f.h
	if h.mux && f.port != 0 && f.gen == h.generation() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.cmd(ctx, append(h.baseArgs(), "-o", "ControlPath="+sshPathOption(h.socket()), "-O", "cancel", "-R", f.spec()), "").Run()
	}
}

// parseForwardPort reads the port `ssh -O forward -R 0:…` prints.
func parseForwardPort(out string) (int, error) {
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		port, err := strconv.Atoi(line)
		if err != nil || port <= 0 || port > 65535 {
			return 0, fmt.Errorf("unexpected answer of ssh -O forward: %q", shorten(line, 80))
		}
		return port, nil
	}
	return 0, errors.New("ssh -O forward printed no port: the server did not allocate one")
}

var allocatedRE = regexp.MustCompile(`Allocated port (\d+) for remote forward to ([^\s:]+):(\d+)`)

// parseAllocated reads the port from ssh's "Allocated port N for remote
// forward to 127.0.0.1:P" message, for the forward of local port P.
func parseAllocated(line string, local int) (int, bool) {
	m := allocatedRE.FindStringSubmatch(line)
	if m == nil || m[3] != strconv.Itoa(local) {
		return 0, false
	}
	port, err := strconv.Atoi(m[1])
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

// process is a long-running ssh started by the host.
type process struct {
	cmd  *exec.Cmd
	done chan struct{}
	out  *lockedBuffer
}

func (p *process) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *process) kill() {
	if p.alive() {
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
}

// lockedBuffer is a bytes.Buffer safe for concurrent writes and reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() > 64<<10 {
		b.buf.Reset()
	}
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}
