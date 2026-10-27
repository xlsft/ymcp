package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/engine"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

// daemonEnv is set to 1 in the environment of a detached runner.
const daemonEnv = "YMCP_PIPELINE_DAEMON"

// Timings of starting and stopping a detached runner.
const (
	// startupWait is how long `run -d` waits for the runner to take its
	// lock. Signing in to YouTrack happens before that, so a slow VPN can
	// use all of it; the runner is then reported as still starting.
	startupWait = 15 * time.Second
	// stopWait is how long `stop` waits for the runner to let go.
	stopWait = 60 * time.Second
	// killWait is how long `stop --force` waits for the process to die.
	killWait = 10 * time.Second
	// daemonPoll is how often both look at the runner lock.
	daemonPoll = 100 * time.Millisecond
)

// pipelineLogPath is the log of the runner of a pipeline: pipeline-NAME.log
// next to the shared log file.
func pipelineLogPath(name string) (string, error) {
	shared := os.Getenv("YMCP_LOG_FILE")
	if shared == "" || shared == "off" {
		var err error
		if shared, err = defaultLogPath(); err != nil {
			return "", err
		}
	}
	return filepath.Join(filepath.Dir(shared), "pipeline-"+name+".log"), nil
}

// prepareRunnerLog points the log of a foreground `pipeline NAME run` at the
// log of the pipeline and reports whether the log file is to be written by
// the logger. A detached runner's stderr already is that file, so it logs
// to stderr only.
func prepareRunnerLog(cmd string, rest []string) (withFile bool) {
	if cmd != "pipeline" || !pipelineForeground(rest) || spec.CheckName(rest[0]) != nil {
		return true
	}
	if os.Getenv(daemonEnv) == "1" {
		return false
	}
	if path, err := pipelineLogPath(rest[0]); err == nil {
		logFileOverride = path
	}
	return true
}

// detachRequested reports whether the arguments of `run` ask for -d.
func detachRequested(args []string) bool {
	for _, a := range args {
		a = strings.TrimLeft(a, "-")
		a, val, _ := strings.Cut(a, "=")
		if (a == "d" || a == "detach") && val != "false" && val != "0" {
			return true
		}
	}
	return false
}

// pipelineRunCmd is `ymcp pipeline NAME run [-d]`.
func pipelineRunCmd(ctx context.Context, name string, args []string, logger *slog.Logger) error {
	const usage = "ymcp pipeline NAME run [-d]"
	fs := pipelineFlags("pipeline run")
	detach := fs.Bool("d", false, "run in the background")
	fs.BoolVar(detach, "detach", false, "run in the background")
	pos, err := parseInterspersed(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected argument %q; usage: %s", pos[0], usage)
	}
	if !*detach {
		return pipelineRun(ctx, name, nil, logger)
	}
	verbose := logger != nil && logger.Enabled(ctx, slog.LevelDebug)
	return pipelineRunDetached(ctx, name, verbose)
}

// daemonCommand is the command that starts a detached runner; a test seam.
var daemonCommand = func(name string, verbose bool) (exe string, args, env []string, err error) {
	exe, err = os.Executable()
	if err != nil {
		return "", nil, nil, err
	}
	if verbose {
		args = append(args, "-v")
	}
	return exe, append(args, "pipeline", name, "run"), []string{daemonEnv + "=1"}, nil
}

// pipelineRunDetached starts the runner of a pipeline as a background
// process and waits until it holds the runner lock.
func pipelineRunDetached(ctx context.Context, name string, verbose bool) error {
	p, err := spec.Load(name)
	if err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	line, live, err := runnerLine(ctx, st, name)
	if err != nil {
		return err
	}
	if live {
		return fmt.Errorf("pipeline %q is already %s; stop it with `ymcp pipeline %s stop`", name, line, name)
	}

	logPath, err := pipelineLogPath(name)
	if err != nil {
		return err
	}
	logFile, err := openLogFile(logPath)
	if err != nil {
		return fmt.Errorf("opening the runner log: %w", err)
	}
	defer logFile.Close()
	info, err := logFile.Stat()
	if err != nil {
		return err
	}
	logStart := info.Size() // what the runner writes starts here

	exe, args, env, err := daemonCommand(name, verbose)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logFile, logFile
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the runner: %w", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.NewTimer(startupWait)
	defer deadline.Stop()
	poll := time.NewTicker(daemonPoll)
	defer poll.Stop()
	for {
		select {
		case err := <-exited:
			return startupError(logPath, logStart, err)
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			fmt.Println(styleWarn.Render("!") + fmt.Sprintf(" The runner of %s is still starting (pid %d)", styleBold.Render(name), pid))
			printRunnerHints(name, logPath)
			return nil
		case <-poll.C:
			holder, _, _, ok, err := st.Runner(ctx, name)
			if err == nil && ok && holder == pid {
				fmt.Println(styleOK.Render("✓") + fmt.Sprintf(" Started the runner of %s (pid %d)", styleBold.Render(name), pid))
				printRunnerHints(name, logPath)
				return nil
			}
		}
	}
}

