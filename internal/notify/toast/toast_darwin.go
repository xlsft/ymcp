package toast

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/xlsft/ymcp/internal/notify"
)

//go:embed assets/ymcp.icns
var icns []byte

// AppKit must run on the main thread, which the main goroutine only stays
// on when locked from an init function. It matters only for the notify
// helper (see HelperMain); other commands don't notice.
func init() { runtime.LockOSThread() }

// Overridden in tests.
var (
	// bundlePath is where the wrapper bundle lives.
	bundlePath = func() (string, error) {
		dir, err := os.UserConfigDir() // ~/Library/Application Support
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "ymcp", bundleName), nil
	}
	// executable is the ymcp binary copied into the bundle.
	executable = func() (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(exe)
	}
)

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

// Check makes sure the wrapper bundle is in place and asks macOS whether
// ymcp may show notifications.
func (p *provider) Check(ctx context.Context) error {
	bin, err := ensureBundle()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "notify", "status")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("asking macOS for the notification settings: %w%s", err, detail(stderr.String()))
	}
	var st authStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return fmt.Errorf("reading the notification settings %q: %w", out, err)
	}
	return st.problem()
}

// Send posts the notification through the wrapper bundle. The helper
// exits as soon as macOS has the notification; if macOS is still asking
// whether ymcp may notify when ctx ends, Send returns and the helper is
// left to finish on its own (it gives up after postDeadline).
func (p *provider) Send(ctx context.Context, e notify.Event) error {
	bin, err := ensureBundle()
	if err != nil {
		return err
	}
	link := ""
	if openable(e.URL) {
		link = e.URL
	}
	id := fmt.Sprintf("ymcp-%s-%d-%s-%d", e.Pipeline, e.Run, e.Kind, time.Now().UnixNano())
	cmd := exec.Command(bin, "notify", "post",
		"--title", e.Heading(), "--body", e.Body, "--url", link, "--id", id,
		fmt.Sprintf("--sound=%t", p.sound))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the notification helper: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("posting the notification: %w%s", err, detail(stderr.String()))
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("macOS hasn't taken the notification yet (it may be asking whether ymcp may send notifications): %w", ctx.Err())
	}
}

func detail(stderr string) string {
	if s := strings.TrimSpace(stderr); s != "" {
		return ": " + s
	}
	return ""
}

var bundleMu sync.Mutex

// ensureBundle builds or refreshes the wrapper bundle when it doesn't hold
// the running binary and returns its main executable.
func ensureBundle() (string, error) {
	bundleMu.Lock()
	defer bundleMu.Unlock()
	app, err := bundlePath()
	if err != nil {
		return "", err
	}
	if err := checkBundleDir(app, []string{os.TempDir(), "/tmp", "/private/tmp", "/var/tmp"}); err != nil {
		return "", err
	}
	exe, err := executable()
	if err != nil {
		return "", fmt.Errorf("finding the ymcp binary: %w", err)
	}
	hash, err := fileHash(exe)
	if err != nil {
		return "", fmt.Errorf("reading the ymcp binary: %w", err)
	}
	plist := infoPlist(moduleVersion(), hash)
	bin := filepath.Join(app, "Contents", "MacOS", bundleExe)
	if bundleUpToDate(app, plist, icns) {
		return bin, nil
	}
	if err := buildBundle(app, exe, plist, icns); err != nil {
		// Another ymcp may have built the same bundle meanwhile.
		if bundleUpToDate(app, plist, icns) {
			return bin, nil
		}
		return "", fmt.Errorf("building the notification app %s: %w", app, err)
	}
	return bin, nil
}

var hashCache sync.Map // path → hex sha256, the binary doesn't change under a running process

func fileHash(path string) (string, error) {
	if h, ok := hashCache.Load(path); ok {
		return h.(string), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	h := hex.EncodeToString(sum.Sum(nil))
	hashCache.Store(path, h)
	return h, nil
}

func moduleVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.Main.Version
	}
	return ""
}

// buildBundle assembles the bundle next to app, signs it ad hoc and swaps
// it in. The binary is copied, not linked: codesign refuses a symlinked
// main executable and macOS ignores a process started through one.
func buildBundle(app, exe string, plist, icns []byte) error {
	parent := filepath.Dir(app)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp(parent, ".ymcp.app-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tmp := filepath.Join(work, bundleName)
	files := []struct {
		path string
		data []byte
	}{
		{filepath.Join(tmp, "Contents", "Info.plist"), plist},
		{filepath.Join(tmp, "Contents", "Resources", iconName+".icns"), icns},
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(f.path, f.data, 0o644); err != nil {
			return err
		}
	}
	if err := copyFile(exe, filepath.Join(tmp, "Contents", "MacOS", bundleExe)); err != nil {
		return err
	}
	if out, err := exec.Command("/usr/bin/codesign", "-s", "-", "--force", tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("codesign: %w%s", err, detail(string(out)))
	}
	old := filepath.Join(work, "old.app")
	if err := os.Rename(app, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tmp, app)
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
