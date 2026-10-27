package views

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	d.has("backend #6", "⚑ NEEDS INPUT",
		"BE-127", "CSV export of the reports",
		"State: In Progress", "Priority: Major", "Assignee: xlsft", "stage working", "⚑ needs input", "⎇ be-127-csv-export",
		"Users want to export reports as CSV.", "Acceptance", "• a button on the report page", "[image]", "GET /reports/42.csv",
		"Comments · 2",
		"Anna Petrova @anna · 50m ago", "Please also cover the admin reports.",
		"@xlsft's agent #6 (backend) · 13m ago", "Which date format should the CSV export use",
		"Log", "captured", "question",
		"waiting 12m", "r to comment on BE-127",
		"r reply", "tab log", "esc close")
	d.hasNot("Estimation", "ymcp:agent", "<!--", "**export**")

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
	d.has("Canceled run #6.", "CANCELED", "canceled")
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
	d.has("WORKING", "State: Open", "Claude · writing · 4m12s",
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
	d.has("Permission", "a /approve", "x /reject", "a approve", "x reject")
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
	d.has("Log · tab shows the issue", "captured")
	d.hasNot("CSV export of the reports")
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
	d.has("YouTrack │ Pull request #40", "PR #40", "t PR thread", "p PR")
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

func TestMarkdownLines(t *testing.T) {
	got := ansi.Strip(strings.Join(mdLines("# Title\n\nSome **bold** and [a link](https://x).\n\n\n\n1. one\n2. two\n> quoted\n<!-- hidden -->\n---", 40), "\n"))
	want := "Title\n\nSome bold and a link.\n\n1. one\n2. two\n│ quoted\n────────────"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestRunModalDump writes the card at 160x45 and 100x30 to the directory in
// YMCP_DUMP, for reviews.
func TestRunModalDump(t *testing.T) {
	dir := os.Getenv("YMCP_DUMP")
	if dir == "" {
		t.Skip("YMCP_DUMP not set")
	}
	f := newFixture(t)
	withIssues(f)
	for _, size := range [][2]int{{160, 45}, {100, 30}} {
		d := drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, false), size[0], size[1])
		var m tea.Model = d.m
		name := fmt.Sprintf("%s/run-%dx%d.txt", dir, size[0], size[1])
		if err := os.WriteFile(name, []byte(ansi.Strip(m.View())+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
