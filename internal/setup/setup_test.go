package setup

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/tui/wizard"
)

// fakeStore keeps profiles in memory.
type fakeStore struct {
	profiles map[string]config.Config
	saved    []wizard.ProfileDraft
	removed  []string
	failSave error
}

func (f *fakeStore) deps() Deps {
	return Deps{
		Profiles: func() ([]Profile, error) {
			var out []Profile
			for _, c := range f.profiles {
				c.Password = ""
				out = append(out, Profile{Config: c, Clients: []string{"Cursor"}})
			}
			slices.SortFunc(out, func(a, b Profile) int { return strings.Compare(a.Config.Name, b.Config.Name) })
			return out, nil
		},
		Load: func(key string) (config.Config, error) {
			c, ok := f.profiles[key]
			if !ok {
				return c, errors.New("no profile " + key)
			}
			return c, nil
		},
		Clients: func(key string) []wizard.Client {
			return []wizard.Client{{ID: "cursor", Name: "Cursor", Registered: key != ""}}
		},
		Save: func(_ context.Context, old string, p wizard.ProfileDraft) ([]string, error) {
			if f.failSave != nil {
				return nil, f.failSave
			}
			f.saved = append(f.saved, p)
			delete(f.profiles, old)
			f.profiles[p.Config.Name] = p.Config
			return []string{"✓ Saved profile " + p.Config.Title()}, nil
		},
		Remove: func(_ context.Context, key string) ([]string, error) {
			f.removed = append(f.removed, key)
			delete(f.profiles, key)
			return []string{"✓ Removed profile " + key}, nil
		},
		Wizard: wizard.ProfileDeps{
			SignIn: func(_ context.Context, c config.Config) (string, error) { return c.Login, nil },
		},
	}
}

// drive runs the app like a tea.Program, synchronously.
type drive struct {
	t    *testing.T
	a    *App
	quit bool
}

var cmdType = reflect.TypeOf((tea.Cmd)(nil))

func (d *drive) run(cmd tea.Cmd, depth int) {
	if cmd == nil || depth > 60 {
		return
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(500 * time.Millisecond):
		return
	}
	switch m := msg.(type) {
	case nil:
		return
	case tea.QuitMsg:
		d.quit = true
		return
	case tea.BatchMsg:
		for _, c := range m {
			d.run(c, depth+1)
		}
		return
	}
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == cmdType {
		for i := 0; i < v.Len(); i++ {
			d.run(v.Index(i).Interface().(tea.Cmd), depth+1)
		}
		return
	}
	_, next := d.a.Update(msg)
	d.run(next, depth+1)
}

func (d *drive) send(msg tea.Msg) {
	_, cmd := d.a.Update(msg)
	d.run(cmd, 0)
}

func (d *drive) key(k string) {
	switch k {
	case "enter":
		d.send(tea.KeyMsg{Type: tea.KeyEnter})
	case "down":
		d.send(tea.KeyMsg{Type: tea.KeyDown})
	case "esc":
		d.send(tea.KeyMsg{Type: tea.KeyEsc})
	case "ctrl+s":
		d.send(tea.KeyMsg{Type: tea.KeyCtrlS})
	default:
		d.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	}
}

func (d *drive) view() string { return ansi.Strip(d.a.View()) }

func (d *drive) must(parts ...string) {
	d.t.Helper()
	v := d.view()
	for _, p := range parts {
		if !strings.Contains(v, p) {
			d.t.Fatalf("view lacks %q:\n%s", p, v)
		}
	}
}

func start(t *testing.T, f *fakeStore, open string) *drive {
	t.Helper()
	d := &drive{t: t, a: New(context.Background(), Options{Deps: f.deps(), Open: open})}
	d.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	d.run(d.a.Init(), 0)
	return d
}

// save presses the wizard's Save button.
func (d *drive) save() {
	for range 20 {
		if strings.Contains(d.view(), "Save") && d.wizardFocus() == "btn:save" {
			d.key("enter")
			return
		}
		d.send(tea.KeyMsg{Type: tea.KeyShiftTab})
	}
	d.t.Fatalf("no Save button:\n%s", d.view())
}

func (d *drive) wizardFocus() string {
	type focuser interface{ FocusID() string }
	if w, ok := d.a.Wizard().(focuser); ok {
		return w.FocusID()
	}
	return ""
}

func TestProfilesScreen(t *testing.T) {
	off := false
	f := &fakeStore{profiles: map[string]config.Config{
		"work":     {Name: "work", DisplayName: "Work", URL: "https://work.example.com", Login: "alice", Password: "pw", Tools: []string{"issues"}},
		"youtrack": {Name: "youtrack", URL: "https://yt.example.com", Login: "bob", Password: "pw", Confirm: &off},
	}}
	d := start(t, f, "")
	d.must("ymcp setup", "Work (work)", "work.example.com", "alice", "15 of 58 tools", "asks before destructive operations",
		"registered in Cursor", "youtrack", "bob", "all 58 tools", "runs destructive operations at once", "n new profile")
	if d.a.Wizard() != nil {
		t.Fatal("a wizard opened with profiles there")
	}

	// Edit the first one and save it as it is.
	d.key("enter")
	d.must("Edit profile work", "signed in")
	d.save()
	if d.a.Wizard() != nil || len(f.saved) != 1 || f.saved[0].Config.Name != "work" {
		t.Fatalf("saved %+v:\n%s", f.saved, d.view())
	}
	d.must("Saved profile Work")

	// Remove the second.
	d.key("down")
	d.key("d")
	d.must("Remove profile youtrack and its client registrations?")
	d.key("n")
	d.must("Nothing removed.")
	d.key("d")
	d.key("y")
	if !slices.Equal(f.removed, []string{"youtrack"}) {
		t.Fatalf("removed %v", f.removed)
	}
	d.must("Removed profile youtrack")
	if strings.Contains(d.view(), "bob") {
		t.Errorf("removed profile still listed:\n%s", d.view())
	}

	d.key("q")
	if !d.quit {
		t.Error("q did not quit")
	}
}

func TestNoProfilesOpensTheWizard(t *testing.T) {
	f := &fakeStore{profiles: map[string]config.Config{}}
	d := start(t, f, "")
	d.must("New profile")
	d.key("esc")
	if d.a.Wizard() != nil {
		t.Fatalf("esc on an untouched draft kept the wizard:\n%s", d.view())
	}
	d.must("No YouTrack profiles yet")
}

func TestOpenAProfileAndKeepTheDraftWhenSavingFails(t *testing.T) {
	f := &fakeStore{profiles: map[string]config.Config{
		"work": {Name: "work", URL: "https://work.example.com", Login: "alice", Password: "pw"},
	}, failSave: errors.New("disk full")}
	d := start(t, f, "work")
	d.must("Edit profile work")
	d.save()
	d.must("Not saved: disk full", "Connection")
	if d.a.Wizard() == nil {
		t.Fatal("the draft was dropped")
	}

	// A key no profile has starts a new one with that name.
	d = start(t, f, "Acme")
	d.must("New profile", "as acme")
}
