package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/agent/agenttest"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
	"github.com/xlsft/ymcp/internal/pipeline/workspace/sshtest"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
	"github.com/xlsft/ymcp/internal/youtrack"
)

func TestMain(m *testing.M) {
	agenttest.Main()
	sshtest.Main()
	os.Exit(m.Run())
}

// waitTimeout bounds every wait of the end-to-end tests.
const waitTimeout = 20 * time.Second

func dur(d time.Duration) spec.Duration { return spec.Duration{Duration: d} }

// isolate keeps git away from the user's configuration.
func isolate(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	empty := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	// Worktrees go to the user cache directory; keep it in the temp home.
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("LocalAppData", filepath.Join(home, "appdata"))
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(home))
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, stderr, err := workspace.Local().Run(context.Background(), dir, append([]string{"git"}, args...), nil)
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr)
	}
	return strings.TrimSpace(string(out))
}

// newRepo builds a bare origin and a clone with one pushed commit on main.
func newRepo(t *testing.T) (repo, origin string) {
	t.Helper()
	isolate(t)
	base := t.TempDir()
	origin, repo = filepath.Join(base, "origin.git"), filepath.Join(base, "repo")
	git(t, base, "init", "--bare", "-b", "main", origin)
	git(t, base, "init", "-b", "main", repo)
	git(t, repo, "config", "user.name", "Dev")
	git(t, repo, "config", "user.email", "dev@xlsft.ru")
	git(t, repo, "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "init")
	git(t, repo, "push", "-u", "origin", "main")
	git(t, repo, "remote", "set-head", "origin", "main")
	return repo, origin
}

// harness is one pipeline over the fake YouTrack, the fake agent, a temp
// repository and a temp store.
type harness struct {
	t      *testing.T
	yt     *fakeYT
	ag     *fakeAgent
	st     *store.Store
	repo   string
	origin string
	p      spec.Pipeline
	// realAgent uses agent.New instead of the fake.
	realAgent bool
	// now, when set, is the engine's clock (Options.Now) and the store's.
	now func() time.Time
	// vcs, when set, is the strategy of the pipeline (Options.NewStrategy).
	vcs *fakeVCS
	// notes records Options.Notify.
	notes *notes
	// host, when set, is Options.Host.
	host workspace.Host
	// logs has every log line of the runners.
	logs *logLines

	cancel context.CancelFunc
	done   chan error
}

func testPipeline(repo string) spec.Pipeline {
	return spec.Pipeline{
		Name:    "backend",
		Profile: "test",
		Source:  spec.Source{Query: "State: Open", PollInterval: dur(25 * time.Millisecond)},
		Stages: spec.Stages{
			Working:    spec.Stage{Enter: "State In Progress", Match: "State: {In Progress}"},
			Review:     spec.Stage{Enter: "State Review", Match: "State: Review"},
			NeedsInput: spec.Stage{Enter: "State Blocked", Match: "State: Blocked"},
			Closed:     spec.Stage{Enter: "State Closed", Match: "State: Closed"},
		},
		Workspace: spec.Workspace{Dir: repo},
		Agent:     spec.Agent{Provider: "acp", Command: "fake-agent", Name: "Fake"},
		Timeouts:  spec.Timeouts{Silence: dur(10 * time.Second), Total: dur(time.Minute), Permission: dur(10 * time.Second)},
		Feedback:  spec.Feedback{Debounce: dur(time.Millisecond)},
	}
}

func newHarness(t *testing.T, mod func(p *spec.Pipeline)) *harness {
	t.Helper()
	repo, origin := newRepo(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, yt: newFakeYT(t), ag: &fakeAgent{}, st: st, repo: repo, origin: origin, p: testPipeline(repo), notes: &notes{}, logs: &logLines{}}
	if mod != nil {
		mod(&h.p)
	}
	t.Cleanup(func() {
		h.stop()
		st.Close()
	})
	return h
}

func (h *harness) options() Options {
	o := Options{
		Pipeline: h.p,
		Store:    h.st,
		Logger:   slog.New(slog.NewTextHandler(io.MultiWriter(testWriter{h.t}, h.logs), &slog.HandlerOptions{Level: slog.LevelDebug})),
		Version:  "test",
		Connect: func(context.Context, string, *slog.Logger) (*youtrack.Client, config.Config, error) {
			return h.yt.client(h.t), config.Config{Name: "test"}, nil
		},
	}
	if h.now != nil {
		o.Now = h.now
		h.st.SetClock(h.now)
	}
	if !h.realAgent {
		o.NewAgent = func(spec.Agent, workspace.Host, *slog.Logger) (agent.Agent, error) { return h.ag, nil }
	}
	if h.vcs != nil {
		o.NewStrategy = h.vcs.newStrategy
	}
	if h.host != nil {
		o.Host = h.host
	}
	o.Notify = h.notes.add
	return o
}

// logLines keeps log lines for the test to look at.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.Split(strings.TrimRight(string(p), "\n"), "\n")...)
	return len(p), nil
}

// matching returns the lines that contain every one of subs.
func (l *logLines) matching(subs ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		ok := true
		for _, s := range subs {
			ok = ok && strings.Contains(line, s)
		}
		if ok {
			out = append(out, line)
		}
	}
	return out
}

// testWriter sends log lines to t.Log while the test runs.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// start runs a new runner in the background.
func (h *harness) start() {
	h.t.Helper()
	r, err := New(context.Background(), h.options())
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- r.Run(ctx) }()
}

