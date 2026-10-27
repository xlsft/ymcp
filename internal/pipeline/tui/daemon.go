package tui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/views"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// Defaults are the real services of the screens on top of the store:
// runners started and stopped through this binary's CLI, the backlog from
// YouTrack and links opened in the system browser.
func Defaults(st *store.Store) views.Deps {
	return views.Deps{
		Store:   st,
		Daemon:  CLIDaemon{},
		Backlog: &YouTrackBacklog{},
		OpenURL: OpenURL,
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

// YouTrackBacklog searches a pipeline's source in YouTrack, one client per
// profile.
type YouTrackBacklog struct {
	mu       sync.Mutex
	watchers map[string]*ytwatch.Watcher
}

// Search runs query in the YouTrack of profile.
func (b *YouTrackBacklog) Search(ctx context.Context, profile, query string) ([]ytwatch.Issue, error) {
	w, err := b.watcher(profile)
	if err != nil {
		return nil, err
	}
	return w.Search(ctx, query)
}

func (b *YouTrackBacklog) watcher(profile string) (*ytwatch.Watcher, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if w := b.watchers[profile]; w != nil {
		return w, nil
	}
	cfg, err := config.Load(profile)
	if err != nil {
		return nil, err
	}
	if !cfg.Complete() {
		return nil, errors.New("profile " + profile + " is not configured")
	}
	yt, err := youtrack.New(youtrack.Options{BaseURL: cfg.URL, Login: cfg.Login, Password: cfg.Password, Proxy: cfg.Proxy})
	if err != nil {
		return nil, err
	}
	if b.watchers == nil {
		b.watchers = map[string]*ytwatch.Watcher{}
	}
	w := ytwatch.New(yt)
	b.watchers[profile] = w
	return w, nil
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
