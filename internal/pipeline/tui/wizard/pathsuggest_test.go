package wizard

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestPathSuggestTilde: a lone ~ lists the home directory instead of
// panicking, and ~-paths complete with the ~ kept.
func TestPathSuggestTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, d := range []string{"proj", "notes", filepath.Join("proj", ".git")} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sep := string(filepath.Separator)
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"~", []string{"~" + sep + "proj" + sep, "~" + sep + "notes" + sep}},
		{"~" + sep, []string{"~" + sep + "proj" + sep, "~" + sep + "notes" + sep}},
		{"~" + sep + "pr", []string{"~" + sep + "proj" + sep}},
		{home + sep + "no", []string{home + sep + "notes" + sep}},
		{"", nil},
	} {
		got, err := pathSuggest(tc.text, len([]rune(tc.text)))(context.Background())
		if err != nil {
			t.Fatalf("%q: %v", tc.text, err)
		}
		var opts []string
		for _, s := range got {
			opts = append(opts, s.Option)
		}
		slices.Sort(opts)
		want := slices.Clone(tc.want)
		slices.Sort(want)
		if tc.text != "" && !slices.Equal(opts, want) {
			t.Errorf("pathSuggest(%q) = %q, want %q", tc.text, opts, want)
		}
	}
}