func printRunnerHints(name, logPath string) {
	fmt.Println("  log:  " + logPath)
	fmt.Println(styleDim.Render("  follow it with `ymcp pipeline " + name + " logs -f`, stop it with `ymcp pipeline " + name + " stop`"))
}

// startupError describes a runner that died before it took its lock, from
// what it wrote to the log after offset.
func startupError(logPath string, offset int64, waitErr error) error {
	reason := "it exited"
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		reason = exitErr.ProcessState.String()
	}
	if lines := logSince(logPath, offset); len(lines) > 0 {
		reason = lines[len(lines)-1]
		for i := len(lines) - 1; i >= 0; i-- {
			if msg, ok := strings.CutPrefix(lines[i], "ymcp: "); ok {
				reason = msg // main prints the error it fails with like this
				break
			}
		}
	}
	return fmt.Errorf("the runner failed to start: %s (log: %s)", reason, logPath)
}

// logSince returns the non-empty lines the file got after offset.
func logSince(path string, offset int64) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	data, _ := io.ReadAll(f)
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// pipelineStop is `ymcp pipeline NAME stop [--force]`.
func pipelineStop(ctx context.Context, name string, args []string) error {
	const usage = "ymcp pipeline NAME stop [--force] [--timeout 1m]"
	fs := pipelineFlags("pipeline stop")
	force := fs.Bool("force", false, "kill the runner instead of asking it to stop")
	timeout := fs.Duration("timeout", stopWait, "how long to wait for the runner to stop")
	pos, err := parseInterspersed(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected argument %q; usage: %s", pos[0], usage)
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	pid, host, _, ok, err := st.Runner(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		if err := knownPipeline(name, nil); err != nil {
			return err
		}
		fmt.Println("No runner is running for pipeline " + name + ".")
		return nil
	}
	here, _ := os.Hostname()
	local := host == here
	if local && !processAlive(pid) {
		if err := st.ReleaseRunner(ctx, name, pid); err != nil {
			return err
		}
		fmt.Printf("The runner of %s (pid %d) was gone already; removed its stale lock.\n", name, pid)
		return nil
	}
	if !local && *force {
		return fmt.Errorf("the runner of %s runs on %s (pid %d); --force can only kill a process on this machine", name, host, pid)
	}

	wait := *timeout
	if *force {
		if err := killProcess(pid); err != nil && processAlive(pid) {
			return fmt.Errorf("killing pid %d: %w", pid, err)
		}
		wait = killWait
	} else {
		if err := engine.RequestStop(ctx, st, name); err != nil {
			return err
		}
		if local {
			_ = terminateProcess(pid) // quicker than the request; Windows has no such signal
		}
	}

	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(daemonPoll)
	defer poll.Stop()
	for {
		holder, _, _, held, err := st.Runner(ctx, name)
		switch {
		case err != nil:
			return err
		case !held || holder != pid:
		case local && !processAlive(pid):
			if err := st.ReleaseRunner(ctx, name, pid); err != nil {
				return err
			}
		default:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				if *force {
					return fmt.Errorf("pid %d is still there %s after it was killed", pid, wait)
				}
				return fmt.Errorf("the runner of %s (pid %d) is still running after %s; it may be finishing an agent turn. Wait, or use `ymcp pipeline %s stop --force`", name, pid, wait, name)
			case <-poll.C:
			}
			continue
		}
		break
	}
	_ = engine.ClearStop(ctx, st, name)
	verb := "Stopped"
	if *force {
		verb = "Killed"
	}
	fmt.Println(styleOK.Render("✓") + fmt.Sprintf(" %s the runner of %s (pid %d)", verb, styleBold.Render(name), pid))
	return nil
}

// pipelineRunnerLog prints the log of the runner of a pipeline.
func pipelineRunnerLog(ctx context.Context, name string, lines int, follow bool) error {
	path, err := pipelineLogPath(name)
	if err != nil {
		return err
	}
	if lines == 0 {
		lines = 1 << 30
	}
	return tailLog(ctx, path, lines, follow, fmt.Sprintf("no runner log yet at %s; it appears once `ymcp pipeline %s run` starts", path, name))
}
