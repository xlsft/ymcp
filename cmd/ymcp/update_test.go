package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xlsft/ymcp/internal/update"
)

func TestStartupUpdateCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/xlsft/ymcp/releases/tag/v0.5.0", http.StatusFound)
	}))
	defer srv.Close()
	oldWeb, oldVersion := releasesWeb, version
	t.Cleanup(func() { releasesWeb, version = oldWeb, oldVersion })
	releasesWeb = srv.URL

	check := func(v string) string {
		version = v
		var logs bytes.Buffer
		<-checkForUpdate(context.Background(), slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		return logs.String()
	}
	t.Setenv("YMCP_UPDATE_CHECK", "")
	t.Setenv("YMCP_INSTALL", "")
	if got := check("0.4.0"); !strings.Contains(got, `msg="update available" current=0.4.0 latest=0.5.0 run="ymcp update"`) {
		t.Errorf("old version:\n%s", got)
	}
	if got := check("0.5.0"); !strings.Contains(got, "up to date") || strings.Contains(got, "update available") {
		t.Errorf("latest version:\n%s", got)
	}
	t.Setenv("YMCP_INSTALL", "docker")
	if got := check("0.4.0"); !strings.Contains(got, `run="docker pull xlsoftware/ymcp:latest"`) {
		t.Errorf("in Docker:\n%s", got)
	}
	t.Setenv("YMCP_UPDATE_CHECK", "off")
	if got := check("0.4.0"); got != "" {
		t.Errorf("check off, logged:\n%s", got)
	}
	if got := check("dev"); got != "" {
		t.Errorf("dev build checked:\n%s", got)
	}
}

func TestFormatChanges(t *testing.T) {
	notes := []update.Notes{
		{Tag: "v0.3.0", Sections: []update.Section{
			{Title: "Added", Entries: []string{"`ymcp setup` opens a TUI of your profiles, and `apply_command`, with a removing command, asks first."}},
		}},
		{Tag: "v0.2.1", Sections: []update.Section{{Title: "Fixed", Entries: []string{"A short fix."}}}},
	}
	got := formatChanges(notes, "0.2.0", 46)
	want := `
What's new since 0.2.0

 v0.3.0 
  Added
  • ymcp setup opens a TUI of your profiles,
    and apply_command, with a removing
    command, asks first.

 v0.2.1 
  Fixed
  • A short fix.
`
	if got != want {
		t.Errorf("formatChanges:\n%s\nwant:\n%s", got, want)
	}
}
