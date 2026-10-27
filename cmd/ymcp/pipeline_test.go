package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
)

// isolatePipelines points config and state at temp dirs.
func isolatePipelines(t *testing.T) {
	t.Helper()
	isolate(t)
	t.Setenv("YMCP_STATE", filepath.Join(t.TempDir(), "state.db"))
}

// pipelineOut runs `ymcp pipeline args…` and returns what it printed.
func pipelineOut(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return captureStdout(t, func() error {
		return pipelineCmd(context.Background(), args, nil)
	})
}

func mustPipeline(t *testing.T, args ...string) string {
	t.Helper()
	out, err := pipelineOut(t, args...)
	if err != nil {
		t.Fatalf("pipeline %v: %v\n%s", args, err, out)
	}
	return out
}

func newPipeline(t *testing.T, name string, extra ...string) {
	t.Helper()
	args := append([]string{"new", name, "--profile", "work", "--dir", t.TempDir(), "--query", "project: A #Unresolved"}, extra...)
	mustPipeline(t, args...)
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newRun creates a run and moves it to the status.
func newRun(t *testing.T, st *store.Store, pipeline, issue string, status store.RunStatus) store.Run {
	t.Helper()
	ctx := context.Background()
	r, err := st.CreateRun(ctx, pipeline, "work", issue, issue)
	if err != nil {
		t.Fatal(err)
	}
	r.Status, r.Stage, r.Branch = status, "working", issue+"-fix"
	if status.Final() {
		now := time.Now()
		r.FinishedAt = &now
	}
	if err := st.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPipelineHelp(t *testing.T) {
	isolatePipelines(t)
	for _, args := range [][]string{{}, {"help"}, {"--help"}, {"-h"}} {
		out := mustPipeline(t, args...)
		if !strings.Contains(out, "ymcp pipeline list") || !strings.Contains(out, "NAME run") {
			t.Errorf("%v: %q", args, out)
		}
	}
	out, err := captureStdout(t, func() error { return run([]string{"pipeline"}) })
	if err != nil || !strings.Contains(out, "ymcp pipeline NAME check") {
		t.Errorf("run(pipeline) = %q, %v", out, err)
	}
	if !strings.Contains(usage, "pipeline") {
		t.Error("main usage does not mention pipeline")
	}
}

func TestPipelineBadArguments(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "backend")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"backend"}, "missing command"},
		{[]string{"backend", "fly"}, "unknown pipeline command"},
		{[]string{"--bogus"}, "unknown flag"},
		{[]string{"Bad Name", "show"}, "invalid"},
		{[]string{"list", "extra"}, "unexpected argument"},
		{[]string{"list", "-x"}, "usage"},
		{[]string{"backend", "show", "extra"}, "unexpected argument"},
		{[]string{"backend", "runs", "17"}, "unexpected argument"},
		{[]string{"backend", "rename"}, "usage"},
		{[]string{"backend", "copy", "a", "b"}, "usage"},
		{[]string{"backend", "logs", "1", "2"}, "usage"},
		{[]string{"backend", "logs", "x"}, "not a run number"},
		{[]string{"backend", "logs", "0"}, "not a run number"},
		{[]string{"backend", "logs", "1"}, "no run #1"},
		{[]string{"backend", "cancel"}, "usage"},
		{[]string{"backend", "answer"}, "usage"},
		{[]string{"backend", "answer", "1", "yes"}, "no run #1"},
		{[]string{"nosuch", "show"}, "not found"},
		{[]string{"nosuch", "runs"}, "not found"},
		{[]string{"nosuch", "status"}, "not found"},
		{[]string{"nosuch", "rm", "-y"}, "not found"},
		{[]string{"list", "new"}, "unexpected argument"},
	} {
		_, err := pipelineOut(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.args, err, c.want)
		}
	}
}

