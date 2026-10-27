// Package toast is the "toast" notification provider: system notifications
// with the ymcp icon, where a click opens the event's URL.
//
// Each OS has its own implementation:
//
//   - macOS: UNUserNotificationCenter through purego (no cgo), from a small
//     app bundle ymcp maintains itself, ~/Library/Application Support/ymcp/ymcp.app
//     (bundle id dev.xlsft.ymcp, a copy of the ymcp binary, ad-hoc signed).
//     Sending runs `ymcp.app/Contents/MacOS/ymcp notify post …`, which exits
//     at once; on a click macOS relaunches the bundle, which opens the URL
//     (see ClickLaunch and HandleClick).
//   - Windows: go-toast with AppID "ymcp", the icon in %LOCALAPPDATA%\ymcp and
//     protocol activation, so Windows opens the URL even after ymcp exits.
//   - Linux: the freedesktop notification service on the D-Bus session
//     bus; a click on the default action runs xdg-open while the provider is
//     open (a runner keeps it open), and without action support the URL goes
//     into the text.
//
// Elsewhere the provider is off by default and its Check says it is
// unsupported.
//
// Importing the package registers the provider with internal/notify.
package toast

import (
	"encoding/json"

	"github.com/xlsft/ymcp/internal/notify"
)

// Name is the provider's name in settings and filters.
const Name = "toast"

// appID is the application name the notifications are shown under.
const appID = "ymcp"

func init() {
	notify.Register(Name, func() notify.Provider { return newProvider() })
}

// settings is the provider's section of the notification settings.
type settings struct {
	notify.Common
	Sound *bool `json:"sound,omitempty"` // play the system sound; default true
}

func parseSettings(raw json.RawMessage) (settings, error) {
	var s settings
	err := notify.DecodeSettings(raw, &s)
	return s, err
}

func (s settings) sound() bool { return s.Sound == nil || *s.Sound }
