package views

import (
	"context"
	"slices"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/store"
)

// The shortcuts work on a Russian layout: each key types a Cyrillic
// letter, and the screens act as on the Latin key at the same place.
// "," is what the "?" key types there.

// typeRunes types s one key at a time, as a terminal sends it.
func (d *driver) typeRunes(s string) *driver {
	d.t.Helper()
	for _, r := range s {
		d.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return d
}

func TestProfilesRussianLayout(t *testing.T) {
	f := newFixture(t)
	d := drive(t, NewProfiles(&f.d), 100, 30)
	d.keys("о", "т") // j n
	if got, ok := d.lastNav().(OpenWizard); !ok || got.Profile != "youtrack-work" {
		t.Errorf("т: %#v", got)
	}
	d.keys("л", "т") // k n
	if got, ok := d.lastNav().(OpenWizard); !ok || got.Profile != "youtrack-acme" {
		t.Errorf("л т: %#v", got)
	}
	d.keys("П", "д") // G l
	if got := d.lastNav(); got != (OpenPipelines{Profile: "youtrack-work"}) {
		t.Errorf("П д: %#v", got)
	}
	d.keys("ш") // i
	if got := d.lastNav(); got != (OpenInbox{}) {
		t.Errorf("ш: %#v", got)
	}
	d.keys(",")
	if !d.m.(*Profiles).fullHelp {
		t.Error(", (the ? key) does not show the help")
	}
	d.keys("й") // q
	if _, ok := d.lastNav().(tea.QuitMsg); !ok {
		t.Error("й does not quit")
	}
}

func TestPipelinesRussianLayout(t *testing.T) {
	f := newFixture(t)
	c := withCatalog(f, 0)
	d := drive(t, NewPipelines(&f.d, "youtrack-work"), 120, 30)
	d.keys("щ") // o
	if got := d.lastNav(); got != (OpenBoard{Pipeline: "backend"}) {
		t.Errorf("щ: %#v", got)
	}
	d.keys("у") // e
	if got, ok := d.lastNav().(OpenWizard); !ok || got.Existing == nil || got.Existing.Name != "backend" {
		t.Errorf("у: %#v", got)
	}
	d.keys("к") // r
	d.has("backend is already running.")
	d.keys("ы") // s
	d.has("Stopped backend.")
	d.keys("д", "к") // l r
	d.has("Started frontend in the background (pid 777)")
	if want := []string{"stop backend", "start frontend"}; !slices.Equal(f.daemon.calls, want) {
		t.Errorf("daemon calls %v, want %v", f.daemon.calls, want)
	}
	d.keys("в") // d
	d.has("Delete pipeline frontend?")
	d.keys("т") // n
	d.has("Nothing changed.")
	d.keys("в", "Н") // d Y
	if !slices.Equal(f.deleted, []string{"frontend"}) {
		t.Fatalf("deleted %v", f.deleted)
	}
	d.keys("р", "т") // h n: the wizard for a new pipeline
	if got, ok := d.lastNav().(OpenWizard); !ok || got.Existing != nil {
		t.Errorf("т: %#v", got)
	}
	d.keys(",")
	d.has("L logs", "c copy", "R rename")

	// The menu: its item keys too; й (q) closes it.
	d.keys("enter")
	d.has("─ backend ─")
	d.keys("й")
	d.hasNot("─ backend ─")
	if got := d.lastNav(); got != nil {
		t.Errorf("й in the menu: %#v", got)
	}
	f.d.RunnerLog = func(context.Context, string) (string, error) { return "", nil }
	d.keys("enter", "Д") // L
	if got := d.lastNav(); got != (OpenLogs{Pipeline: "backend"}) {
		t.Errorf("Д in the menu: %#v", got)
	}

	// The name box takes Cyrillic as typed.
	d.keys("enter", "с") // c: Copy…
	d.has("─ Copy backend ─")
	d.typeRunes("тсоку")
	if got := d.m.(*Pipelines).prompt.in.Value(); got != "backend-copyтсоку" {
		t.Errorf("name box = %q", got)
	}
	d.keys("esc")
	if len(c.copies) != 0 {
		t.Errorf("copied %v", c.copies)
	}
}

func TestBoardRussianLayout(t *testing.T) {
	f := newFixture(t)
	d := drive(t, NewBoard(&f.d, "backend"), 100, 30)
	d.keys("щ") // o, on BE-123
	if want := "https://example.youtrack.cloud/issue/BE-123"; len(f.opened) != 1 || f.opened[0] != want {
		t.Errorf("opened %v, want %s", f.opened, want)
	}
	d.keys("д", "ф") // l a
	if got := d.lastNav(); got != (OpenRun{Pipeline: "backend", Number: f.runs["BE-127"].Number, Answer: true}) {
		t.Errorf("ф: %#v", got)
	}
	d.keys("д", "з") // l p
	if len(f.opened) != 2 || f.opened[1] != "https://github.com/acme/backend/pull/40" {
		t.Fatalf("з: opened %v", f.opened)
	}
	d.keys("с") // c
	d.has("Cancel run #3 (BE-118)?")
	d.keys("н") // y
	d.has("Canceled run #3.")
	r, err := f.st.Run(context.Background(), f.runs["BE-118"].ID)
	if err != nil || r.Status != store.StatusCanceled {
		t.Errorf("BE-118 is %s (%v), want canceled", r.Status, err)
	}
	d.keys(",")
	if !d.m.(*Board).fullHelp {
		t.Error(", (the ? key) does not show the help")
	}
}

func TestRunRussianLayout(t *testing.T) {
	f := newFixture(t)
	yt := withIssues(f)
	q := f.runs["BE-127"]
	d := drive(t, NewRun(&f.d, "backend", q.Number, false), 160, 45)

	// enter opens the box; what is typed there stays as typed, shortcut
	// letters and "," included.
	d.keys("enter")
	d.has("enter send")
	const answer = "Да, конечно? Ёж й ш"
	d.typeRunes(answer)
	if got := d.m.(*Run).reply.ta.Value(); got != answer {
		t.Fatalf("box = %q, want %q", got, answer)
	}
	d.keys("enter")
	if len(yt.added) != 1 || yt.added[0] != answer {
		t.Fatalf("posted %q", yt.added)
	}

	d.keys("щ") // o
	if len(f.opened) != 1 || f.opened[0] != "https://example.youtrack.cloud/issue/BE-127" {
		t.Errorf("opened %v", f.opened)
	}
	d.keys("з") // p
	d.has("run #6 has no pull request")
	d.keys("ч") // x
	d.has("does not wait for a permission")
	d.keys(",")
	if !d.m.(*Run).fullHelp {
		t.Error(", (the ? key) does not show the help")
	}
	d.keys("с") // c
	d.has("Cancel run #6 (BE-127)?")
	d.keys("н") // y
	d.has("Canceled run #6.")
	d.keys("ш") // i
	if got := d.lastNav(); got != (OpenInbox{}) {
		t.Errorf("ш: %#v", got)
	}
	d.keys("й") // q
	if _, ok := d.lastNav().(tea.QuitMsg); !ok {
		t.Error("й does not quit")
	}
}

func TestInboxRussianLayout(t *testing.T) {
	f := newFixture(t)
	d := drive(t, NewInbox(&f.d), 100, 30)
	d.keys("о", "ф") // j a
	d.has("enter send")
	d.typeRunes("ДА, ISO")
	d.keys("enter")
	d.has("Answer sent to #6")
	items, _ := f.st.PendingInbox(context.Background(), f.runs["BE-127"].ID)
	if len(items) != 1 || items[0].Body != "ДА, ISO" {
		t.Errorf("inbox %+v", items)
	}
	d.keys("м") // v
	if got := d.lastNav(); got != (OpenRun{Pipeline: "backend", Number: 6}) {
		t.Errorf("м: %#v", got)
	}
}

func TestLogsRussianLayout(t *testing.T) {
	f := newFixture(t)
	calls := 0
	f.d.RunnerLog = func(context.Context, string) (string, error) {
		calls++
		s := ""
		for range 100 {
			s += "log line\n"
		}
		return s, nil
	}
	d := drive(t, NewLogs(&f.d, "backend"), 80, 24)
	d.keys("п") // g
	d.has("lines 1–20 of 100")
	d.keys("о") // j
	d.has("lines 2–21 of 100")
	d.keys("П") // G
	d.has("following")
	d.keys("к") // r
	if calls != 2 {
		t.Errorf("%d reads, want 2", calls)
	}
	d.keys("й") // q
	if _, ok := d.lastNav().(tea.QuitMsg); !ok {
		t.Error("й does not quit")
	}
}
