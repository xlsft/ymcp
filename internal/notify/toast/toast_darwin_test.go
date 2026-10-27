package toast

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildBundle(t *testing.T) {
	if _, err := os.Stat("/usr/bin/codesign"); err != nil {
		t.Skip("no codesign")
	}
	app := filepath.Join(t.TempDir(), "support", bundleName)
	plist := infoPlist("v1.2.3", "h1")
	if err := buildBundle(app, "/usr/bin/true", plist, icns); err != nil {
		t.Fatal(err)
	}
	if !bundleUpToDate(app, plist, icns) {
		t.Fatal("fresh bundle is not up to date")
	}
	if out, err := exec.Command("/usr/bin/codesign", "--verify", "--strict", app).CombinedOutput(); err != nil {
		t.Fatalf("signature: %v %s", err, out)
	}
	info, err := os.Lstat(filepath.Join(app, "Contents", "MacOS", bundleExe))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("main executable = %v, %v", info, err)
	}

	// A rebuild replaces the bundle in place and leaves no work dirs behind.
	plist2 := infoPlist("v1.2.4", "h2")
	if err := buildBundle(app, "/usr/bin/true", plist2, icns); err != nil {
		t.Fatal(err)
	}
	if !bundleUpToDate(app, plist2, icns) {
		t.Error("rebuilt bundle is not up to date")
	}
	entries, _ := os.ReadDir(filepath.Dir(app))
	if len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestEnsureBundleRefusesTemp(t *testing.T) {
	saved := bundlePath
	t.Cleanup(func() { bundlePath = saved })
	bundlePath = func() (string, error) { return filepath.Join(t.TempDir(), bundleName), nil }
	if _, err := ensureBundle(); err == nil || !strings.Contains(err.Error(), "temporary directory") {
		t.Errorf("ensureBundle in a temp dir = %v", err)
	}
}

func TestHelperOutsideBundle(t *testing.T) {
	if err := HelperMain([]string{"status"}); err == nil || !strings.Contains(err.Error(), "only runs inside") {
		t.Errorf("HelperMain outside the bundle = %v", err)
	}
	if ClickLaunch() {
		t.Error("the test binary counts as a click launch")
	}
}