// stop stops the runner and waits for it.
func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("Run: %v", err)
		}
	case <-time.After(waitTimeout):
		h.t.Error("the runner did not stop")
	}
	h.cancel = nil
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) run(number int) store.Run {
	h.t.Helper()
	r, err := h.st.RunByNumber(context.Background(), h.p.Name, number)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// waitStatus waits for run #number to reach status.
func (h *harness) waitStatus(number int, status store.RunStatus) store.Run {
	h.t.Helper()
	var r store.Run
	h.waitFor("run #"+string(rune('0'+number))+" "+string(status), func() bool {
		var err error
		r, err = h.st.RunByNumber(context.Background(), h.p.Name, number)
		return err == nil && r.Status == status
	})
	return r
}

// waitReview waits for run #number to be in review with its review
// command applied (the store is written first, YouTrack after).
func (h *harness) waitReview(number int, key string) store.Run {
	h.t.Helper()
	r := h.waitStatus(number, store.StatusReview)
	h.waitFor(key+" in Review", func() bool { return h.yt.state(key) == "Review" })
	return r
}

// waitInvocations waits until the run has n finished invocations.
func (h *harness) waitInvocations(runID int64, n int) []store.Invocation {
	h.t.Helper()
	var invs []store.Invocation
	h.waitFor("invocations", func() bool {
		all, err := h.st.Invocations(context.Background(), runID)
		if err != nil {
			return false
		}
		invs = invs[:0]
		for _, inv := range all {
			if inv.EndedAt != nil {
				invs = append(invs, inv)
			}
		}
		return len(invs) >= n
	})
	return invs
}

func (h *harness) events(runID int64, kind string) []store.Event {
	h.t.Helper()
	all, err := h.st.Events(context.Background(), runID, 0, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []store.Event
	for _, e := range all {
		if kind == "" || e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func contains(list []string, sub string) bool {
	return slices.ContainsFunc(list, func(s string) bool { return strings.Contains(s, sub) })
}

// writeFile writes a file into the agent's worktree.
func writeFile(t *fakeTurn, name, content string) {
	os.WriteFile(filepath.Join(t.s.opts.Dir, name), []byte(content), 0o644)
}

func TestCaptureWorkPushReviewFeedback(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Feedback.YouTrack = []string{"carol"} })
	is := h.yt.addIssue("Fix the login page", "Open")
	h.yt.addIssue("Already in progress", "In Progress") // not in the source

	h.ag.script(
		func(t *fakeTurn) agent.Result {
			if _, err := t.Tool("report", map[string]any{"text": "Halfway there"}); err != nil {
				t.Say("report failed: " + err.Error())
			}
			writeFile(t, "login.go", "package login\n")
			t.Say("Fixed the login page.\nAdded login.go.")
			return agent.Result{}
		},
		func(t *fakeTurn) agent.Result {
			writeFile(t, "login.go", "package login // renamed\n")
			t.Say("Updated as asked.\n[1] Renamed it.\n[2] Done too.")
			return agent.Result{}
		},
	)
	h.start()

	run := h.waitReview(1, is.Key)
	if run.IssueKey != is.Key || run.Branch == "" || run.Worktree == "" || run.AgentSession != "session-1" {
		t.Fatalf("run = %+v", run)
	}
	// The worktree is in the user cache directory, outside the repository.
	cache, _ := os.UserCacheDir()
	want, _ := filepath.EvalSymlinks(filepath.Join(cache, "ymcp", "worktrees", "backend", run.Branch))
	if got, _ := filepath.EvalSymlinks(run.Worktree); got != want || want == "" {
		t.Errorf("worktree = %q, want %q", run.Worktree, want)
	}
	if got := h.yt.state(is.Key); got != "Review" {
		t.Errorf("state = %q, want Review", got)
	}
	if cmds := h.yt.commandLog(); !slices.Equal(cmds, []string{is.Key + ": State In Progress", is.Key + ": State Review"}) {
		t.Errorf("commands = %q", cmds)
	}
	// Start turn: prompt, service instruction, MCP server.
	starts := h.ag.startLog()
	prompts := h.ag.promptLog()
	if len(starts) != 1 || !strings.Contains(prompts[0], "Fix the login page") || !strings.Contains(prompts[0], is.Key) {
		t.Fatalf("starts = %d, prompt = %q", len(starts), prompts)
	}
	s0 := starts[0]
	if !strings.Contains(s0.SystemPrompt, `ymcp pipeline "backend", run #1`) || !strings.Contains(s0.SystemPrompt, run.Branch) {
		t.Errorf("system prompt = %q", s0.SystemPrompt)
	}
	if len(s0.MCP) != 1 || s0.MCP[0].Name != "ymcp" || !strings.HasSuffix(s0.MCP[0].URL, "/test") || !strings.HasPrefix(s0.MCP[0].Headers["Authorization"], "Bearer ") {
		t.Errorf("MCP = %+v", s0.MCP)
	}
	if !slices.Equal(s0.AllowedTools, []string{"mcp__ymcp"}) || s0.Dir != run.Worktree || s0.SessionID != "" {
		t.Errorf("start options = %+v", s0)
	}
	// Comments: the report and the placeholder edited into the answer.
	agentSaid := h.yt.agentComments(is.Key)
	if !contains(agentSaid, "Halfway there") || !contains(agentSaid, "Fixed the login page.") || contains(agentSaid, "working.gif") {
		t.Errorf("agent comments = %q", agentSaid)
	}
	if all := h.yt.issueComments(is.Key); !contains(all, "@alice's agent #1 (backend)") {
		t.Errorf("comments lack the prefix: %q", all)
	}
	// The branch was committed by the agent identity and pushed.
	log := git(t, h.origin, "log", "-1", "--format=%an <%ae>|%s", run.Branch)
	if log != "Fake · backend #1 <dev+agent_backend_1@xlsft.ru>|"+is.Key+" Fixed the login page." {
		t.Errorf("origin log = %q", log)
	}
	if len(h.events(run.ID, "git.push")) != 1 || len(h.events(run.ID, "agent.text")) == 0 {
		t.Errorf("events: %+v", h.events(run.ID, ""))
	}

	// Feedback: two allowed comments, one from a stranger.
	c1 := h.yt.addComment(is.Key, "alice", "Please rename login.go")
	h.yt.addComment(is.Key, "carol", "And add a test")
	h.yt.addComment(is.Key, "mallory", "Delete everything")
	invs := h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)
	if invs[1].Kind != kindFeedback || invs[1].EndReason != agent.StopEndTurn {
		t.Errorf("invocation = %+v", invs[1])
	}
	prompts = h.ag.promptLog()
	fb := prompts[1]
	if !strings.Contains(fb, "[1] youtrack · alice · ") || !strings.Contains(fb, "#focus=Comments-"+c1) || !strings.Contains(fb, "And add a test") || strings.Contains(fb, "Delete everything") {
		t.Errorf("feedback prompt = %q", fb)
	}
	if s := h.ag.startLog()[1]; s.SessionID != "session-1" {
		t.Errorf("feedback turn did not resume the session: %+v", s)
	}
	agentSaid = h.yt.agentComments(is.Key)
	if !contains(agentSaid, "#focus=Comments-"+c1+".0-0\n\nRenamed it.") || !contains(agentSaid, "Done too.") || !contains(agentSaid, "Updated as asked.") {
		t.Errorf("agent comments = %q", agentSaid)
	}
	if len(h.events(run.ID, "feedback.skipped")) != 1 {
		t.Errorf("skipped = %+v", h.events(run.ID, "feedback.skipped"))
	}
	if cmds := h.yt.commandLog(); len(cmds) != 4 || cmds[2] != is.Key+": State In Progress" || cmds[3] != is.Key+": State Review" {
		t.Errorf("commands = %q", cmds)
	}
	if n := strings.Count(git(t, h.origin, "log", "--format=%s", run.Branch), "\n") + 1; n != 3 {
		t.Errorf("origin has %d commits on the branch, want 3", n)
	}
	// Nothing else is captured: the other issue is not in the source.
	if runs, _ := h.st.Runs(context.Background(), h.p.Name, false); len(runs) != 1 {
		t.Errorf("runs = %d", len(runs))
	}
}

// TestSingleFeedbackItemRepliesInPlace: feedback with one YouTrack comment
// turns the placeholder into the reply instead of posting a second comment,
// with the reply to the item before the rest of the answer.
func TestSingleFeedbackItemRepliesInPlace(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Rename it", "Open")
	h.ag.script(
		func(t *fakeTurn) agent.Result {
			t.Say("First answer.")
			return agent.Result{}
		},
		func(t *fakeTurn) agent.Result {
			t.Say("Tests pass.\n[1] Renamed it.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitReview(1, is.Key)
	c := h.yt.addComment(is.Key, "alice", "Please rename login.go")
	h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)
	said := h.yt.agentComments(is.Key)
	if len(said) != 2 || said[0] != "First answer." || !strings.HasPrefix(said[1], "Re: ") ||
		!strings.HasSuffix(said[1], "#focus=Comments-"+c+".0-0\n\nRenamed it.\n\nTests pass.") {
		t.Errorf("agent comments = %q", said)
	}
}

func TestRequestInputThenCLIAnswer(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Pick a database", "Open")
	h.ag.script(
		func(t *fakeTurn) agent.Result {
			out, err := t.Tool("request_input", map[string]any{"question": "Postgres or SQLite? I recommend SQLite."})
			if err != nil {
				t.Say(err.Error())
			}
			t.Say("Asked: " + out)
			return agent.Result{}
		},
		func(t *fakeTurn) agent.Result {
			t.Say("Went with " + strings.TrimSpace(strings.SplitAfter(t.prompt, "use ")[1]))
			return agent.Result{}
		},
	)
	h.start()

	run := h.waitStatus(1, store.StatusNeedsInput)
	if !strings.Contains(run.NeedsInputReason, "Postgres or SQLite?") {
		t.Errorf("reason = %q", run.NeedsInputReason)
	}
	h.waitFor("State Blocked", func() bool { return h.yt.state(is.Key) == "Blocked" })
	// Nothing was committed yet: the branch is not pushed.
	if refs := git(t, h.origin, "for-each-ref", "--format=%(refname)", "refs/heads/"+run.Branch); refs != "" || len(h.events(run.ID, "git.push")) != 0 {
		t.Errorf("an empty branch was pushed: %q", refs)
	}
	if len(h.events(run.ID, "git.push_skipped")) != 1 {
		t.Errorf("events = %+v", h.events(run.ID, ""))
	}
	h.waitFor("the question", func() bool {
		said := h.yt.agentComments(is.Key)
		return contains(said, "Postgres or SQLite? I recommend SQLite.") && !contains(said, "working.gif")
	})

	if err := Answer(context.Background(), h.st, h.p.Name, 1, "use SQLite"); err != nil {
		t.Fatal(err)
	}
	h.waitInvocations(run.ID, 2)
	run = h.waitReview(1, is.Key)
	prompts := h.ag.promptLog()
	if len(prompts) != 2 || !strings.Contains(prompts[1], "use SQLite") {
		t.Errorf("prompts = %q", prompts)
	}
	if said := h.yt.agentComments(is.Key); !contains(said, "Went with SQLite") {
		t.Errorf("agent comments = %q", said)
	}
	if h.yt.state(is.Key) != "Review" {
		t.Errorf("state = %q", h.yt.state(is.Key))
	}
	invs, _ := h.st.Invocations(context.Background(), run.ID)
	if invs[1].Kind != kindAnswer {
		t.Errorf("kind = %q", invs[1].Kind)
	}
}

func TestPermissionAsk(t *testing.T) {
	for _, tc := range []struct {
		reply, option string
	}{
		{"/approve", "yes"},
		{"/reject because no", "no"},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			h := newHarness(t, nil)
			is := h.yt.addIssue("Run the tests", "Open")
			h.ag.script(func(t *fakeTurn) agent.Result {
				t.Say("answer: " + t.Ask("make test"))
				return agent.Result{}
			})
			h.start()
			run := h.waitStatus(1, store.StatusNeedsInput)
			if !strings.Contains(run.NeedsInputReason, "make test") {
				t.Errorf("reason = %q", run.NeedsInputReason)
			}
			h.waitFor("the permission comment", func() bool { return contains(h.yt.agentComments(is.Key), "Reply `/approve`") })
			if err := Answer(context.Background(), h.st, h.p.Name, 1, tc.reply); err != nil {
				t.Fatal(err)
			}
			h.waitReview(1, is.Key)
			if said := h.yt.agentComments(is.Key); !contains(said, "answer: "+tc.option) {
				t.Errorf("agent comments = %q", said)
			}
			if n := len(h.ag.startLog()); n != 1 {
				t.Errorf("%d sessions, want the one turn", n)
			}
		})
	}
}

// allOptions are the four answers kimi offers.
var allOptions = []agent.Option{
	{ID: "once", Name: "Allow", Kind: "allow_once"},
	{ID: "always", Name: "Allow always", Kind: "allow_always"},
	{ID: "no", Name: "Reject", Kind: "reject_once"},
	{ID: "never", Name: "Reject always", Kind: "reject_always"},
}

// TestOwnToolsAreAllowed: an ACP agent's requests for ymcp's own MCP tools
// are allowed at once, for the session when it can be, whatever the
// pipeline's permissions say; a request that only borrows the name is not.
func TestOwnToolsAreAllowed(t *testing.T) {
	for _, mode := range []string{"ask", "deny"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, func(p *spec.Pipeline) { p.Permissions = mode })
			is := h.yt.addIssue("Read the issue", "Open")
			h.ag.script(func(t *fakeTurn) agent.Result {
				a := t.AskWith("mcp__ymcp__get_issue", "other", allOptions)
				b := t.AskWith("ymcp_get_issue_comments", "", allOptions[:1])
				c := t.AskWith("mcp__ymcp__request_input", "other", append([]agent.Option{}, allOptions[1], allOptions[2]))
				t.Say(fmt.Sprintf("answers: %s %s %s", a, b, c))
				if mode == "deny" {
					t.Say(" fake: " + t.AskWith("mcp__ymcp__x", "execute", allOptions))
				}
				return agent.Result{}
			})
			h.start()
			run := h.waitReview(1, is.Key)
			want := "answers: always once always"
			if mode == "deny" {
				want += " fake: no"
			}
			if said := h.yt.agentComments(is.Key); !contains(said, want) {
				t.Errorf("agent comments = %q, want %q", said, want)
			}
			if len(h.events(run.ID, "run.needs_input")) != 0 {
				t.Error("the run waited for a human")
			}
		})
	}
}

