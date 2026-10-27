package toast

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/xlsft/ymcp/internal/notify"
)

// The freedesktop (Linux) logic that doesn't need a bus, kept apart so it is
// tested on every OS.

// errNoSessionBus says why there is no bus to talk to. godbus would start a
// private bus with dbus-launch instead, which no notification daemon
// listens on, so ymcp doesn't let it.
var errNoSessionBus = errors.New("no D-Bus session bus: DBUS_SESSION_BUS_ADDRESS is not set and $XDG_RUNTIME_DIR/bus does not exist (notifications need a desktop session)")

// sessionBusAddress finds the session bus from the environment only.
func sessionBusAddress(getenv func(string) string, exists func(string) bool) (string, error) {
	if a := strings.TrimSpace(getenv("DBUS_SESSION_BUS_ADDRESS")); a != "" {
		return a, nil
	}
	if dir := getenv("XDG_RUNTIME_DIR"); dir != "" {
		if p := filepath.Join(dir, "bus"); exists(p) {
			return "unix:path=" + p, nil
		}
	}
	return "", errNoSessionBus
}

// iconDir is where the icon PNG goes: $XDG_DATA_HOME/ymcp, by default
// ~/.local/share/ymcp.
func iconDir(getenv func(string) string) (string, error) {
	if d := getenv("XDG_DATA_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "ymcp"), nil
	}
	home := getenv("HOME")
	if home == "" {
		return "", errors.New("neither XDG_DATA_HOME nor HOME is set")
	}
	return filepath.Join(home, ".local", "share", "ymcp"), nil
}

// fdNote is a notification for the freedesktop service.
type fdNote struct {
	Summary, Body string
	Action        bool // offer the "default" action that opens the URL
	Silent        bool // the suppress-sound hint
}

// freedesktopNote builds the notification for a server with the given
// capabilities: the click opens the URL through the default action when
// the server has "actions", otherwise the URL is added to the text. With
// "body-markup" the body is escaped, since the server reads it as markup.
func freedesktopNote(e notify.Event, caps []string, sound bool) fdNote {
	n := fdNote{Summary: e.Heading(), Silent: !sound}
	body := e.Body
	link := openable(e.URL)
	if link && slices.Contains(caps, "actions") {
		n.Action = true
	} else if link {
		if body != "" {
			body += "\n"
		}
		body += e.URL
	}
	if slices.Contains(caps, "body-markup") {
		body = markupReplacer.Replace(body)
	}
	n.Body = body
	return n
}

var markupReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
