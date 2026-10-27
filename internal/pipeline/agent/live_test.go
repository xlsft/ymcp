package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestLive sends one tiny prompt to each real agent installed. It bills
// the user's accounts, so it runs only with YMCP_PIPELINE_LIVE=1.
func TestLive(t *testing.T) {
	if os.Getenv("YMCP_PIPELINE_LIVE") != "1" {
		t.Skip("set YMCP_PIPELINE_LIVE=1 to run against real agents")
	}
	tests := []struct {
		provider string
		opts     StartOptions
	}{
		{provider: "claude"},
		{provider: "kimi"},
		{provider: "opencode", opts: StartOptions{Model: "kimi-code-plan-global/k3"}},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if _, err := exec.LookPath(tt.provider); err != nil {
				t.Skipf("%s not installed", tt.provider)
			}
			dir := t.TempDir()
			if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			a, err := New(tt.provider, "", nil, WithModel(tt.opts.Model))
			if err != nil {
				t.Fatal(err)
			}
			info, err := a.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s %s at %s, warnings %q", tt.provider, info.Version, info.Path, info.Warnings)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			opts := tt.opts
			opts.Dir = dir
			s, err := a.Start(ctx, opts)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer s.Close()
			r := record(s)
			res, err := s.Prompt(ctx, "Reply with the single word OK and nothing else. Do not use any tools.")
			if err != nil {
				t.Fatalf("Prompt: %v (%+v)", err, res)
			}
			t.Logf("session %s: %+v; events %s", s.ID(), res, kinds(r.all()))
			if res.IsError || res.StopReason != StopEndTurn || !strings.Contains(strings.ToUpper(res.Text), "OK") {
				t.Errorf("result = %+v", res)
			}
			if s.ID() == "" {
				t.Error("no session ID")
			}
		})
	}
}
