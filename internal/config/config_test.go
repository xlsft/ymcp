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
	for _, k := range []string{"YOUTRACK_URL", "YOUTRACK_LOGIN", "YOUTRACK_PASSWORD", "YOUTRACK_PROXY", "YMCP_PROFILE"} {
		t.Setenv(k, "")
	}
	return path
}

func TestSaveKeepsPasswordInKeychain(t *testing.T) {
	path := setup(t)
	in := Config{Name: "youtrack", URL: "https://yt.example.com", Login: "alice", Password: "s3cret", Proxy: "none"}
	store, err := Save(in, "")
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

	got, err := Load("")
	if err != nil || got != in {
		t.Errorf("Load = %+v, %v", got, err)
	}
}

func TestEnvOverridesAndMissingFile(t *testing.T) {
	setup(t)
	got, err := Load("")
	if err != nil || got.Complete() || got.Name != DefaultProfile {
		t.Fatalf("empty Load = %+v, %v", got, err)
	}
	t.Setenv("YOUTRACK_URL", "https://env.example.com")
	t.Setenv("YOUTRACK_LOGIN", "bob")
	t.Setenv("YOUTRACK_PASSWORD", "pw")
	got, _ = Load("")
	if !got.Complete() || got.Login != "bob" {
		t.Errorf("env Load = %+v", got)
	}
}

func TestLegacyFileBecomesDefaultProfile(t *testing.T) {
	path := setup(t)
	os.WriteFile(path, []byte(`{"url":"https://old.example.com","login":"alice","proxy":"none"}`), 0o600)
	keyring.Set(keyringService, "alice@old.example.com", "pw")

	got, err := Load("")
	if err != nil || got.Name != DefaultProfile || got.URL != "https://old.example.com" || got.Password != "pw" || got.Proxy != "none" {
		t.Fatalf("legacy Load = %+v, %v", got, err)
	}
	// Saving another profile rewrites the file in the profiles format.
	if _, err := Save(Config{DisplayName: "YouTrack (beget)", URL: "https://beget.example.com", Login: "bob", Password: "b"}, ""); err != nil {
		t.Fatal(err)
	}
	list, _ := Profiles()
	if len(list) != 2 || list[0].Name != "youtrack" || list[1].Name != "youtrack-beget" || list[1].Title() != "YouTrack (beget)" {
		t.Errorf("profiles = %+v", list)
	}
}

func TestResolve(t *testing.T) {
	setup(t)
	if key, _ := Resolve(""); key != DefaultProfile {
		t.Errorf("no profiles: %q", key)
	}
	Save(Config{Name: "work", URL: "https://w.example.com", Login: "a", Password: "p"}, "")
	if key, _ := Resolve(""); key != "work" {
		t.Errorf("single profile: %q", key)
	}
	Save(Config{Name: "home", URL: "https://h.example.com", Login: "a", Password: "p"}, "")
	if _, err := Resolve(""); err == nil || !strings.Contains(err.Error(), "home, work") {
		t.Errorf("ambiguous: %v", err)
	}
	t.Setenv("YMCP_PROFILE", "home")
	if key, _ := Resolve(""); key != "home" {
		t.Errorf("YMCP_PROFILE: %q", key)
	}
	if key, _ := Resolve("work"); key != "work" {
		t.Errorf("explicit: %q", key)
	}
	Save(Config{Name: DefaultProfile, URL: "https://d.example.com", Login: "a", Password: "p"}, "")
	t.Setenv("YMCP_PROFILE", "")
	if key, _ := Resolve(""); key != DefaultProfile {
		t.Errorf("default among several: %q", key)
	}
}

func TestRenameAndDeleteKeepSharedPasswords(t *testing.T) {
	setup(t)
	Save(Config{Name: "a", URL: "https://yt.example.com", Login: "alice", Password: "pw"}, "")
	Save(Config{Name: "b", URL: "https://yt.example.com", Login: "alice", Password: "pw"}, "")

	// Rename a to c: the entry moves, the shared password stays.
	if _, err := Save(Config{Name: "c", URL: "https://yt.example.com", Login: "alice", Password: "pw"}, "a"); err != nil {
		t.Fatal(err)
	}
	list, _ := Profiles()
	if len(list) != 2 || list[0].Name != "b" || list[1].Name != "c" {
		t.Errorf("after rename = %+v", list)
	}
	if err := Delete("b"); err != nil {
		t.Fatal(err)
	}
	if pw, _ := keyring.Get(keyringService, "alice@yt.example.com"); pw != "pw" {
		t.Error("password removed while profile c still uses it")
	}
	if err := Delete("c"); err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Get(keyringService, "alice@yt.example.com"); err == nil {
		t.Error("password kept after the last profile using it was deleted")
	}
	if err := Delete("nope"); err == nil {
		t.Error("deleting a missing profile succeeded")
	}
}

func TestChangingLoginDropsOldPassword(t *testing.T) {
	setup(t)
	Save(Config{Name: "youtrack", URL: "https://yt.example.com", Login: "alice", Password: "a"}, "")
	Save(Config{Name: "youtrack", URL: "https://yt.example.com", Login: "bob", Password: "b"}, "")
	if _, err := keyring.Get(keyringService, "alice@yt.example.com"); err == nil {
		t.Error("old password still in keychain")
	}
	if pw, _ := keyring.Get(keyringService, "bob@yt.example.com"); pw != "b" {
		t.Errorf("new password = %q", pw)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"youtrack":         "youtrack",
		"YouTrack (xlsft)": "youtrack-xlsft",
		"youtrack-beget":   "youtrack-beget",
		"  Work / Prod  ":  "work-prod",
		"Ютрек":            "youtrack",
		"":                 "youtrack",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("YMCP_CONFIG", "")
	p, err := Path()
	if err != nil || filepath.Base(p) != "config.json" || filepath.Base(filepath.Dir(p)) != "ymcp" {
		t.Errorf("Path = %q, %v", p, err)
	}
}
