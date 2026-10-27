package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

// captureStderr runs f and returns what it wrote to stderr.
func captureStderr(t *testing.T, f func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	ferr := f()
	w.Close()
	os.Stderr = old
	return <-done, ferr
}

// pipelineErrOut runs `ymcp pipeline args…` and returns its stdout and
// stderr.
func pipelineErrOut(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	stderr, err = captureStderr(t, func() error {
		var ferr error
		stdout, ferr = pipelineOut(t, args...)
		return ferr
	})
	return stdout, stderr, err
}

func TestPipelinePresetsListsUserPresets(t *testing.T) {
	isolatePipelines(t)
	out := mustPipeline(t, "presets")
	if !strings.Contains(out, "TYPE") || !strings.Contains(out, "built-in") || strings.Contains(out, " user ") {
		t.Errorf("built-ins only:\n%s", out)
	}
	if !strings.Contains(out, "presets copy fix NAME") {
		t.Errorf("no hint to make a preset:\n%s", out)
	}
	mustPipeline(t, "presets", "copy", "docs", "my-docs")
	out = mustPipeline(t, "presets")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "my-docs") || !strings.Contains(last, "user") {
		t.Errorf("last line = %q\n%s", last, out)
	}
	if strings.Contains(out, "presets copy fix NAME") {
		t.Errorf("hint shown although a user preset exists:\n%s", out)
	}
}

func TestPipelinePresetsShowCopy(t *testing.T) {
	isolatePipelines(t)
	out := mustPipeline(t, "presets", "show", "fix")
	var p spec.Preset
	if err := json.Unmarshal([]byte(out), &p); err != nil || p.Name != "fix" || p.Start == "" {
		t.Fatalf("show fix = %+v, %v\n%s", p, err, out)
	}
	if strings.Contains(out, "Builtin") {
		t.Errorf("show prints the Builtin flag:\n%s", out)
	}

	out = mustPipeline(t, "presets", "copy", "fix", "team")
	if !strings.Contains(out, "Copied preset fix to team") || !strings.Contains(out, "presets edit team") {
		t.Errorf("copy = %q", out)
	}
	team, err := spec.LoadPreset("team")
	if err != nil || team.Builtin || team.Start != p.Start {
		t.Fatalf("team = %+v, %v", team, err)
	}
	dir, _ := spec.PresetDir()
	if _, err := os.Stat(filepath.Join(dir, "team.json")); err != nil {
		t.Errorf("team.json: %v", err)
	}
	if out := mustPipeline(t, "presets", "show", "team"); !strings.Contains(out, `"name": "team"`) {
		t.Errorf("show team = %q", out)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"presets", "show"}, "usage"},
		{[]string{"presets", "show", "nope"}, "not found"},
		{[]string{"presets", "show", "Bad Name"}, "invalid"},
		{[]string{"presets", "copy", "fix"}, "usage"},
		{[]string{"presets", "copy", "fix", "team"}, "already exists"},
		{[]string{"presets", "copy", "team", "docs"}, "built in"},
		{[]string{"presets", "copy", "nope", "x"}, "not found"},
		{[]string{"presets", "fly"}, "unknown presets command"},
		{[]string{"presets", "-x"}, "usage"},
	} {
		_, err := pipelineOut(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.args, err, c.want)
		}
	}
	if out := mustPipeline(t, "presets", "help"); !strings.Contains(out, "presets save NAME --from PIPELINE") {
		t.Errorf("presets help = %q", out)
	}
}

func TestPipelinePresetsRenameRemove(t *testing.T) {
	isolatePipelines(t)
	mustPipeline(t, "presets", "copy", "fix", "mine")
	// A pipeline made from the preset copies its texts; one that only names
	// it (written by hand) depends on it.
	newPipeline(t, "copied", "--preset", "mine")
	if p, _ := spec.Load("copied"); p.Prompts.Preset != "mine" || p.Prompts.Start == "" || p.Prompts.Answer == "" {
		t.Fatalf("pipeline from a user preset = %+v", p.Prompts)
	}
	newPipeline(t, "bare")
	bare, _ := spec.Load("bare")
	bare.Prompts = spec.Prompts{Preset: "mine"}
	if err := spec.Save(bare); err != nil {
		t.Fatal(err)
	}
	// A pipeline from a built-in preset still resolves it at run time.
	newPipeline(t, "builtin", "--preset", "docs")
	if p, _ := spec.Load("builtin"); p.Prompts.Start != "" {
		t.Errorf("built-in preset texts copied: %+v", p.Prompts)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"presets", "rename", "fix", "x"}, "presets copy fix NEW"},
		{[]string{"presets", "rename", "mine"}, "usage"},
		{[]string{"presets", "rename", "nope", "x"}, "not found"},
		{[]string{"presets", "rename", "mine", "docs"}, "built in"},
		{[]string{"presets", "rm", "fix", "-y"}, "cannot be removed"},
		{[]string{"presets", "rm", "nope", "-y"}, "not found"},
		{[]string{"presets", "rm", "mine"}, "-y"},
		{[]string{"presets", "rm"}, "usage"},
	} {
		_, err := pipelineOut(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.args, err, c.want)
		}
	}

	out, warn, err := pipelineErrOut(t, "presets", "rename", "mine", "ours")
	if err != nil || !strings.Contains(out, "Renamed preset mine to ours") {
		t.Fatalf("rename = %q, %v", out, err)
	}
	if !strings.Contains(warn, "keep their own copies of its prompts: copied") || !strings.Contains(warn, "fail once it is renamed: bare") {
		t.Errorf("rename warnings = %q", warn)
	}
	if _, err := spec.LoadPreset("mine"); !errors.Is(err, spec.ErrPresetNotFound) {
		t.Errorf("old name: %v", err)
	}

	// Point the pipelines at the new name, then remove it.
	for _, name := range []string{"copied", "bare"} {
		p, _ := spec.Load(name)
		p.Prompts.Preset = "ours"
		spec.Save(p)
	}
	out, warn, err = pipelineErrOut(t, "presets", "rm", "ours", "-y")
	if err != nil || !strings.Contains(out, "Removed preset ours") {
		t.Fatalf("rm = %q, %v", out, err)
	}
	if !strings.Contains(warn, "copied") || !strings.Contains(warn, "fail once it is removed: bare") {
		t.Errorf("rm warnings = %q", warn)
	}
	if exists, _ := spec.PresetExists("ours"); exists {
		t.Error("preset still there")
	}
	// The pipeline with its own copy still validates.
	if p, _ := spec.Load("copied"); p.Validate() != nil {
		t.Errorf("copied: %v", p.Validate())
	}
	// Removing a preset nobody names warns about nothing.
	mustPipeline(t, "presets", "copy", "fix", "lonely")
	if _, warn, err := pipelineErrOut(t, "presets", "rm", "--yes", "lonely"); err != nil || warn != "" {
		t.Errorf("rm lonely: %q, %v", warn, err)
	}
}

