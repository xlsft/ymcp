package agent

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// killGrace is how long an interrupted agent gets before its group is
// killed, and how long os/exec waits for pipes after the agent exits.
var killGrace = 3 * time.Second

// tail keeps the last bytes written to it, for the agent's stderr.
type tail struct {
	mu    sync.Mutex
	buf   []byte
	ended chan struct{} // closed when the stream has ended
}

const tailSize = 8 << 10

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailSize {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-tailSize:]...)
	}
	return len(p), nil
}

// end says the stream written to t has ended.
func (t *tail) end() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended == nil {
		t.ended = make(chan struct{})
	}
	close(t.ended)
}

// wait waits up to d for the stream to end, so that the last lines the
// agent wrote before it exited are in t.
func (t *tail) wait(d time.Duration) {
	t.mu.Lock()
	if t.ended == nil {
		t.ended = make(chan struct{})
	}
	ended := t.ended
	t.mu.Unlock()
	select {
	case <-ended:
	case <-time.After(d):
	}
}

// ender is a stderr writer told when the stream has ended.
type ender interface{ end() }

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// withStderr appends the agent's stderr to a message, if there is any.
func withStderr(msg string, t *tail) string {
	if s := t.String(); s != "" {
		return msg + ": " + s
	}
	return msg
}

// startPiped starts cmd in its own process group with stdout and stderr on
// pipes of our own, which Wait neither waits for nor closes: children of
// the agent may hold them open, and the agent's last output must not be
// cut off. stderr is copied into errs, which is then told the stream
// ended if it is an ender; the caller reads and closes stdout.
func startPiped(cmd *exec.Cmd, errs io.Writer) (stdout *os.File, g *procGroup, err error) {
	or, ow, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	er, ew, err := os.Pipe()
	if err != nil {
		or.Close()
		ow.Close()
		return nil, nil, err
	}
	cmd.Stdout, cmd.Stderr = ow, ew
	cmd.WaitDelay = killGrace
	setGroup(cmd)
	err = cmd.Start()
	ow.Close()
	ew.Close()
	if err != nil {
		or.Close()
		er.Close()
		return nil, nil, err
	}
	go func() {
		io.Copy(errs, er)
		er.Close()
		if e, ok := errs.(ender); ok {
			e.end()
		}
	}()
	return or, attach(cmd), nil
}
