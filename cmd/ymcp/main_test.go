package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/youtrack/youtracktest"
)

// captureStdout runs f and returns what it printed to stdout.
func captureStdout(t *testing.T, f func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	ferr := f()
	w.Close()
	os.Stdout = old
	return <-done, ferr
}

// isolate points config, log and credentials at a temp dir.
func isolate(t *testing.T) string {
	t.Helper()
	keyring.MockInit()
	profile = "" // run() resets it; tests that skip run() must not inherit it
	dir := t.TempDir()
	t.Setenv("YMCP_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("YMCP_LOG_FILE", filepath.Join(dir, "ymcp.log"))
	t.Setenv("YMCP_LOG_LEVEL", "")
	for _, k := range []string{"YOUTRACK_URL", "YOUTRACK_LOGIN", "YOUTRACK_PASSWORD", "YOUTRACK_PROXY", "YMCP_PROFILE"} {
		t.Setenv(k, "")
	}
	return dir
}

func TestRunVersionHelpAndUnknown(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"version"}, {"--version"}, {"-version"}, {"--profile", "nope", "--version"}} {
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil || out != "ymcp dev\n" {
			t.Errorf("%v = %q, %v", args, out, err)
		}
	}
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		out, err := captureStdout(t, func() error { return run(args) })
		if err != nil || !strings.Contains(out, "Commands:") {
			t.Errorf("%v = %q, %v", args, out, err)
		}
	}
	if err := run([]string{"nope"}); err == nil || !strings.Contains(err.Error(), `unknown command "nope"`) {
		t.Errorf("unknown command err = %v", err)
	}
}

func TestCheckWithoutConfig(t *testing.T) {
	isolate(t)
	if err := run([]string{"check"}); err == nil || !strings.Contains(err.Error(), "ymcp setup") {
		t.Errorf("err = %v, want a hint to run ymcp setup", err)
	}
}

func TestCallAgainstFakeYouTrack(t *testing.T) {
	dir := isolate(t)
	f := youtracktest.New(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"login":"alice","fullName":"Alice"}`))
	}))
	envFile := filepath.Join(dir, ".env")
	os.WriteFile(envFile, []byte("YOUTRACK_URL="+f.URL+"\nYOUTRACK_LOGIN="+youtracktest.Login+"\nYOUTRACK_PASSWORD="+youtracktest.Password+"\n"), 0o600)
	t.Setenv("YOUTRACK_PROXY", "none")
	// --env only fills variables that aren't set; isolate set them empty.
	for _, k := range []string{"YOUTRACK_URL", "YOUTRACK_LOGIN", "YOUTRACK_PASSWORD"} {
		os.Unsetenv(k)
	}

	out, err := captureStdout(t, func() error { return run([]string{"--env", envFile, "call", "get_current_user"}) })
	if err != nil || !strings.Contains(out, `"login": "alice"`) {
		t.Fatalf("call = %q, %v", out, err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "ymcp.log"))
	for _, want := range []string{"signed in", "tool=get_current_user", "pid="} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}

	out, err = captureStdout(t, func() error { return run([]string{"check"}) })
	if err != nil || !strings.Contains(out, "logged in to "+f.URL+" as alice") {
		t.Errorf("check = %q, %v", out, err)
	}
	if _, err := captureStdout(t, func() error { return run([]string{"call", "nope"}) }); err == nil {
		t.Error("unknown tool accepted")
	}
	if err := run([]string{"call", "get_current_user", "{not json"}); err == nil {
		t.Error("bad JSON arguments accepted")
	}
	if err := run([]string{"call"}); err == nil {
		t.Error("call without a tool accepted")
	}
}

func TestProfileFlagPicksTheInstance(t *testing.T) {
	isolate(t)
	for _, name := range []string{"work", "home"} {
		f := youtracktest.New(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"login":%q,"fullName":"At %s"}`, name, name)
		}))
		cfg := config.Config{Name: name, URL: f.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"}
		if _, err := config.Save(cfg, ""); err != nil {
			t.Fatal(err)
		}
	}

	if err := run([]string{"check"}); err == nil || !strings.Contains(err.Error(), "home, work") {
		t.Errorf("check with two profiles = %v, want a list to pick from", err)
	}
	out, err := captureStdout(t, func() error { return run([]string{"--profile", "home", "check"}) })
	if err != nil || !strings.Contains(out, "home: logged in") || !strings.Contains(out, "as home") {
		t.Errorf("--profile home check = %q, %v", out, err)
	}
	t.Setenv("YMCP_PROFILE", "work")
	out, err = captureStdout(t, func() error { return run([]string{"call", "get_current_user"}) })
	if err != nil || !strings.Contains(out, `"login": "work"`) {
		t.Errorf("YMCP_PROFILE=work call = %q, %v", out, err)
	}
	if err := run([]string{"--profile", "nope", "check"}); err == nil || !strings.Contains(err.Error(), `"nope" is not configured`) {
		t.Errorf("unknown profile = %v", err)
	}
}
