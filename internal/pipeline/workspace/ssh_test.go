package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/workspace/sshtest"
)

func TestMain(m *testing.M) {
	sshtest.Main()
	os.Exit(m.Run())
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":          "''",
		"plain":     "plain",
		"/a/b-c.d":  "/a/b-c.d",
		"a b":       "'a b'",
		"it's":      `'it'\''s'`,
		"$HOME":     "'$HOME'",
		"a=b":       "'a=b'",
		"~/x":       "'~/x'",
		"новый":     "'новый'",
		"--flag=1":  "'--flag=1'",
		"user@host": "user@host",
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestQuoteArgsRoundTrip has sh read quoted words back.
func TestQuoteArgsRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	argv := []string{"", "a b", "it's", `"double"`, "$HOME", "`id`", `back\slash`, "new\nline", "*", "-n", "a=b", "tab\there", "!bang", "новый", ";|&<>()"}
	script := `for a in ` + QuoteArgs(argv) + `; do printf '[%s]' "$a"; done`
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, a := range argv {
		want.WriteString("[" + a + "]")
	}
	if string(out) != want.String() {
		t.Errorf("round trip:\n got %q\nwant %q", out, want.String())
	}
}

func TestParseForwardPort(t *testing.T) {
	for _, tc := range []struct {
		out  string
		port int
		ok   bool
	}{
		{"45829\n", 45829, true},
		{"\n 2222 \r\n", 2222, true},
		{"", 0, false},
		{"Allocated port 1\n", 0, false},
		{"70000\n", 0, false},
	} {
		port, err := parseForwardPort(tc.out)
		if (err == nil) != tc.ok || port != tc.port {
			t.Errorf("parseForwardPort(%q) = %d, %v", tc.out, port, err)
		}
	}
}

func TestParseAllocated(t *testing.T) {
	for _, tc := range []struct {
		line  string
		local int
		port  int
		ok    bool
	}{
		{"Allocated port 37759 for remote forward to 127.0.0.1:9999", 9999, 37759, true},
		// OpenSSH for Windows ends lines with CRLF.
		{"Allocated port 50123 for remote forward to 127.0.0.1:8080\r", 8080, 50123, true},
		{"Allocated port 50123 for remote forward to 127.0.0.1:8081", 8080, 0, false},
		{"Warning: Permanently added 'h' (ED25519) to the list of known hosts.", 8080, 0, false},
		{"Allocated port 0 for remote forward to 127.0.0.1:8080", 8080, 0, false},
	} {
		port, ok := parseAllocated(tc.line, tc.local)
		if ok != tc.ok || port != tc.port {
			t.Errorf("parseAllocated(%q, %d) = %d, %v", tc.line, tc.local, port, ok)
		}
	}
}

// fakeSSH makes an SSHHost that runs the fake ssh, and returns it with the
// log of the fake.
func fakeSSH(t *testing.T, mux bool) (*SSHHost, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh needs a POSIX sh")
	}
	log := filepath.Join(t.TempDir(), "ssh.jsonl")
	program, env := sshtest.Command(log)
	for k, v := range env {
		t.Setenv(k, v)
	}
	// Unix socket paths are short; t.TempDir may be too long on macOS.
	sockDir, err := os.MkdirTemp("", "ys")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	h := NewSSH("devbox", SSHOptions{Program: program, ConfigFile: "/dev/null", Multiplex: &mux, SocketDir: sockDir, ConnectTimeout: 5 * time.Second})
	t.Cleanup(func() { h.Close() })
	return h, log
}