// TestApprovedACPRequestIsAllowedForTheSession: /approve answers with the
// lasting allow option when there is one; /reject only refuses the call.
func TestApprovedACPRequestIsAllowedForTheSession(t *testing.T) {
	for reply, want := range map[string]string{"/approve": "always", "/reject": "no"} {
		t.Run(reply, func(t *testing.T) {
			h := newHarness(t, nil)
			is := h.yt.addIssue("Run the tests", "Open")
			h.ag.script(func(t *fakeTurn) agent.Result {
				t.Say("answer: " + t.AskWith("Bash", "execute", allOptions))
				return agent.Result{}
			})
			h.start()
			h.waitStatus(1, store.StatusNeedsInput)
			h.waitFor("the permission comment", func() bool { return contains(h.yt.agentComments(is.Key), "Reply `/approve`") })
			if err := Answer(context.Background(), h.st, h.p.Name, 1, reply); err != nil {
				t.Fatal(err)
			}
			h.waitReview(1, is.Key)
			if said := h.yt.agentComments(is.Key); !contains(said, "answer: "+want) {
				t.Errorf("agent comments = %q", said)
			}
		})
	}
}

func TestOwnTool(t *testing.T) {
	for _, tc := range []struct {
		title, kind string
		want        bool
	}{
		{"mcp__ymcp__get_issue", "other", true},
		{"mcp__ymcp__report", "", true},
		{"ymcp_get_issue", "read", true},
		{"ymcp/search_issues", "search", true},
		{"mcp__ymcp__get_issue", "execute", false},
		{"mcp__ymcp__", "other", false},
		{"mcp__ymcp__get issue; rm -rf", "other", false},
		{"mcp__github__get_issue", "other", false},
		{"Bash", "other", false},
		{"ymcpx_get_issue", "other", false},
	} {
		if got := ownTool(agent.ToolCall{Title: tc.title, Kind: tc.kind}); got != tc.want {
			t.Errorf("ownTool(%q, %q) = %v", tc.title, tc.kind, got)
		}
	}
}

func TestPermissionAllowAndDeny(t *testing.T) {
	for mode, want := range map[string]string{"allow": "yes", "deny": "no"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, func(p *spec.Pipeline) { p.Permissions = mode })
			is := h.yt.addIssue("Run the tests", "Open")
			h.ag.script(func(t *fakeTurn) agent.Result {
				t.Say("answer: " + t.Ask("make test"))
				return agent.Result{}
			})
			h.start()
			run := h.waitReview(1, is.Key)
			if said := h.yt.agentComments(is.Key); !contains(said, "answer: "+want) {
				t.Errorf("agent comments = %q", said)
			}
			if len(h.events(run.ID, "run.needs_input")) != 0 {
				t.Error("the run waited for a human")
			}
		})
	}
}

func TestPermissionTimeoutThenLateApprove(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Timeouts.Permission = dur(200 * time.Millisecond) })
	is := h.yt.addIssue("Run the tests", "Open")
	h.ag.script(
		func(t *fakeTurn) agent.Result {
			t.Say("first: " + t.Ask("make test"))
			return agent.Result{}
		},
		func(t *fakeTurn) agent.Result {
			t.Say("second: " + t.Ask("make test"))
			return agent.Result{}
		},
	)
	h.start()
	h.waitFor("the refusal", func() bool { return contains(h.yt.agentComments(is.Key), "first: no") })
	run := h.waitStatus(1, store.StatusNeedsInput)
	if !strings.Contains(run.NeedsInputReason, "not allowed to use: make test") {
		t.Errorf("reason = %q", run.NeedsInputReason)
	}
	if err := Answer(context.Background(), h.st, h.p.Name, 1, "/approve"); err != nil {
		t.Fatal(err)
	}
	h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)
	prompts := h.ag.promptLog()
	if !strings.Contains(prompts[1], "Approved: make test. Retry what was denied.") {
		t.Errorf("prompt = %q", prompts[1])
	}
	if said := h.yt.agentComments(is.Key); !contains(said, "second: yes") {
		t.Errorf("the remembered approval was not used: %q", said)
	}
}

func TestClaudeDeniedThenApprove(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Agent = spec.Agent{Provider: "claude", Name: "Claude"} })
	is := h.yt.addIssue("Build it", "Open")
	h.ag.script(
		func(t *fakeTurn) agent.Result {
			t.Say("I could not run the build.")
			return agent.Result{Denied: []agent.ToolCall{
				{Title: "Bash", Status: "denied", Input: json.RawMessage(`{"command":"make build","description":"Build"}`)},
				{Title: "Bash", Status: "denied", Input: json.RawMessage(`{"command":"make build","description":"Build"}`)},
				{Title: "Write", Status: "denied", Input: json.RawMessage(`{"file_path":"/tmp/out.txt","content":"x"}`)},
			}}
		},
		func(t *fakeTurn) agent.Result {
			t.Say("Built.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitStatus(1, store.StatusNeedsInput)
	// The reason says what /approve would allow, once per call.
	if !strings.Contains(run.NeedsInputReason, "Bash, Write.") || strings.Count(run.NeedsInputReason, "- Bash: `make build`") != 1 ||
		!strings.Contains(run.NeedsInputReason, "- Write: `/tmp/out.txt`") {
		t.Errorf("reason = %q", run.NeedsInputReason)
	}
	if err := Answer(context.Background(), h.st, h.p.Name, 1, "/approve"); err != nil {
		t.Fatal(err)
	}
	h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)
	starts := h.ag.startLog()
	// Exactly the denied calls are allowed, never the whole tool.
	if len(starts) != 2 || !slices.Equal(starts[1].AllowedTools, []string{"mcp__ymcp", "Bash(make build)", "Edit(//tmp/out.txt)"}) || !slices.Equal(starts[0].AllowedTools, []string{"mcp__ymcp"}) {
		t.Errorf("allowed tools: %q / %q", starts[0].AllowedTools, starts[len(starts)-1].AllowedTools)
	}
	if !strings.Contains(h.ag.promptLog()[1], "Approved: Bash(make build), Edit(//tmp/out.txt)") {
		t.Errorf("prompt = %q", h.ag.promptLog()[1])
	}
}

// TestClaudeApproveNeverGrantsTheWholeTool: a denied call that no rule
// allows exactly (a wildcard, a subshell, no command) is not approved by
// the bare tool name, also not with permissions: allow.
func TestClaudeApproveNeverGrantsTheWholeTool(t *testing.T) {
	for _, mode := range []string{"ask", "allow"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, func(p *spec.Pipeline) {
				p.Agent = spec.Agent{Provider: "claude", Name: "Claude"}
				p.Permissions = mode
			})
			h.yt.addIssue("Clean up", "Open")
			denied := []agent.ToolCall{
				{Title: "Bash", Status: "denied", Input: json.RawMessage(`{"command":"rm -rf build/*"}`)},
				{Title: "Bash", Status: "denied", Input: json.RawMessage(`{"command":"git commit -m \"$(cat msg)\""}`)},
				{Title: "Bash", Status: "denied"},
				{Title: "Bash", Status: "denied", Input: json.RawMessage(`{"command":"make clean"}`)},
			}
			h.ag.script(
				func(t *fakeTurn) agent.Result {
					t.Say("Could not clean.")
					return agent.Result{Denied: denied}
				},
				func(t *fakeTurn) agent.Result {
					t.Say("Still could not.")
					return agent.Result{Denied: denied}
				},
			)
			h.start()
			run := h.waitStatus(1, store.StatusNeedsInput)
			if !strings.Contains(run.NeedsInputReason, "`/approve` allows exactly `Bash(make clean)`") || !strings.Contains(run.NeedsInputReason, "stay denied") {
				t.Errorf("reason = %q", run.NeedsInputReason)
			}
			if mode == "ask" {
				if err := Answer(context.Background(), h.st, h.p.Name, 1, "/approve"); err != nil {
					t.Fatal(err)
				}
				h.waitInvocations(run.ID, 2)
				h.waitStatus(1, store.StatusNeedsInput)
			}
			starts := h.ag.startLog()
			if len(starts) != 2 || !slices.Equal(starts[1].AllowedTools, []string{"mcp__ymcp", "Bash(make clean)"}) {
				t.Errorf("allowed tools: %q", starts[len(starts)-1].AllowedTools)
			}
			for _, s := range starts {
				if slices.Contains(s.AllowedTools, "Bash") {
					t.Errorf("the whole of Bash was allowed: %q", s.AllowedTools)
				}
			}
		})
	}
}

