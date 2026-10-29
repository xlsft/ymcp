package spec

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExportImportRoundTrip(t *testing.T) {
	setup(t)
	if err := SavePreset(userPreset("mine")); err != nil {
		t.Fatal(err)
	}
	a, b := minimal("a"), minimal("b")
	a.Prompts.Preset = "mine"
	b.Prompts.Preset = "feature" // built in: not bundled
	for _, p := range []Pipeline{a, b} {
		if err := Save(p); err != nil {
			t.Fatal(err)
		}
	}
	bundle, err := Export("a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Pipelines) != 2 || len(bundle.Presets) != 1 || bundle.Presets[0].Name != "mine" {
		t.Fatalf("Export = %+v", bundle)
	}
	path := filepath.Join(t.TempDir(), "out.json")
	if err := WriteBundle(path, bundle); err != nil {
		t.Fatal(err)
	}

	// Another machine: nothing there yet.
	setup(t)
	read, err := ReadBundle(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Import(read, "other")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pipelines) != 2 || len(res.Presets) != 1 || res.Presets[0].Renamed() {
		t.Fatalf("Import = %+v", res)
	}
	got, err := Load("a")
	if err != nil || got.Profile != "other" || got.Prompts.Preset != "mine" {
		t.Fatalf("Load(a) = %+v, %v", got, err)
	}
	if pr, err := LoadPreset("mine"); err != nil || pr.Start != userPreset("mine").Start {
		t.Fatalf("LoadPreset = %+v, %v", pr, err)
	}
}

func TestImportRenamesTakenNames(t *testing.T) {
	setup(t)
	if err := Save(minimal("a")); err != nil {
		t.Fatal(err)
	}
	same, other := userPreset("same"), userPreset("clash")
	for _, pr := range []Preset{same, other} {
		if err := SavePreset(pr); err != nil {
			t.Fatal(err)
		}
	}
	p1, p2 := minimal("a"), minimal("a")
	p1.Prompts.Preset, p2.Prompts.Preset = "same", "clash"
	changed := other
	changed.Start = "something else"
	b := Bundle{Format: BundleFormat, Version: BundleVersion, Pipelines: []Pipeline{p1, p2}, Presets: []Preset{same, changed}}

	res, err := Import(b, "")
	if err != nil {
		t.Fatal(err)
	}
	// "same" is identical and reused; "clash" differs and is saved as clash-2.
	if len(res.Presets) != 1 || res.Presets[0] != (Imported{"clash", "clash-2"}) {
		t.Fatalf("presets = %+v", res.Presets)
	}
	if len(res.Pipelines) != 2 || res.Pipelines[0].To != "a-2" || res.Pipelines[1].To != "a-3" {
		t.Fatalf("pipelines = %+v", res.Pipelines)
	}
	if p, _ := Load("a-2"); p.Prompts.Preset != "same" {
		t.Errorf("a-2 preset = %q", p.Prompts.Preset)
	}
	if p, _ := Load("a-3"); p.Prompts.Preset != "clash-2" {
		t.Errorf("a-3 preset = %q", p.Prompts.Preset)
	}
	if pr, _ := LoadPreset("clash"); pr.Start == "something else" {
		t.Error("the existing preset was overwritten")
	}
}

func TestImportPresetWithBuiltinName(t *testing.T) {
	setup(t)
	pr := userPreset("fix")
	p := minimal("a")
	p.Prompts.Preset = "fix"
	res, err := Import(Bundle{Format: BundleFormat, Version: 1, Pipelines: []Pipeline{p}, Presets: []Preset{pr}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Presets) != 1 || res.Presets[0].To != "fix-2" {
		t.Fatalf("presets = %+v", res.Presets)
	}
	if got, _ := Load("a"); got.Prompts.Preset != "fix-2" {
		t.Errorf("preset = %q", got.Prompts.Preset)
	}
}

func TestFreeNameStaysValid(t *testing.T) {
	long := strings.Repeat("x", 40)
	got, err := freeName(long, func(n string) (bool, error) { return n == long, nil })
	if err != nil || len(got) != 40 || !strings.HasSuffix(got, "-2") || CheckName(got) != nil {
		t.Fatalf("freeName = %q, %v", got, err)
	}
}

func TestFreeNameGivesUp(t *testing.T) {
	if _, err := freeName("a", func(string) (bool, error) { return true, nil }); err == nil {
		t.Fatal("no error when every name is taken")
	}
}

func TestImportFailsOnUnreadableDir(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs unix permissions")
	}
	dir := setup(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	b := Bundle{Format: BundleFormat, Version: 1, Pipelines: []Pipeline{minimal("a")}}
	if _, err := Import(b, ""); err == nil {
		t.Fatal("imported into an unreadable directory")
	}
}

func TestReadBundleRejects(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"other":  `{"format":"something","version":1,"pipelines":[]}`,
		"newer":  `{"format":"ymcp-pipelines","version":99,"pipelines":[]}`,
		"name":   `{"format":"ymcp-pipelines","version":1,"pipelines":[{"name":"Bad Name"}]}`,
		"field":  `{"format":"ymcp-pipelines","version":1,"pipelines":[],"extra":1}`,
		"preset": `{"format":"ymcp-pipelines","version":1,"pipelines":[],"presets":[{"name":"p"}]}`,
	} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadBundle(path); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
