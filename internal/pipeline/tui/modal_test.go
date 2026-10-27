package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/views"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

type fakeIssues struct {
	mu    sync.Mutex
	added []string
}

func (f *fakeIssues) Issue(_ context.Context, _, issue string) (ytwatch.Issue, error) {
	return ytwatch.Issue{ID: issue, Key: "BE-1", Summary: "Fix the login cookie", Fields: map[string]string{"State": "In Progress"}}, nil
}

func (f *fakeIssues) Comments(context.Context, string, string) ([]views.IssueComment, error) {
	return []views.IssueComment{{ID: "4-1", Author: "anna", Text: "Which cookie?", Created: time.Now().Add(-time.Hour)}}, nil
}

func (f *fakeIssues) AddComment(_ context.Context, _, _, text string) (views.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, text)
	return views.IssueComment{ID: "4-2", Author: "x", Text: text, Created: time.Now()}, nil
}

// modalFixture is an app on the board of backend, with one run that waits
// for input.
func modalFixture(t *testing.T) (*fixture, *fakeIssues, store.Run) {
	t.Helper()
	f := newFixture(t, "a", "b")
	ctx := context.Background()
	r, err := f.st.CreateRun(ctx, "backend", "a", "2-1", "BE-1")
	if err != nil {
		t.Fatal(err)
	}
	r.Status, r.NeedsInputReason = store.StatusNeedsInput, "Which cookie should expire?"
	if err := f.st.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	yt := &fakeIssues{}
	deps := f.app.opts.Deps
	deps.Issues = yt
	f.app = New(ctx, Options{Deps: deps, Pipeline: "backend", TickEvery: -1})
	f.send(tea.WindowSizeMsg{Width: 120, Height: 40})
	f.run(f.app.Init(), 0)
	return f, yt, r
}

func TestRunModalOverBoard(t *testing.T) {
	f, yt, r := modalFixture(t)
	board := topIs[*views.Board](t, f)
	depth := f.app.Depth()
	under := f.view()

	f.send(views.OpenRun{Pipeline: "backend", Number: r.Number})
	topIs[*views.Run](t, f)
	f.has(t, "backend #1", "BE-1", "Fix the login cookie", "State: In Progress", "Which cookie?", "Log", "esc close")
	lines := strings.Split(f.view(), "\n")
	if len(lines) != 40 {
		t.Fatalf("%d lines", len(lines))
	}
	// The board shows around the box: its first lines are the board's.
	first := strings.Split(under, "\n")[0]
	if lines[0] != first || strings.Contains(lines[0], "╭") {
		t.Errorf("first line %q, board's %q", lines[0], first)
	}
	if !strings.Contains(f.view(), "╭─") || !strings.Contains(f.view(), "╰─") {
		t.Errorf("no box:\n%s", f.view())
	}

	// A reply goes to YouTrack.
	f.keys("r")
	f.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("The session one.")})
	f.keys("enter")
	if len(yt.added) != 1 || yt.added[0] != "The session one." {
		t.Errorf("posted %q", yt.added)
	}
	f.has(t, "Commented on BE-1")

	f.keys("esc")
	if topIs[*views.Board](t, f) != board || f.app.Depth() != depth {
		t.Errorf("esc did not go back to the same board (depth %d)", f.app.Depth())
	}
}

// TestIssueModalOverBoard: a backlog issue opens the card over the board,
// and esc goes back to it.
func TestIssueModalOverBoard(t *testing.T) {
	f, yt, _ := modalFixture(t)
	board := topIs[*views.Board](t, f)
	f.send(views.OpenIssue{Pipeline: "backend", Profile: "a", Issue: ytwatch.Issue{ID: "2-1", Key: "BE-1", Summary: "Fix the login cookie"}})
	topIs[*views.Run](t, f)
	f.has(t, "BE-1", "Fix the login cookie", "Which cookie?", "Not taken yet")
	f.keys("r")
	f.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Any cookie.")})
	f.keys("enter")
	if len(yt.added) != 1 || yt.added[0] != "Any cookie." {
		t.Errorf("posted %q", yt.added)
	}
	f.has(t, "Commented on BE-1.")
	f.keys("esc")
	if topIs[*views.Board](t, f) != board {
		t.Error("esc did not go back to the board")
	}
}

func TestRunModalOverInbox(t *testing.T) {
	f, _, _ := modalFixture(t)
	f.keys("i")
	inbox := topIs[*views.Inbox](t, f)
	f.keys("enter") // opens the card with the reply box focused
	run := topIs[*views.Run](t, f)
	if !run.Answering() {
		t.Error("the reply box has no focus")
	}
	f.has(t, "enter send", "BE-1")
	f.keys("esc") // leaves the box
	if run.Answering() {
		t.Error("esc kept the focus in the box")
	}
	f.keys("esc")
	if topIs[*views.Inbox](t, f) != inbox {
		t.Error("esc did not go back to the inbox")
	}
}