// TestClaudeOnlyInexactDenials: when no denied call can be approved
// exactly, the reply is not offered /approve and approves nothing.
func TestClaudeOnlyInexactDenials(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Agent = spec.Agent{Provider: "claude", Name: "Claude"} })
	h.yt.addIssue("Clean up", "Open")
	h.ag.script(func(t *fakeTurn) agent.Result {
		t.Say("Could not clean.")
		return agent.Result{Denied: []agent.ToolCall{{Title: "Bash", Status: "denied", Input: json.RawMessage(`{"command":"rm *"}`)}}}
	})
	h.start()
	run := h.waitStatus(1, store.StatusNeedsInput)
	if strings.Contains(run.NeedsInputReason, "/approve") || !strings.Contains(run.NeedsInputReason, "stay denied") {
		t.Errorf("reason = %q", run.NeedsInputReason)
	}
	if v, _, _ := h.st.RunState(context.Background(), run.ID, statePending); v != "" {
		t.Errorf("pending = %q", v)
	}
}

// TestQuestionKeepsDenials: a turn that asks a question and had calls
// denied tells about both; the denied calls wait through a plain answer
// for a later /approve, which allows exactly them.
func TestQuestionKeepsDenials(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Agent = spec.Agent{Provider: "claude", Name: "Claude"} })
	is := h.yt.addIssue("Pick a database", "Open")
	h.ag.script(
		func(t *fakeTurn) agent.Result {
			if _, err := t.Tool("request_input", map[string]any{"question": "Postgres or SQLite?"}); err != nil {
				t.Say(err.Error())
			}
			t.Say("Asked.")
			return agent.Result{Denied: []agent.ToolCall{{Title: "Bash", Status: "denied", Input: json.RawMessage(`{"command":"make db"}`)}}}
		},
		func(t *fakeTurn) agent.Result {
			t.Say("Using SQLite.")
			if _, err := t.Tool("request_input", map[string]any{"question": "Which file name?"}); err != nil {
				t.Say(err.Error())
			}
			return agent.Result{}
		},
		func(t *fakeTurn) agent.Result {
			t.Say("Done.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitStatus(1, store.StatusNeedsInput)
	for _, want := range []string{"Postgres or SQLite?", "not allowed to use: Bash", "- Bash: `make db`", "Reply `/approve`"} {
		if !strings.Contains(run.NeedsInputReason, want) {
			t.Errorf("reason lacks %q: %q", want, run.NeedsInputReason)
		}
	}
	h.waitFor("the question", func() bool {
		said := h.yt.agentComments(is.Key)
		return contains(said, "Postgres or SQLite?") && contains(said, "- Bash: `make db`")
	})
	if v, _, _ := h.st.RunState(context.Background(), run.ID, statePending); !strings.Contains(v, "Bash(make db)") {
		t.Fatalf("pending = %q", v)
	}

	// A plain answer keeps them waiting.
	if err := Answer(context.Background(), h.st, h.p.Name, 1, "SQLite"); err != nil {
		t.Fatal(err)
	}
	h.waitInvocations(run.ID, 2)
	h.waitStatus(1, store.StatusNeedsInput)
	if p := h.ag.promptLog()[1]; strings.Contains(p, "Approved") || !strings.Contains(p, "SQLite") {
		t.Errorf("prompt = %q", p)
	}
	if v, _, _ := h.st.RunState(context.Background(), run.ID, statePending); !strings.Contains(v, "Bash(make db)") {
		t.Fatalf("pending after a plain answer = %q", v)
	}

	// /approve with an answer allows exactly them.
	if err := Answer(context.Background(), h.st, h.p.Name, 1, "/approve call it app.db"); err != nil {
		t.Fatal(err)
	}
	h.waitInvocations(run.ID, 3)
	h.waitReview(1, is.Key)
	p := h.ag.promptLog()[2]
	if !strings.Contains(p, "Approved: Bash(make db). Retry what was denied.") || !strings.Contains(p, "call it app.db") {
		t.Errorf("prompt = %q", p)
	}
	if starts := h.ag.startLog(); !slices.Contains(starts[2].AllowedTools, "Bash(make db)") {
		t.Errorf("allowed tools = %q", starts[2].AllowedTools)
	}
}

// gitIn runs git in the agent's worktree, as the agent would.
func gitIn(t *fakeTurn, args ...string) {
	out, stderr, err := workspace.Local().Run(context.Background(), t.s.opts.Dir, append([]string{"git"}, args...), nil)
	if err != nil {
		t.Say(fmt.Sprintf("git %v failed: %v: %s%s", args, err, stderr, out))
	}
}

// TestRewrittenBranchIsPushedWithLease: after the agent rewrites the
// branch (a rebase, an amend) the push is refused, and the runner pushes
// over the commit it pushed last; commits someone else pushed are never
// overwritten.
func TestRewrittenBranchIsPushedWithLease(t *testing.T) {
	for _, theirs := range []bool{false, true} {
		t.Run(fmt.Sprintf("someone else pushed=%v", theirs), func(t *testing.T) {
			h := newHarness(t, nil)
			is := h.yt.addIssue("Rebase me", "Open")
			h.ag.script(
				func(t *fakeTurn) agent.Result {
					writeFile(t, "a.txt", "a\n")
					t.Say("Added a.")
					return agent.Result{}
				},
				func(t *fakeTurn) agent.Result {
					writeFile(t, "a.txt", "a, rebased\n")
					gitIn(t, "add", "a.txt")
					gitIn(t, "commit", "--amend", "-m", "a, rebased")
					t.Say("Rebased.")
					return agent.Result{}
				},
			)
			h.start()
			run := h.waitReview(1, is.Key)
			first := git(t, h.origin, "rev-parse", "refs/heads/"+run.Branch)
			if v, _, _ := h.st.RunState(context.Background(), run.ID, statePushedHead); v != first {
				t.Fatalf("pushed head = %q, want %q", v, first)
			}
			other := ""
			if theirs {
				dir := filepath.Join(t.TempDir(), "other")
				git(t, filepath.Dir(dir), "clone", "-b", run.Branch, h.origin, dir)
				git(t, dir, "-c", "user.name=O", "-c", "user.email=o@x", "commit", "--allow-empty", "-m", "theirs")
				git(t, dir, "push", "origin", run.Branch)
				other = git(t, dir, "rev-parse", "HEAD")
			}
			h.yt.addComment(is.Key, "alice", "Please rebase")
			h.waitInvocations(run.ID, 2)
			if theirs {
				run = h.waitStatus(1, store.StatusNeedsInput)
				if !strings.Contains(run.NeedsInputReason, "Pushing the branch failed") || !strings.Contains(run.NeedsInputReason, "stale info") {
					t.Errorf("reason = %q", run.NeedsInputReason)
				}
				if got := git(t, h.origin, "rev-parse", "refs/heads/"+run.Branch); got != other {
					t.Errorf("origin = %s, want their commit %s", got, other)
				}
				return
			}
			run = h.waitReview(1, is.Key)
			head := git(t, run.Worktree, "rev-parse", "HEAD")
			if got := git(t, h.origin, "rev-parse", "refs/heads/"+run.Branch); got != head || head == first {
				t.Errorf("origin = %s, worktree = %s, first push %s", got, head, first)
			}
			pushes := h.events(run.ID, "git.push")
			if len(pushes) != 2 || !strings.Contains(string(pushes[1].Data), "with lease") || len(h.events(run.ID, "git.push_rejected")) != 1 {
				t.Errorf("events = %+v", h.events(run.ID, ""))
			}
		})
	}
}

// TestFeedbackTurnSeesFreshOrigin: before a feedback turn the runner
// fetches origin, so "rebase onto main" works without the agent fetching.
func TestFeedbackTurnSeesFreshOrigin(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Rebase me", "Open")
	seen := make(chan string, 1)
	h.ag.script(
		func(t *fakeTurn) agent.Result {
			writeFile(t, "a.txt", "a\n")
			t.Say("Added a.")
			return agent.Result{}
		},
		func(t *fakeTurn) agent.Result {
			out, _, _ := workspace.Local().Run(context.Background(), t.s.opts.Dir, []string{"git", "rev-parse", "origin/main"}, nil)
			seen <- strings.TrimSpace(string(out))
			t.Say("Rebased.")
			return agent.Result{}
		},
	)
	h.start()
	h.waitReview(1, is.Key)
	dir := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(dir), "clone", h.origin, dir)
	git(t, dir, "-c", "user.name=O", "-c", "user.email=o@x", "commit", "--allow-empty", "-m", "main moved")
	git(t, dir, "push", "origin", "main")
	want := git(t, dir, "rev-parse", "HEAD")
	h.yt.addComment(is.Key, "alice", "Please rebase onto main")
	select {
	case got := <-seen:
		if got != want {
			t.Errorf("origin/main in the second turn = %s, want %s", got, want)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no feedback turn")
	}
}

// TestDeniedAfterProgressGoesToReview: a turn that committed work but had
// calls denied goes to review with the denied calls in its answer; a
// /approve there retries them.
func TestDeniedAfterProgressGoesToReview(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Agent = spec.Agent{Provider: "claude", Name: "Claude"} })
	is := h.yt.addIssue("Hide done tasks", "Open")
	h.ag.script(
		func(t *fakeTurn) agent.Result {
			writeFile(t, "list.go", "package list\n")
			t.Say("Done tasks are hidden now.")
			return agent.Result{Denied: []agent.ToolCall{{Title: "Bash", Status: "denied", Input: json.RawMessage(`{"command":"make lint"}`)}}}
		},
		func(t *fakeTurn) agent.Result {
			t.Say("Lint is clean.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitReview(1, is.Key)
	if len(h.events(run.ID, "run.needs_input")) != 0 {
		t.Error("the run waited for a human")
	}
	h.waitFor("the answer", func() bool {
		return contains(h.yt.agentComments(is.Key), "Done tasks are hidden now.") &&
			contains(h.yt.agentComments(is.Key), "- Bash: `make lint`") &&
			contains(h.yt.agentComments(is.Key), "Reply `/approve` to let the agent retry them.")
	})
	if err := Answer(context.Background(), h.st, h.p.Name, 1, "/approve"); err != nil {
		t.Fatal(err)
	}
	invs := h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)
	if invs[1].Kind != kindAnswer {
		t.Errorf("the /approve ran a %s turn", invs[1].Kind)
	}
	if p := h.ag.promptLog()[1]; !strings.Contains(p, "Approved: Bash(make lint). Retry what was denied.") {
		t.Errorf("prompt = %q", p)
	}
	if starts := h.ag.startLog(); !slices.Contains(starts[1].AllowedTools, "Bash(make lint)") {
		t.Errorf("allowed tools = %q", starts[1].AllowedTools)
	}
}

// TestApprovalSurvivesRestart: the denied tools a reply is about and the
// approved ones are kept in the store, so a restarted runner still knows
// what /approve means.
func TestApprovalSurvivesRestart(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Agent = spec.Agent{Provider: "claude", Name: "Claude"} })
	is := h.yt.addIssue("Build it", "Open")
	h.ag.script(
		func(t *fakeTurn) agent.Result {
			t.Say("I could not run the build.")
			return agent.Result{Denied: []agent.ToolCall{{Title: "Bash", Status: "denied", Input: json.RawMessage(`{"command":"make"}`)}}}
		},
		func(t *fakeTurn) agent.Result {
			t.Say("Built.")
			return agent.Result{}
		},
		func(t *fakeTurn) agent.Result {
			t.Say("Built again.")
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitStatus(1, store.StatusNeedsInput)
	h.waitFor("the permission comment", func() bool { return contains(h.yt.agentComments(is.Key), "Reply `/approve`") })
	if v, ok, err := h.st.RunState(context.Background(), run.ID, statePending); err != nil || !ok || !strings.Contains(v, "Bash") {
		t.Fatalf("pending state = %q, %v, %v", v, ok, err)
	}
	h.stop()

	// A new runner, a new process as far as memory goes.
	h.start()
	if err := Answer(context.Background(), h.st, h.p.Name, 1, "/approve"); err != nil {
		t.Fatal(err)
	}
	h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)
	if v, _, _ := h.st.RunState(context.Background(), run.ID, statePending); v != "" {
		t.Errorf("pending state after the answer = %q", v)
	}
	if v, ok, _ := h.st.RunState(context.Background(), run.ID, stateApproved); !ok || !strings.Contains(v, "Bash") {
		t.Errorf("approved state = %q, %v", v, ok)
	}
	if !strings.Contains(h.ag.promptLog()[1], "Approved: Bash(make)") {
		t.Errorf("prompt = %q", h.ag.promptLog()[1])
	}
	h.stop()

	// The approval outlives another restart: the next turn allows Bash from the start.
	h.start()
	h.yt.addComment(is.Key, "alice", "Once more")
	h.waitInvocations(run.ID, 3)
	h.waitReview(1, is.Key)
	starts := h.ag.startLog()
	if len(starts) != 3 || !slices.Contains(starts[1].AllowedTools, "Bash(make)") || !slices.Contains(starts[2].AllowedTools, "Bash(make)") {
		t.Errorf("allowed tools: %+v", starts)
	}
}

// TestFakeClockStampsStore: with a fake clock set on both the engine and
// the store, what the store writes follows it, inbox items included.
func TestFakeClockStampsStore(t *testing.T) {
	// An hour behind the real time: comments carry YouTrack's own (here real)
	// timestamps, and one written before a run's CreatedAt is not feedback.
	began := time.Now()
	base := began.Add(-time.Hour)
	early := began.Add(-30 * time.Minute)
	// The debounce is long, so the feedback stays in the inbox to be looked at.
	h := newHarness(t, func(p *spec.Pipeline) { p.Feedback.Debounce = dur(time.Hour) })
	h.now = func() time.Time { return base.Add(time.Since(began)) }
	is := h.yt.addIssue("Clocked", "Open")
	h.start()
	run := h.waitReview(1, is.Key)
	if !run.CreatedAt.Before(early) || !run.UpdatedAt.Before(early) {
		t.Errorf("run times = %v / %v", run.CreatedAt, run.UpdatedAt)
	}
	h.yt.addComment(is.Key, "alice", "More please")
	var items []store.InboxItem
	h.waitFor("the inbox item", func() bool {
		items, _ = h.st.PendingInbox(context.Background(), run.ID)
		return len(items) == 1
	})
	if !items[0].CreatedAt.Before(early) {
		t.Errorf("inbox item created at %v, want the fake clock (before %v)", items[0].CreatedAt, early)
	}
	for _, e := range h.events(run.ID, "") {
		if !e.At.Before(early) {
			t.Errorf("event %s at %v", e.Kind, e.At)
		}
	}
}

func TestSilenceTimeout(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Timeouts.Silence = dur(300 * time.Millisecond) })
	is := h.yt.addIssue("Hang", "Open")
	h.ag.script(func(t *fakeTurn) agent.Result {
		writeFile(t, "partial.txt", "half done\n")
		t.Say("step 1\nstep 2\n")
		t.Hang()
		return agent.Result{}
	})
	h.start()
	run := h.waitStatus(1, store.StatusNeedsInput)
	if !strings.Contains(run.NeedsInputReason, "no output for 300ms") || !strings.Contains(run.NeedsInputReason, "step 2") {
		t.Errorf("reason = %q", run.NeedsInputReason)
	}
	h.waitFor("the reason in the placeholder", func() bool { return contains(h.yt.agentComments(is.Key), "no output for 300ms") })
	invs := h.waitInvocations(run.ID, 1)
	if invs[0].EndReason != "timeout: silence" || !strings.Contains(invs[0].LastOutput, "step 1") {
		t.Errorf("invocation = %+v", invs[0])
	}
	// The leftovers were still committed and pushed.
	if out := git(t, h.origin, "ls-tree", "--name-only", run.Branch); !strings.Contains(out, "partial.txt") {
		t.Errorf("origin tree = %q", out)
	}
}

