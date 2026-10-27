package views

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// fakeIssues is YouTrack for the run card: one issue per readable or
// database id, comments that AddComment appends to.
type fakeIssues struct {
	mu       sync.Mutex
	issues   map[string]ytwatch.Issue
	comments map[string][]IssueComment
	login    string
	now      func() time.Time
	added    []string // texts posted
	addErr   error
	readErr  error
	reads    int
}

func newFakeIssues(now func() time.Time) *fakeIssues {
	return &fakeIssues{issues: map[string]ytwatch.Issue{}, comments: map[string][]IssueComment{}, login: "xlsft", now: now}
}

func (f *fakeIssues) Issue(_ context.Context, _, issue string) (ytwatch.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.readErr != nil {
		return ytwatch.Issue{}, f.readErr
	}
	is, ok := f.issues[issue]
	if !ok {
		return ytwatch.Issue{}, fmt.Errorf("issue %s not found", issue)
	}
	return is, nil
}

func (f *fakeIssues) Comments(_ context.Context, _, issue string) ([]IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	return append([]IssueComment(nil), f.comments[issue]...), nil
}

func (f *fakeIssues) AddComment(_ context.Context, _, issue, body string) (IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return IssueComment{}, f.addErr
	}
	f.added = append(f.added, body)
	c := IssueComment{ID: fmt.Sprintf("4-%d", 100+len(f.added)), Author: f.login, Text: body, Created: f.now()}
	f.comments[issue] = append(f.comments[issue], c)
	return c, nil
}

func (f *fakeIssues) add(issue string, c IssueComment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[issue] = append(f.comments[issue], c)
}

type fakeChanges struct {
	comments []vcs.Comment
	err      error
}

func (f *fakeChanges) Comments(context.Context, store.Run) ([]vcs.Comment, error) {
	return f.comments, f.err
}

// agentComment is a comment the runner of backend wrote for run n.
func agentComment(id string, n int, body string, at time.Time) IssueComment {
	mk := text.Mark{Pipeline: "backend", Run: n, Msg: "ab12"}
	return IssueComment{ID: id, Author: "xlsft", Text: text.Decorate(body, "@xlsft", mk), Created: at}
}

// withIssues gives the fixture a YouTrack with BE-127 (a question) and
// BE-123 (the agent working) and their comments.
func withIssues(f *fixture) *fakeIssues {
	yt := newFakeIssues(func() time.Time { return f.clock })
	yt.issues["2-127"] = ytwatch.Issue{ID: "2-127", Key: "BE-127", Summary: "CSV export of the reports",
		URL:         "https://xlsft.youtrack.cloud/issue/BE-127",
		Description: "Users want to **export** reports as CSV.\n\n## Acceptance\n- a button on the report page\n- dates in one format\n\n![](https://x/y.png)\n\n```\nGET /reports/42.csv\n```",
		Fields:      map[string]string{"State": "In Progress", "Priority": "Major", "Assignee": "xlsft", "Estimation": "2d"}}
	yt.issues["2-123"] = ytwatch.Issue{ID: "2-123", Key: "BE-123", Summary: "Fix login", Fields: map[string]string{"State": "Open"}}
	yt.add("2-127", IssueComment{ID: "4-1", Author: "anna", AuthorName: "Anna Petrova", Text: "Please also cover the admin reports.", Created: t0.Add(-50 * time.Minute)})
	yt.add("2-127", agentComment("4-2", 6, "Which date format should the CSV export use: ISO 8601 or the user's locale?", t0.Add(-13*time.Minute)))
	yt.add("2-123", IssueComment{ID: "4-3", Author: "xlsft", Text: "Go ahead.", Created: t0.Add(-5 * time.Minute)})
	yt.add("2-123", agentComment("4-4", 5, text.Working, t0.Add(-4*time.Minute)))
	f.d.Issues = yt
	return yt
}

