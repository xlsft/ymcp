package spec

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupPresets isolates the config dir and returns the user presets dir.
func setupPresets(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(setup(t)), "presets")
}

func userPreset(name string) Preset {
	return Preset{Name: name, Rationale: "why " + name, Start: "start {{issue.id}}", Feedback: "fb {{feedback}}", Answer: "ans {{answer}}"}
}

func presetNamesOf(list []Preset) []string {
	var out []string
	for _, p := range list {
		out = append(out, p.Name)
	}
	return out
}

func TestPresetDirFollowsConfig(t *testing.T) {
	want := setupPresets(t)
	if got, err := PresetDir(); err != nil || got != want {
		t.Fatalf("PresetDir = %q, %v; want %q", got, err, want)
	}
}

func TestSavePresetAndAllPresets(t *testing.T) {
	dir := setupPresets(t)
	// No directory yet: only the built-ins.
	all, err := AllPresets()
	if err != nil || len(all) != len(Presets()) {
		t.Fatalf("AllPresets = %v, %v", presetNamesOf(all), err)
	}
	for _, p := range all {
		if !p.Builtin {
			t.Errorf("built-in %s is not marked", p.Name)
		}
	}

	for _, n := range []string{"zeta", "alpha"} {
		if err := SavePreset(userPreset(n)); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, "alpha.json"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("alpha.json: %v, %v", fi, err)
		}
	}
	all, err = AllPresets()
	if err != nil {
		t.Fatal(err)
	}
	want := append(presetNames(), "alpha", "zeta")
	if got := presetNamesOf(all); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("AllPresets = %v, want %v", got, want)
	}
	if a := all[len(all)-2]; a.Builtin || a != userPreset("alpha") {
		t.Errorf("alpha = %+v", a)
	}
	if len(Presets()) != 6 {
		t.Error("Presets lists user presets")
	}

	got, ok := LookupPreset("alpha")
	if !ok || got != userPreset("alpha") {
		t.Errorf("LookupPreset(alpha) = %+v, %v", got, ok)
	}
	if fix, ok := LookupPreset("fix"); !ok || !fix.Builtin {
		t.Errorf("LookupPreset(fix) = %+v, %v", fix, ok)
	}
	for name, want := range map[string]bool{"fix": true, "alpha": true, "beta": false} {
		if got, err := PresetExists(name); err != nil || got != want {
			t.Errorf("PresetExists(%s) = %v, %v", name, got, err)
		}
	}

	// Saving again replaces the user preset.
	changed := userPreset("alpha")
	changed.Start = "new start"
	if err := SavePreset(changed); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadPreset("alpha"); got.Start != "new start" {
		t.Errorf("start = %q", got.Start)
	}
}

func TestSavePresetRefuses(t *testing.T) {
	setupPresets(t)
	empty := userPreset("mine")
	empty.Feedback, empty.Answer = " ", ""
	for name, tc := range map[string]struct {
		p    Preset
		want string
	}{
		"built-in name": {userPreset("fix"), "built in"},
		"bad name":      {userPreset("My Preset"), "invalid"},
		"empty name":    {userPreset(""), "empty"},
		"no texts":      {empty, "no feedback, answer prompt"},
	} {
		if err := SavePreset(tc.p); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if got, _ := AllPresets(); len(got) != len(Presets()) {
		t.Errorf("something was saved: %v", presetNamesOf(got))
	}
}

func TestLoadPresetFileNameIsIdentity(t *testing.T) {
	dir := setupPresets(t)
	os.MkdirAll(dir, 0o700)
	raw := `{"name":"other","rationale":"r","start":"s","feedback":"f","answer":"a"}`
	os.WriteFile(filepath.Join(dir, "mine.json"), []byte(raw), 0o600)
	p, err := LoadPreset("mine")
	if err != nil || p.Name != "mine" || p.Builtin {
		t.Fatalf("LoadPreset = %+v, %v", p, err)
	}
	if _, err := LoadPreset("nope"); !errors.Is(err, ErrPresetNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := LoadPreset("../x"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("bad name: %v", err)
	}
}

func TestAllPresetsReportsBrokenFiles(t *testing.T) {
	dir := setupPresets(t)
	if err := SavePreset(userPreset("good")); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"broken.json":   `{nope`,
		"typo.json":     `{"strat":"x"}`,
		"fix.json":      `{"start":"s","feedback":"f","answer":"a"}`,
		"Bad Name.json": `{}`,
		"notes.txt":     `x`,
	} {
		os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600)
	}
	all, err := AllPresets()
	names := presetNamesOf(all)
	if names[len(names)-1] != "good" || len(names) != len(Presets())+1 {
		t.Errorf("AllPresets = %v", names)
	}
	for _, s := range []string{"broken.json", "strat", "fix.json", "built-in"} {
		if err == nil || !strings.Contains(err.Error(), s) {
			t.Errorf("error lacks %q: %v", s, err)
		}
	}
	if err != nil && strings.Contains(err.Error(), "Bad Name") {
		t.Errorf("a file with an invalid name is reported: %v", err)
	}
	if fix, _ := LookupPreset("fix"); !fix.Builtin {
		t.Error("a user file shadows the built-in fix")
	}
}

