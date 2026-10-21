package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte(`
# comment
YTT_PLAIN=a=b
export YTT_QUOTED="p@ss word"
YTT_SINGLE='x'
YTT_EXISTING=from-file
`), 0o600)
	t.Setenv("YTT_EXISTING", "from-env")
	for _, k := range []string{"YTT_PLAIN", "YTT_QUOTED", "YTT_SINGLE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"YTT_PLAIN":    "a=b",
		"YTT_QUOTED":   "p@ss word",
		"YTT_SINGLE":   "x",
		"YTT_EXISTING": "from-env",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}