func TestRunModal(t *testing.T) {
	f := newFixture(t)
	yt := withIssues(f)
	q := f.runs["BE-127"]
	d := drive(t, NewRun(&f.d, "backend", q.Number, false), 160, 45)
	d.fits(160, 45)
	d.has("backend #6",
		"BE-127", "CSV export of the reports",
		"State: In Progress", "Priority: Major", "stage working", "⚑ needs input", "⎇ be-127-csv-export",
		"Users want to export reports as CSV.", "Acceptance", "• a button on the report page", "[image]", "GET /reports/42.csv",
		"Comments · 2",
		"Anna Petrova @anna · 50m ago", "Please also cover the admin reports.",
		"@xlsft's agent #6 (backend) · 13m ago", "Which date format should the CSV export use",
		"Log", "captured", "question",
		"waiting 12m", "Write a comment… (/approve, /reject)",
		"r reply", "tab log", "esc close")
	d.hasNot("Estimation", "Assignee", "ymcp:agent", "<!--", "**export**",
		"NEEDS INPUT", "Permission", "posts the comment") // no separate rows any more
	lines := strings.Split(d.view(), "\n")
	// The top bar runs across the card: the title on the left, the chips
	// on the right of the same line.
	if !slices.ContainsFunc(lines, func(l string) bool {
		return strings.Contains(l, "backend #6") && strings.Contains(l, "State: In Progress")
	}) {
		t.Errorf("no top bar with the title and the chips side by side:\n%s", d.view())
	}
	// The reason the run waits is the last comment, not repeated above
	// the comment box.
	if n := strings.Count(d.view(), "Which date format should the CSV export use"); n != 1 {
		t.Errorf("the question shows %d times:\n%s", n, d.view())
	}
	// The comment box is a framed rectangle at the bottom of the pane,
	// padded away from the frame of the card.
	box := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "Write a comment") })
	if box < 1 || !strings.Contains(lines[box-1], "╭─") || !strings.Contains(lines[box+1], "╰─") {
		t.Errorf("the comment box has no frame:\n%s", d.view())
	}
	if !strings.Contains(lines[box], "│  │ Write a comment") {
		t.Errorf("the comment box is not padded: %q", lines[box])
	}

	// The comment is posted through YouTrack and shown at once.
	d.keys("r")
	d.has("enter send", "alt+enter new line", "esc leave the box")
	d.typeText("ISO 8601, please.")
	d.keys("enter")
	if len(yt.added) != 1 || yt.added[0] != "ISO 8601, please." {
		t.Fatalf("posted %q", yt.added)
	}
	d.has("Comments · 3", "xlsft · just now", "ISO 8601, please.", "Commented on BE-127. The runner passes it")
	d.hasNot("sending…", "enter send")

	d.keys("o")
	if len(f.opened) != 1 || f.opened[0] != "https://xlsft.youtrack.cloud/issue/BE-127" {
		t.Errorf("opened %v", f.opened)
	}
	d.keys("p")
	d.has("run #6 has no pull request")
	d.keys("x")
	d.has("does not wait for a permission")

	d.keys("c")
	d.has("Cancel run #6 (BE-127)?")
	d.keys("y")
	d.has("Canceled run #6.", "canceled")
	d.keys("esc")
	if got := d.lastNav(); got != (Back{}) {
		t.Errorf("esc: %#v", got)
	}
}

func TestRunModalWorkingPlaceholder(t *testing.T) {
	f := newFixture(t)
	withIssues(f)
	d := drive(t, NewRun(&f.d, "backend", f.runs["BE-123"].Number, false), 120, 40)
	d.fits(120, 40)
	d.has("working", "State: Open", "Claude · writing · 4m12s",
		"@xlsft's agent #5 (backend) · 4m ago", "⠋ working…", "Go ahead.")
	d.hasNot("Working...", "working.gif", "![](")
	d.tick()
	d.has("⠙ working…")
}

func TestRunModalSendFails(t *testing.T) {
	f := newFixture(t)
	yt := withIssues(f)
	yt.addErr = errors.New("403 Forbidden")
	d := drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, true), 120, 40)
	d.typeText("hello")
	d.keys("enter")
	d.has("comment not posted: 403 Forbidden", "hello  (draft)", "Comments · 2")
	d.hasNot("sending…")
	// r brings the draft back.
	d.keys("r")
	if got := d.m.(*Run).reply.draft(); got != "hello" {
		t.Errorf("draft %q", got)
	}
}