func TestMovedBackToBacklogCancels(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Do it", "Open")
	h.start()
	run := h.waitReview(1, is.Key)
	h.yt.setState(is.Key, "Open")
	run = h.waitStatus(1, store.StatusCanceled)
	if !strings.Contains(run.NeedsInputReason, "backlog") {
		t.Errorf("reason = %q", run.NeedsInputReason)
	}
	// It stays in the source and is not captured again, until it leaves.
	time.Sleep(200 * time.Millisecond)
	if runs, _ := h.st.Runs(context.Background(), h.p.Name, false); len(runs) != 1 {
		t.Fatalf("captured again: %d runs", len(runs))
	}
	// The worktree goes, the branch stays.
	h.waitFor("the worktree removal", func() bool {
		_, err := os.Stat(run.Worktree)
		return os.IsNotExist(err)
	})
	if b := git(t, h.repo, "branch", "--list", run.Branch); b == "" {
		t.Error("the branch of a canceled run was removed")
	}
	h.yt.setState(is.Key, "Postponed")
	time.Sleep(100 * time.Millisecond)
	h.yt.setState(is.Key, "Open")
	// The next run starts afresh on a branch of its own.
	run2 := h.waitReview(2, is.Key)
	if run2.Branch != run.Branch+"-2" || run2.Worktree == run.Worktree {
		t.Errorf("run 2 on %q in %q; run 1 was on %q in %q", run2.Branch, run2.Worktree, run.Branch, run.Worktree)
	}
}

