//go:build !windows

package workspace

import (
	"context"
	"io"
	"sync"
	"testing"
)

// crashyHost is this machine, where the first n git calls die of a
// segfault, as git on a busy macOS runner now and then does.
type crashyHost struct {
	Host
	mu    sync.Mutex
	n     int
	calls int
}

func (h *crashyHost) Run(ctx context.Context, dir string, argv []string, stdin io.Reader) ([]byte, []byte, error) {
	h.mu.Lock()
	h.calls++
	crash := h.n > 0
	h.n--
	h.mu.Unlock()
	if crash {
		return h.Host.Run(ctx, "", []string{"sh", "-c", "kill -SEGV $$"}, nil)
	}
	return h.Host.Run(ctx, dir, argv, stdin)
}

// TestCrashedGitRunsAgain: a crashed read or config call runs again; a
// crashed call that changes something does not.
func TestCrashedGitRunsAgain(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(context.Background(), Local(), dir, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	h := &crashyHost{Host: Local(), n: 2}
	if _, err := run(context.Background(), h, dir, "config", "--local", "user.name", "Agent"); err != nil {
		t.Fatalf("config after two crashes: %v", err)
	}
	if h.calls != 3 {
		t.Errorf("%d calls, want 3", h.calls)
	}
	h = &crashyHost{Host: Local(), n: 1}
	_, err := run(context.Background(), h, dir, "commit", "--allow-empty", "-m", "x")
	if !crashed(err) || h.calls != 1 {
		t.Errorf("commit: %v after %d calls, want the crash after one", err, h.calls)
	}
}