func TestRunModalPermission(t *testing.T) {
	f := newFixture(t)
	yt := withIssues(f)
	fe := f.runs["FE-9"]
	yt.issues["2-9"] = ytwatch.Issue{ID: "2-9", Key: "FE-9", Summary: "Dark mode"}
	d := drive(t, NewRun(&f.d, "frontend", fe.Number, false), 120, 40)
	d.has("a approve", "x reject", "Write a comment… (/approve, /reject)")
	d.hasNot("Permission", "posts the comment", "/approve  allow")
	d.keys("a")
	d.has("Post /approve on FE-9?")
	d.keys("n")
	if len(yt.added) != 0 {
		t.Fatalf("posted %q after no", yt.added)
	}
	d.keys("a", "y")
	d.keys("x", "y")
	if strings.Join(yt.added, ",") != "/approve,/reject" {
		t.Errorf("posted %q", yt.added)
	}
	d.has("/approve", "/reject", "Comments · 2")

	// "/" in the box offers the commands; tab or enter completes, a
	// second enter sends.
	d.keys("r")
	d.typeText("/")
	d.has("› /approve  allow what the agent asked for", "/reject   refuse it", "tab complete")
	d.keys("down")
	d.has("› /reject")
	d.keys("tab")
	d.hasNot("allow what the agent asked for")
	if got := d.m.(*Run).reply.ta.Value(); got != "/reject" {
		t.Fatalf("completed %q, want /reject", got)
	}
	d.keys("enter")
	if got := yt.added[len(yt.added)-1]; got != "/reject" || len(yt.added) != 3 {
		t.Errorf("posted %q", yt.added)
	}
	// Narrowed by what is typed; esc closes the popup, then the box.
	d.keys("r")
	d.typeText("/ap")
	d.has("› /approve")
	d.hasNot("refuse it")
	d.keys("enter")
	if got := d.m.(*Run).reply.ta.Value(); got != "/approve" {
		t.Fatalf("completed %q, want /approve", got)
	}
	d.keys("esc")
	if d.m.(*Run).reply.focused {
		t.Error("esc kept the focus in the box")
	}
	d.keys("r")
	d.send(tea.KeyMsg{Type: tea.KeyBackspace})
	d.has("› /approve")
	d.keys("esc")
	d.hasNot("allow what the agent asked for")
	if !d.m.(*Run).reply.focused {
		t.Error("esc on the popup left the box")
	}
}

// TestRunModalNoSuggestions: "/" offers nothing when the run does not
// wait on a permission.
func TestRunModalNoSuggestions(t *testing.T) {
	f := newFixture(t)
	withIssues(f)
	d := drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, true), 120, 40)
	d.typeText("/")
	d.hasNot("allow what the agent asked for")
}

func TestRunModalLogPanel(t *testing.T) {
	f := newFixture(t)
	withIssues(f)
	w := f.runs["BE-123"]
	for i := range 40 {
		f.event(w, 0, "agent.tool_call", map[string]any{"tool": map[string]string{"title": fmt.Sprintf("Step %02d", i)}})
	}
	for range 3 {
		f.event(w, 0, "stage.command", map[string]string{"stage": "working", "command": "State In Progress"})
	}
	d := drive(t, NewRun(&f.d, "backend", w.Number, false), 120, 40)
	d.has("×3 working: State", "tool     Step 39")
	d.keys("tab")
	d.has("tab issue")
	d.keys("k", "k", "k", "k", "k")
	d.has("↓ 5 more · G to follow", "tool     Step 35")
	d.hasNot("tool     Step 39", "×3")
	// The thread did not scroll.
	d.has("Go ahead.")
	d.keys("G")
	d.has("tool     Step 39")
	d.keys("tab")
	d.has("tab log")

	// New events follow live.
	f.event(w, 0, "agent.report", map[string]string{"text": "Fixed the cookie expiry."})
	d.tick()
	d.has("report   Fixed the cookie")
}

func TestRunModalNarrowTakesTurns(t *testing.T) {
	f := newFixture(t)
	withIssues(f)
	d := drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, false), 60, 24)
	d.fits(60, 24)
	d.has("BE-127", "tab shows the log")
	d.hasNot("captured")
	d.keys("tab")
	d.has("Log · tab shows the issue", "captured", "CSV export") // the top bar stays
	d.hasNot("Comments ·", "Write a comment")
}

func TestRunModalPullRequestTab(t *testing.T) {
	f := newFixture(t)
	withIssues(f)
	f.d.Changes = &fakeChanges{comments: []vcs.Comment{
		{ID: "1", Author: "octocat", Body: "Looks good, one nit.", Kind: vcs.CommentReview, Created: t0.Add(-time.Hour)},
		{ID: "2", Author: "octocat", Body: "Rename this.", File: "cache/keys.go", Line: 12, Kind: vcs.CommentReviewComment, Created: t0.Add(-50 * time.Minute)},
		{ID: "3", Author: "xlsft", Body: text.DecorateCodeHost("Renamed.", "@xlsft", text.Mark{Pipeline: "backend", Run: 3, Msg: "c0"}), Kind: vcs.CommentConversation, Created: t0.Add(-40 * time.Minute)},
	}}
	f.issuesFor("2-118", "BE-118", "Cache keys")
	d := drive(t, NewRun(&f.d, "backend", f.runs["BE-118"].Number, false), 160, 45)
	d.has("YouTrack     Pull request #40", "PR #40", "t PR thread", "p PR")
	d.keys("t")
	d.has("Read-only", "octocat · 1h ago · review", "Looks good, one nit.",
		"octocat · 50m ago · cache/keys.go:12", "Rename this.",
		"@xlsft's agent #3 (backend) · 40m ago", "Renamed.")
	d.hasNot("ymcp:agent")
	d.keys("t")
	d.has("No comments yet.")
}

