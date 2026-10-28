package views

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// TestScreens writes the screens the README and the site show, in true
// colour, to the directory in YMCP_SCREENS: one .ans file per screen,
// drawn from the test fixture. The Site workflow turns them into
// pictures (see .github/scripts/screens.py).
func TestScreens(t *testing.T) {
	dir := os.Getenv("YMCP_SCREENS")
	if dir == "" {
		t.Skip("YMCP_SCREENS not set")
	}
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(0) // termenv.TrueColor, without importing termenv
	lipgloss.SetHasDarkBackground(true)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	f := newFixture(t)
	yt := withIssues(f)
	withFields(f, yt)
	dress(f, yt)
	write := func(name, view string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name+".ans"), []byte(view), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const w = 120
	write("pipelines", drive(t, NewPipelines(&f.d, "youtrack-work"), w, 20).m.View())
	write("board", drive(t, NewBoard(&f.d, "backend"), w, 22).m.View())
	write("inbox", drive(t, NewInbox(&f.d), w, 12).m.View())

	// The cards, over the board they open from.
	const h = 33
	board := drive(t, NewBoard(&f.d, "backend"), w, h)
	run := drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, false), w, h)
	write("run", run.m.(*Run).Overlay(board.m.View()))
	perm := drive(t, NewRun(&f.d, "frontend", f.runs["FE-9"].Number, true), w, h)
	perm.typeText("/")
	write("permission", perm.m.(*Run).Overlay(board.m.View()))
	card := issueCard(t, f, w, h)
	card.keys("e")
	write("fields", card.m.(*Run).Overlay(board.m.View()))
}

// dress gives the issues on screen what a real one has: a description,
// fields and a short discussion. The fixture keeps them bare, or in
// Russian, to test layouts.
func dress(f *fixture, yt *fakeIssues) {
	at := func(ago time.Duration) time.Time { return f.clock.Add(-ago) }

	be131 := yt.issues["2-131"]
	be131.Fields["Stage"] = "Backlog"
	be131.URL = "https://example.youtrack.cloud/issue/BE-131"
	be131.Description = "Bots try passwords on POST /api/login every night.\n\n" +
		"- at most 5 attempts a minute per IP\n- 429 with Retry-After past the limit"
	yt.issues["2-131"] = be131
	yt.add("2-131", IssueComment{ID: "4-10", Author: "anna", AuthorName: "Anna Petrova",
		Text: "Keep the counters in Redis.", Created: at(3 * time.Hour)})

	be127 := yt.issues["2-127"]
	be127.Description = "Users want to **export** a report as CSV from the report page: one row per issue, " +
		"with the columns the report shows."
	yt.issues["2-127"] = be127

	yt.issues["2-9"] = ytwatch.Issue{ID: "2-9", Key: "FE-9", Summary: "Dark mode",
		URL: "https://example.youtrack.cloud/issue/FE-9",
		Description: "Follow the system theme, with a switch in the settings that overrides it. " +
			"Charts and code blocks need their own dark palette.",
		Fields: map[string]string{"State": "In Progress", "Priority": "Normal", "Assignee": "boris", "Type": "Feature"}}
	yt.add("2-9", IssueComment{ID: "4-20", Author: "boris", AuthorName: "Boris Ivanov",
		Text: "Start with the dashboard; the editor can wait.", Created: at(3 * time.Hour)})
}
