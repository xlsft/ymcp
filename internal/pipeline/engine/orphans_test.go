package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/store"
)

func TestParseEtime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"00:07":       7 * time.Second,
		"12:34":       12*time.Minute + 34*time.Second,
		"01:02:03":    time.Hour + 2*time.Minute + 3*time.Second,
		"3-04:05:06":  3*24*time.Hour + 4*time.Hour + 5*time.Minute + 6*time.Second,
		"10-00:00:00": 240 * time.Hour,
	} {
		if got, err := parseEtime(in); err != nil || got != want {
			t.Errorf("parseEtime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "7", "1h05", "a:b", "1-2", "1:2:3:4", "-1:00"} {
		if _, err := parseEtime(in); err == nil {
			t.Errorf("parseEtime(%q) worked", in)
		}
	}
}

// TestAgentGroupIsRecordedDuringTheTurn: the agent's process group is in
// the store while it runs and gone once its session closed.
func TestAgentGroupIsRecordedDuringTheTurn(t *testing.T) {
	h := newHarness(t, nil)
	h.ag.group = 999999 // never a live group, so nothing real is ever killed
	is := h.yt.addIssue("Group", "Open")
	seen := make(chan string, 1)
	h.ag.script(func(ft *fakeTurn) agent.Result {
		run, _ := h.st.RunByNumber(context.Background(), h.p.Name, 1)
		v, _, _ := h.st.RunState(context.Background(), run.ID, stateAgentGroup)
		seen <- v
		ft.Say("done")
		return agent.Result{}
	})
	h.start()
	run := h.waitReview(1, is.Key)
	var g agentGroup
	if err := json.Unmarshal([]byte(<-seen), &g); err != nil || g.PGID != 999999 || g.Host != "local" || g.At == 0 {
		t.Errorf("recorded group = %+v, %v", g, err)
	}
	if v, ok, _ := h.st.RunState(context.Background(), run.ID, stateAgentGroup); ok || v != "" {
		t.Errorf("group after the turn = %q", v)
	}
}

// putGroup records g as the agent group of a new run in review.
func putGroup(t *testing.T, h *harness, key string, g agentGroup) store.Run {
	t.Helper()
	ctx := context.Background()
	run, err := h.st.CreateRun(ctx, h.p.Name, "test", "id-"+key, key)
	if err != nil {
		t.Fatal(err)
	}
	run, err = h.st.ModifyRun(ctx, run.ID, func(r *store.Run) error {
		r.Status, r.Stage = store.StatusReview, stageReview
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(g)
	if err := h.st.SetRunState(ctx, run.ID, stateAgentGroup, string(b)); err != nil {
		t.Fatal(err)
	}
	return run
}
