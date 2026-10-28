package views

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// Promo timing: the track runs at 118 BPM with its beat grid from 0.238 s,
// so an eighth note is 0.2542 s, grid(k) is the k-th eighth and a bar is
// eight of them.
const (
	promoW, promoH = 120, 34
	promoEighth    = 60.0 / 118 / 2
	promoGrid0     = 0.238
)

func grid(k int) float64 { return promoGrid0 + float64(k)*promoEighth }

var promoSummaries = []string{
	"Rate limit the login endpoint", "Pagination for /issues", "Audit log for admin actions",
	"Retry Hub token refresh on 502", "Validate sprint dates", "CSV export for reports",
	"Fix time zones in due dates", "Webhook retries with backoff", "Drop the legacy v1 API",
	"Stream large attachments", "Search by custom fields", "Plain-text emails",
	"Speed up the board query", "Cache project field schemas", "Alerts for blocked issues",
	"Bulk tag issues", "Archive old projects", "Soft delete for comments",
	"Fix the flaky auth test", "Upgrade to Go 1.26", "Index issue history",
	"Lazy load avatars", "Merge duplicate users", "Localize error messages",
	"Health check endpoint", "Paginate time reports", "Encrypt stored secrets",
	"Trim log noise", "Fix N+1 in sprints", "Expose Prometheus metrics",
	"Retry email delivery", "Reorder board columns", "Export issues to JSON",
	"Deduplicate notifications", "Faster cold start", "Clean up feature flags",
	"Limit upload size", "Fix sorting by priority", "Graceful shutdown",
}

// The scenes the board harness draws, in eighths.
const (
	promoFlow   = 40 // bar 5: the board gets going (bars 3–4 are the agent)
	promoAsk    = 48 // bars 6–7: a run asks, the card opens, /approve
	promoOpen   = 50
	promoClose  = 58
	promoPeak   = 64 // bars 8–9: the backlog drains
	promoEnd    = 80 // bar 10: the outro
	promoAskIdx = 4
)

// promoIssue is one issue's path through the board, in eighths of the
// track.
type promoIssue struct {
	key, summary string
	arrive       float64 // seconds; below 0: in the backlog from the start
	start        int
	ask, answer  int // ask < 0: no question
	review, done int
	run          *store.Run
	state        store.RunStatus
}

func promoPlan() []*promoIssue {
	var out []*promoIssue
	for i, s := range promoSummaries {
		is := &promoIssue{key: fmt.Sprintf("YPC-%d", 201+i), summary: s, arrive: -1, ask: -1}
		if i >= 30 { // arrive during the first bar, one an eighth
			is.arrive = grid(i - 30)
		}
		out = append(out, is)
	}
	for i, is := range out {
		work, rev := 3, 2
		switch {
		case i < 8: // one an eighth
			is.start, work, rev = promoFlow+i, 4, 3
		case i < 24:
			is.start = promoAsk + i - 8
		default: // two an eighth
			is.start, work = promoPeak+(i-24)/2, 2
		}
		is.review = is.start + work
		switch i {
		case 10, 15: // questions answered half a bar later, on the board
			is.ask, is.answer = is.start+2, is.start+6
			is.review = is.answer + 2
		case promoAskIdx: // the one the card opens on
			is.ask, is.answer = promoAsk, promoClose
			is.review = is.answer + 2
		}
		is.done = is.review + rev
	}
	return out
}

