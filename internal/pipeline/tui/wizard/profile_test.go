package wizard

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/tools"
)

// fakeSignIn signs in when the password is right.
type fakeSignIn struct{ calls []config.Config }

func (f *fakeSignIn) signIn(_ context.Context, c config.Config) (string, error) {
	f.calls = append(f.calls, c)
	if c.Password != "right" {
		return "", errors.New("401 Unauthorized")
	}
	return c.Login, nil
}

var testClients = []Client{
	{ID: "cursor", Name: "Cursor"},
	{ID: "claude-desktop", Name: "Claude Desktop", Note: "restart it", Running: true},
}

func startProfile(t *testing.T, existing *config.Config, taken []string, clients []Client) (*driver, *fakeSignIn) {
	t.Helper()
	f := &fakeSignIn{}
	deps := ProfileDeps{SignIn: f.signIn, Command: func(key string) string { return "/opt/ymcp --profile " + key }}
	top := NewProfile(context.Background(), deps, existing, taken, clients).(*profileModel)
	top.sh.delay = 0
	d := &driver{t: t, m: top.Model, top: top}
	d.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	d.run(top.Init(), 0)
	return d, f
}

func TestNewProfile(t *testing.T) {
	d, f := startProfile(t, nil, []string{"other"}, testClients)
	d.must("New profile", "Profile name", "YouTrack URL", "Login", "Password", "Proxy", "MCP clients list this YouTrack as youtrack")
	if d.focused() != "name" {
		t.Fatalf("focus = %s", d.focused())
	}
	d.send(ctrlU())
	d.typ("YouTrack (work)")
	d.must("as youtrack-work")
	d.focus("url")
	d.typ("yt.example.com")
	d.focus("login")
	d.typ("alice")
	d.focus("password")
	d.typ("wrong")
	d.mustNot("wrong") // masked
	d.must("can't sign in: 401 Unauthorized")
	d.send(ctrlU())
	d.typ("right")
	d.must("signed in to https://yt.example.com as alice")
	if last := f.calls[len(f.calls)-1]; last.URL != "https://yt.example.com" {
		t.Errorf("signed in to %q", last.URL)
	}

	// Tools: every one is on; a group goes off with space.
	d.keys("ctrl+n")
	if d.step() != "Tools" {
		t.Fatalf("step = %s", d.step())
	}
	d.must("Allowed tools", "all ", "Ask before destructive operations", "[x] Ask before")
	d.focus("tools")
	d.keys("space") // issues off
	d.must("[ ] issues")
	d.keys("down", "right") // open comments
	d.must("delete_issue_comment", "destructive")
	d.keys("down", "space") // get_issue_comments off
	d.focus("confirm")
	d.keys("space")
	d.must("[ ] Ask before")

	// Clients: a new profile goes into each; a running app may quit.
	d.keys("ctrl+n")
	d.must("Register in", "[x] Cursor", "[x] Claude Desktop", "Quit and reopen running apps", "/opt/ymcp --profile youtrack-work")
	d.focus("btn:save")
	d.keys("enter")
	if len(d.out) != 1 {
		t.Fatalf("out = %v\n%s", d.out, d.view())
	}
	done := d.out[0].(ProfileDone)
	c := done.Profile.Config
	if done.Old != "" || c.Name != "youtrack-work" || c.DisplayName != "YouTrack (work)" || c.URL != "https://yt.example.com" ||
		c.Login != "alice" || c.Password != "right" || c.Confirms() {
		t.Errorf("saved %+v", done)
	}
	enabled, err := tools.Select(c.Tools)
	if err != nil || enabled["search_issues"] || enabled["get_issue_comments"] || !enabled["add_issue_comment"] || !enabled["call_api"] {
		t.Errorf("tools %v = %v, %v", c.Tools, enabled, err)
	}
	if !slices.Equal(done.Profile.Clients, []string{"cursor", "claude-desktop"}) || !done.Profile.Quit {
		t.Errorf("clients %v, quit %v", done.Profile.Clients, done.Profile.Quit)
	}
}

func TestEditProfile(t *testing.T) {
	existing := config.Config{Name: "work", DisplayName: "Work", URL: "https://yt.example.com", Login: "alice", Password: "right", Tools: []string{"-api"}}
	d, _ := startProfile(t, &existing, []string{"youtrack"}, []Client{{ID: "cursor", Name: "Cursor", Registered: true}, {ID: "zed", Name: "Zed"}})
	d.must("Edit profile work", "signed in to https://yt.example.com as alice")
	d.goStep(1)
	d.must("57 of 58", "[x] Ask before")
	d.goStep(2)
	d.must("[x] Cursor", "[ ] Zed", "registered")
	d.mustNot("Quit and reopen")

	// A taken name blocks saving.
	d.goStep(0)
	d.focus("name")
	d.send(ctrlU())
	d.typ("YouTrack")
	d.must("a profile youtrack already exists")
	d.focus("btn:save")
	d.keys("enter")
	if len(d.out) != 0 {
		t.Fatalf("saved onto a taken name: %v", d.out)
	}
	d.focus("name")
	d.send(ctrlU())
	d.typ("Work 2")
	d.focus("btn:save")
	d.keys("enter")
	done := d.out[0].(ProfileDone)
	if done.Old != "work" || done.Profile.Config.Name != "work-2" || strings.Join(done.Profile.Config.Tools, ",") != "-api" ||
		!slices.Equal(done.Profile.Clients, []string{"cursor"}) {
		t.Errorf("saved %+v", done)
	}
}

func TestProfileNeedsASignIn(t *testing.T) {
	existing := config.Config{Name: "work", URL: "https://yt.example.com", Login: "alice", Password: "nope"}
	d, f := startProfile(t, &existing, nil, nil)
	d.must("not signed in")
	d.focus("password")
	d.must("can't sign in: 401 Unauthorized")
	d.goStep(2)
	d.must("No MCP clients found")
	d.focus("btn:save")
	d.keys("enter")
	if len(d.out) != 0 {
		t.Fatal("saved without signing in")
	}
	d.must("can't sign in with this connection")
	d.goStep(0)
	d.focus("password")
	d.send(ctrlU())
	d.typ("right")
	if len(f.calls) < 2 {
		t.Fatalf("signed in %d times", len(f.calls))
	}
	d.focus("btn:save")
	d.keys("enter")
	if len(d.out) != 1 {
		t.Fatalf("not saved after signing in:\n%s", d.view())
	}
}

func TestProfileViews(t *testing.T) {
	existing := config.Config{Name: "work", DisplayName: "Work", URL: "https://yt.example.com", Login: "alice", Password: "right"}
	for _, size := range [][2]int{{60, 16}, {80, 24}, {120, 36}} {
		d, _ := startProfile(t, &existing, nil, testClients)
		d.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for i, name := range []string{"connection", "tools", "clients"} {
			d.goStep(i)
			d.dump("profile-" + name)
		}
		d.goStep(1)
		d.focus("tools")
		d.keys("right")
		d.dump("profile-tools-open")
	}
}