// TestCanceledRunKeepsWorktree: with workspace.keepWorktree the worktree
// of a canceled run stays.
func TestCanceledRunKeepsWorktree(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Workspace.KeepWorktree = true })
	is := h.yt.addIssue("Do it", "Open")
	h.start()
	run := h.waitReview(1, is.Key)
	h.yt.setState(is.Key, "Open")
	run = h.waitStatus(1, store.StatusCanceled)
	h.waitFor("the cleanup", func() bool {
		v, _, _ := h.st.RunState(context.Background(), run.ID, stateCanceled)
		return v == ""
	})
	if _, err := os.Stat(run.Worktree); err != nil {
		t.Errorf("the kept worktree was removed: %v", err)
	}
}

// TestOtherPipelineGetsItsOwnBranch: a run of another pipeline on the same
// issue renders the same branch name, but never takes over the branch or
// the worktree of the first pipeline's run, canceled or not.
func TestOtherPipelineGetsItsOwnBranch(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Workspace.KeepWorktree = true })
	is := h.yt.addIssue("Shared issue", "Open")
	h.start()
	first := h.waitReview(1, is.Key)
	if err := Cancel(context.Background(), h.st, h.p.Name, 1); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(1, store.StatusCanceled)
	h.stop()

	h.p.Name = "frontend"
	h.yt.setState(is.Key, "Open")
	h.start()
	h.waitFor("the frontend run", func() bool {
		r, err := h.st.RunByNumber(context.Background(), "frontend", 1)
		return err == nil && r.Status == store.StatusReview
	})
	second, _ := h.st.RunByNumber(context.Background(), "frontend", 1)
	if second.Branch != first.Branch+"-2" {
		t.Errorf("branch = %q, the first pipeline's is %q", second.Branch, first.Branch)
	}
	cache, _ := os.UserCacheDir()
	want, _ := filepath.EvalSymlinks(filepath.Join(cache, "ymcp", "worktrees", "frontend", second.Branch))
	if got, _ := filepath.EvalSymlinks(second.Worktree); got != want || want == "" {
		t.Errorf("worktree = %q, want %q", second.Worktree, want)
	}
	if _, err := os.Stat(first.Worktree); err != nil {
		t.Errorf("the first pipeline's kept worktree: %v", err)
	}
}

func TestMovedBackDuringTurnStopsAgent(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Long one", "Open")
	h.ag.script(func(t *fakeTurn) agent.Result {
		t.Say("working")
		t.Hang()
		return agent.Result{}
	})
	h.start()
	h.waitFor("the turn", func() bool { return len(h.ag.promptLog()) == 1 })
	h.waitFor("In Progress", func() bool { return h.yt.state(is.Key) == "In Progress" })
	time.Sleep(100 * time.Millisecond) // the runner sees it out of the source
	h.yt.setState(is.Key, "Open")
	run := h.waitStatus(1, store.StatusCanceled)
	h.waitFor("the placeholder edit", func() bool { return contains(h.yt.agentComments(is.Key), "Stopped: the run was canceled.") })
	invs := h.waitInvocations(run.ID, 1)
	if invs[0].EndReason != "canceled" {
		t.Errorf("end reason = %q", invs[0].EndReason)
	}
}

func TestDeletedIssueCancels(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Doomed", "Open")
	h.start()
	h.waitReview(1, is.Key)
	h.yt.deleteIssue(is.Key)
	run := h.waitStatus(1, store.StatusCanceled)
	if !strings.Contains(run.NeedsInputReason, "deleted") {
		t.Errorf("reason = %q", run.NeedsInputReason)
	}
}

// TestDeletedIssueDuringTurnIsQuiet: the placeholder of an issue deleted
// during a turn can't be edited; that is no warning.
func TestDeletedIssueDuringTurnIsQuiet(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Doomed", "Open")
	h.ag.script(func(t *fakeTurn) agent.Result {
		t.Hang()
		return agent.Result{}
	})
	h.start()
	h.waitFor("the turn", func() bool { return len(h.ag.promptLog()) == 1 })
	h.waitFor("In Progress", func() bool { return h.yt.state(is.Key) == "In Progress" })
	h.yt.deleteIssue(is.Key)
	run := h.waitStatus(1, store.StatusCanceled)
	h.waitInvocations(run.ID, 1)
	h.waitFor("the placeholder edit", func() bool { return len(h.logs.matching("placeholder failed")) > 0 })
	if warns := h.logs.matching("level=WARN", "placeholder"); len(warns) != 0 {
		t.Errorf("warnings: %q", warns)
	}
}

// TestIssueOfAnotherPipelineIsLoggedOnce: an issue of the source that
// another pipeline's run has is not captured, and the log says so once.
func TestIssueOfAnotherPipelineIsLoggedOnce(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Taken", "Open")
	if _, err := h.st.CreateRun(context.Background(), "frontend", "test", is.ID, is.Key); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.waitFor("the log line", func() bool { return len(h.logs.matching("another pipeline's run has", is.Key)) > 0 })
	time.Sleep(200 * time.Millisecond) // a few more polls
	if n := len(h.logs.matching("another pipeline's run has")); n != 1 {
		t.Errorf("logged %d times", n)
	}
	if runs, _ := h.st.Runs(context.Background(), h.p.Name, false); len(runs) != 0 {
		t.Errorf("captured: %+v", runs)
	}
}

// TestLogNamesThePipelineOnce: no log line carries the pipeline twice.
func TestLogNamesThePipelineOnce(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Log it", "Open")
	h.ag.script(func(ft *fakeTurn) agent.Result {
		if _, err := ft.Tool("report", map[string]any{"text": "Halfway"}); err != nil {
			ft.Say(err.Error())
		}
		ft.Say("Done.")
		return agent.Result{}
	})
	h.start()
	h.waitReview(1, is.Key)
	h.stop()
	if len(h.logs.matching("MCP access granted")) == 0 || len(h.logs.matching("MCP access revoked")) == 0 {
		t.Fatal("no MCP log lines")
	}
	for _, l := range h.logs.matching("pipeline=") {
		if strings.Count(l, "pipeline=") != 1 {
			t.Errorf("the pipeline is named twice: %s", l)
		}
	}
}

// TestEveryPromptHasTheLanguageRule: feedback and answer turns repeat the
// pipeline's language rule, not only the first prompt.
func TestEveryPromptHasTheLanguageRule(t *testing.T) {
	h := newHarness(t, func(p *spec.Pipeline) { p.Language = "Russian" })
	is := h.yt.addIssue("Speak Russian", "Open")
	h.start()
	run := h.waitReview(1, is.Key)
	h.yt.addComment(is.Key, "alice", "One more thing")
	h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)
	for i, p := range h.ag.promptLog() {
		if !strings.Contains(p, "questions) in Russian;") {
			t.Errorf("prompt %d lacks the language rule: %q", i, p)
		}
	}
}

func TestMovedOutsideStagesPausesAndClosedFinishes(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Wander", "Open")
	h.start()
	run := h.waitReview(1, is.Key)
	h.yt.setState(is.Key, "Postponed")
	run = h.waitStatus(1, store.StatusNeedsInput)
	if !strings.Contains(run.NeedsInputReason, "outside the pipeline's stages") {
		t.Errorf("reason = %q", run.NeedsInputReason)
	}
	if h.yt.state(is.Key) != "Postponed" {
		t.Errorf("a pause applied a command: state %q", h.yt.state(is.Key))
	}
	h.yt.setState(is.Key, "Closed")
	run = h.waitStatus(1, store.StatusClosed)
	h.waitFor("the worktree removal", func() bool {
		_, err := os.Stat(run.Worktree)
		return os.IsNotExist(err)
	})
	if b := git(t, h.repo, "branch", "--list", run.Branch); b == "" {
		t.Error("the branch was removed with the worktree")
	}
}

func TestWorkingCommandFails(t *testing.T) {
	h := newHarness(t, nil)
	h.yt.fail("State In Progress", "State expected: In Progress")
	h.yt.fail("State Blocked", "State expected: Blocked") // the issue stays in the source
	is := h.yt.addIssue("No rights", "Open")
	h.start()
	run := h.waitStatus(1, store.StatusNeedsInput)
	if !strings.Contains(run.NeedsInputReason, "State expected: In Progress") {
		t.Errorf("reason = %q", run.NeedsInputReason)
	}
	h.waitFor("the reason comment", func() bool { return contains(h.yt.agentComments(is.Key), "State expected: In Progress") })
	if n := len(h.ag.promptLog()); n != 0 {
		t.Errorf("the agent ran %d turns", n)
	}
	// The issue stays in the source but its run is not canceled for it.
	time.Sleep(150 * time.Millisecond)
	if run = h.run(1); run.Status != store.StatusNeedsInput {
		t.Errorf("status = %s", run.Status)
	}
	// Fixed: an answer starts the agent.
	h.yt.mu.Lock()
	delete(h.yt.failing, "State In Progress")
	h.yt.mu.Unlock()
	if h.yt.state(is.Key) != "Open" {
		t.Fatalf("state = %q", h.yt.state(is.Key))
	}
	if err := Answer(context.Background(), h.st, h.p.Name, 1, "rights granted, go"); err != nil {
		t.Fatal(err)
	}
	h.waitReview(1, is.Key)
	invs, _ := h.st.Invocations(context.Background(), run.ID)
	if len(invs) != 1 || invs[0].Kind != kindStart {
		t.Errorf("invocations = %+v", invs)
	}
}