func calls(t *testing.T, log string) []sshtest.Call {
	t.Helper()
	cs, err := sshtest.Calls(log)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestSSHRunMultiplexed(t *testing.T) {
	h, log := fakeSSH(t, true)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "it's a dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out, _, err := h.Run(ctx, dir, []string{"sh", "-c", `printf '%s|%s|%s|%s' "$(pwd -P)" "$1" "$LC_ALL" "$GIT_TERMINAL_PROMPT"`, "sh", "a 'b' $c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := realPath(t, dir) + "|a 'b' $c|C|0"; string(out) != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	out, _, err = h.Run(ctx, "", []string{"cat"}, strings.NewReader("from stdin"))
	if err != nil || string(out) != "from stdin" {
		t.Errorf("stdin: %q, %v", out, err)
	}
	_, stderr, err := h.Run(ctx, "", []string{"sh", "-c", "echo oops >&2; exit 3"}, nil)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 || strings.TrimSpace(string(stderr)) != "oops" {
		t.Errorf("exit: %v, stderr %q", err, stderr)
	}
	if _, _, err := h.Run(ctx, filepath.Join(dir, "missing"), []string{"true"}, nil); err == nil {
		t.Error("Run in a missing directory succeeded")
	}
	t.Setenv("HOME", filepath.Dir(dir))
	if out, _, err := h.Run(ctx, "~/"+filepath.Base(dir), []string{"pwd", "-P"}, nil); err != nil || strings.TrimSpace(string(out)) != realPath(t, dir) {
		t.Errorf("Run in ~/: %q, %v", out, err)
	}

	cs := calls(t, log)
	if len(cs) != 6 {
		t.Fatalf("%d ssh calls, want a master and 5 commands: %+v", len(cs), cs)
	}
	m := cs[0]
	if m.Options["ControlMaster"] != "yes" || m.Options["ControlPersist"] != "no" || m.Options["ClearAllForwardings"] != "yes" || m.Command != "cat" {
		t.Errorf("master call = %+v", m)
	}
	sock := m.Options["ControlPath"]
	for _, c := range cs[1:] {
		if c.Options["ControlMaster"] != "no" || c.Options["ControlPath"] != sock {
			t.Errorf("command does not use the master: %+v", c)
		}
		if c.Alias != "devbox" || c.Options["BatchMode"] != "yes" || c.Options["ConnectTimeout"] != "5" || c.Options["ServerAliveInterval"] == "" {
			t.Errorf("command options = %+v", c)
		}
		if !strings.HasPrefix(c.Command, "sh -c ") {
			t.Errorf("command = %q", c.Command)
		}
		if i := indexOf(c.Args, "devbox"); i < 1 || c.Args[i-1] != "-T" || c.Args[i+1] != "--" || c.Args[0] != "-F" {
			t.Errorf("argv = %q", c.Args)
		}
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("control socket still there after Close: %v", err)
	}
	if _, _, err := h.Run(ctx, "", []string{"true"}, nil); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("Run after Close: %v", err)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func TestSSHRestartsMaster(t *testing.T) {
	h, log := fakeSSH(t, true)
	ctx := context.Background()
	if _, _, err := h.Run(ctx, "", []string{"true"}, nil); err != nil {
		t.Fatal(err)
	}
	sock := calls(t, log)[0].Options["ControlPath"]
	gen := h.generation()
	os.Remove(sock) // the fake master exits
	waitFor(t, func() bool { return h.generation() == 0 })
	if _, _, err := h.Run(ctx, "", []string{"true"}, nil); err != nil {
		t.Fatal(err)
	}
	if g := h.generation(); g == gen || g == 0 {
		t.Errorf("generation %d after a restart, was %d", g, gen)
	}
	masters := 0
	for _, c := range calls(t, log) {
		if c.Options["ControlMaster"] == "yes" {
			masters++
		}
	}
	if masters != 2 {
		t.Errorf("%d masters started, want 2", masters)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSSHWithoutMultiplexing(t *testing.T) {
	h, log := fakeSSH(t, false)
	ctx := context.Background()
	out, _, err := h.Run(ctx, "", []string{"echo", "hi"}, nil)
	if err != nil || string(out) != "hi\n" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	cs := calls(t, log)
	if len(cs) != 1 || cs[0].Options["ControlMaster"] != "no" || cs[0].Options["ControlPath"] != "none" {
		t.Errorf("calls = %+v", cs)
	}
}

func TestSSHConnectionFails(t *testing.T) {
	for _, mux := range []bool{true, false} {
		h, _ := fakeSSH(t, mux)
		t.Setenv(sshtest.EnvFail, "1")
		_, err := h.CacheDir(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Permission denied") || !strings.Contains(err.Error(), "devbox") {
			t.Errorf("mux=%v: CacheDir error = %v", mux, err)
		}
	}
}

func TestSSHAliasChecked(t *testing.T) {
	for _, alias := range []string{"", "-oProxyCommand=touch /tmp/x", "a b"} {
		h := NewSSH(alias, SSHOptions{Program: "/nonexistent/ssh"})
		if _, _, err := h.Run(context.Background(), "", []string{"true"}, nil); err == nil || !strings.Contains(err.Error(), "ssh host") {
			t.Errorf("alias %q: %v", alias, err)
		}
	}
}

func TestSSHIsDirAndCacheDir(t *testing.T) {
	h, _ := fakeSSH(t, true)
	ctx := context.Background()
	dir := t.TempDir()
	if ok, err := h.IsDir(ctx, dir); !ok || err != nil {
		t.Errorf("IsDir(dir) = %v, %v", ok, err)
	}
	if ok, err := h.IsDir(ctx, filepath.Join(dir, "nope")); ok || err != nil {
		t.Errorf("IsDir(missing) = %v, %v", ok, err)
	}
	t.Setenv("XDG_CACHE_HOME", "/srv/cache/")
	if c, err := h.CacheDir(ctx); c != "/srv/cache" || err != nil {
		t.Errorf("CacheDir = %q, %v", c, err)
	}
	h2, _ := fakeSSH(t, true)
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "/home/dev")
	if c, err := h2.CacheDir(ctx); c != "/home/dev/.cache" || err != nil {
		t.Errorf("CacheDir without XDG_CACHE_HOME = %q, %v", c, err)
	}
	root, err := DefaultRoot(ctx, h2, "backend")
	if root != "/home/dev/.cache/ymcp/worktrees/backend" || err != nil {
		t.Errorf("DefaultRoot = %q, %v", root, err)
	}
	for in, want := range map[string]string{"~": "/home/dev", "~/": "/home/dev", "~/wts": "/home/dev/wts", "~/wts/": "/home/dev/wts", "/srv/wts": "/srv/wts", "~dev/wts": "~dev/wts"} {
		if got, err := ExpandRoot(ctx, h2, in); got != want || err != nil {
			t.Errorf("ExpandRoot(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestSSHForwardMultiplexed(t *testing.T) {
	h, log := fakeSSH(t, true)
	ctx := context.Background()
	f, err := h.Forward(ctx, 4321)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := f.Port(ctx); p != 4321 || err != nil {
		t.Fatalf("Port = %d, %v", p, err)
	}
	var fw []sshtest.Call
	for _, c := range calls(t, log) {
		if c.Control == "forward" {
			fw = append(fw, c)
		}
	}
	if len(fw) != 1 || fw[0].Forward != "0:127.0.0.1:4321" {
		t.Fatalf("forward calls = %+v", fw)
	}

	// The master dies: Port forwards again on a new one.
	os.Remove(calls(t, log)[0].Options["ControlPath"])
	waitFor(t, func() bool { return h.generation() == 0 })
	t.Setenv(sshtest.EnvPort, "5555")
	if p, err := f.Port(ctx); p != 5555 || err != nil {
		t.Fatalf("Port after the master died = %d, %v", p, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	var cancels int
	for _, c := range calls(t, log) {
		if c.Control == "cancel" && c.Forward == "0:127.0.0.1:4321" {
			cancels++
		}
	}
	if cancels != 1 {
		t.Errorf("%d cancel calls", cancels)
	}
	if _, err := f.Port(ctx); err == nil {
		t.Error("Port after Close succeeded")
	}
}

func TestSSHForwardDedicated(t *testing.T) {
	h, log := fakeSSH(t, false)
	ctx := context.Background()
	t.Setenv(sshtest.EnvPort, "40001")
	f, err := h.Forward(ctx, 8080)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := f.Port(ctx); p != 40001 {
		t.Errorf("Port = %d", p)
	}
	cs := calls(t, log)
	if len(cs) != 1 || !cs[0].N || cs[0].Forward != "0:127.0.0.1:8080" || cs[0].Options["ExitOnForwardFailure"] != "yes" || cs[0].Options["ControlPath"] != "none" {
		t.Fatalf("calls = %+v", cs)
	}
	for _, a := range cs[0].Args {
		if a == "-q" || strings.HasPrefix(a, "LogLevel") {
			t.Errorf("forward runs with %s, which hides the allocated port", a)
		}
	}
	proc := f.proc
	if err := h.Close(); err != nil { // closes the forward too
		t.Fatal(err)
	}
	if proc.alive() {
		t.Error("the forward process survived Close")
	}
}

// TestSSHWorkspace runs the git side of a run through the ssh host.
func TestSSHWorkspace(t *testing.T) {
	f := newFixture(t)
	h, _ := fakeSSH(t, true)
	ctx := context.Background()
	checks, err := Inspect(ctx, h, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if !c.OK {
			t.Errorf("check %s failed: %s", c.Name, c.Detail)
		}
	}
	root := filepath.Join(t.TempDir(), "work trees")
	wt, err := Create(ctx, h, f.repo, root, "BE-1-fix", "", AgentIdentity("dev@xlsft.ru", "Claude · backend #3", "agent_backend_3"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.Path, "new file.txt"), "x\n")
	if ok, err := CommitLeftovers(ctx, h, wt, "BE-1 it's done"); !ok || err != nil {
		t.Fatalf("CommitLeftovers = %v, %v", ok, err)
	}
	if err := Push(ctx, h, wt); err != nil {
		t.Fatal(err)
	}
	if got := git(t, f.repo, "--git-dir", f.origin, "log", "-1", "--format=%s|%ae", "BE-1-fix"); got != "BE-1 it's done|dev+agent_backend_3@xlsft.ru" {
		t.Errorf("pushed commit = %q", got)
	}
	if _, err := RunDir(ctx, h, f.repo, 3); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, h, f.repo, wt); err != nil {
		t.Fatal(err)
	}
}