func TestParseRunNumber(t *testing.T) {
	for in, want := range map[string]int{"17": 17, "#3": 3, "1": 1} {
		if got, err := parseRunNumber(in); err != nil || got != want {
			t.Errorf("parseRunNumber(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0", "-2", "x", "#", "1.5"} {
		if _, err := parseRunNumber(in); err == nil {
			t.Errorf("parseRunNumber(%q) succeeded", in)
		}
	}
}

func TestPipelineNewShow(t *testing.T) {
	isolatePipelines(t)
	dir := t.TempDir()
	out := mustPipeline(t, "new", "backend", "--profile", "work", "--dir", dir, "--query", "project: BE #Unresolved", "--agent", "kimi", "--preset", "feature")
	if !strings.Contains(out, "Created pipeline backend") || !strings.Contains(out, "ymcp pipeline backend check") {
		t.Errorf("new output: %q", out)
	}
	p, err := spec.Load("backend")
	if err != nil {
		t.Fatal(err)
	}
	if p.Profile != "work" || p.Source.Query != "project: BE #Unresolved" || p.Workspace.Dir != dir ||
		p.Agent.Provider != "kimi" || p.Prompts.Preset != "feature" || p.Stages.Working.Enter == "" || p.Strategy.Kind != "branch" {
		t.Errorf("pipeline = %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("new pipeline does not validate: %v", err)
	}
	// Stage commands valid in YouTrack's default workflow; closed starts empty.
	want := spec.Stages{
		Working:    spec.Stage{Enter: "State In Progress", Match: "State: {In Progress}"},
		Review:     spec.Stage{Enter: "State Fixed", Match: "State: Fixed"},
		NeedsInput: spec.Stage{Enter: "tag needs-input", Exit: "untag needs-input", Match: "tag: needs-input"},
	}
	got := p.Stages
	got.Done = spec.Stage{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stages = %+v, want %+v", got, want)
	}
	if p.Agent.Mode != "" || len(p.Agent.Args) != 0 {
		t.Errorf("kimi got claude's defaults: %+v", p.Agent)
	}

	var shown spec.Pipeline
	if err := json.Unmarshal([]byte(mustPipeline(t, "backend", "show")), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Name != "backend" || shown.Workspace.Dir != dir || shown.Agent.Provider != "kimi" {
		t.Errorf("show = %+v", shown)
	}
}

func TestPipelineNewClaudeDefaults(t *testing.T) {
	isolatePipelines(t)
	mustPipeline(t, "new", "web", "--profile", "work", "--dir", t.TempDir(), "--query", "q")
	p, err := spec.Load("web")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"--dangerously-skip-permissions"}
	if p.Agent.Provider != "claude" || p.Agent.Mode != "" || !slices.Equal(p.Agent.Args, wantArgs) {
		t.Errorf("agent = %+v", p.Agent)
	}
}

func TestPipelineNewFlagsAfterName(t *testing.T) {
	isolatePipelines(t)
	// Flags may come before or after the name, and a relative dir is made absolute.
	mustPipeline(t, "new", "--profile", "work", "web", "--dir", "repo", "--query", "q")
	p, err := spec.Load("web")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p.Workspace.Dir) || filepath.Base(p.Workspace.Dir) != "repo" {
		t.Errorf("dir = %q", p.Workspace.Dir)
	}
}

func TestPipelineNewGlobalProfileAndHome(t *testing.T) {
	isolatePipelines(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	profile = "from-global"
	defer func() { profile = "" }()
	mustPipeline(t, "new", "a", "--dir", "~/code", "--query", "q")
	p, err := spec.Load("a")
	if err != nil {
		t.Fatal(err)
	}
	if p.Profile != "from-global" || p.Workspace.Dir != filepath.Join(home, "code") {
		t.Errorf("profile %q, dir %q", p.Profile, p.Workspace.Dir)
	}
}

func TestPipelineNewErrors(t *testing.T) {
	isolatePipelines(t)
	dir := t.TempDir()
	base := []string{"new", "a", "--profile", "p", "--dir", dir, "--query", "q"}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"new"}, "exactly one name"},
		{[]string{"new", "a", "b"}, "exactly one name"},
		{[]string{"new", "list", "--profile", "p", "--dir", dir, "--query", "q"}, "reserved"},
		{[]string{"new", "A!"}, "invalid"},
		{[]string{"new", "a", "--dir", dir}, "missing --profile, --query"},
		{[]string{"new", "a", "--profile", "p"}, "missing --dir, --query"},
		{append(append([]string{}, base...), "--agent", "bogus"), "agent.provider"},
		{append(append([]string{}, base...), "--agent", "acp"), "agent.command"},
		{append(append([]string{}, base...), "--preset", "nope"), "unknown preset"},
		{append(append([]string{}, base...), "--nope"), "usage"},
	} {
		_, err := pipelineOut(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.args, err, c.want)
		}
	}
	if list, _ := spec.List(); len(list) != 0 {
		t.Errorf("failed new left %d pipeline(s)", len(list))
	}

	mustPipeline(t, base...)
	if _, err := pipelineOut(t, base...); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second new: %v", err)
	}
	mustPipeline(t, "new", "z", "--profile", "p", "--dir", dir, "--query", "q", "--agent", "acp", "--command", "my-agent", "--host", "box")
	p, _ := spec.Load("z")
	if p.Agent.Command != "my-agent" || p.Workspace.Host != "box" {
		t.Errorf("z = %+v", p)
	}
}

