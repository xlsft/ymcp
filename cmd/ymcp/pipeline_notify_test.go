package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xlsft/ymcp/internal/notify"
)

// These tests never check or send: Check on macOS would build the wrapper
// bundle, and a test notification would show up on the desktop.

func TestPipelineNotifySet(t *testing.T) {
	t.Setenv("YMCP_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	ctx := context.Background()
	if err := pipelineNotify(ctx, []string{"set", "toast.sound=false", "toast.events=run_done,needs_input"}); err != nil {
		t.Fatal(err)
	}
	s, err := notify.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(s.Providers["toast"]); got != `{"events":["run_done","needs_input"],"sound":false}` {
		t.Errorf("toast = %s", got)
	}
	for _, args := range [][]string{
		{"set"},
		{"set", "toast.sound"},
		{"set", "toast.events=party"},
		{"set", "nope.enabled=false"},
		{"frobnicate"},
	} {
		if err := pipelineNotify(ctx, args); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
}

func TestPipelineNotifyTestNeedsAProvider(t *testing.T) {
	t.Setenv("YMCP_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	ctx := context.Background()
	if err := pipelineNotify(ctx, []string{"set", "toast.enabled=false"}); err != nil {
		t.Fatal(err)
	}
	err := pipelineNotify(ctx, []string{"test"})
	if err == nil || !strings.Contains(err.Error(), "no notification provider is on") {
		t.Errorf("test with everything off = %v", err)
	}
	err = pipelineNotify(ctx, []string{"test", "--provider", "toast"})
	if err == nil || !strings.Contains(err.Error(), "toast is off") {
		t.Errorf("test --provider toast = %v", err)
	}
	if err := pipelineNotify(ctx, []string{"test", "--provider", "nope"}); err == nil {
		t.Error("unknown provider accepted")
	}
}

func TestProviderSettings(t *testing.T) {
	for raw, want := range map[string]string{
		``:                                 "defaults",
		`{"enabled":false,"events":["x"]}`: "defaults",
		`{"sound":false,"enabled":true}`:   "sound=false",
		`{"b":"x","a":1}`:                  `a=1 b="x"`,
	} {
		if got := providerSettings([]byte(raw)); got != want {
			t.Errorf("providerSettings(%s) = %q, want %q", raw, got, want)
		}
	}
}
