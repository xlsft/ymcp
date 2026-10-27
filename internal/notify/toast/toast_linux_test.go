package toast

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/esiqveland/notify"

	ynotify "github.com/xlsft/ymcp/internal/notify"
)

// fakeBus is a notification service with fixed capabilities.
type fakeBus struct {
	caps    []string
	capsErr error
	sendErr error

	mu     sync.Mutex
	sent   []notify.Notification
	closed bool
}

func (b *fakeBus) GetCapabilities() ([]string, error) { return b.caps, b.capsErr }
func (b *fakeBus) SendNotification(n notify.Notification) (uint32, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sendErr != nil {
		return 0, b.sendErr
	}
	b.sent = append(b.sent, n)
	return uint32(len(b.sent)), nil
}
func (b *fakeBus) Close() error { b.closed = true; return nil }

type harness struct {
	p        *provider
	bus      *fakeBus
	dials    []string
	opened   []string
	onAction func(uint32, string)
	onClosed func(uint32)
}

func newHarness(t *testing.T, env map[string]string, b *fakeBus) *harness {
	h := &harness{bus: b}
	if env == nil {
		env = map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/fake/bus"}
	}
	env["XDG_DATA_HOME"] = t.TempDir()
	h.p = newProvider()
	h.p.getenv = func(k string) string { return env[k] }
	h.p.exists = func(string) bool { return false }
	h.p.dial = func(addr string, onAction func(uint32, string), onClosed func(uint32)) (bus, error) {
		h.dials = append(h.dials, addr)
		h.onAction, h.onClosed = onAction, onClosed
		return b, nil
	}
	h.p.open = func(u string) error { h.opened = append(h.opened, u); return nil }
	return h
}

var linuxEvent = ynotify.Event{Kind: ynotify.KindNeedsInput, Pipeline: "backend", Run: 17, Issue: "BE-123",
	Body: "Which date format?", URL: "https://yt.example.com/issue/BE-123"}

func TestLinuxActionOpensURL(t *testing.T) {
	h := newHarness(t, nil, &fakeBus{caps: []string{"actions", "body"}})
	ctx := context.Background()
	if err := h.p.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.p.Send(ctx, linuxEvent); err != nil {
		t.Fatal(err)
	}
	if len(h.dials) != 1 || h.dials[0] != "unix:path=/fake/bus" {
		t.Errorf("dials = %v", h.dials)
	}
	n := h.bus.sent[0]
	if n.AppName != "ymcp" || n.Summary != "BE-123 · backend #17: needs input" || n.Body != "Which date format?" ||
		len(n.Actions) != 1 || n.Actions[0].Key != "default" || !strings.HasSuffix(n.AppIcon, "/ymcp/ymcp.png") {
		t.Errorf("sent %+v", n)
	}
	h.onAction(1, "other")
	h.onAction(1, "default")
	h.onAction(1, "default") // a second click: already handled
	if len(h.opened) != 1 || h.opened[0] != linuxEvent.URL {
		t.Errorf("opened %v", h.opened)
	}
	if err := h.p.Close(); err != nil || !h.bus.closed {
		t.Errorf("Close = %v, closed %v", err, h.bus.closed)
	}
}

func TestLinuxNoActionsPutsURLInBody(t *testing.T) {
	h := newHarness(t, nil, &fakeBus{caps: []string{"body"}})
	h.p.Configure([]byte(`{"sound":false}`))
	if err := h.p.Send(context.Background(), linuxEvent); err != nil {
		t.Fatal(err)
	}
	n := h.bus.sent[0]
	if len(n.Actions) != 0 || n.Body != "Which date format?\n"+linuxEvent.URL {
		t.Errorf("sent %+v", n)
	}
	if v, ok := n.Hints["suppress-sound"]; !ok || v.Value() != true {
		t.Errorf("hints = %v", n.Hints)
	}
	h.onAction(1, "default")
	if len(h.opened) != 0 {
		t.Errorf("opened %v", h.opened)
	}
}

func TestLinuxClosedForgetsURL(t *testing.T) {
	h := newHarness(t, nil, &fakeBus{caps: []string{"actions"}})
	h.p.Send(context.Background(), linuxEvent)
	h.onClosed(1)
	h.onAction(1, "default")
	if len(h.opened) != 0 {
		t.Errorf("opened %v after close", h.opened)
	}
}

func TestLinuxBusErrors(t *testing.T) {
	h := newHarness(t, map[string]string{}, &fakeBus{})
	if err := h.p.Check(context.Background()); !errors.Is(err, errNoSessionBus) || len(h.dials) != 0 {
		t.Errorf("no bus: %v, dials %v", err, h.dials)
	}

	h = newHarness(t, nil, &fakeBus{capsErr: errors.New("ServiceUnknown")})
	if err := h.p.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "notification daemon") {
		t.Errorf("no daemon: %v", err)
	}

	b := &fakeBus{caps: []string{"actions"}, sendErr: errors.New("connection reset")}
	h = newHarness(t, nil, b)
	if err := h.p.Send(context.Background(), linuxEvent); err == nil {
		t.Fatal("send error lost")
	}
	b.sendErr = nil
	if err := h.p.Send(context.Background(), linuxEvent); err != nil {
		t.Fatal(err)
	}
	if len(h.dials) != 2 {
		t.Errorf("did not reconnect after a failed send: %v", h.dials)
	}
}

func TestLinuxPendingIsBounded(t *testing.T) {
	h := newHarness(t, nil, &fakeBus{caps: []string{"actions"}})
	for i := 0; i < maxPending+20; i++ {
		h.p.Send(context.Background(), linuxEvent)
	}
	if len(h.p.urls) != maxPending || len(h.p.order) != maxPending {
		t.Errorf("pending = %d/%d", len(h.p.urls), len(h.p.order))
	}
}
