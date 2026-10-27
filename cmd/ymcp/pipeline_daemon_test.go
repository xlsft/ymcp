package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/proc"
)

// Modes of the fake runner that TestDaemonHelper plays.
const (
	helperModeEnv = "YMCP_TEST_DAEMON_MODE"
	helperNameEnv = "YMCP_TEST_DAEMON_NAME"

	modeGraceful   = "graceful" // stops on SIGTERM and on the stop request
	modeRequest    = "request"  // ignores SIGTERM, like a Windows runner
	modeStubborn   = "stubborn" // only dies when killed
	modeFailsEarly = "fails"    // exits before it takes the lock
)

// TestDaemonHelper is not a test: the daemon tests start the test binary
// with it as a stand-in for `ymcp pipeline NAME run`.
func TestDaemonHelper(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		t.Skip("only runs as a child of the daemon tests")
	}
	go func() {
		time.Sleep(2 * time.Minute) // never outlive a broken test
		os.Exit(3)
	}()
	if mode == modeFailsEarly {
		fmt.Fprintln(os.Stderr, "ymcp: YouTrack profile \"nope\" is not configured")
		os.Exit(1)
	}
	name := os.Getenv(helperNameEnv)
	var term chan os.Signal // nil blocks: the runner takes no SIGTERM
	if mode == modeGraceful {
		term = make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
	} else {
		signal.Ignore(syscall.SIGTERM)
	}

	st, err := openStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ymcp:", err)
		os.Exit(1)
	}
	ctx := context.Background()
	host, _ := os.Hostname()
	started := time.Now()
	if err := st.AcquireRunner(ctx, name, os.Getpid(), host, 30*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "ymcp:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "helper runner started")
	for {
		select {
		case <-term:
		case <-time.After(20 * time.Millisecond):
			_ = st.Heartbeat(ctx, name, os.Getpid())
			if mode == modeStubborn {
				continue
			}
			if ok, _ := st.StopRequested(ctx, name, started); !ok {
				continue
			}
			_ = st.ClearStop(ctx, name)
		}
		_ = st.ReleaseRunner(ctx, name, os.Getpid())
		fmt.Fprintln(os.Stderr, "helper runner stopped")
		os.Exit(0)
	}
}

// useHelperRunner makes `run -d` start the fake runner in the given mode.
func useHelperRunner(t *testing.T, mode string) {
	t.Helper()
	old := daemonCommand
	daemonCommand = func(name string, _ bool) (string, []string, []string, error) {
		return os.Args[0], []string{"-test.run=^TestDaemonHelper$"}, []string{helperModeEnv + "=" + mode, helperNameEnv + "=" + name}, nil
	}
	t.Cleanup(func() { daemonCommand = old })
}

// runnerPID is the pid that holds the runner lock, or 0.
func runnerPID(t *testing.T, st *store.Store, name string) int {
	t.Helper()
	pid, _, _, ok, err := st.Runner(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return 0
	}
	return pid
}

