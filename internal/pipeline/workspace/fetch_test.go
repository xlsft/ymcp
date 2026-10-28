package workspace

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// racyHost is this machine, where fetches are counted and run slowly, and
// the first fails of them fail with git's answer to a ref another fetch
// moved.
type racyHost struct {
	Host
	mu      sync.Mutex
	fails   int
	fetches int
	now     int // fetches running now
	most    int // the most at once
}

func (h *racyHost) Run(ctx context.Context, dir string, argv []string, stdin io.Reader) ([]byte, []byte, error) {
	if len(argv) < 2 || argv[1] != "fetch" {
		return h.Host.Run(ctx, dir, argv, stdin)
	}
	h.mu.Lock()
	h.fetches++
	h.now++
	h.most = max(h.most, h.now)
	fail := h.fails > 0
	h.fails--
	h.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	h.mu.Lock()
	h.now--
	h.mu.Unlock()
	if fail {
		return nil, []byte("error: cannot lock ref 'refs/remotes/origin/BH-1': is at 018a but expected d375\n ! d375..018a  BH-1  -> origin/BH-1  (unable to update local ref)"), errors.New("exit status 1")
	}
	return h.Host.Run(ctx, dir, argv, stdin)
}

// TestFetchOneAtATime: fetches of one repository don't overlap.
func TestFetchOneAtATime(t *testing.T) {
	f := newFixture(t)
	h := &racyHost{Host: Local()}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Fetch(context.Background(), h, f.repo)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if h.fetches != 4 || h.most != 1 {
		t.Errorf("%d fetches, %d at once", h.fetches, h.most)
	}
}

// TestFetchRaceIsRetried: a fetch that lost a race to another process is
// made again; other failures and a race that goes on are reported.
func TestFetchRaceIsRetried(t *testing.T) {
	defer func(d time.Duration) { fetchRetryAfter = d }(fetchRetryAfter)
	fetchRetryAfter = time.Millisecond
	f := newFixture(t)

	h := &racyHost{Host: Local(), fails: 2}
	if err := Fetch(context.Background(), h, f.repo); err != nil || h.fetches != 3 {
		t.Errorf("after two races: %v, %d fetches", err, h.fetches)
	}
	h = &racyHost{Host: Local(), fails: fetchTries}
	if err := Fetch(context.Background(), h, f.repo); err == nil || !strings.Contains(err.Error(), "cannot lock ref") || h.fetches != fetchTries {
		t.Errorf("a race that goes on: %v, %d fetches", err, h.fetches)
	}
	if err := Fetch(context.Background(), Local(), t.TempDir()); err == nil || strings.Contains(err.Error(), "cannot lock") {
		t.Errorf("not a repository: %v", err)
	}
}
