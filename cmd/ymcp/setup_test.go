package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/youtrack/youtracktest"
)

// fakeHome gives setup a home with only Cursor installed.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))
	t.Setenv("PATH", t.TempDir())
	os.MkdirAll(filepath.Join(home, ".cursor"), 0o755)
	old := executable
	executable = func() (string, error) { return "/opt/ymcp", nil }
	t.Cleanup(func() { executable = old })
	return filepath.Join(home, ".cursor", "mcp.json")
}

func TestSetupRenamesAddsAndRemovesProfiles(t *testing.T) {
	isolate(t)
	cursor := fakeHome(t)
	f := youtracktest.New(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"login":"alice","fullName":"Alice"}`))
	}))
	if _, err := config.Save(config.Config{Name: "youtrack", URL: f.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"}, ""); err != nil {
		t.Fatal(err)
	}

	// Rename the default profile: the client entry moves with it.
	if _, err := captureStdout(t, func() error {
		return run([]string{"setup", "--name", "YouTrack (xlsft)", "--clients", "cursor"})
	}); err != nil {
		t.Fatal(err)
	}
	got := read(t, cursor)
	if !strings.Contains(got, `"youtrack-xlsft"`) || strings.Contains(got, `"youtrack":`) || !strings.Contains(got, `"--profile",`) {
		t.Errorf("after rename, mcp.json:\n%s", got)
	}
	cfg, err := config.Load("youtrack-xlsft")
	if err != nil || cfg.DisplayName != "YouTrack (xlsft)" || !cfg.Complete() {
		t.Errorf("renamed profile = %+v, %v", cfg, err)
	}

	// Add a second instance from YOUTRACK_* variables.
	t.Setenv("YOUTRACK_URL", f.URL)
	t.Setenv("YOUTRACK_LOGIN", youtracktest.Login)
	t.Setenv("YOUTRACK_PASSWORD", youtracktest.Password)
	t.Setenv("YOUTRACK_PROXY", "none")
	out, err := captureStdout(t, func() error {
		return run([]string{"--profile", "beget", "setup", "--name", "youtrack-beget", "--all"})
	})
	if err != nil || !strings.Contains(out, "Logged in") {
		t.Fatalf("add = %q, %v", out, err)
	}
	for _, k := range []string{"YOUTRACK_URL", "YOUTRACK_LOGIN", "YOUTRACK_PASSWORD", "YOUTRACK_PROXY"} {
		t.Setenv(k, "")
	}
	if got := read(t, cursor); !strings.Contains(got, `"youtrack-xlsft"`) || !strings.Contains(got, `"youtrack-beget"`) {
		t.Errorf("after add, mcp.json:\n%s", got)
	}
	if err := run([]string{"setup", "--all"}); err == nil || !strings.Contains(err.Error(), "--profile") {
		t.Errorf("setup with two profiles and no --profile = %v", err)
	}
	if err := run([]string{"--profile", "youtrack-beget", "setup", "--name", "YouTrack (xlsft)", "--all"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("rename onto another profile = %v", err)
	}

	// Remove it again.
	if _, err := captureStdout(t, func() error {
		return run([]string{"--profile", "youtrack-beget", "setup", "--remove"})
	}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, cursor); strings.Contains(got, "youtrack-beget") || !strings.Contains(got, `"youtrack-xlsft"`) {
		t.Errorf("after remove, mcp.json:\n%s", got)
	}
	if list, _ := config.Profiles(); len(list) != 1 || list[0].Name != "youtrack-xlsft" {
		t.Errorf("profiles = %+v", list)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