// TestPromo writes the board frames of the promo video to $YMCP_PROMO/board:
// one .ans per keyframe, every sixteenth note, and board.txt listing
// "seconds file". The issues and their runs are made up.
func TestPromo(t *testing.T) {
	dir := os.Getenv("YMCP_PROMO")
	if dir == "" {
		t.Skip("YMCP_PROMO not set")
	}
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(0)
	lipgloss.SetHasDarkBackground(true)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	out := filepath.Join(dir, "board")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}

	f, backlog := promoFixture(t)
	plan := promoPlan()
	yt := newFakeIssues(func() time.Time { return f.clock })
	for _, is := range plan {
		yt.issues["2-"+is.key[4:]] = ytwatch.Issue{ID: "2-" + is.key[4:], Key: is.key, Summary: is.summary,
			URL:    "https://example.youtrack.cloud/issue/" + is.key,
			Fields: map[string]string{"State": "Open", "Priority": "Major", "Assignee": "jane", "Type": "Task"}}
	}
	asked := plan[promoAskIdx]
	ask := yt.issues["2-"+asked.key[4:]]
	ask.Description = "A sprint can end before it starts. Reject those dates in the API and in the database."
	ask.Fields["State"] = "In Progress"
	yt.issues["2-"+asked.key[4:]] = ask
	f.d.Issues = yt
	f.d.People = fakePeople{{Login: "jane", FullName: "Jane Doe", Email: "jane@example.com"}}

	ctx := context.Background()
	at := func(sec float64) { f.clock = t0.Add(time.Duration(sec * float64(time.Second))) }
	sync := func(sec float64, k int) {
		var waiting []ytwatch.Issue
		for _, is := range plan {
			want := store.StatusQueued // not started
			switch {
			case k >= is.done:
				want = store.StatusDone
			case k >= is.review:
				want = store.StatusReview
			case is.ask >= 0 && k >= is.ask && k < is.answer:
				want = store.StatusNeedsInput
			case k >= is.start:
				want = store.StatusWorking
			}
			if want == store.StatusQueued {
				if is.arrive < 0 || sec >= is.arrive {
					waiting = append(waiting, ytwatch.Issue{ID: "2-" + is.key[4:], Key: is.key, Summary: is.summary})
				}
				continue
			}
			if is.run == nil {
				r, err := f.st.CreateRun(ctx, "backend", "youtrack-work", "2-"+is.key[4:], is.key)
				f.must(err)
				f.must(f.st.AddEvent(ctx, r.ID, "run.captured", map[string]string{"issue": is.key, "summary": is.summary}))
				r.Agent, r.Stage = "Claude", "working"
				r.Branch = strings.ToLower(is.key)
				is.run = &r
			}
			if want == is.state {
				continue
			}
			r := is.run
			r.Status = want
			switch want {
			case store.StatusWorking:
				if is.state == store.StatusNeedsInput {
					f.must(f.st.AddEvent(ctx, r.ID, "inbox", map[string]string{"author": "jane", "text": "/approve"}))
					f.must(f.st.AddEvent(ctx, r.ID, "permission.approved", map[string]any{"tools": []string{"Bash(make migrate)"}}))
				}
				r.Stage, r.NeedsInputReason = "working", ""
				f.must(f.st.AddEvent(ctx, r.ID, "turn.start", map[string]string{"kind": "start"}))
				f.must(f.st.AddEvent(ctx, r.ID, "agent.tool_call", map[string]any{"tool": map[string]string{"title": "Edit " + promoFile(is.key), "status": "pending"}}))
			case store.StatusNeedsInput:
				r.NeedsInputReason = "The agent wants to run: Bash(make migrate). Answer /approve or /reject."
				f.must(f.st.AddEvent(ctx, r.ID, "agent.tool_update", map[string]any{"tool": map[string]any{"id": "mig", "title": "Bash", "status": "denied", "input": map[string]string{"command": "make migrate"}}}))
				f.must(f.st.AddEvent(ctx, r.ID, "turn.end", map[string]string{"reason": "permission needed"}))
				f.must(f.st.AddEvent(ctx, r.ID, "run.needs_input", map[string]string{"reason": r.NeedsInputReason}))
				yt.add("2-"+is.key[4:], agentComment(fmt.Sprintf("4-%d", 500+r.Number), r.Number,
					"I need to run `make migrate` to add the check on sprint dates. Answer /approve or /reject.", f.clock))
			case store.StatusReview:
				r.Stage, r.PRNumber = "review", 300+promoIndex(plan, is)
				r.PRURL = fmt.Sprintf("https://github.com/acme/backend/pull/%d", r.PRNumber)
				f.must(f.st.AddEvent(ctx, r.ID, "turn.end", map[string]string{"reason": "done"}))
			case store.StatusDone:
				r.Stage = "done"
			}
			f.must(f.st.UpdateRun(ctx, *r))
			is.state = want
		}
		// The source lists the newest first: what arrives lands on top.
		slices.SortStableFunc(waiting, func(x, y ytwatch.Issue) int { return promoArrival(plan, y.Key) - promoArrival(plan, x.Key) })
		backlog.issues = waiting
	}

	at(0)
	sync(0, -1)
	b := drive(t, NewBoard(&f.d, "backend"), promoW, promoH)
	var card *driver // the run card over the board, when one is open
	var list []string
	sixteenth := promoEighth / 2
	shoot := func(sec float64, k, step int) {
		at(sec)
		sync(sec, k)
		// The card: the question.
		switch {
		case k == promoOpen && card == nil:
			card = drive(t, NewRun(&f.d, "backend", asked.run.Number, true), promoW, promoH)
		case k >= promoClose && card != nil:
			card = nil
		}
		if card != nil && k >= promoOpen && k < promoClose {
			// "/" at the first beat, then "approve" a letter a sixteenth.
			typed := step - 2*(promoOpen+2)
			if typed == 0 {
				card.typeText("/")
			} else if typed > 0 && typed <= len("approve") {
				card.typeText(string("approve"[typed-1]))
			}
		}
		bd := b.m.(*Board)
		bd.backlogAt = time.Time{} // search the source again on this tick
		b.send(Tick{Now: f.clock})
		view := b.m.View()
		if card != nil {
			card.send(Tick{Now: f.clock})
			view = card.m.(*Run).Overlay(view)
		}
		name := fmt.Sprintf("board-%04d.ans", len(list))
		if err := os.WriteFile(filepath.Join(out, name), []byte(view), 0o644); err != nil {
			t.Fatal(err)
		}
		list = append(list, fmt.Sprintf("%.4f %s", sec, name))
	}
	// Bar 0 (the backlog grows) and bars 5–9, a keyframe every sixteenth.
	for _, span := range [][2]int{{0, 16}, {2 * promoFlow, 2 * promoEnd}} {
		for step := span[0]; step <= span[1]; step++ {
			sec := promoGrid0 + float64(step)*sixteenth
			if step == 0 {
				sec = 0
			}
			k := step / 2
			if sec < promoGrid0 {
				k = -1
			}
			shoot(sec, k, step)
		}
	}
	if err := os.WriteFile(filepath.Join(out, "board.txt"), []byte(strings.Join(list, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func promoArrival(plan []*promoIssue, key string) int {
	for i, p := range plan {
		if p.key == key && p.arrive >= 0 {
			return 1000 + i
		}
	}
	return 0
}

func promoIndex(plan []*promoIssue, is *promoIssue) int {
	for i, p := range plan {
		if p == is {
			return i
		}
	}
	return 0
}

func promoFile(key string) string {
	files := []string{"internal/auth/limit.go", "internal/api/issues.go", "internal/audit/log.go", "internal/hub/token.go",
		"internal/fields/cache.go", "internal/report/csv.go", "internal/sprint/dates.go", "internal/hooks/retry.go"}
	n := 0
	fmt.Sscanf(key, "YPC-%d", &n)
	return files[n%len(files)]
}

// promoFixture is a fixture with one pipeline and no runs yet.
func promoFixture(t *testing.T) (*fixture, *fakeBacklog) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{t: t, st: st, clock: t0, daemon: &fakeDaemon{}, runs: map[string]store.Run{}}
	st.SetClock(func() time.Time { return f.clock })
	f.backlog = &fakeBacklog{}
	p := pipelineSpec("backend", "youtrack-work", "project: YPC #Unresolved tag: ai")
	p.Workspace.Host, p.Workspace.Parallel = "build-box", 8
	f.d = Deps{
		Store: st,
		Profiles: func() ([]config.Config, error) {
			return []config.Config{{Name: "youtrack-work", DisplayName: "YouTrack (work)", URL: "https://example.youtrack.cloud", Login: "jane"}}, nil
		},
		Pipelines: func() ([]spec.Pipeline, error) { return []spec.Pipeline{p}, nil },
		Daemon:    f.daemon,
		Backlog:   f.backlog,
		OpenURL:   func(string) error { return nil },
		Now:       func() time.Time { return f.clock },
	}
	f.d = f.d.WithDefaults()
	f.must(st.AcquireRunner(context.Background(), "backend", 4242, "build-box", 0))
	return f, f.backlog
}