// killLeftover keeps a failed test from leaving the fake runner behind.
func killLeftover(t *testing.T, st *store.Store, name string) {
	t.Cleanup(func() {
		if pid := runnerPID(t, st, name); pid != 0 && proc.Alive(pid) {
			_ = killProcess(pid)
		}
	})
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for proc.Alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d is still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDaemonRoundTrip(t *testing.T) {
	for _, mode := range []string{modeGraceful, modeRequest} {
		t.Run(mode, func(t *testing.T) {
			isolatePipelines(t)
			newPipeline(t, "api")
			useHelperRunner(t, mode)
			st := openTestStore(t)
			killLeftover(t, st, "api")

			out := mustPipeline(t, "api", "run", "-d")
			pid := runnerPID(t, st, "api")
			if pid == 0 || pid == os.Getpid() || !proc.Alive(pid) {
				t.Fatalf("runner pid = %d\n%s", pid, out)
			}
			logPath, _ := pipelineLogPath("api")
			if !strings.Contains(out, "(pid "+strconv.Itoa(pid)+")") || !strings.Contains(out, logPath) {
				t.Errorf("run -d output has no pid or log path:\n%s", out)
			}
			if filepath.Base(logPath) != "pipeline-api.log" {
				t.Errorf("log path = %s", logPath)
			}
			if out := mustPipeline(t, "api", "status"); !strings.Contains(out, "running (pid "+strconv.Itoa(pid)+" on ") {
				t.Errorf("status:\n%s", out)
			}

			// A live runner holds the pipeline.
			if _, err := pipelineOut(t, "api", "run", "-d"); err == nil || !strings.Contains(err.Error(), "already") {
				t.Errorf("second run -d: err = %v", err)
			}

			out = mustPipeline(t, "api", "stop")
			if !strings.Contains(out, "Stopped the runner of api") {
				t.Errorf("stop output:\n%s", out)
			}
			if got := runnerPID(t, st, "api"); got != 0 {
				t.Errorf("the lock is still held by %d", got)
			}
			waitGone(t, pid)
			if ok, _ := st.StopRequested(context.Background(), "api", time.Time{}); ok {
				t.Error("the stop request was left behind")
			}
			if out := mustPipeline(t, "api", "stop"); !strings.Contains(out, "No runner is running") {
				t.Errorf("stopping again:\n%s", out)
			}

			// What the runner wrote is in its log, and `logs` shows it.
			out = mustPipeline(t, "api", "logs")
			if !strings.Contains(out, "helper runner started") || !strings.Contains(out, "helper runner stopped") {
				t.Errorf("logs:\n%s", out)
			}
			// It can be started again.
			mustPipeline(t, "api", "run", "-d")
			if runnerPID(t, st, "api") == 0 {
				t.Error("the runner did not start again")
			}
			mustPipeline(t, "api", "stop")
		})
	}
}

func TestDaemonStopForce(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "api")
	useHelperRunner(t, modeStubborn)
	st := openTestStore(t)
	killLeftover(t, st, "api")

	mustPipeline(t, "api", "run", "-d")
	pid := runnerPID(t, st, "api")
	if pid == 0 {
		t.Fatal("the runner did not start")
	}
	_, err := pipelineOut(t, "api", "stop", "--timeout", "300ms")
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("stop of a stubborn runner: err = %v", err)
	}
	if !proc.Alive(pid) {
		t.Fatal("a plain stop killed the runner")
	}
	out := mustPipeline(t, "api", "stop", "--force")
	if !strings.Contains(out, "Killed the runner of api") {
		t.Errorf("stop --force output:\n%s", out)
	}
	waitGone(t, pid)
	if got := runnerPID(t, st, "api"); got != 0 {
		t.Errorf("the lock is still held by %d", got)
	}
}

func TestDaemonStartupFailure(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "api")
	useHelperRunner(t, modeFailsEarly)
	st := openTestStore(t)

	_, err := pipelineOut(t, "api", "run", "-d")
	logPath, _ := pipelineLogPath("api")
	if err == nil || !strings.Contains(err.Error(), `profile "nope" is not configured`) || !strings.Contains(err.Error(), logPath) {
		t.Errorf("err = %v", err)
	}
	if runnerPID(t, st, "api") != 0 {
		t.Error("a runner holds the lock")
	}
	// Only what the new runner wrote is reported, not older log lines.
	if err := os.WriteFile(logPath, []byte("ymcp: an old failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pipelineOut(t, "api", "run", "-d"); err == nil || strings.Contains(err.Error(), "old failure") {
		t.Errorf("err = %v", err)
	}
}