func TestPipelineCopyRenameRemove(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "one")
	mustPipeline(t, "one", "copy", "two")
	one, _ := spec.Load("one")
	two, err := spec.Load("two")
	if err != nil {
		t.Fatal(err)
	}
	if two.Name != "two" || two.Source.Query != one.Source.Query || two.Workspace.Dir != one.Workspace.Dir {
		t.Errorf("copy = %+v", two)
	}
	if _, err := pipelineOut(t, "one", "copy", "two"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("copy over existing: %v", err)
	}
	if _, err := pipelineOut(t, "one", "copy", "presets"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("copy to reserved: %v", err)
	}
	if _, err := pipelineOut(t, "missing", "copy", "x"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("copy of missing: %v", err)
	}

	mustPipeline(t, "two", "rename", "three")
	if _, err := spec.Load("two"); err == nil {
		t.Error("old name still loads")
	}
	if p, err := spec.Load("three"); err != nil || p.Name != "three" {
		t.Errorf("renamed = %+v, %v", p, err)
	}
	if _, err := pipelineOut(t, "three", "rename", "one"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("rename over existing: %v", err)
	}
	if _, err := pipelineOut(t, "three", "rename", "new"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("rename to reserved: %v", err)
	}
	if _, err := pipelineOut(t, "ghost", "rename", "x"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("rename of missing: %v", err)
	}

	// Without a terminal, rm needs -y.
	if _, err := pipelineOut(t, "three", "rm"); err == nil || !strings.Contains(err.Error(), "-y") {
		t.Errorf("rm without -y: %v", err)
	}
	if _, err := spec.Load("three"); err != nil {
		t.Errorf("rm without -y removed the pipeline: %v", err)
	}
	mustPipeline(t, "three", "rm", "-y")
	mustPipeline(t, "one", "rm", "--yes")
	if list, _ := spec.List(); len(list) != 0 {
		t.Errorf("left %d pipeline(s)", len(list))
	}
}

func TestPipelineRenameRefusedWithActiveRuns(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "busy")
	st := openTestStore(t)
	newRun(t, st, "busy", "A-1", store.StatusWorking)
	if _, err := pipelineOut(t, "busy", "rename", "idle"); err == nil || !strings.Contains(err.Error(), "active run") {
		t.Errorf("rename with an active run: %v", err)
	}
	if _, err := spec.Load("busy"); err != nil {
		t.Errorf("pipeline lost: %v", err)
	}
}

