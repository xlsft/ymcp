package tui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/termimg"
	"github.com/xlsft/ymcp/internal/pipeline/tui/views"
)

// Defaults are the real services of the screens on top of the store:
// runners started and stopped through this binary's CLI, the backlog and
// the run card's issue, comments and fields from YouTrack (one client per
// profile), pull request threads from the code host and links opened in
// the system browser.
func Defaults(st *store.Store) views.Deps {
	yt := &YouTrack{}
	return views.Deps{
		Store:     st,
		Daemon:    CLIDaemon{},
		RunnerLog: CLIDaemon{}.Log,
		Backlog:   yt,
		Issues:    yt,
		People:    yt,
		Fields:    yt,
		Pictures:  yt,
		// Pictures in place where the terminal can, links elsewhere.
		PictureMode: termimg.Detect(os.Getenv),
		Changes:     &CodeHost{},
		OpenURL:     OpenURL,
	}
}

// CLIDaemon starts and stops runners by running this binary:
// `ymcp pipeline NAME run -d` and `ymcp pipeline NAME stop`.
type CLIDaemon struct {
	// Exe is the ymcp binary; empty means the running one.
	Exe string
}

// Start runs `ymcp pipeline NAME run -d`.
func (c CLIDaemon) Start(ctx context.Context, pipeline string) (string, error) {
	return c.run(ctx, "pipeline", pipeline, "run", "-d")
}

// Stop runs `ymcp pipeline NAME stop`.
func (c CLIDaemon) Stop(ctx context.Context, pipeline string) (string, error) {
	return c.run(ctx, "pipeline", pipeline, "stop")
}

// runnerLogLines is how much of a runner log the TUI reads.
const runnerLogLines = "500"

// Log runs `ymcp pipeline NAME logs -n 500`: the tail of the runner log.
func (c CLIDaemon) Log(ctx context.Context, pipeline string) (string, error) {
	return c.run(ctx, "pipeline", pipeline, "logs", "-n", runnerLogLines)
}

func (c CLIDaemon) run(ctx context.Context, args ...string) (string, error) {
	exe := c.Exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return "", err
		}
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdin = nil
	// A detached runner must not keep our pipes open; don't wait for it.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// OpenURL opens an http(s) link in the system browser.
func OpenURL(url string) error {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return errors.New("not a web link: " + url)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() //nolint:errcheck // reap the opener
	return nil
}

// lastLine is the last non-empty line of a command's output, or def.
func lastLine(out, def string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "ymcp:")); l != "" {
			return l
		}
	}
	return def
}
