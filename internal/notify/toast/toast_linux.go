package toast

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/esiqveland/notify"
	"github.com/godbus/dbus/v5"

	ynotify "github.com/xlsft/ymcp/internal/notify"
)

//go:embed assets/ymcp.png
var iconPNG []byte

// maxPending bounds the notification → URL map kept for clicks.
const maxPending = 200

// bus is the part of the freedesktop notification service the provider
// uses; tests replace it.
type bus interface {
	GetCapabilities() ([]string, error)
	SendNotification(n notify.Notification) (uint32, error)
	Close() error
}

// dialer connects to the notification service at a bus address and reports
// clicked actions and closed notifications.
type dialer func(addr string, onAction func(id uint32, key string), onClosed func(id uint32)) (bus, error)

type provider struct {
	sound  bool
	getenv func(string) string
	exists func(string) bool
	dial   dialer
	open   func(url string) error // xdg-open

	mu    sync.Mutex
	conn  bus
	caps  []string
	urls  map[uint32]string // notification id → URL a click opens
	order []uint32
}

func newProvider() *provider {
	return &provider{
		sound:  true,
		getenv: os.Getenv,
		exists: func(p string) bool { _, err := os.Stat(p); return err == nil },
		dial:   dialSession,
		open:   xdgOpen,
		urls:   map[uint32]string{},
	}
}

func (p *provider) Name() string           { return Name }
func (p *provider) EnabledByDefault() bool { return true }

func (p *provider) Configure(raw json.RawMessage) error {
	s, err := parseSettings(raw)
	if err != nil {
		return err
	}
	p.sound = s.sound()
	return nil
}

// Check connects to the session bus and asks the notification service for
// its capabilities, which fails at once when no daemon is running.
func (p *provider) Check(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.connect()
}

func (p *provider) Send(_ context.Context, e ynotify.Event) error {
	icon := installIcon(p.getenv)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.connect(); err != nil {
		return err
	}
	fd := freedesktopNote(e, p.caps, p.sound)
	n := notify.Notification{
		AppName:       appID,
		AppIcon:       icon,
		Summary:       fd.Summary,
		Body:          fd.Body,
		ExpireTimeout: notify.ExpireTimeoutSetByNotificationServer,
	}
	if fd.Action {
		n.Actions = []notify.Action{{Key: "default", Label: "Open"}}
	}
	if fd.Silent {
		n.AddHint(notify.Hint{ID: "suppress-sound", Variant: dbus.MakeVariant(true)})
	}
	id, err := p.conn.SendNotification(n)
	if err != nil {
		// The bus or the daemon may have gone; reconnect next time.
		go p.detach().Close()
		return fmt.Errorf("sending the notification: %w", err)
	}
	if fd.Action {
		p.remember(id, e.URL)
	}
	return nil
}

// Close stops listening for clicks.
func (p *provider) Close() error {
	p.mu.Lock()
	c := p.detach()
	p.mu.Unlock()
	// Closing waits for the signal loop, which may be waiting for p.mu in
	// action, so it happens outside the lock.
	if c == nil {
		return nil
	}
	return c.Close()
}

// connect opens the bus and reads the capabilities once; p.mu is held.
func (p *provider) connect() error {
	if p.conn != nil {
		return nil
	}
	addr, err := sessionBusAddress(p.getenv, p.exists)
	if err != nil {
		return err
	}
	conn, err := p.dial(addr, p.action, p.closed)
	if err != nil {
		return fmt.Errorf("connecting to the D-Bus session bus: %w", err)
	}
	caps, err := conn.GetCapabilities()
	if err != nil {
		go conn.Close() // not under p.mu, see Close
		return fmt.Errorf("no notification service on the session bus (is a notification daemon running?): %w", err)
	}
	p.conn, p.caps = conn, caps
	return nil
}

// detach forgets the connection and returns it for closing; p.mu is held.
func (p *provider) detach() bus {
	c := p.conn
	p.conn, p.caps = nil, nil
	return c
}

func (p *provider) remember(id uint32, url string) {
	if _, ok := p.urls[id]; !ok {
		p.order = append(p.order, id)
	}
	p.urls[id] = url
	for len(p.order) > maxPending {
		delete(p.urls, p.order[0])
		p.order = p.order[1:]
	}
}

func (p *provider) forget(id uint32) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	url, ok := p.urls[id]
	delete(p.urls, id)
	return url, ok
}

// action handles a click: the default action opens the URL.
func (p *provider) action(id uint32, key string) {
	if key != "default" {
		return
	}
	if url, ok := p.forget(id); ok && url != "" {
		_ = p.open(url)
	}
}

func (p *provider) closed(id uint32) { p.forget(id) }

// installIcon writes the icon PNG under $XDG_DATA_HOME/ymcp and returns its
// path; "" (no icon) when that fails.
func installIcon(getenv func(string) string) string {
	dir, err := iconDir(getenv)
	if err != nil {
		return ""
	}
	path := filepath.Join(dir, "ymcp.png")
	if b, err := os.ReadFile(path); err == nil && bytes.Equal(b, iconPNG) {
		return path
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, iconPNG, 0o644); err != nil {
		return ""
	}
	if err := os.Rename(tmp, path); err != nil {
		return ""
	}
	return path
}

func xdgOpen(url string) error {
	cmd := exec.Command("xdg-open", url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// sessionConn is the real bus: a godbus connection to addr (never one
// started by dbus-launch) with esiqveland/notify's signal listener on it.
type sessionConn struct {
	notify.Notifier
	conn *dbus.Conn
}

func dialSession(addr string, onAction func(uint32, string), onClosed func(uint32)) (bus, error) {
	conn, err := dbus.Connect(addr)
	if err != nil {
		return nil, err
	}
	n, err := notify.New(conn,
		notify.WithOnAction(func(s *notify.ActionInvokedSignal) { onAction(s.ID, s.ActionKey) }),
		notify.WithOnClosed(func(s *notify.NotificationClosedSignal) { onClosed(s.ID) }),
	)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &sessionConn{Notifier: n, conn: conn}, nil
}

func (c *sessionConn) Close() error {
	err := c.Notifier.Close()
	if cerr := c.conn.Close(); err == nil {
		err = cerr
	}
	return err
}
