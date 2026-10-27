package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent/agenttest"
	"github.com/xlsft/ymcp/internal/pipeline/workspace/sshtest"
)

func TestMain(m *testing.M) {
	agenttest.Main() // first: a remote fake agent inherits the fake ssh's environment
	sshtest.Main()
	os.Exit(m.Run())
}

// fake starts a fake agent of kind "claude" or "acp" running script, with
// its state in a temp dir, and collects its events.
func fake(t *testing.T, kind string, script agenttest.Script, opts StartOptions, provider ...string) (*recorder, string) {
	t.Helper()
	if script.State == "" {
		script.State = t.TempDir()
	}
	command, env := agenttest.Command(kind, script)
	p := kind
	if len(provider) > 0 {
		p = provider[0]
	}
	a, err := New(p, command, nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.Env == nil {
		opts.Env = map[string]string{}
	}
	for k, v := range env {
		opts.Env[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := a.Start(ctx, opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r := record(s)
	t.Cleanup(func() { s.Close() })
	return r, script.State
}

// startErr starts a fake agent that is expected to fail starting.
func startErr(t *testing.T, kind string, script agenttest.Script, opts StartOptions) error {
	t.Helper()
	if script.State == "" {
		script.State = t.TempDir()
	}
	command, env := agenttest.Command(kind, script)
	a, err := New(kind, command, nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	opts.Env = env
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := a.Start(ctx, opts)
	if err == nil {
		s.Close()
		t.Fatal("Start succeeded")
	}
	return err
}

// recorder collects the events of a session.
type recorder struct {
	Session
	mu     sync.Mutex
	events []Event
	notify chan struct{}
	done   chan struct{}
}

func record(s Session) *recorder {
	r := &recorder{Session: s, notify: make(chan struct{}, 1), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for e := range s.Events() {
			r.mu.Lock()
			r.events = append(r.events, e)
			r.mu.Unlock()
			select {
			case r.notify <- struct{}{}:
			default:
			}
		}
	}()
	return r
}

func (r *recorder) all() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// wait returns the first event of kind for which match is true.
func (r *recorder) wait(t *testing.T, kind Kind, match func(Event) bool) Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		for _, e := range r.all() {
			if e.Kind == kind && (match == nil || match(e)) {
				return e
			}
		}
		select {
		case <-r.notify:
		case <-deadline:
			t.Fatalf("no %v event; got %s", kind, kinds(r.all()))
		}
	}
}

func (r *recorder) prompt(t *testing.T, text string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := r.Prompt(ctx, text)
	if err != nil {
		t.Fatalf("Prompt: %v (result %+v)", err, res)
	}
	return res
}

func kinds(es []Event) string {
	var out []string
	for _, e := range es {
		out = append(out, e.Kind.String())
	}
	return strings.Join(out, ",")
}

func calls(t *testing.T, state, method string) []agenttest.Call {
	t.Helper()
	all, err := agenttest.Calls(state)
	if err != nil {
		t.Fatal(err)
	}
	var out []agenttest.Call
	for _, c := range all {
		if method == "" || c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func params(t *testing.T, c agenttest.Call) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Params, &m); err != nil {
		t.Fatalf("params %s: %v", c.Params, err)
	}
	return m
}

// gone waits for a process to disappear.
func gone(t *testing.T, pid int) {
	t.Helper()
	if pid == 0 {
		t.Fatal("no child pid recorded")
	}
	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d still running", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitChild(t *testing.T, state string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if pid := agenttest.ChildPID(state); pid != 0 {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("child never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