func TestDaemonStopStaleLock(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "api")
	st := openTestStore(t)
	ctx := context.Background()

	if out := mustPipeline(t, "api", "stop"); !strings.Contains(out, "No runner is running") {
		t.Errorf("stop without a runner:\n%s", out)
	}
	if _, err := pipelineOut(t, "nope", "stop"); err == nil {
		t.Error("stop of an unknown pipeline worked")
	}

	// A runner that died without releasing its lock.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	dead := cmd.Process.Pid
	host, _ := os.Hostname()
	if err := st.AcquireRunner(ctx, "api", dead, host, time.Minute); err != nil {
		t.Fatal(err)
	}
	out := mustPipeline(t, "api", "stop")
	if !strings.Contains(out, "stale lock") || runnerPID(t, st, "api") != 0 {
		t.Errorf("stop of a dead runner (%d):\n%s", dead, out)
	}
}

func TestDaemonStopRemoteRunner(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "api")
	st := openTestStore(t)
	if err := st.AcquireRunner(context.Background(), "api", 1, "some-other-host", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := pipelineOut(t, "api", "stop", "--force"); err == nil || !strings.Contains(err.Error(), "some-other-host") {
		t.Errorf("force-stopping a remote runner: err = %v", err)
	}
	// A plain stop leaves the request; it is not honored here, so it times out.
	if _, err := pipelineOut(t, "api", "stop", "--timeout", "200ms"); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("stopping a remote runner: err = %v", err)
	}
	if ok, _ := st.StopRequested(context.Background(), "api", time.Time{}); !ok {
		t.Error("the stop request was not left for the remote runner")
	}
}

func TestDetachRequestedAndForeground(t *testing.T) {
	for _, c := range []struct {
		args   []string
		detach bool
	}{
		{nil, false},
		{[]string{"-d"}, true},
		{[]string{"--detach"}, true},
		{[]string{"-d=true"}, true},
		{[]string{"--detach=false"}, false},
		{[]string{"-x"}, false},
	} {
		if got := detachRequested(c.args); got != c.detach {
			t.Errorf("detachRequested(%v) = %v", c.args, got)
		}
	}
	if !pipelineForeground([]string{"api", "run"}) || pipelineForeground([]string{"api", "run", "-d"}) || pipelineForeground([]string{"api", "stop"}) {
		t.Error("pipelineForeground is wrong")
	}
}

func TestPrepareRunnerLog(t *testing.T) {
	dir := isolate(t)
	t.Cleanup(func() { logFileOverride = "" })
	t.Setenv(daemonEnv, "")

	if !prepareRunnerLog("serve", nil) || logFileOverride != "" {
		t.Error("serve changed its log")
	}
	if !prepareRunnerLog("pipeline", []string{"api", "run"}) {
		t.Error("a foreground runner does not log to a file")
	}
	want := filepath.Join(dir, "pipeline-api.log")
	if got, _ := logFilePath(); got != want {
		t.Errorf("log file = %s, want %s", got, want)
	}
	// With the file disabled, it stays disabled.
	t.Setenv("YMCP_LOG_FILE", "off")
	if got, _ := logFilePath(); got != "off" {
		t.Errorf("log file with YMCP_LOG_FILE=off = %s", got)
	}
	// A detached runner logs through its stderr, which is the file.
	logFileOverride = ""
	t.Setenv(daemonEnv, "1")
	if prepareRunnerLog("pipeline", []string{"api", "run"}) {
		t.Error("a detached runner writes its log file twice")
	}
}

func TestPipelineRunnerLogMissing(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "api")
	if _, err := pipelineOut(t, "api", "logs"); err == nil || !strings.Contains(err.Error(), "no runner log yet") {
		t.Errorf("err = %v", err)
	}
	logPath, _ := pipelineLogPath("api")
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(logPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	out := mustPipeline(t, "api", "logs", "-n", "3")
	if out != "line 28\nline 29\nline 30\n" {
		t.Errorf("logs -n 3:\n%s", out)
	}
	if out := mustPipeline(t, "api", "logs", "-n", "0"); !strings.HasPrefix(out, "line 1\n") {
		t.Errorf("logs -n 0:\n%s", out)
	}
	if _, err := pipelineOut(t, "api", "logs", "1", "2"); err == nil {
		t.Error("two run numbers worked")
	}
}
