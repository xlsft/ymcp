package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSectionsSurviveProfileWrites(t *testing.T) {
	path := setup(t)
	if got, err := Section("notifications"); err != nil || got != nil {
		t.Fatalf("missing file: %s, %v", got, err)
	}
	if err := SetSection("notifications", json.RawMessage(`{"providers":{"toast":{"enabled":false}}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(Config{Name: "work", URL: "https://w.example.com", Login: "a", Password: "p"}, ""); err != nil {
		t.Fatal(err)
	}
	got, err := Section("notifications")
	if err != nil || !strings.Contains(string(got), `"enabled":false`) {
		t.Fatalf("after Save: %s, %v", got, err)
	}
	if err := SetSection("notifications", json.RawMessage(`{"providers":{}}`)); err != nil {
		t.Fatal(err)
	}
	list, err := Profiles()
	if err != nil || len(list) != 1 || list[0].Name != "work" {
		t.Fatalf("profiles after SetSection = %+v, %v", list, err)
	}
	if err := Delete("work"); err != nil {
		t.Fatal(err)
	}
	if got, _ := Section("notifications"); string(got) != `{"providers":{}}` {
		t.Errorf("after Delete: %s", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}

	if err := SetSection("notifications", nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "notifications") {
		t.Errorf("section not removed: %s", data)
	}
}

func TestSetSectionRejects(t *testing.T) {
	setup(t)
	if err := SetSection("profiles", json.RawMessage(`{}`)); err == nil {
		t.Error("profiles accepted")
	}
	if err := SetSection("notifications", json.RawMessage(`{`)); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestLegacyFieldsDroppedWithSections(t *testing.T) {
	path := setup(t)
	os.WriteFile(path, []byte(`{"url":"https://old.example.com","login":"alice","notifications":{"x":1}}`), 0o600)
	if _, err := Save(Config{Name: "work", URL: "https://w.example.com", Login: "a", Password: "p"}, ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["url"]; ok || string(top["notifications"]) == "" {
		t.Errorf("file = %s", data)
	}
	// The legacy connection became the default profile.
	if list, _ := Profiles(); len(list) != 2 || list[1].Name != DefaultProfile {
		t.Errorf("profiles = %+v", list)
	}
}