func TestPipelineRemoveRefusedWithLiveRunner(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "live")
	st := openTestStore(t)
	if err := st.AcquireRunner(context.Background(), "live", os.Getpid(), "testhost", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := pipelineOut(t, "live", "rm", "-y"); err == nil || !strings.Contains(err.Error(), "runner") {
		t.Errorf("rm with a live runner: %v", err)
	}
	if err := st.ReleaseRunner(context.Background(), "live", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	mustPipeline(t, "live", "rm", "-y")
}

func TestPipelinePresets(t *testing.T) {
	isolatePipelines(t)
	out := mustPipeline(t, "presets")
	for _, p := range spec.Presets() {
		if !strings.Contains(out, p.Name) {
			t.Errorf("presets output lacks %q:\n%s", p.Name, out)
		}
	}
	if !strings.Contains(out, "fix (default) ") || !strings.Contains(out, "built-in") {
		t.Errorf("default preset not marked:\n%s", out)
	}
	if strings.Contains(out, "\n\n") {
		t.Errorf("blank lines in output:\n%s", out)
	}
}

func TestPipelineListEmptyAndFull(t *testing.T) {
	isolatePipelines(t)
	if out := mustPipeline(t, "list"); !strings.Contains(out, "No pipelines yet") || !strings.Contains(out, "pipeline new") {
		t.Errorf("empty list: %q", out)
	}
	newPipeline(t, "beta")
	newPipeline(t, "alpha")
	st := openTestStore(t)
	newRun(t, st, "alpha", "A-1", store.StatusWorking)
	newRun(t, st, "alpha", "A-2", store.StatusNeedsInput)
	newRun(t, st, "alpha", "A-3", store.StatusDone)
	if err := st.AcquireRunner(context.Background(), "alpha", os.Getpid(), "h", time.Minute); err != nil {
		t.Fatal(err)
	}

	out := mustPipeline(t, "list")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("list = %q", out)
	}
	for i, want := range [][]string{{"NAME", "PROFILE", "AGENT", "RUNNER", "ACTIVE"}, {"alpha", "work", "Claude", "running", "1 needs_input", "1 working"}, {"beta", "work", "Claude", "–"}} {
		for _, w := range want {
			if !strings.Contains(lines[i], w) {
				t.Errorf("line %d = %q, want %q", i, lines[i], w)
			}
		}
	}
	if strings.Contains(lines[1], "done") {
		t.Errorf("finished run counted as active: %q", lines[1])
	}
}

func TestPipelineListReportsBrokenFiles(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "good")
	dir, _ := spec.Dir()
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := mustPipeline(t, "list")
	if !strings.Contains(out, "good") || strings.Contains(out, "bad ") {
		t.Errorf("list = %q", out)
	}
}

func TestPipelineRunsStatus(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "api")
	if out := mustPipeline(t, "api", "runs"); !strings.Contains(out, "No active runs") {
		t.Errorf("runs on empty: %q", out)
	}
	if out := mustPipeline(t, "api", "runs", "--all"); !strings.Contains(out, "No runs yet") {
		t.Errorf("runs --all on empty: %q", out)
	}
	if out := mustPipeline(t, "api", "status"); !strings.Contains(out, "not running") || !strings.Contains(out, "none active") {
		t.Errorf("status on empty: %q", out)
	}

	st := openTestStore(t)
	ctx := context.Background()
	r1 := newRun(t, st, "api", "A-1", store.StatusReview)
	r2 := newRun(t, st, "api", "A-2", store.StatusNeedsInput)
	r2.NeedsInputReason = "which\ndatabase?"
	if err := st.UpdateRun(ctx, r2); err != nil {
		t.Fatal(err)
	}
	newRun(t, st, "api", "A-3", store.StatusClosed)
	newRun(t, st, "other", "B-1", store.StatusWorking)
	newRun(t, st, "api", "A-4", store.StatusNeedsInput) // no reason recorded
	_ = r1

	out := mustPipeline(t, "api", "runs")
	for _, want := range []string{"RUN", "ISSUE", "STATUS", "STAGE", "BRANCH", "UPDATED", "#1", "A-1", "review", "#2", "A-2-fix", "just now",
		"? #2 needs input: which database?", "? #4 needs input: waiting for an answer", "answer with: ymcp pipeline api answer RUN TEXT"} {
		if !strings.Contains(out, want) {
			t.Errorf("runs lacks %q:\n%s", want, out)
		}
	}
	// The status comes before the stage the run waits in.
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 3 && f[0] == "#2" && (f[2] != "needs_input" || f[3] != "working") {
			t.Errorf("row #2 = %q", line)
		}
		if f := strings.Fields(line); len(f) > 3 && f[0] == "RUN" && (f[2] != "STATUS" || f[3] != "STAGE") {
			t.Errorf("header = %q", line)
		}
	}
	if strings.Contains(out, "A-3") || strings.Contains(out, "B-1") {
		t.Errorf("runs shows a finished or foreign run:\n%s", out)
	}
	if all := mustPipeline(t, "api", "runs", "--all"); !strings.Contains(all, "A-3") || strings.Contains(all, "B-1") {
		t.Errorf("runs --all:\n%s", all)
	}

	// Status tells when the runner went quiet.
	if err := st.AcquireRunner(ctx, "api", 4242, "box", time.Minute); err != nil {
		t.Fatal(err)
	}
	if out := mustPipeline(t, "api", "status"); !strings.Contains(out, "running (pid 4242 on box") || !strings.Contains(out, "2 needs_input, 1 review") {
		t.Errorf("status:\n%s", out)
	}
}

