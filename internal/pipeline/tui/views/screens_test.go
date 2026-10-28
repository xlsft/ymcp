package views

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestScreens writes the screens the README and the site show, in true
// colour, to the directory in YMCP_SCREENS: one .ans file per screen,
// drawn from the test fixture. The Screens workflow turns them into
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
	yt.issues["2-131"].Fields["Stage"] = "Backlog" // the fixture's Cyrillic value checks layouts, not looks
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
	const h = 30
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