func TestCopyRenameDeletePreset(t *testing.T) {
	setupPresets(t)
	// Copy a built-in into a user preset.
	if err := CopyPreset("fix", "my-fix"); err != nil {
		t.Fatal(err)
	}
	fix, _ := LookupPreset("fix")
	mine, err := LoadPreset("my-fix")
	if err != nil || mine.Builtin || mine.Start != fix.Start || mine.Rationale != fix.Rationale {
		t.Fatalf("copy = %+v, %v", mine, err)
	}
	// Copy a user preset.
	if err := CopyPreset("my-fix", "my-fix-2"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"copy onto a user preset", CopyPreset("fix", "my-fix"), `"my-fix" already exists`},
		{"copy onto a built-in", CopyPreset("my-fix", "docs"), "built in"},
		{"copy a missing preset", CopyPreset("nope", "x"), "not found"},
		{"copy to a bad name", CopyPreset("fix", "X Y"), "invalid"},
		{"rename a built-in", RenamePreset("fix", "x"), "read-only"},
		{"rename onto a user preset", RenamePreset("my-fix", "my-fix-2"), "already exists"},
		{"rename onto a built-in", RenamePreset("my-fix", "feature"), "built in"},
		{"rename a missing preset", RenamePreset("nope", "x"), "not found"},
		{"delete a built-in", DeletePreset("fix"), "built in"},
		{"delete a missing preset", DeletePreset("nope"), "not found"},
	} {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, tc.err, tc.want)
		}
	}
	if err := DeletePreset("nope"); !errors.Is(err, ErrPresetNotFound) {
		t.Errorf("delete missing: %v", err)
	}

	// Rename to the same name is a no-op; a real rename moves the file.
	if err := RenamePreset("my-fix", "my-fix"); err != nil {
		t.Fatal(err)
	}
	if err := RenamePreset("my-fix", "team-fix"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPreset("my-fix"); !errors.Is(err, ErrPresetNotFound) {
		t.Errorf("old name still loads: %v", err)
	}
	if p, err := LoadPreset("team-fix"); err != nil || p.Start != fix.Start {
		t.Errorf("renamed = %+v, %v", p, err)
	}

	if err := DeletePreset("team-fix"); err != nil {
		t.Fatal(err)
	}
	if got := presetNamesOf(must(AllPresets())); got[len(got)-1] != "my-fix-2" || len(got) != len(Presets())+1 {
		t.Errorf("after delete: %v", got)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestPipelineWithUserPreset(t *testing.T) {
	setupPresets(t)
	p := minimal("backend")
	p.Prompts = Prompts{Preset: "mine"}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), `prompts.preset "mine"`) {
		t.Fatalf("unknown user preset: %v", err)
	}
	if err := SavePreset(userPreset("mine")); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("user preset refused: %v", err)
	}
	got := p.ResolvedPrompts()
	if want := userPreset("mine"); got.Start != want.Start || got.Feedback != want.Feedback || got.Answer != want.Answer {
		t.Errorf("resolved = %+v", got)
	}
	// Deleting the preset leaves a pipeline with its own texts valid.
	p.Prompts = Prompts{Preset: "mine", Start: "s", Feedback: "f", Answer: "a"}
	if err := DeletePreset("mine"); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("pipeline with its own texts: %v", err)
	}
}