func (f *fixture) issuesFor(id, key, summary string) {
	f.d.Issues.(*fakeIssues).issues[id] = ytwatch.Issue{ID: id, Key: key, Summary: summary}
}

func TestRunModalWithoutYouTrack(t *testing.T) {
	f := newFixture(t)
	d := drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, true), 100, 30)
	d.has("YouTrack is not connected here", "CSV export")
	d.keys("r")
	d.has("open the issue with o to comment")
}

func TestRunModalReadError(t *testing.T) {
	f := newFixture(t)
	yt := withIssues(f)
	yt.readErr = errors.New("connection refused")
	d := drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, false), 100, 30)
	d.has("Could not read the issue: connection refused", "Could not read the comments: connection refused")
}

func TestRunModalRefreshesComments(t *testing.T) {
	f := newFixture(t)
	yt := withIssues(f)
	d := drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, false), 120, 40)
	reads := yt.reads
	yt.add("2-127", IssueComment{ID: "4-9", Author: "anna", Text: "Any news?", Created: t0})
	d.tick() // under 5s: no reread
	d.hasNot("Any news?")
	f.clock = f.clock.Add(6 * time.Second)
	d.tick()
	d.has("Any news?", "Comments · 3")
	if yt.reads != reads+1 {
		t.Errorf("reads %d, want %d", yt.reads, reads+1)
	}
}

func TestRunModalMissing(t *testing.T) {
	f := newFixture(t)
	d := drive(t, NewRun(&f.d, "backend", 99, false), 80, 24)
	d.has("pipeline backend has no run #99")
}

// TestRunModalSizes renders the card at common and tiny sizes, the reply
// box open and closed.
func TestRunModalSizes(t *testing.T) {
	for _, size := range [][2]int{{200, 50}, {160, 45}, {120, 40}, {100, 30}, {80, 24}, {60, 20}, {40, 12}, {20, 6}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			f := newFixture(t)
			withIssues(f)
			for _, k := range []string{"BE-127", "BE-123", "FE-9"} {
				d := drive(t, NewRun(&f.d, f.runs[k].Pipeline, f.runs[k].Number, false), size[0], size[1])
				d.fits(size[0], size[1])
				d.keys("r")
				d.fits(size[0], size[1])
				d.keys("esc", "tab", "?")
				d.fits(size[0], size[1])
			}
		})
	}
}

func TestModalOver(t *testing.T) {
	under := "abcdefghij\nklmnopqrst\nuvwxyz0123"
	got := ansi.Strip(modalOver(under, []string{"XX", "YY"}, 3, 1, 10, 4))
	want := "abcdefghij\nklmXXpqrst\nuvwYYz0123\n          "
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// TestModalOverDims draws a box over coloured content with colours on:
// the content around the box loses its own colours and bold and is drawn
// faint and grey, the box keeps its own, and nothing bleeds across.
func TestModalOverDims(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(1) // termenv.ANSI256, without importing termenv
	lipgloss.SetHasDarkBackground(true)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	red := lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	under := red.Render("RED RED RED RED") + "\n" + "\x1b]8;;https://x\x1b\\link text here\x1b]8;;\x1b\\ plain" + "\n" + red.Render("open")
	boxLine := lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Render("BOX")
	got := modalOver(under, []string{boxLine, boxLine}, 4, 1, 20, 3)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines", len(lines))
	}
	if strings.Contains(lines[0], "[1;") || strings.Contains(lines[0], "38;5;9") || strings.Contains(lines[0], "91") {
		t.Errorf("the content under the box kept its colours: %q", lines[0])
	}
	if !strings.Contains(lines[0], "\x1b[2;38;5;240m") {
		t.Errorf("the content under the box is not faint grey: %q", lines[0])
	}
	if strings.Contains(got, "]8;;") {
		t.Errorf("a link of the content under the box survived: %q", got)
	}
	for _, i := range []int{1, 2} {
		if !strings.Contains(lines[i], "\x1b[94mBOX") {
			t.Errorf("line %d lost the box's colour: %q", i, lines[i])
		}
		if ansi.StringWidth(lines[i]) != 20 {
			t.Errorf("line %d is %d wide", i, ansi.StringWidth(lines[i]))
		}
		// After the box the grey starts afresh.
		after := lines[i][strings.Index(lines[i], "BOX")+3:]
		if strings.TrimSpace(ansi.Strip(after)) != "" && !strings.Contains(after, "\x1b[2;38;5;240m") {
			t.Errorf("line %d: the right of the box is not dimmed: %q", i, after)
		}
	}
	if !strings.HasPrefix(ansi.Strip(lines[1]), "linkBOXxt here") {
		t.Errorf("line 1 %q", ansi.Strip(lines[1]))
	}
}

