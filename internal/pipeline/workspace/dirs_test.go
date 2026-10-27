package workspace

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// dirsTree makes a home directory with a repository, a plain and a hidden
// directory, a file and a link to a directory.
func dirsTree(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, d := range []string{"code/app/.git", "code/notes", ".config", "it's here"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "code", "file.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "code", "app"), filepath.Join(home, "code", "link")); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestDirsLocal(t *testing.T) {
	home := dirsTree(t)
	got, err := Dirs(context.Background(), Local(), filepath.Join(home, "code"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Dir{{Name: "app", Git: true}, {Name: "link", Git: true}, {Name: "notes"}}
	if !slices.Equal(got, want) {
		t.Errorf("Dirs = %v, want %v", got, want)
	}
	if got, err := Dirs(context.Background(), Local(), filepath.Join(home, "missing")); err != nil || got != nil {
		t.Errorf("missing dir: %v, %v", got, err)
	}
}

func TestDirsSSH(t *testing.T) {
	h, _ := fakeSSH(t, true)
	home := dirsTree(t)
	t.Setenv("HOME", home)
	ctx := context.Background()
	for _, tc := range []struct {
		dir  string
		want []Dir
	}{
		{"~/code", []Dir{{Name: "app", Git: true}, {Name: "link", Git: true}, {Name: "notes"}}},
		{"~", []Dir{{Name: ".config"}, {Name: "code"}, {Name: "it's here"}}},
		{"", []Dir{{Name: ".config"}, {Name: "code"}, {Name: "it's here"}}},
		{"~/missing", nil},
	} {
		got, err := Dirs(ctx, h, tc.dir)
		if err != nil {
			t.Fatalf("%q: %v", tc.dir, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("Dirs(%q) = %v, want %v", tc.dir, got, tc.want)
		}
	}
}
