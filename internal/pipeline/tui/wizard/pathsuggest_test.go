package wizard

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xlsft/ymcp/internal/pipeline/workspace"
)

// suggestOpts runs dirSuggest on this machine and returns the options and
// labels it offers.
func suggestOpts(t *testing.T, text, sep string, local bool) (opts, labels []string) {
	t.Helper()
	list := func(ctx context.Context, dir string) ([]workspace.Dir, error) {
		if !local { // the "host" is this machine, its ~ is $HOME
			if dir == "~/" || dir == "" {
				dir = os.Getenv("HOME")
			} else if rest, ok := strings.CutPrefix(dir, "~/"); ok {
				dir = filepath.Join(os.Getenv("HOME"), rest)
			}
		}
		return workspace.Dirs(ctx, workspace.Local(), dir)
	}
	got, err := dirSuggest(text, sep, local, list)(context.Background())
	if err != nil {
		t.Fatalf("%q: %v", text, err)
	}
	for _, s := range got {
		opts = append(opts, s.Option)
		labels = append(labels, s.Label)
	}
	return opts, labels
}

// TestDirSuggest: a lone ~ or nothing lists the home directory instead of
// panicking, ~-paths complete with the ~ kept, repositories come first
// and ".." goes up.
func TestDirSuggest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, d := range []string{"proj", "notes", ".hidden", filepath.Join("proj", ".git"), filepath.Join("notes", "deep")} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sep := string(filepath.Separator)
	up := filepath.Dir(home) + sep
	if up == sep+sep {
		up = sep
	}
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"", []string{up, "~" + sep + "proj" + sep, "~" + sep + "notes" + sep}},
		{"~", []string{up, "~" + sep + "proj" + sep, "~" + sep + "notes" + sep}},
		{"~" + sep, []string{up, "~" + sep + "proj" + sep, "~" + sep + "notes" + sep}},
		{"~" + sep + "pr", []string{"~" + sep + "proj" + sep}},
		{"~" + sep + ".", []string{"~" + sep + ".hidden" + sep}},
		{"~" + sep + "notes" + sep, []string{"~" + sep, "~" + sep + "notes" + sep + "deep" + sep}},
		{home + sep + "no", []string{home + sep + "notes" + sep}},
	} {
		if opts, _ := suggestOpts(t, tc.text, sep, true); !slices.Equal(opts, tc.want) {
			t.Errorf("dirSuggest(%q) = %q, want %q", tc.text, opts, tc.want)
		}
	}
	if _, labels := suggestOpts(t, "~"+sep+"notes"+sep, sep, true); !slices.Equal(labels, []string{"..", "deep" + sep}) {
		t.Errorf("labels = %q", labels)
	}
}

// TestDirSuggestHost: on an ssh host the path stays as typed, with /, and
// there is nothing above its ~.
func TestDirSuggestHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range []string{"code/app/.git", "code/lib"} {
		if err := os.MkdirAll(filepath.Join(home, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"~/", []string{"~/code/"}},
		{"~/code/", []string{"~/", "~/code/app/", "~/code/lib/"}},
		{"~/code/l", []string{"~/code/lib/"}},
	} {
		if opts, _ := suggestOpts(t, tc.text, "/", false); !slices.Equal(opts, tc.want) {
			t.Errorf("dirSuggest(%q) = %q, want %q", tc.text, opts, tc.want)
		}
	}
}
