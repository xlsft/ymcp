package engine

import (
	"context"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
)

func fastStopPoll(t *testing.T) {
	t.Helper()
	old := stopPoll
	stopPoll = 10 * time.Millisecond
	t.Cleanup(func() { stopPoll = old })
}

func TestStopRequestEndsRun(t *testing.T) {
	fastStopPoll(t)
	h := newHarness(t, nil)
	ctx := context.Background()
	// A request from before the runner started is not for this runner.
	if err := RequestStop(ctx, h.st, h.p.Name); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	h.start()
	h.waitFor("the runner lock", func() bool {
		_, _, _, ok, err := h.st.Runner(ctx, h.p.Name)
		return ok && err == nil
	})
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-h.done:
		t.Fatalf("a stale stop request stopped the runner: %v", err)
	default:
	}

	if err := RequestStop(ctx, h.st, h.p.Name); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("the runner did not stop")
	}
	h.cancel = nil
	if _, _, _, ok, err := h.st.Runner(ctx, h.p.Name); ok || err != nil {
		t.Errorf("the runner lock is still held: %v, %v", ok, err)
	}
	// The request is consumed, so the next runner starts normally.
	if ok, err := h.st.StopRequested(ctx, h.p.Name, time.Time{}); ok || err != nil {
		t.Errorf("the stop request was not cleared: %v, %v", ok, err)
	}
}

func TestStopRequestStopsAgents(t *testing.T) {
	fastStopPoll(t)
	h := newHarness(t, nil)
	h.yt.addIssue("Long one", "Open")
	h.ag.script(func(t *fakeTurn) agent.Result {
		t.Hang()
		return agent.Result{}
	})
	h.start()
	h.waitFor("the turn", func() bool { return len(h.ag.promptLog()) == 1 })
	if err := RequestStop(context.Background(), h.st, h.p.Name); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("the runner did not stop while a turn was running")
	}
	h.cancel = nil
	// The run is left active for the next runner to resume.
	if r := h.run(1); r.Status.Final() {
		t.Errorf("status = %s", r.Status)
	}
}
