package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/xlsft/ymcp/internal/clients"
	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/tui/wizard"
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
		return run([]string{"setup", "--name", "YouTrack (work)", "--clients", "cursor"})
	}); err != nil {
		t.Fatal(err)
	}
	got := read(t, cursor)
	if !strings.Contains(got, `"youtrack-work"`) || strings.Contains(got, `"youtrack":`) || !strings.Contains(got, `"--profile",`) {
		t.Errorf("after rename, mcp.json:\n%s", got)
	}
	cfg, err := config.Load("youtrack-work")
	if err != nil || cfg.DisplayName != "YouTrack (work)" || !cfg.Complete() {
		t.Errorf("renamed profile = %+v, %v", cfg, err)
	}

	// Add a second instance from YOUTRACK_* variables.
	t.Setenv("YOUTRACK_URL", f.URL)
	t.Setenv("YOUTRACK_LOGIN", youtracktest.Login)
	t.Setenv("YOUTRACK_PASSWORD", youtracktest.Password)
	t.Setenv("YOUTRACK_PROXY", "none")
	out, err := captureStdout(t, func() error {
		return run([]string{"--profile", "acme", "setup", "--name", "youtrack-acme", "--all"})
	})
	if err != nil || !strings.Contains(out, "Logged in") {
		t.Fatalf("add = %q, %v", out, err)
	}
	for _, k := range []string{"YOUTRACK_URL", "YOUTRACK_LOGIN", "YOUTRACK_PASSWORD", "YOUTRACK_PROXY"} {
		t.Setenv(k, "")
	}
	if got := read(t, cursor); !strings.Contains(got, `"youtrack-work"`) || !strings.Contains(got, `"youtrack-acme"`) {
		t.Errorf("after add, mcp.json:\n%s", got)
	}
	if err := run([]string{"setup", "--all"}); err == nil || !strings.Contains(err.Error(), "--profile") {
		t.Errorf("setup with two profiles and no --profile = %v", err)
	}
	if err := run([]string{"--profile", "youtrack-acme", "setup", "--name", "YouTrack (work)", "--all"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("rename onto another profile = %v", err)
	}

	// Narrow its tools without a terminal, then widen them again.
	if _, err := captureStdout(t, func() error {
		return run([]string{"--profile", "youtrack-acme", "setup", "--tools", "issues,comments", "--all"})
	}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := config.Load("youtrack-acme"); strings.Join(cfg.Tools, ",") != "issues,comments" || !cfg.Complete() {
		t.Errorf("tools = %v, complete = %v", cfg.Tools, cfg.Complete())
	}
	if err := run([]string{"--profile", "youtrack-acme", "setup", "--tools", "issues,nope", "--all"}); err == nil || !strings.Contains(err.Error(), "unknown tool or group") {
		t.Errorf("bad --tools = %v", err)
	}
	captureStdout(t, func() error { return run([]string{"--profile", "youtrack-acme", "setup", "--tools", "all", "--all"}) })
	if cfg, _ := config.Load("youtrack-acme"); cfg.Tools != nil {
		t.Errorf("tools after all = %v", cfg.Tools)
	}

	// Turn its confirmations off and on again.
	captureStdout(t, func() error { return run([]string{"--profile", "youtrack-acme", "setup", "--confirm", "off", "--all"}) })
	if cfg, _ := config.Load("youtrack-acme"); cfg.Confirms() {
		t.Error("confirmations still on after --confirm off")
	}
	captureStdout(t, func() error { return run([]string{"--profile", "youtrack-acme", "setup", "--confirm", "on", "--all"}) })
	if cfg, _ := config.Load("youtrack-acme"); !cfg.Confirms() || cfg.Confirm != nil {
		t.Errorf("after --confirm on: %v", cfg.Confirm)
	}
	if err := run([]string{"--profile", "youtrack-acme", "setup", "--confirm", "maybe", "--all"}); err == nil {
		t.Error("--confirm maybe was taken")
	}

	// Remove it again.
	if _, err := captureStdout(t, func() error {
		return run([]string{"--profile", "youtrack-acme", "setup", "--remove"})
	}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, cursor); strings.Contains(got, "youtrack-acme") || !strings.Contains(got, `"youtrack-work"`) {
		t.Errorf("after remove, mcp.json:\n%s", got)
	}
	if list, _ := config.Profiles(); len(list) != 1 || list[0].Name != "youtrack-work" {
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

// TestSetupTUIDeps: what the setup TUI saves lands in the config and the
// clients, and a removal takes it out of both.
func TestSetupTUIDeps(t *testing.T) {
	isolate(t)
	cursor := fakeHome(t)
	env, err := clients.System(runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	d := setupDeps(env)
	off := false
	cfg := config.Config{Name: "youtrack-work", DisplayName: "YouTrack (work)", URL: "https://yt.example.com", Login: "alice", Password: "pw", Tools: []string{"-api"}, Confirm: &off}
	lines, err := d.Save(context.Background(), "", wizard.ProfileDraft{Config: cfg, Clients: []string{"cursor"}})
	if err != nil {
		t.Fatal(err)
	}
	if out := strings.Join(lines, "\n"); !strings.Contains(out, "Saved profile") || !strings.Contains(out, "Cursor") {
		t.Errorf("lines:\n%s", out)
	}
	if got := read(t, cursor); !strings.Contains(got, `"youtrack-work"`) {
		t.Errorf("mcp.json:\n%s", got)
	}
	list, err := d.Profiles()
	if err != nil || len(list) != 1 || !slices.Equal(list[0].Clients, []string{"Cursor"}) || list[0].Config.Confirms() {
		t.Fatalf("profiles = %+v, %v", list, err)
	}
	cs := d.Clients("youtrack-work")
	if i := slices.IndexFunc(cs, func(c wizard.Client) bool { return c.ID == "cursor" }); i < 0 || !cs[i].Registered {
		t.Errorf("clients = %+v", cs)
	}
	if _, err := d.Save(context.Background(), "", wizard.ProfileDraft{Config: cfg}); err == nil {
		t.Error("a second profile under the same key was saved")
	}
	if _, err := d.Remove(context.Background(), "youtrack-work"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, cursor); strings.Contains(got, "youtrack-work") {
		t.Errorf("after remove, mcp.json:\n%s", got)
	}
}
