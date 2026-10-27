package toast

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	toast "git.sr.ht/~jackmordaunt/go-toast"
	"golang.org/x/sys/windows/registry"

	"github.com/xlsft/ymcp/internal/notify"
)

//go:embed assets/ymcp.png
var iconPNG []byte

// appGUID is ymcp's toast activator CLSID, registered through SetAppData.
const appGUID = "{B07E12ED-93A6-4F07-8C61-F3A34D1B9A6E}"

// Registry keys, all under HKEY_CURRENT_USER.
const (
	appKey      = `SOFTWARE\Classes\AppUserModelId\` + appID
	pushKey     = `SOFTWARE\Microsoft\Windows\CurrentVersion\PushNotifications`
	appSettings = `SOFTWARE\Microsoft\Windows\CurrentVersion\Notifications\Settings\` + appID
)

// settingsHintWindows says where Windows notification settings are.
const settingsHintWindows = "Settings → System → Notifications"

type provider struct {
	sound bool
}

func newProvider() *provider { return &provider{sound: true} }

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

// Check reports notifications turned off for everything or for ymcp.
func (p *provider) Check(context.Context) error {
	if _, err := iconPath(); err != nil {
		return err
	}
	if v, ok := registryDWORD(pushKey, "ToastEnabled"); ok && v == 0 {
		return errors.New("notifications are turned off in Windows; turn them on in " + settingsHintWindows)
	}
	if v, ok := registryDWORD(appSettings, "Enabled"); ok && v == 0 {
		return errors.New("notifications from ymcp are turned off; turn them on in " + settingsHintWindows + " → ymcp")
	}
	return nil
}

// pushMu serialises pushes: go-toast keeps global registry state.
var pushMu sync.Mutex

func (p *provider) Send(ctx context.Context, e notify.Event) error {
	icon, err := installIcon()
	if err != nil {
		return err
	}
	pushMu.Lock()
	defer pushMu.Unlock()
	// The Windows Runtime calls go-toast makes are per thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	clearStaleAppData(icon)
	if err := toast.SetAppData(toast.AppData{AppID: appID, GUID: appGUID, IconPath: icon}); err != nil {
		return fmt.Errorf("registering ymcp for notifications: %w", err)
	}
	t := windowsToast(e, icon, p.sound)
	n := toast.Notification{
		AppID: appID,
		Title: t.Title,
		Body:  t.Body,
		Icon:  t.Icon,
	}
	if t.Launch != "" {
		n.ActivationType = toast.Protocol
		n.ActivationArguments = t.Launch
	}
	if t.Silent {
		n.Audio = toast.Silent
	}
	if err := n.Push(); err != nil {
		return fmt.Errorf("showing the notification: %w", err)
	}
	return nil
}

// iconPath is %LOCALAPPDATA%\ymcp\ymcp.png.
func iconPath() (string, error) {
	dir := os.Getenv("LOCALAPPDATA")
	if dir == "" {
		return "", errors.New("LOCALAPPDATA is not set")
	}
	return filepath.Join(dir, "ymcp", "ymcp.png"), nil
}

// installIcon writes the icon PNG unless it is already there.
func installIcon() (string, error) {
	path, err := iconPath()
	if err != nil {
		return "", err
	}
	if b, err := os.ReadFile(path); err == nil && bytes.Equal(b, iconPNG) {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, iconPNG, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// clearStaleAppData deletes registry values left by an older setup: go-toast
// only writes values that are missing, so a changed icon path or activator
// GUID would never take effect.
func clearStaleAppData(icon string) {
	k, err := registry.OpenKey(registry.CURRENT_USER, appKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	for name, want := range map[string]string{"IconUri": icon, "CustomActivator": appGUID, "DisplayName": appID} {
		if got, _, err := k.GetStringValue(name); err == nil && got != want {
			_ = k.DeleteValue(name)
		}
	}
}

func registryDWORD(path, name string) (uint64, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE)
	if err != nil {
		return 0, false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(name)
	return v, err == nil
}
