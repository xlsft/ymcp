package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func setup(t *testing.T) string {
	t.Helper()
	keyring.MockInit()
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("YMCP_CONFIG", path)
	for _, k := range []string{"YOUTRACK_URL", "YOUTRACK_LOGIN", "YOUTRACK_PASSWORD", "YOUTRACK_PROXY"} {
		t.Setenv(k, "")
	}
	return path
}

func TestSaveKeepsPasswordInKeychain(t *testing.T) {
	path := setup(t)
	in := Config{URL: "https://yt.example.com", Login: "alice", Password: "s3cret", Proxy: "none"}
	store, err := Save(in)
	if err != nil || store != "keychain" {
		t.Fatalf("Save = %q, %v", store, err)
	}

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "s3cret") {
		t.Errorf("password leaked into %s: %s", path, data)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}

	got, err := Load()
	if err != nil || got != in {
		t.Errorf("Load = %+v, %v", got, err)
	}
}

func TestEnvOverridesAndMissingFile(t *testing.T) {
	setup(t)
	got, err := Load()
	if err != nil || got.Complete() {
		t.Fatalf("empty Load = %+v, %v", got, err)
	}
	t.Setenv("YOUTRACK_URL", "https://env.example.com")
	t.Setenv("YOUTRACK_LOGIN", "bob")
	t.Setenv("YOUTRACK_PASSWORD", "pw")
	got, _ = Load()
	if !got.Complete() || got.Login != "bob" {
		t.Errorf("env Load = %+v", got)
	}
}

func TestChangingLoginDropsOldPassword(t *testing.T) {
	setup(t)
	Save(Config{URL: "https://yt.example.com", Login: "alice", Password: "a"})
	Save(Config{URL: "https://yt.example.com", Login: "bob", Password: "b"})
	if _, err := keyring.Get(keyringService, "alice@yt.example.com"); err == nil {
		t.Error("old password still in keychain")
	}
	if pw, _ := keyring.Get(keyringService, "bob@yt.example.com"); pw != "b" {
		t.Errorf("new password = %q", pw)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("YMCP_CONFIG", "")
	p, err := Path()
	if err != nil || filepath.Base(p) != "config.json" || filepath.Base(filepath.Dir(p)) != "ymcp" {
		t.Errorf("Path = %q, %v", p, err)
	}
}