func TestCancelFromCLI(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Long one", "Open")
	h.ag.script(func(t *fakeTurn) agent.Result {
		t.Hang()
		return agent.Result{}
	})
	h.start()
	h.waitFor("the turn", func() bool { return len(h.ag.promptLog()) == 1 })
	if err := Cancel(context.Background(), h.st, h.p.Name, 1); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the placeholder edit", func() bool { return contains(h.yt.agentComments(is.Key), "Stopped: the run was canceled.") })
	run := h.run(1)
	if run.Status != store.StatusCanceled {
		t.Errorf("status = %s", run.Status)
	}
	h.waitFor("the worktree removal", func() bool {
		_, err := os.Stat(run.Worktree)
		return os.IsNotExist(err)
	})
	if err := Cancel(context.Background(), h.st, h.p.Name, 1); err == nil {
		t.Error("canceling twice worked")
	}
	if err := Answer(context.Background(), h.st, h.p.Name, 1, "hello"); err == nil {
		t.Error("answering a canceled run worked")
	}
	if err := Answer(context.Background(), h.st, h.p.Name, 9, "hello"); err == nil {
		t.Error("answering an unknown run worked")
	}
}

func TestRestartResumes(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Resume me", "Open")
	h.start()
	run := h.waitReview(1, is.Key)
	h.stop()
	if _, _, _, held, _ := h.st.Runner(context.Background(), h.p.Name); held {
		t.Error("the runner lock is still held")
	}

	// Feedback arrives while no runner runs; a new runner picks it up.
	h.yt.addComment(is.Key, "alice", "One more thing")
	h.start()
	h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)
	starts := h.ag.startLog()
	if len(starts) != 2 || starts[1].SessionID != "session-1" || starts[1].Dir != run.Worktree {
		t.Errorf("starts = %+v", starts)
	}
	if !strings.Contains(h.ag.promptLog()[1], "One more thing") {
		t.Errorf("prompt = %q", h.ag.promptLog()[1])
	}
}

func TestRestartDuringTurn(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Interrupted", "Open")
	h.ag.script(func(t *fakeTurn) agent.Result {
		t.Say("working")
		t.Hang()
		return agent.Result{}
	})
	h.start()
	h.waitFor("the turn", func() bool { return len(h.ag.promptLog()) == 1 })
	h.stop()
	if run := h.run(1); run.Status != store.StatusWorking {
		t.Fatalf("status after stop = %s", run.Status)
	}
	h.start()
	run := h.waitStatus(1, store.StatusNeedsInput)
	if !strings.Contains(run.NeedsInputReason, "runner stopped") {
		t.Errorf("reason = %q", run.NeedsInputReason)
	}
	h.waitFor("the placeholder edit", func() bool { return contains(h.yt.agentComments(is.Key), "runner stopped") })
	if contains(h.yt.agentComments(is.Key), "working.gif") {
		t.Errorf("a placeholder was left: %q", h.yt.agentComments(is.Key))
	}
	if err := Answer(context.Background(), h.st, h.p.Name, 1, "go on"); err != nil {
		t.Fatal(err)
	}
	h.waitReview(1, is.Key)
	// Nobody asked a question: the reply resumes the cut-short turn.
	p := h.ag.promptLog()[1]
	if !strings.Contains(p, "Your previous turn on "+is.Key+" was interrupted") || !strings.Contains(p, "go on") || strings.Contains(p, "Reply to your question") {
		t.Errorf("prompt = %q", p)
	}
	// It is said once: the next answer is an answer again.
	if v, _, _ := h.st.RunState(context.Background(), run.ID, stateInterrupted); v != "" {
		t.Errorf("interrupted state = %q", v)
	}
}

func TestNoResumeStartsNewSessionWithRecap(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Recap", "Open")
	h.yt.issue(is.Key).Description = "The full story of the bug."
	h.yt.addComment(is.Key, "carol", "Context from before the run")
	h.ag.script(func(t *fakeTurn) agent.Result {
		writeFile(t, "a.txt", "a\n")
		t.Say("First answer.")
		return agent.Result{}
	})
	h.start()
	run := h.waitReview(1, is.Key)
	h.ag.mu.Lock()
	h.ag.noResume = true
	h.ag.mu.Unlock()
	h.yt.addComment(is.Key, "alice", "Again")
	h.waitInvocations(run.ID, 2)
	run = h.waitReview(1, is.Key)
	p := h.ag.promptLog()[1]
	if !strings.Contains(p, "could not be resumed") || !strings.Contains(p, "a.txt") || !strings.Contains(p, "First answer.") || !strings.Contains(p, "Again") {
		t.Errorf("prompt = %q", p)
	}
	// The recap repeats the task with the issue's description and comments.
	if !strings.Contains(p, "The task as it was first given to you") || !strings.Contains(p, "The full story of the bug.") || !strings.Contains(p, "Context from before the run") {
		t.Errorf("the recap lacks the issue: %q", p)
	}
	// turn.start records the prompt that was sent, recap included.
	var sent []string
	for _, e := range h.events(run.ID, "turn.start") {
		sent = append(sent, string(e.Data))
	}
	if len(sent) != 2 || !strings.Contains(sent[0], `"system":`) || strings.Contains(sent[0], "could not be resumed") || !strings.Contains(sent[1], "could not be resumed") || !strings.Contains(sent[1], `"system":`) {
		t.Errorf("turn.start events = %q", sent)
	}
	starts := h.ag.startLog()
	if last := starts[len(starts)-1]; last.SessionID != "" || last.SystemPrompt == "" {
		t.Errorf("start = %+v", last)
	}
	if run.AgentSession != "session-2" {
		t.Errorf("session = %q", run.AgentSession)
	}
}