func TestPipelinePresetsSave(t *testing.T) {
	isolatePipelines(t)
	newPipeline(t, "api", "--preset", "docs")
	p, _ := spec.Load("api")
	p.Prompts.Start = "Custom start {{issue.id}}"
	spec.Save(p)

	out := mustPipeline(t, "presets", "save", "api-prompts", "--from", "api")
	if !strings.Contains(out, "Saved the prompts of pipeline api as preset api-prompts") {
		t.Errorf("save = %q", out)
	}
	docs, _ := spec.LoadPreset("docs")
	got, err := spec.LoadPreset("api-prompts")
	if err != nil || got.Start != "Custom start {{issue.id}}" || got.Feedback != docs.Feedback || got.Answer != docs.Answer ||
		!strings.Contains(got.Rationale, "pipeline api") {
		t.Fatalf("saved = %+v, %v", got, err)
	}
	mustPipeline(t, "presets", "save", "--from", "api", "--rationale", "For API work", "api-2")
	if got, _ := spec.LoadPreset("api-2"); got.Rationale != "For API work" {
		t.Errorf("rationale = %q", got.Rationale)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"presets", "save", "x"}, "missing --from"},
		{[]string{"presets", "save", "--from", "api"}, "usage"},
		{[]string{"presets", "save", "fix", "--from", "api"}, "built in"},
		{[]string{"presets", "save", "api-2", "--from", "api"}, "already exists"},
		{[]string{"presets", "save", "x", "--from", "nope"}, "not found"},
		{[]string{"presets", "save", "Bad", "--from", "api"}, "invalid"},
	} {
		_, err := pipelineOut(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.args, err, c.want)
		}
	}
}

func TestPipelinePresetsEdit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake editor is a shell script")
	}
	isolatePipelines(t)
	bin := t.TempDir()
	editor := func(script string) {
		path := filepath.Join(bin, "editor.sh")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("VISUAL", "")
		t.Setenv("EDITOR", path)
	}
	editor(`exit 0`)
	if _, err := pipelineOut(t, "presets", "edit", "fix"); err == nil || !strings.Contains(err.Error(), "presets copy fix NEW") {
		t.Errorf("edit built-in: %v", err)
	}
	if _, err := pipelineOut(t, "presets", "edit", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("edit missing: %v", err)
	}
	if err := spec.SavePreset(spec.Preset{Name: "mine", Rationale: "Mine", Start: "s {{issue.id}}", Feedback: "f", Answer: "a"}); err != nil {
		t.Fatal(err)
	}
	if out := mustPipeline(t, "presets", "edit", "mine"); !strings.Contains(out, "No changes") {
		t.Errorf("unchanged edit: %q", out)
	}

	editor(`sed 's/"rationale": "[^"]*"/"rationale": "Mine now"/; s/"name": "mine"/"name": "other"/' "$1" > "$1.new" && mv "$1.new" "$1"`)
	if out := mustPipeline(t, "presets", "edit", "mine"); !strings.Contains(out, "Saved preset mine") {
		t.Errorf("edit: %q", out)
	}
	if p, err := spec.LoadPreset("mine"); err != nil || p.Rationale != "Mine now" {
		t.Errorf("after edit = %+v, %v", p, err)
	}
	if exists, _ := spec.PresetExists("other"); exists {
		t.Error("a name changed inside the file renamed the preset")
	}

	for name, script := range map[string]string{
		"bad json":    `echo '{nope' > "$1"`,
		"unknown key": `sed 's/"rationale"/"rational"/' "$1" > "$1.new" && mv "$1.new" "$1"`,
		"empty start": `sed 's/"start": "[^"]*"/"start": ""/' "$1" > "$1.new" && mv "$1.new" "$1"`,
	} {
		editor(script)
		_, err := pipelineOut(t, "presets", "edit", "mine")
		if err == nil || !strings.Contains(err.Error(), "not saved") {
			t.Errorf("%s: err = %v", name, err)
		}
		if p, err := spec.LoadPreset("mine"); err != nil || p.Rationale != "Mine now" || p.Start == "" {
			t.Errorf("%s: preset changed: %+v, %v", name, p, err)
		}
	}
}
