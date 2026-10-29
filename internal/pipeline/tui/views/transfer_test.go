package views

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

// fakeFiles is a file dialog that returns path, or err.
type fakeFiles struct {
	path   string
	err    error
	titles []string
	names  []string
}

func (f *fakeFiles) Save(title, name string) (string, error) {
	f.titles, f.names = append(f.titles, title), append(f.names, name)
	return f.path, f.err
}

func (f *fakeFiles) Open(title string) (string, error) {
	f.titles = append(f.titles, title)
	return f.path, f.err
}

type exported struct {
	path  string
	names []string
}

// withTransfer records exports and answers imports with res.
func withTransfer(f *fixture, files Files, res spec.ImportResult) (*[]exported, *[][2]string) {
	var out []exported
	var in [][2]string
	f.d.Files = files
	f.d.ExportPipelines = func(path string, names []string) error {
		out = append(out, exported{path, names})
		return nil
	}
	f.d.ImportPipelines = func(path, profile string) (spec.ImportResult, error) {
		in = append(in, [2]string{path, profile})
		return res, nil
	}
	return &out, &in
}

func TestPipelinesExport(t *testing.T) {
	f := newFixture(t)
	files := &fakeFiles{path: "/tmp/out.json"}
	out, _ := withTransfer(f, files, spec.ImportResult{})
	d := drive(t, NewPipelines(&f.d, "youtrack-work"), 120, 30)
	d.keys("?")
	d.has("x export", "X export all", "I import")
	d.keys("?")

	// x on a card exports that pipeline, suggesting a file name.
	d.keys("x")
	if len(*out) != 1 || (*out)[0].path != "/tmp/out.json" || !slices.Equal((*out)[0].names, []string{"backend"}) {
		t.Fatalf("exports = %+v", *out)
	}
	if files.names[0] != "ymcp-backend.json" {
		t.Errorf("suggested %q", files.names[0])
	}
	d.has("Exported backend to /tmp/out.json.")

	// X exports every pipeline of the profile.
	d.keys("X")
	if len(*out) != 2 || !slices.Equal((*out)[1].names, []string{"backend", "frontend"}) {
		t.Fatalf("exports = %+v", *out)
	}
	if files.names[1] != "ymcp-youtrack-work.json" {
		t.Errorf("suggested %q", files.names[1])
	}

	// A cancelled dialog does nothing.
	files.path = ""
	d.keys("x")
	if len(*out) != 2 {
		t.Fatalf("exported after cancel: %+v", *out)
	}
}

func TestPipelinesExportWithoutDialog(t *testing.T) {
	f := newFixture(t)
	out, _ := withTransfer(f, nil, spec.ImportResult{})
	d := drive(t, NewPipelines(&f.d, "youtrack-work"), 120, 30)
	m := d.pipelines()
	m.home = "/home/dev"

	// No dialog (an SSH session): the path is typed, starting at ~.
	d.keys("x")
	d.has("Export backend", "File", "~/ymcp-backend.json")
	d.keys("enter")
	if len(*out) != 1 || (*out)[0].path != filepath.Join("/home/dev", "ymcp-backend.json") {
		t.Fatalf("exports = %+v", *out)
	}

	// A long name is not cut to the 40 characters of a pipeline name.
	long := strings.Repeat("x", 40)
	m.exportFile([]string{"backend"}, "backend", exportName(long))
	if got := m.prompt.in.Value(); got != "~/"+exportName(long) {
		t.Fatalf("prompt = %q", got)
	}
	d.keys("esc")

	// A file that is there is replaced only on y.
	home := t.TempDir()
	m.home = home
	if err := os.WriteFile(filepath.Join(home, "ymcp-backend.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.keys("x", "enter")
	d.has("Replace ~/ymcp-backend.json?")
	d.keys("n")
	if len(*out) != 1 {
		t.Fatalf("replaced without asking: %+v", *out)
	}
	d.keys("x", "enter", "y")
	if len(*out) != 2 || (*out)[1].path != filepath.Join(home, "ymcp-backend.json") {
		t.Fatalf("exports = %+v", *out)
	}

	// So is a link that points nowhere.
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(home, "gone"), filepath.Join(home, "link.json")); err != nil {
			t.Fatal(err)
		}
		m.exportFile([]string{"backend"}, "backend", "link.json")
		d.keys("enter")
		d.has("Replace ~/link.json?")
		d.keys("n")
		if len(*out) != 2 {
			t.Fatalf("replaced a link without asking: %+v", *out)
		}
	}

	// A dialog that fails falls back to the same box.
	f.d.Files = &fakeFiles{err: errors.New("zenity: not found")}
	d.keys("x")
	d.has("the file dialog can't be shown", "Export backend", "~/ymcp-backend.json")
	d.keys("esc")
	if len(*out) != 2 {
		t.Fatalf("exported after esc: %+v", *out)
	}
}

func TestPipelinesImport(t *testing.T) {
	f := newFixture(t)
	res := spec.ImportResult{
		Pipelines: []spec.Imported{{From: "api", To: "api"}, {From: "backend", To: "backend-2"}},
		Presets:   []spec.Imported{{From: "mine", To: "mine"}},
	}
	_, in := withTransfer(f, &fakeFiles{path: "/tmp/in.json"}, res)
	d := drive(t, NewPipelines(&f.d, "youtrack-work"), 120, 30)
	d.keys("I")
	if len(*in) != 1 || (*in)[0] != [2]string{"/tmp/in.json", "youtrack-work"} {
		t.Fatalf("imports = %+v", *in)
	}
	d.has("Imported pipelines api and backend → backend-2, preset mine.")

	// An import that stops halfway says what it saved before the error.
	f.d.ImportPipelines = func(path, profile string) (spec.ImportResult, error) {
		return spec.ImportResult{Pipelines: []spec.Imported{{From: "api", To: "api"}}}, errors.New("disk full")
	}
	d.keys("I")
	d.has("✗ Imported pipeline api, then failed: disk full")
}

func TestImportSummaryOnError(t *testing.T) {
	err := errors.New("disk full")
	if got := importSummary(spec.ImportResult{}, err); got != "disk full" {
		t.Errorf("nothing saved: %q", got)
	}
	res := spec.ImportResult{Presets: []spec.Imported{{From: "p", To: "p"}}}
	if got, want := importSummary(res, err), "Imported preset p, then failed: disk full"; got != want {
		t.Errorf("preset saved: %q, want %q", got, want)
	}
}

func TestImportSummary(t *testing.T) {
	for _, c := range []struct {
		res  spec.ImportResult
		want string
	}{
		{spec.ImportResult{}, "The file has no pipelines."},
		{spec.ImportResult{Pipelines: []spec.Imported{{From: "a", To: "a"}}}, "Imported pipeline a."},
		{spec.ImportResult{
			Pipelines: []spec.Imported{{From: "a", To: "a"}, {From: "b", To: "b"}, {From: "c", To: "c-2"}},
			Presets:   []spec.Imported{{From: "p", To: "p-2"}, {From: "q", To: "q"}},
		}, "Imported pipelines a, b and c → c-2, presets p → p-2 and q."},
	} {
		if got := importSummary(c.res, nil); got != c.want {
			t.Errorf("importSummary = %q, want %q", got, c.want)
		}
	}
}
