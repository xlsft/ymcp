package toast

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The macOS wrapper bundle logic that doesn't need macOS, kept apart so it
// is tested on every OS.

const (
	bundleID   = "dev.xlsft.ymcp"
	bundleName = "ymcp.app"
	bundleExe  = "ymcp" // Contents/MacOS/ymcp
	iconName   = "ymcp" // Contents/Resources/ymcp.icns
)

// infoPlist is the wrapper's Info.plist. hash identifies the ymcp binary the
// bundle holds a copy of: a bundle whose Info.plist differs from the one the
// running ymcp would write is rebuilt.
func infoPlist(version, hash string) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	for _, kv := range [][2]string{
		{"CFBundleIdentifier", bundleID},
		{"CFBundleName", "ymcp"},
		{"CFBundleDisplayName", "ymcp"},
		{"CFBundleExecutable", bundleExe},
		{"CFBundleIconFile", iconName},
		{"CFBundlePackageType", "APPL"},
		{"CFBundleShortVersionString", bundleVersion(version)},
		{"CFBundleVersion", bundleVersion(version)},
		{"LSMinimumSystemVersion", "11.0"},
		{"YMCPBinarySHA256", hash},
	} {
		fmt.Fprintf(&b, "  <key>%s</key><string>%s</string>\n", kv[0], plistEscaper.Replace(kv[1]))
	}
	b.WriteString(`  <key>LSUIElement</key><true/>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
`)
	return b.Bytes()
}

var plistEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

var semverCore = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// bundleVersion turns a Go module version into the dotted numbers a bundle
// version must be: v1.4.2 and v1.4.2-0.2026… become 1.4.2, anything else
// (a dev build) 0.0.0.
func bundleVersion(v string) string {
	m := semverCore.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return "0.0.0"
	}
	return m[1] + "." + m[2] + "." + m[3]
}

// bundleUpToDate reports whether the bundle at app is complete, signed and
// matches the wanted Info.plist and icon.
func bundleUpToDate(app string, plist, icns []byte) bool {
	got, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil || !bytes.Equal(got, plist) {
		return false
	}
	icon, err := os.ReadFile(filepath.Join(app, "Contents", "Resources", iconName+".icns"))
	if err != nil || !bytes.Equal(icon, icns) {
		return false
	}
	for _, p := range []string{
		filepath.Join(app, "Contents", "MacOS", bundleExe),
		filepath.Join(app, "Contents", "_CodeSignature", "CodeResources"),
	} {
		if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// checkBundleDir refuses bundle locations macOS won't deliver
// notifications from: anything under a temporary directory.
func checkBundleDir(app string, tempDirs []string) error {
	resolved := resolveExisting(app)
	for _, t := range tempDirs {
		if t == "" {
			continue
		}
		t = resolveExisting(t)
		if resolved == t || strings.HasPrefix(resolved, strings.TrimSuffix(t, "/")+"/") {
			return fmt.Errorf("the notification app %s is in a temporary directory (%s), where macOS doesn't deliver its notifications", app, t)
		}
	}
	return nil
}

// resolveExisting resolves symlinks in the longest existing prefix of p, so
// /tmp/x/y becomes /private/tmp/x/y even before x exists.
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// inBundle reports whether exe is the main executable of a ymcp.app bundle.
func inBundle(exe string) bool {
	dir := filepath.Dir(exe)
	return filepath.Base(exe) == bundleExe &&
		filepath.Base(dir) == "MacOS" &&
		filepath.Base(filepath.Dir(dir)) == "Contents" &&
		filepath.Base(filepath.Dir(filepath.Dir(dir))) == bundleName
}

// clickArgs reports whether the arguments are those of a launch by macOS
// (none, or an old-style -psn_ process serial number), not a command.
func clickArgs(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-psn_") {
			return false
		}
	}
	return true
}

// authStatus is the output of `ymcp notify status`.
type authStatus struct {
	Authorization string `json:"authorization"` // notDetermined, denied, authorized, provisional, ephemeral
	AlertStyle    int    `json:"alertStyle"`    // 0 none, 1 banner, 2 alert
}

// settingsHint says where macOS notification settings for ymcp are.
const settingsHint = "System Settings → Notifications → ymcp"

// problem turns an authorization status into the error Check reports;
// nil when notifications will show (or macOS will ask on the first one).
func (s authStatus) problem() error {
	switch s.Authorization {
	case "denied":
		return errors.New("notifications from ymcp are turned off; allow them in " + settingsHint)
	case "authorized", "provisional", "ephemeral":
		if s.AlertStyle == 0 {
			return errors.New("notifications from ymcp are allowed but their alert style is None, so nothing shows; choose Banners or Alerts in " + settingsHint)
		}
		return nil
	case "notDetermined":
		return nil // macOS asks on the first notification
	}
	return fmt.Errorf("unexpected notification authorization status %q", s.Authorization)
}
