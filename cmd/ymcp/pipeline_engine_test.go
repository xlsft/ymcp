package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
)

func TestPipelineAnswer(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "api")
	st := openTestStore(t)
	ctx := context.Background()
	r := newRun(t, st, "api", "A-1", store.StatusNeedsInput)
	done := newRun(t, st, "api", "A-2", store.StatusDone)

	out := mustPipeline(t, "api", "answer", "#"+itoa(r.Number), "use", "Postgres,", "please")
	if !strings.Contains(out, "Answer sent to run #1") || !strings.Contains(out, "No runner is running") {
		t.Errorf("answer output: %q", out)
	}
	items, err := st.PendingInbox(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Source != "cli" || items[0].Body != "use Postgres, please" {
		t.Errorf("inbox = %+v", items)
	}

	// "--" lets an answer start with a dash.
	mustPipeline(t, "api", "answer", "--", "1", "-- as asked")
	if items, _ = st.PendingInbox(ctx, r.ID); len(items) != 2 {
		t.Errorf("inbox has %d items", len(items))
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"api", "answer", itoa(done.Number), "late"}, "already done"},
		{[]string{"api", "answer", "9", "x"}, "no run #9"},
		{[]string{"api", "answer", "1"}, "empty"},
		{[]string{"api", "answer", "1", "  "}, "empty"},
	} {
		if _, err := pipelineOut(t, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.args, err, c.want)
		}
	}
	if items, _ = st.PendingInbox(ctx, done.ID); len(items) != 0 {
		t.Errorf("finished run got %d item(s)", len(items))
	}
}

func TestPipelineCancel(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "api")
	st := openTestStore(t)
	ctx := context.Background()
	r := newRun(t, st, "api", "A-1", store.StatusWorking)

	if out := mustPipeline(t, "api", "cancel", "1"); !strings.Contains(out, "Canceled run #1") {
		t.Errorf("cancel output: %q", out)
	}
	got, err := st.Run(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusCanceled {
		t.Errorf("status = %s", got.Status)
	}
	if _, err := pipelineOut(t, "api", "cancel", "1"); err == nil || !strings.Contains(err.Error(), "already canceled") {
		t.Errorf("second cancel: %v", err)
	}
	if _, err := pipelineOut(t, "api", "cancel", "5"); err == nil || !strings.Contains(err.Error(), "no run #5") {
		t.Errorf("cancel of a missing run: %v", err)
	}
}

func TestPipelineRunAndCheckRefuseBadPipelines(t *testing.T) {
	isolatePipelines(t)
	for _, verb := range []string{"run", "check"} {
		if _, err := pipelineOut(t, "ghost", verb); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%s of a missing pipeline: %v", verb, err)
		}
	}
	newPipeline(t, "api")
	p, _ := spec.Load("api")
	p.Agent.Provider = "bogus"
	if err := spec.Save(p); err != nil {
		t.Fatal(err)
	}
	if _, err := pipelineOut(t, "api", "run"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("run of an invalid pipeline: %v", err)
	}
	if _, err := pipelineOut(t, "api", "run", "extra"); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("run with an argument: %v", err)
	}

	// The profile does not exist here, so the check cannot pass, and
	// nothing reaches a network.
	p.Agent.Provider = "claude"
	if err := spec.Save(p); err != nil {
		t.Fatal(err)
	}
	if _, err := pipelineOut(t, "api", "check"); err == nil {
		t.Error("check passed for an unconfigured profile")
	}
	if _, err := pipelineOut(t, "api", "run"); err == nil {
		t.Error("run started for an unconfigured profile")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestPipelineDelete(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "api")
	st := openTestStore(t)
	ctx := context.Background()
	active := newRun(t, st, "api", "A-1", store.StatusWorking)
	done := newRun(t, st, "api", "A-2", store.StatusDone)

	if _, err := pipelineOut(t, "api", "delete", "1", "-y"); err == nil || !strings.Contains(err.Error(), "cancel it first") {
		t.Errorf("deleting an active run: %v", err)
	}
	if _, err := pipelineOut(t, "api", "delete", "2"); err == nil || !strings.Contains(err.Error(), "pass -y") {
		t.Errorf("deleting without -y and no terminal: %v", err)
	}
	if _, err := st.Run(ctx, done.ID); err != nil {
		t.Fatalf("deleted without asking: %v", err)
	}
	if out := mustPipeline(t, "api", "delete", "2", "-y"); !strings.Contains(out, "Deleted run #2") {
		t.Errorf("delete output: %q", out)
	}
	if _, err := st.Run(ctx, done.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("run #2 is still there: %v", err)
	}
	if _, err := st.Run(ctx, active.ID); err != nil {
		t.Errorf("run #1 is gone: %v", err)
	}
	if _, err := pipelineOut(t, "api", "delete", "2", "-y"); err == nil || !strings.Contains(err.Error(), "no run #2") {
		t.Errorf("deleting it again: %v", err)
	}
	if _, err := pipelineOut(t, "api", "delete"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no run: %v", err)
	}
}