func TestRunnerLineStale(t *testing.T) {
	isolatePipelines(t)
	st := openTestStore(t)
	ctx := context.Background()
	line, live, err := runnerLine(ctx, st, "x")
	if err != nil || live || line != "not running" {
		t.Errorf("no runner: %q %v %v", line, live, err)
	}
	if err := st.AcquireRunner(ctx, "x", 7, "h", time.Minute); err != nil {
		t.Fatal(err)
	}
	if line, live, _ = runnerLine(ctx, st, "x"); !live || !strings.HasPrefix(line, "running") {
		t.Errorf("fresh runner: %q %v", line, live)
	}
}

func TestPipelineLogs(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "api")
	st := openTestStore(t)
	ctx := context.Background()
	r := newRun(t, st, "api", "A-1", store.StatusWorking)

	if out := mustPipeline(t, "api", "logs", "1"); !strings.Contains(out, "No events yet") {
		t.Errorf("empty logs: %q", out)
	}
	for _, e := range []struct {
		kind string
		data any
	}{
		{"agent.text", map[string]any{"text": "Looking at\nthe code"}},
		{"tool", map[string]any{"title": "Edit main.go", "status": "ok"}},
		{"usage", map[string]any{"input": 12, "output": 3}},
		{"note", "plain string"},
		{"empty", nil},
	} {
		if err := st.AddEvent(ctx, r.ID, e.kind, e.data); err != nil {
			t.Fatal(err)
		}
	}
	out := mustPipeline(t, "api", "logs", "#1")
	for _, want := range []string{"agent.text  Looking at\n    the code", "tool  Edit main.go", `usage  {"input":12,"output":3}`, "note  plain string"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lacks %q:\n%s", want, out)
		}
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); !strings.HasSuffix(lines[len(lines)-1], "empty") {
		t.Errorf("last line = %q", lines[len(lines)-1])
	}

	tail := mustPipeline(t, "api", "logs", "1", "-n", "2")
	if strings.Contains(tail, "agent.text") || !strings.Contains(tail, "note") || !strings.Contains(tail, "empty") {
		t.Errorf("logs -n 2:\n%s", tail)
	}
	if all := mustPipeline(t, "api", "logs", "-n", "0", "1"); !strings.Contains(all, "agent.text") {
		t.Errorf("logs -n 0:\n%s", all)
	}
}

func TestPipelineLogsFollowEndsWithRun(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "api")
	st := openTestStore(t)
	ctx := context.Background()
	r := newRun(t, st, "api", "A-1", store.StatusWorking)
	if err := st.AddEvent(ctx, r.ID, "old", map[string]any{"text": "before"}); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(700 * time.Millisecond)
		st.AddEvent(ctx, r.ID, "new", map[string]any{"text": "while following"})
		r.Status = store.StatusDone
		now := time.Now()
		r.FinishedAt = &now
		st.UpdateRun(ctx, r)
	}()
	done := make(chan struct{})
	var out string
	var err error
	go func() {
		out, err = pipelineOut(t, "api", "logs", "1", "-f")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("logs -f did not end after the run finished")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "before") || !strings.Contains(out, "while following") {
		t.Errorf("logs -f:\n%s", out)
	}
}