// TestDeletedWorktreeIsCreatedAgain: a worktree deleted between turns is
// made again before the next turn, with the branch's earlier work.
func TestDeletedWorktreeIsCreatedAgain(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Keep my work", "Open")
	h.ag.script(
		func(t *fakeTurn) agent.Result {
			writeFile(t, "a.txt", "a\n")
			t.Say("First.")
			return agent.Result{}
		},
		func(t *fakeTurn) agent.Result {
			if _, err := os.Stat(filepath.Join(t.s.opts.Dir, "a.txt")); err != nil {
				t.Say("lost a.txt: " + err.Error())
			} else {
				t.Say("Second.")
			}
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitReview(1, is.Key)
	if err := os.RemoveAll(run.Worktree); err != nil {
		t.Fatal(err)
	}
	h.yt.addComment(is.Key, "alice", "Once more")
	h.waitInvocations(run.ID, 2)
	h.waitReview(1, is.Key)
	if said := h.yt.agentComments(is.Key); !contains(said, "Second.") {
		t.Errorf("agent comments = %q", said)
	}
	if len(h.events(run.ID, "worktree.missing")) != 1 {
		t.Errorf("events = %+v", h.events(run.ID, ""))
	}
	if starts := h.ag.startLog(); len(starts) != 2 || starts[1].Dir == "" {
		t.Errorf("starts = %+v", starts)
	} else if _, err := os.Stat(starts[1].Dir); err != nil {
		t.Errorf("the second turn's directory: %v", err)
	}
}

// TestMissingWorkingDirectoryIsNamed: an agent that can't start because
// its directory is gone gets an error that says so, not exec's "no such
// file" about the binary.
func TestMissingWorkingDirectoryIsNamed(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Lose the directory", "Open")
	h.ag.script(
		func(t *fakeTurn) agent.Result {
			os.RemoveAll(t.s.opts.Dir)
			t.err = errors.New("fake: start: fork/exec /usr/bin/fake: no such file or directory")
			return agent.Result{}
		},
		func(t *fakeTurn) agent.Result {
			t.Say("Back in " + filepath.Base(t.s.opts.Dir))
			return agent.Result{}
		},
	)
	h.start()
	run := h.waitStatus(1, store.StatusNeedsInput)
	if !strings.Contains(run.NeedsInputReason, "working directory "+run.Worktree+" does not exist") || !strings.Contains(run.NeedsInputReason, "fork/exec") {
		t.Errorf("reason = %q", run.NeedsInputReason)
	}
	if err := Answer(context.Background(), h.st, h.p.Name, 1, "try again"); err != nil {
		t.Fatal(err)
	}
	h.waitReview(1, is.Key)
	if said := h.yt.agentComments(is.Key); !contains(said, "Back in "+run.Branch) {
		t.Errorf("agent comments = %q", said)
	}
}

func TestDeletedPlaceholderIsPostedAgain(t *testing.T) {
	h := newHarness(t, nil)
	is := h.yt.addIssue("Delete my placeholder", "Open")
	started := make(chan struct{})
	release := make(chan struct{})
	h.ag.script(func(t *fakeTurn) agent.Result {
		close(started)
		<-release
		t.Say("The answer.")
		return agent.Result{}
	})
	h.start()
	<-started
	msgs, _ := h.st.Messages(context.Background(), h.run(1).ID)
	h.yt.deleteComment(msgs[0].ExternalID)
	close(release)
	h.waitReview(1, is.Key)
	if said := h.yt.agentComments(is.Key); !slices.Equal(said, []string{"The answer."}) {
		t.Errorf("agent comments = %q", said)
	}
}

func TestParallelLimit(t *testing.T) {
	h := newHarness(t, nil)
	one := h.yt.addIssue("One", "Open")
	two := h.yt.addIssue("Two", "Open")
	release := make(chan struct{})
	h.ag.script(func(t *fakeTurn) agent.Result {
		<-release
		return agent.Result{}
	})
	h.start()
	h.waitFor("the first turn", func() bool { return len(h.ag.promptLog()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if runs, _ := h.st.Runs(context.Background(), h.p.Name, false); len(runs) != 1 {
		t.Fatalf("%d runs while one is working with parallel 1", len(runs))
	}
	close(release)
	h.waitReview(1, one.Key)
	h.waitReview(2, two.Key) // a run in review does not hold the slot
}

func TestACPAgentEndToEnd(t *testing.T) {
	h := newHarness(t, nil)
	command, env := agenttest.Command("acp", agenttest.Script{
		State: t.TempDir(),
		Turns: [][]agenttest.Step{{{Text: "Looking. "}, {Permission: &agenttest.Tool{Title: "make test", Kind: "execute"}}, {Text: " Done."}}},
	})
	h.p.Agent = spec.Agent{Provider: "acp", Command: command, Env: env, Name: "Fake ACP"}
	h.realAgent = true
	is := h.yt.addIssue("Real adapter", "Open")
	h.start()
	h.waitStatus(1, store.StatusNeedsInput)
	if err := Answer(context.Background(), h.st, h.p.Name, 1, "/approve"); err != nil {
		t.Fatal(err)
	}
	h.waitReview(1, is.Key)
	said := h.yt.agentComments(is.Key)
	// /approve picks the option that keeps the tool allowed in the session.
	if !contains(said, "allowed approve_always") || !contains(said, "Done.") {
		t.Errorf("agent comments = %q", said)
	}
}

func TestCheck(t *testing.T) {
	h := newHarness(t, nil)
	h.yt.addIssue("Sample", "Open")
	h.yt.fail("State Closed", "State expected: Closed")
	// A tag that doesn't exist yet: tag creates it, untag works after that.
	h.p.Stages.NeedsInput = spec.Stage{Enter: "tag needs-input", Exit: "untag needs-input", Match: "tag: needs-input"}
	h.yt.fail("tag needs-input", "tag expected: needs-input")
	h.yt.fail("untag needs-input", "tag expected: needs-input")
	o := h.options()
	o.Pipeline = h.p
	res, err := CheckWith(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]CheckResult{}
	for _, r := range res {
		if _, dup := byName[r.Name]; !dup {
			byName[r.Name] = r
		}
	}
	want := map[string]bool{
		"pipeline": true, "YouTrack": true, "source query": true,
		"stages.working.enter command": true, "stages.closed.enter command": false,
		"working leaves the source": true, "workspace: git repository": true, "agent": true,
		"workspace: worktrees": true,
	}
	for name, ok := range want {
		r, found := byName[name]
		if !found || r.OK != ok {
			t.Errorf("%s: %+v (found %v), want OK=%v", name, r, found, ok)
		}
	}
	if r := byName["source query"]; !strings.Contains(r.Detail, "1 issues match: YPC-1") {
		t.Errorf("source detail = %q", r.Detail)
	}
	if !slices.ContainsFunc(res, func(r CheckResult) bool { return r.Warning && r.Detail == "fake warning" }) {
		t.Errorf("agent warning missing: %+v", res)
	}
	if r := byName["stages.needsInput.enter command"]; !r.OK || !r.Warning || r.Detail != "tag needs-input doesn't exist yet; YouTrack creates it on first use" {
		t.Errorf("tag: %+v", r)
	}
	if r := byName["stages.needsInput.exit command"]; !r.OK || !r.Warning || !strings.Contains(r.Detail, "tag needs-input doesn't exist yet") {
		t.Errorf("untag: %+v", r)
	}

	if r := byName["workspace: worktrees"]; !strings.HasSuffix(filepath.ToSlash(r.Detail), "/ymcp/worktrees/backend") {
		t.Errorf("worktrees detail = %q", r.Detail)
	}
	// A worktree root inside .git: a warning.
	h.p.Workspace.Worktrees = filepath.Join(h.repo, ".git", "wts")
	o.Pipeline = h.p
	res, _ = CheckWith(context.Background(), o)
	if !slices.ContainsFunc(res, func(r CheckResult) bool { return r.Name == "workspace: worktrees" && r.Warning }) {
		t.Errorf("no .git warning: %+v", res)
	}
	h.p.Workspace.Worktrees = ""

	// An issue in both the source and the working stage: a warning.
	h.p.Stages.Working.Match = "State: Open"
	o.Pipeline = h.p
	res, _ = CheckWith(context.Background(), o)
	if !slices.ContainsFunc(res, func(r CheckResult) bool { return r.Name == "working leaves the source" && r.Warning }) {
		t.Errorf("no overlap warning: %+v", res)
	}
}

func TestNewRejectsBadPipeline(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := New(context.Background(), Options{Pipeline: spec.Pipeline{Name: "x"}, Store: st}); err == nil {
		t.Error("an invalid pipeline was accepted")
	}
	if _, err := New(context.Background(), Options{Pipeline: testPipeline("x")}); err == nil {
		t.Error("no store was accepted")
	}
}

func TestRunnerBusy(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.st.AcquireRunner(context.Background(), h.p.Name, 1, "elsewhere", time.Minute); err != nil {
		t.Fatal(err)
	}
	r, err := New(context.Background(), h.options())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background()); !errors.Is(err, store.ErrRunnerBusy) {
		t.Errorf("err = %v", err)
	}
}

func TestCommitMessage(t *testing.T) {
	long := "The README edit is made but `git add`/`git commit` were denied, so the change is left uncommitted in the working tree."
	for _, tc := range []struct{ prefix, answer, want string }{
		{"YPC-5", "Fixed the login page.\nAdded login.go.", "YPC-5 Fixed the login page."},
		{"YPC-5", "Fixed the login page. Added login.go and a test for it.", "YPC-5 Fixed the login page."},
		{"YPC-5", "## Summary\n\nDone.", "YPC-5 Summary"},
		{"YPC-5", "**Renamed** login.go, e.g. to auth.go. Tests pass.", "YPC-5 Renamed login.go, e.g. to auth.go."},
		{"YPC-5", long, "YPC-5 The README edit is made but `git add`/`git commit` were denied, so"},
		{"YPC-5", "", "YPC-5 agent changes"},
		{"", "Привет, мир. Ещё одно предложение.", "Привет, мир."},
		{"X-1", strings.Repeat("a", 100), "X-1 agent changes"},
	} {
		got := commitMessage(tc.prefix, tc.answer)
		if got != tc.want {
			t.Errorf("commitMessage(%q, %q) = %q, want %q", tc.prefix, tc.answer, got, tc.want)
		}
		if n := len([]rune(got)); n > subjectLimit {
			t.Errorf("commitMessage(%q, %q) has %d runes", tc.prefix, tc.answer, n)
		}
	}
}

func TestHelpers(t *testing.T) {
	var tl tail
	for i := range 30 {
		tl.add("line " + string(rune('a'+i%26)) + "\n")
	}
	tl.add("partial")
	if lines := strings.Split(tl.String(), "\n"); len(lines) != tailLines || lines[len(lines)-1] != "partial" {
		t.Errorf("tail = %q", tl.String())
	}
	for in, want := range map[string][2]string{
		"/approve":          {"/approve", ""},
		" /REJECT not now ": {"/reject", "not now"},
		"approve":           {"", "approve"},
		"":                  {"", ""},
	} {
		if c, r := command(in); c != want[0] || r != want[1] {
			t.Errorf("command(%q) = %q, %q", in, c, r)
		}
	}
	for _, tc := range []struct{ cmd, text, want string }{
		{"tag needs-input", "tag expected: needs-input", "needs-input"},
		{"Untag {needs input}", "Tag expected: needs input", "needs input"},
		{"tag x", "tag expected: x; State expected: Bogus", ""},
		{"State Bogus", "tag expected: x", ""},
		{"tag", "tag expected", ""},
	} {
		if got := newTag(tc.cmd, ytwatch.CommandCheck{State: ytwatch.Invalid, Text: tc.text}); got != tc.want {
			t.Errorf("newTag(%q, %q) = %q, want %q", tc.cmd, tc.text, got, tc.want)
		}
	}
	for in, want := range map[string]string{"YPC-12": "YPC", "A-B-3": "A-B", "nokey": "", "X-y": ""} {
		if got := projectOf(in); got != want {
			t.Errorf("projectOf(%q) = %q", in, got)
		}
	}
	if got := trimText("привет", 5); got != "пр… [trimmed]" {
		t.Errorf("trimText = %q", got)
	}
	if got := fmtDuration(10 * time.Minute); got != "10m" {
		t.Errorf("fmtDuration = %q", got)
	}
	if got := fmtDuration(90 * time.Minute); got != "1h30m" {
		t.Errorf("fmtDuration = %q", got)
	}
}