func TestMarkdownLines(t *testing.T) {
	got := ansi.Strip(strings.Join(mdLines("# Title\n\nSome **bold** and [a link](https://x).\n\n\n\n1. one\n2. two\n> quoted\n<!-- hidden -->\n---", 40), "\n"))
	want := "Title\n\nSome bold and a link.\n\n1. one\n2. two\n│ quoted\n────────────"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestDump writes the pipelines grid and the run card over the dimmed
// board at 120x34 and 100x30 to the directory in YMCP_DUMP, ANSI stripped,
// for reviews.
func TestDump(t *testing.T) {
	dir := os.Getenv("YMCP_DUMP")
	if dir == "" {
		t.Skip("YMCP_DUMP not set")
	}
	f := newFixture(t)
	withIssues(f)
	write := func(name, view string) {
		t.Helper()
		if err := os.WriteFile(dir+"/"+name, []byte(ansi.Strip(view)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, size := range [][2]int{{120, 34}, {100, 30}, {160, 45}} {
		w, h := size[0], size[1]
		grid := drive(t, NewPipelines(&f.d, "youtrack-xlsft"), w, h)
		write(fmt.Sprintf("grid-%dx%d.txt", w, h), grid.m.View())
		board := drive(t, NewBoard(&f.d, "backend"), w, h)
		run := drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, false), w, h)
		write(fmt.Sprintf("run-%dx%d.txt", w, h), run.m.(*Run).Overlay(board.m.View()))
		run.keys("r")
		write(fmt.Sprintf("run-reply-%dx%d.txt", w, h), run.m.(*Run).Overlay(board.m.View()))
		perm := drive(t, NewRun(&f.d, "frontend", f.runs["FE-9"].Number, true), w, h)
		perm.typeText("/")
		write(fmt.Sprintf("run-slash-%dx%d.txt", w, h), perm.m.(*Run).Overlay(board.m.View()))
	}
}

func TestIsWorking(t *testing.T) {
	old := "![](https://raw.githubusercontent.com/xlsft/ymcp/main/docs/working.gif) Working..."
	for _, tc := range []struct {
		body string
		want bool
	}{
		{text.Working, true},
		{old, true},
		{"  " + text.Working + "\n", true},
		{"Done. " + text.Working, false},
		{"Working...", false},
		{"Fixed it.", false},
	} {
		if got := isWorking(tc.body); got != tc.want {
			t.Errorf("isWorking(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// TestRuleReachesTheFrame: the line under the top bar runs through the
// padding to the frame on both sides, at the side-by-side and the narrow
// layout.
func TestRuleReachesTheFrame(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {160, 45}, {70, 30}} {
		f := newFixture(t)
		withIssues(f)
		d := drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, false), size[0], size[1])
		found := false
		for _, l := range strings.Split(ansi.Strip(d.m.View()), "\n") {
			s := strings.TrimSpace(l)
			if strings.HasPrefix(s, "│─") && strings.HasSuffix(s, "─│") {
				found = true
			}
		}
		if !found {
			t.Errorf("%dx%d: no rule from frame to frame:\n%s", size[0], size[1], ansi.Strip(d.m.View()))
		}
	}
}

// TestModalOverHidesTheScreensHelp: below the card the screen is blank, so
// the key help of the screen underneath doesn't show.
func TestModalOverHidesTheScreensHelp(t *testing.T) {
	under := "head\nrow\nrow\nr run  s stop  q quit"
	got := strings.Split(modalOver(under, []string{"BOX"}, 2, 1, 20, 4), "\n")
	if len(got) != 4 {
		t.Fatalf("%d lines", len(got))
	}
	if !strings.Contains(ansi.Strip(got[0]), "head") {
		t.Errorf("the screen above the card is gone: %q", got[0])
	}
	for _, i := range []int{2, 3} {
		if strings.TrimSpace(ansi.Strip(got[i])) != "" || ansi.StringWidth(got[i]) != 20 {
			t.Errorf("line %d under the card is %q", i, got[i])
		}
	}
}