func TestPipelineLogsFollowStopsOnContext(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "api")
	st := openTestStore(t)
	newRun(t, st, "api", "A-1", store.StatusWorking)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	_, err := captureStdout(t, func() error { return pipelineCmd(ctx, []string{"api", "logs", "1", "-f"}, nil) })
	if err != nil {
		t.Fatal(err)
	}
}

func TestPipelineEdit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake editor is a shell script")
	}
	isolatePipelines(t)
	newPipeline(t, "api")
	bin := t.TempDir()
	editor := func(script string) {
		path := filepath.Join(bin, "editor.sh")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("VISUAL", "")
		t.Setenv("EDITOR", path)
	}

	// No change, nothing is written.
	editor(`exit 0`)
	if out := mustPipeline(t, "api", "edit"); !strings.Contains(out, "No changes") {
		t.Errorf("unchanged edit: %q", out)
	}

	// A valid edit is saved.
	editor(`sed 's/project: A #Unresolved/project: Z/' "$1" > "$1.new" && mv "$1.new" "$1"`)
	if out := mustPipeline(t, "api", "edit"); !strings.Contains(out, "Saved pipeline api") {
		t.Errorf("edit: %q", out)
	}
	if p, _ := spec.Load("api"); p.Source.Query != "project: Z" {
		t.Errorf("query = %q", p.Source.Query)
	}

	// A name changed inside the file does not rename the pipeline.
	editor(`sed 's/"name": "api"/"name": "other"/' "$1" > "$1.new" && mv "$1.new" "$1"`)
	mustPipeline(t, "api", "edit")
	if list, _ := spec.List(); len(list) != 1 || list[0].Name != "api" {
		t.Errorf("pipelines after a name edit: %+v", list)
	}

	// Invalid JSON and invalid values are refused and nothing is saved.
	for name, script := range map[string]string{
		"bad json":     `echo '{nope' > "$1"`,
		"unknown key":  `sed 's/"profile"/"profil"/' "$1" > "$1.new" && mv "$1.new" "$1"`,
		"bad provider": `sed 's/"provider": "claude"/"provider": "bogus"/' "$1" > "$1.new" && mv "$1.new" "$1"`,
	} {
		editor(script)
		_, err := pipelineOut(t, "api", "edit")
		if err == nil || !strings.Contains(err.Error(), "not saved") {
			t.Errorf("%s: err = %v", name, err)
		}
		if p, err := spec.Load("api"); err != nil || p.Agent.Provider != "claude" || p.Source.Query != "project: Z" {
			t.Errorf("%s: pipeline changed: %+v, %v", name, p, err)
		}
	}

	// A failing editor is an error.
	editor(`exit 3`)
	if _, err := pipelineOut(t, "api", "edit"); err == nil || !strings.Contains(err.Error(), "editor") {
		t.Errorf("failing editor: %v", err)
	}
}

func TestEditorCommand(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	if got := editorCommand(); len(got) != 1 {
		t.Errorf("default editor = %v", got)
	}
	t.Setenv("EDITOR", "nano -w")
	if got := editorCommand(); strings.Join(got, " ") != "nano -w" {
		t.Errorf("EDITOR = %v", got)
	}
	t.Setenv("VISUAL", "code --wait")
	if got := editorCommand(); strings.Join(got, " ") != "code --wait" {
		t.Errorf("VISUAL = %v", got)
	}
}

func TestPipelineForeground(t *testing.T) {
	for args, want := range map[string]bool{"a run": true, "a status": false, "list": false, "": false, "a": false} {
		if got := pipelineForeground(strings.Fields(args)); got != want {
			t.Errorf("pipelineForeground(%q) = %v", args, got)
		}
	}
}
