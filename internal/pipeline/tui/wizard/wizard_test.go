package wizard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

// TestNewPipeline walks through every step of a new pipeline with the
// keyboard and checks the live help each one shows and the pipeline it
// ends with.
func TestNewPipeline(t *testing.T) {
	f := newFakes()
	d := start(t, f.deps, nil)

	// Basics: the first profile is picked and signed in to at once.
	d.must("New pipeline", "Basics", "step 1 of 6", "Pipeline name", "YouTrack profile", "YouTrack (xlsft)",
		"can't sign in with youtrack-beget", "‹ Back", "Next ›")
	d.typ("backend")
	d.keys("enter")
	if d.focused() != "profile" {
		t.Fatalf("enter in the name should go to the next field, at %s", d.focused())
	}
	d.keys("down")
	d.must("signed in to https://xlsft.youtrack.cloud as admin")
	if d.m.sh.p.Name != "backend" || d.m.sh.p.Profile != "youtrack-xlsft" {
		t.Fatalf("name %q profile %q", d.m.sh.p.Name, d.m.sh.p.Profile)
	}
	d.keys("tab")

	// The source query: completions from search/assist, live count and samples.
	d.typ("project: YPC #Unr")
	d.must("#Unresolved", "#Unreviewed")
	d.dump("1-basics-completions")
	d.keys("down", "enter")
	if got := d.m.sh.p.Source.Query; got != "project: YPC #Unresolved" {
		t.Fatalf("query after completion = %q", got)
	}
	d.must("3 issues match right now", "YPC-3", "Fix login redirect", "Rate limit the API")
	d.keys("ctrl+s")
	d.must("Saved searches", "Assigned to me", "AI backlog")
	d.keys("esc")
	d.mustNot("Saved searches")
	d.dump("1-basics")
	// Enter on the last field goes to Next and stays on the step.
	d.keys("enter")
	if d.step() != "Basics" || d.focused() != btnNext {
		t.Fatalf("enter on the last field: at %s, focus %s", d.step(), d.focused())
	}
	d.keys("enter")

	// Board: the default commands are checked on YPC-3 and the searches run.
	if d.step() != "Board" {
		t.Fatalf("at %s", d.step())
	}
	d.must("Set State In Progress · YPC-3", "1 in this stage now: YPC-1", "they leave the source query", "Review", "More stages")
	if v := f.yt.called("validate State In Progress @YPC-3"); len(v) == 0 {
		t.Fatalf("working command not validated: %v", f.yt.calls)
	}
	d.dump("2-board")
	if d.focused() != "working.enter" {
		t.Fatalf("focus %s", d.focused())
	}

	// Field picker: Review becomes State Verified.
	d.keys("tab", "tab", "ctrl+f")
	d.must("Review stage", "Pick a field", "State", "Priority", "Assignee", "Tag")
	d.keys("enter")
	d.typ("veri")
	d.must("Pick a value of State", "command State Verified", "search State: Verified")
	d.dump("2-board-picker")
	d.keys("enter")
	if st := d.m.sh.p.Stages.Review; st.Enter != "State Verified" || st.Match != "State: Verified" {
		t.Fatalf("review stage = %+v", st)
	}
	d.must("Set State Verified")

	// Adding to a stage: Working also assigns the issue.
	d.focus("working.enter")
	d.keys("ctrl+f", "down", "down", "enter", "tab")
	if st := d.m.sh.p.Stages.Working; st.Enter != "State In Progress Assignee me" || st.Match != "State: {In Progress} Assignee: me" {
		t.Fatalf("working stage = %+v", st)
	}

	// Typing a command: completions from commands/assist and a live check.
	d.keys("tab", "tab")
	if d.focused() != "review.enter" {
		t.Fatalf("focus %s", d.focused())
	}
	d.send(ctrlU())
	d.typ("State Bogus")
	d.must("invalid: State expected: Bogus")
	d.send(ctrlU())
	d.typ("State In")
	d.must("In Progress", "Incomplete")
	d.dump("2-board-command-completions")
	d.keys("down", "enter")
	if got := d.m.sh.p.Stages.Review.Enter; got != "State In Progress" {
		t.Fatalf("review enter after completion = %q", got)
	}
	d.send(ctrlU())
	d.typ("State Verified")

	// Needs input, Done and Closed are behind More stages.
	d.mustNot("Needs input", "Use the Done stage")
	d.keys("tab", "tab")
	if d.focused() != "more" {
		t.Fatalf("focus %s", d.focused())
	}
	d.keys("enter", "tab")
	if d.focused() != "needsInput.enter" || d.step() != "Board" {
		t.Fatalf("focus %s on %s", d.focused(), d.step())
	}
	d.must("Needs input", "Command when it asks", "Command when answered", "untag needs-input")
	d.focus("done")
	d.must("Done", "[ ] Use the Done stage")
	d.mustNot("Command when merged") // Done is off
	d.focus("closed.match")
	d.must("Closed", "Search for them")
	d.dump("2-board-more")
	d.focus("more")
	d.keys("enter")
	d.mustNot("Needs input")
	d.keys("ctrl+n")

	// Repository: path completion and the repository checks.
	if d.step() != "Repository" {
		t.Fatalf("at %s", d.step())
	}
	d.must("Repository directory", "on this machine", "Publishing", "▸ Advanced: host, base branch, issues at once, names")
	// Next is blocked until there is a repository.
	d.keys("ctrl+n")
	if d.step() != "Repository" || d.focused() != "dir" {
		t.Fatalf("Next left a step with errors: at %s, focus %s", d.step(), d.focused())
	}
	d.must("needs: a repository directory", "Type the path of the repository.")
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	for _, p := range []string{filepath.Join(repo, ".git"), filepath.Join(root, "notes")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d.typ(root + string(filepath.Separator) + "re")
	d.must("repo"+string(filepath.Separator), "git repository")
	d.dump("3-repository-path-completion")
	d.keys("down", "enter")
	if got := d.m.sh.p.Workspace.Dir; got != repo {
		t.Fatalf("dir = %q, want %q", got, repo)
	}
	d.must("✓ git repository · origin git@github.com:xlsft/ymcp.git",
		"origin: github.com/xlsft/ymcp", "suggests github-pr", "token from `gh auth token`", "github-pr        push and open a GitHub pull request")
	d.mustNot("needs:")
	if dirs := f.inspect.dirs; len(dirs) == 0 || dirs[len(dirs)-1] != repo {
		t.Fatalf("inspected %v", dirs)
	}
	// A new pipeline takes the detected strategy.
	if s := d.m.sh.p.Strategy; s.Kind != "github-pr" {
		t.Fatalf("strategy = %+v", s)
	}
	d.keys("tab", "tab")
	if d.focused() != "draft" {
		t.Fatalf("focus %s", d.focused())
	}
	d.keys("space")
	if !d.m.sh.p.Strategy.Draft {
		t.Fatal("draft not ticked")
	}
	d.keys("shift+tab", "down", "down")
	d.must("the strategy is for gitlab but origin is on github")
	d.typ("u")
	if s := d.m.sh.p.Strategy; s.Kind != "github-pr" || !s.Draft {
		t.Fatalf("strategy = %+v", s)
	}
	d.dump("3-repository")
	// The rest is under Advanced.
	d.mustNot("Base branch", "Branch name template")
	d.focus("advanced")
	d.keys("enter", "tab", "tab")
	if d.focused() != "base" {
		t.Fatalf("focus %s", d.focused())
	}
	d.must("Base branch", "Empty: the default branch.", "the default branch of origin (main)")
	d.focus("branch")
	d.must("Branch name template", "preview: YPC-3-fix-login-redirect")
	d.dump("3-repository-advanced")
	d.keys("ctrl+n")

	// Agent: every provider is checked; switching keeps each draft.
	if d.step() != "Agent" {
		t.Fatalf("at %s", d.step())
	}
	d.must("claude ✓", "kimi ✗", "opencode ✓", "acp ✗", "2.1.0 (Claude Code)", "runs every tool without asking")
	d.dump("4-agent")
	d.keys("right")
	if d.m.sh.p.Agent.Provider != "kimi" {
		t.Fatalf("provider = %q", d.m.sh.p.Agent.Provider)
	}
	d.must("kimi: executable file not found")
	d.keys("right", "right")
	d.must("set Command to start it", "Command", "The command that starts the ACP agent")
	d.keys("left", "left", "left", "tab")
	if d.focused() != "permissions" || d.m.sh.p.Agent.Provider != "claude" {
		t.Fatalf("focus %s, provider %s", d.focused(), d.m.sh.p.Agent.Provider)
	}
	d.keys("right")
	if d.m.sh.p.Permissions != "allow" {
		t.Fatalf("permissions = %q", d.m.sh.p.Permissions)
	}
	d.must("Every permission request is approved.")
	d.keys("left")

	// Prompts: preview on the real issue, preset switch.
	d.keys("tab", "tab")
	if d.focused() != "prompts" {
		t.Fatalf("focus %s", d.focused())
	}
	d.must("based on fix", "Preview of start", "on YPC-3", "Resolve issue YPC-3: Fix login redirect", "Login redirects to /404.")
	d.dump("4-agent-prompts")
	d.keys("shift+tab", "right", "enter")
	if pr := d.m.sh.p.Prompts; pr.Preset != "bugfix-test-first" || !strings.HasPrefix(pr.Start, "Fix bug {{issue.id}}") {
		t.Fatalf("prompts = %+v", pr.Preset)
	}
	d.keys("tab")
	d.must("Fix bug YPC-3", "based on bugfix-test-first")

	// Mode, arguments and timeouts are under Advanced.
	d.mustNot("Silence timeout")
	d.keys("tab", "enter", "tab")
	if d.focused() != "mode" {
		t.Fatalf("focus %s", d.focused())
	}
	d.typ("acc")
	d.must("acceptEdits")
	d.keys("down", "enter")
	if a := d.m.sh.p.Agent; a.Provider != "claude" || a.Mode != "acceptEdits" || len(a.Args) != 1 {
		t.Fatalf("agent = %+v", a)
	}
	d.focus("args")
	d.must("Extra arguments", "--dangerously-skip-permissions")
	d.focus("silence")
	d.must("Silence timeout", "the run counts as stuck")
	d.send(ctrlU())
	d.typ("soon")
	d.must("Use a duration such as 30m, 2h or 90s.")
	d.keys("ctrl+n")
	if d.step() != "Agent" || d.focused() != "silence" {
		t.Fatalf("Next left the agent with a bad timeout")
	}
	d.must("needs: a valid silence timeout")
	d.send(ctrlU())
	d.typ("15m")
	d.dump("4-agent-advanced")
	d.keys("ctrl+n")

	// People & alerts: YouTrack logins from the users API.
	if d.step() != "People & alerts" {
		t.Fatalf("at %s", d.step())
	}
	d.must("admin always counts")
	d.typ("iv")
	d.must("ivan.petrov", "Ivan Petrov")
	d.dump("5-people-users")
	d.keys("down", "enter")
	d.must("ivan.petrov")
	d.keys("tab")
	if d.focused() != "github" {
		t.Fatalf("focus %s", d.focused())
	}
	d.mustNot("GitLab logins") // the strategy is on GitHub
	d.typ("octocat, @hubot")
	if fb := d.m.sh.p.Feedback; !slices.Equal(fb.YouTrack, []string{"ivan.petrov"}) || !slices.Equal(fb.GitHub, []string{"octocat", "hubot"}) {
		t.Fatalf("feedback = %+v", fb)
	}
	// Notifications: only questions, from every provider.
	d.must("Notifications", "now: the machine's settings")
	d.focus("notify")
	d.keys("enter", "tab")
	d.must("toast", "on here · needs_input, run_failed")
	d.keys("tab", "space")
	d.must("[x] needs_input", "runner_stopped")
	d.dump("5-people")
	if n := d.m.sh.p.Notifications; n == nil || len(n.Providers) != 0 || !slices.Equal(n.Events, []string{"needs_input"}) {
		t.Fatalf("notifications = %+v", n)
	}
	d.keys("ctrl+n")

	// Review: the full check, then save & run.
	d.must("Summary", "Checks", "3 issues match: YPC-3, YPC-4, YPC-7", "stages.needsInput.enter command", "Save & run",
		"Alerts      providers - · events needs_input")
	if d.focused() != btnSave {
		t.Fatalf("focus %s", d.focused())
	}
	d.dump("6-review")
	d.keys("right", "enter")
	if len(d.out) != 1 {
		t.Fatalf("messages = %v", d.out)
	}
	done, ok := d.out[0].(Done)
	if !ok || !done.RunNow {
		t.Fatalf("got %#v", d.out[0])
	}
	p := done.Pipeline
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"name", p.Name, "backend"},
		{"profile", p.Profile, "youtrack-xlsft"},
		{"query", p.Source.Query, "project: YPC #Unresolved"},
		{"working", p.Stages.Working.Enter, "State In Progress Assignee me"},
		{"review", p.Stages.Review.Enter, "State Verified"},
		{"needs input exit", p.Stages.NeedsInput.Exit, "untag needs-input"},
		{"done on", p.Stages.Done.On(), false},
		{"dir", p.Workspace.Dir, repo},
		{"parallel", p.Workspace.Parallel, 1},
		{"provider", p.Agent.Provider, "claude"},
		{"args", strings.Join(p.Agent.Args, " "), "--dangerously-skip-permissions"},
		{"mode", p.Agent.Mode, "acceptEdits"},
		{"permissions", p.Permissions, "ask"},
		{"silence", p.Timeouts.Silence.Duration, 15 * time.Minute},
		{"total", p.Timeouts.Total.Duration, spec.DefaultTotal},
		{"strategy", p.Strategy.Kind, "github-pr"},
		{"draft", p.Strategy.Draft, true},
		{"preset", p.Prompts.Preset, "bugfix-test-first"},
		{"language", p.Language, "English"},
		{"poll", p.Source.PollInterval.Duration, 60 * time.Second},
		{"debounce", p.Feedback.Debounce.Duration, spec.DefaultDebounce},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if len(f.checker.got) == 0 || f.checker.got[len(f.checker.got)-1].Name != "backend" {
		t.Errorf("check ran on %v", f.checker.got)
	}
}

// TestEditExisting starts from a saved pipeline and keeps what the wizard
// does not show.
func TestEditExisting(t *testing.T) {
	f := newFakes()
	off := false
	existing := spec.Pipeline{
		Name:    "backend",
		Profile: "youtrack-xlsft",
		Source:  spec.Source{Query: "project: YPC #Unresolved"},
		Stages: spec.Stages{
			Working:    spec.Stage{Enter: "State In Progress", Match: "State: {In Progress}"},
			Review:     spec.Stage{Enter: "State Fixed", Match: "State: Fixed"},
			NeedsInput: spec.Stage{Enter: "tag needs-input", Exit: "untag needs-input"},
			Closed:     spec.Stage{Match: "State: {Won't fix}"},
		},
		Workspace:     spec.Workspace{Dir: "/src/repo", Parallel: 2},
		Agent:         spec.Agent{Provider: "opencode", Model: "anthropic/claude-sonnet-4", Env: map[string]string{"A": "1"}},
		Timeouts:      spec.Timeouts{Silence: spec.Duration{Duration: 5 * time.Minute}},
		Feedback:      spec.Feedback{YouTrack: []string{"ivan.petrov"}},
		Prompts:       spec.Prompts{Preset: "docs", Start: "Custom start for {{issue.id}}"},
		Messages:      map[string]string{"thinking": "…"},
		Service:       &off,
		Notifications: &spec.Notifications{Providers: []string{"toast"}},
	}
	d := start(t, f.deps, &existing)
	d.must("Edit pipeline backend", "signed in to https://xlsft.youtrack.cloud as admin")
	if got := f.factory.calls; !slices.Equal(got, []string{"youtrack-xlsft"}) {
		t.Fatalf("connected to %v", got)
	}
	// Keeping the name of the edited pipeline is fine.
	d.must("project: YPC #Unresolved", "3 issues match")
	d.mustNot("already exists")
	d.keys("ctrl+n")
	d.must("Set State In Progress")
	d.focus("needsInput.enter") // More stages is open: Closed has a search
	d.must("Needs input", "tag needs-input")
	d.focus("closed.match")
	d.must("Closed", "State: {Won't fix}")
	d.keys("ctrl+n")
	d.must("/src/repo")
	d.focus("parallel") // Advanced is open: 2 at once
	d.must("Issues at once")
	d.keys("ctrl+n")
	d.must("opencode ✓", "0.15.3")
	d.focus("prompts")
	d.must("based on docs, modified: start", "Custom start for YPC-3")
	d.focus("model") // Advanced is open: a model is set
	d.must("anthropic/claude-sonnet-4")
	d.focus("service")
	d.must("[ ] Add ymcp's service instruction")
	d.keys("ctrl+n")
	d.must("ivan.petrov")
	d.focus("providers")
	d.must("[x] toast")
	d.keys("ctrl+n")
	d.dump("edit-review")
	d.keys("enter")
	if len(d.out) != 1 {
		t.Fatalf("messages = %v", d.out)
	}
	done := d.out[0].(Done)
	p := done.Pipeline
	if done.RunNow || p.Name != "backend" || p.Agent.Env["A"] != "1" || p.Messages["thinking"] != "…" ||
		p.Timeouts.Silence.Duration != 5*time.Minute || p.ServiceEnabled() || p.Workspace.Parallel != 2 ||
		p.Prompts.Start != "Custom start for {{issue.id}}" || p.Stages.Closed.Match != "State: {Won't fix}" ||
		p.Notifications == nil || !slices.Equal(p.Notifications.Providers, []string{"toast"}) {
		t.Fatalf("pipeline = %+v", p)
	}
	if existing.Agent.Env["A"] != "1" || existing.Workspace.Parallel != 2 {
		t.Fatal("the wizard changed the pipeline it was given")
	}
}

// Next stays on a step with errors and says why; Back always works.
func TestValidationBlocksNext(t *testing.T) {
	d := start(t, newFakes().deps, nil)
	d.mustNot("Give the pipeline a name.") // not before the user tried
	d.must("needs: a pipeline name; a source query")
	d.keys("ctrl+n")
	if d.step() != "Basics" {
		t.Fatalf("moved on to %s without a name", d.step())
	}
	d.must("✗ needs: a pipeline name; a source query", "Give the pipeline a name.")
	if d.focused() != "name" {
		t.Fatalf("focus %s", d.focused())
	}
	d.focus("query")
	d.must("Type the query that finds issues for the agent.")
	// The Next button is blocked the same way.
	d.focus(btnNext)
	d.keys("enter")
	if d.step() != "Basics" {
		t.Fatalf("Next button moved on to %s", d.step())
	}
	d.dump("blocked-next")
	// alt+number can't skip it either.
	d.alt('3')
	if d.step() != "Basics" {
		t.Fatalf("alt+3 moved on to %s", d.step())
	}

	// Back works from a step with errors.
	d.goStep(2)
	d.keys("ctrl+n")
	if d.step() != "Repository" {
		t.Fatalf("at %s", d.step())
	}
	d.keys("ctrl+p")
	if d.step() != "Board" {
		t.Fatalf("ctrl+p went to %s", d.step())
	}
	d.goStep(2)
	d.focus(btnBack)
	d.keys("enter")
	if d.step() != "Board" {
		t.Fatalf("Back went to %s", d.step())
	}
	d.alt('1')
	if d.step() != "Basics" {
		t.Fatalf("alt+1 went to %s", d.step())
	}
	// Back on the first step has nowhere to go.
	d.keys("ctrl+p")
	if d.step() != "Basics" {
		t.Fatalf("at %s", d.step())
	}
}

// Enter moves through the fields and onto Next, never to another step.
func TestEnterDoesNotChangeSteps(t *testing.T) {
	existing := spec.Pipeline{Name: "x", Profile: "youtrack-xlsft", Source: spec.Source{Query: "project: YPC #Unresolved"}}
	d := start(t, newFakes().deps, &existing)
	for i := 0; i < 3; i++ {
		d.keys("enter")
		if d.step() != "Basics" {
			t.Fatalf("enter %d went to %s", i+1, d.step())
		}
	}
	if d.focused() != btnNext {
		t.Fatalf("focus %s", d.focused())
	}
	d.keys("enter")
	if d.step() != "Board" {
		t.Fatalf("enter on Next went to %s", d.step())
	}
	// Tab goes round the form: fields, then Back and Next.
	ids := d.m.focusables()
	d.focus(ids[len(ids)-3]) // the last field
	d.keys("tab")
	if d.focused() != btnBack {
		t.Fatalf("focus %s", d.focused())
	}
	d.keys("tab")
	if d.focused() != btnNext {
		t.Fatalf("focus %s", d.focused())
	}
	d.keys("tab")
	if d.focused() != "working.enter" || d.step() != "Board" {
		t.Fatalf("focus %s on %s", d.focused(), d.step())
	}
	d.keys("shift+tab", "shift+tab")
	if d.focused() != btnBack {
		t.Fatalf("shift+tab: focus %s", d.focused())
	}
}

func TestAdvancedSections(t *testing.T) {
	existing := spec.Pipeline{Name: "x", Profile: "youtrack-xlsft", Source: spec.Source{Query: "project: YPC #Unresolved"}, Workspace: spec.Workspace{Dir: "/src/repo"}}
	d := start(t, newFakes().deps, &existing)
	d.goStep(2)
	d.must("▸ Advanced: host, base branch, issues at once, names")
	d.mustNot("Branch name template")
	if slices.Contains(d.m.focusables(), "branch") {
		t.Fatal("a closed section's fields take the focus")
	}
	d.focus("advanced")
	d.keys("enter")
	d.must("▾ Advanced")
	d.focus("branch")
	d.must("Branch name template", "{{issue.id}}-{{issue.summary | slug}}")
	// A broken template keeps its section open and blocks Next.
	d.typ("{{")
	d.focus("advanced")
	d.keys("enter")
	d.must("▾ Advanced")
	d.keys("ctrl+n")
	if d.step() != "Repository" || d.focused() != "branch" {
		t.Fatalf("at %s, focus %s", d.step(), d.focused())
	}
	d.must("needs: a valid branch template", "Branch name template")

	d.goStep(4)
	d.mustNot("Wait for more feedback")
	d.focus("advanced")
	d.keys("space", "tab")
	d.must("Wait for more feedback", "several comments make one turn")
}

func TestNameTaken(t *testing.T) {
	d := start(t, newFakes().deps, nil)
	d.typ("frontend")
	d.must("a pipeline named \"frontend\" already exists")
	d.keys("ctrl+n")
	if d.step() != "Basics" {
		t.Fatalf("moved on to %s with a taken name", d.step())
	}
	d.must("needs: a valid pipeline name")
}

func TestInvalidPipelineIsNotSaved(t *testing.T) {
	d := start(t, newFakes().deps, nil)
	d.goStep(5)
	d.must("Fix these before saving", "pipeline name is empty", "source.query is required", "workspace.dir is required")
	if d.focused() != btnSave {
		t.Fatalf("focus %s", d.focused())
	}
	d.keys("enter")
	d.focus(btnRun)
	d.keys("enter")
	if len(d.out) != 0 {
		t.Fatalf("saved an invalid pipeline: %v", d.out)
	}
	d.must("the pipeline isn't valid yet")
}

func TestCancel(t *testing.T) {
	// Nothing changed: esc leaves at once.
	d := start(t, newFakes().deps, nil)
	d.keys("esc")
	if len(d.out) != 1 || d.out[0] != (Cancelled{}) {
		t.Fatalf("got %v", d.out)
	}

	d = start(t, newFakes().deps, nil)
	d.typ("x")
	d.keys("esc")
	d.must("Discard your changes?")
	d.typ("n")
	d.mustNot("Discard your changes?")
	if len(d.out) != 0 {
		t.Fatal("cancelled on n")
	}
	d.keys("esc")
	d.typ("y")
	if len(d.out) != 1 || d.out[0] != (Cancelled{}) {
		t.Fatalf("got %v", d.out)
	}

	d = start(t, newFakes().deps, nil)
	d.keys("ctrl+c")
	if len(d.out) != 1 || d.out[0] != (Cancelled{}) {
		t.Fatalf("ctrl+c: got %v", d.out)
	}
}

// Esc closes completions and pickers before it offers to cancel.
func TestEscClosesPopupsFirst(t *testing.T) {
	f := newFakes()
	existing := spec.Pipeline{Name: "x", Profile: "youtrack-xlsft", Source: spec.Source{Query: "project: YPC #Unresolved"}}
	d := start(t, f.deps, &existing)
	d.goStep(1)
	d.keys("ctrl+f")
	d.must("Pick a field")
	d.keys("esc")
	d.mustNot("Pick a field", "Discard your changes?")
	d.typ(" x")
	d.keys("esc")
	d.must("Discard your changes?")
}

func TestStaleSearchResultsAreDropped(t *testing.T) {
	f := newFakes()
	d := start(t, f.deps, nil)
	d.focus("profile")
	d.keys("down") // a profile that signs in
	d.focus("query")
	src := d.m.steps[0].(*basicsStep)
	old := src.deb.seq
	d.typ("project: YPC #Unresolved")
	d.send(sampleMsg{id: src.deb.id, seq: old, query: "old", err: os.ErrClosed})
	d.mustNot("file already closed")
	d.must("3 issues match")
}

func TestSourceErrors(t *testing.T) {
	d := start(t, newFakes().deps, nil)
	d.typ("x")
	d.focus("profile")
	d.keys("down")
	d.focus("query")
	d.typ("bogus:")
	d.must("Could not parse query")
	if d.m.steps[0].mark() != markBad {
		t.Fatal("basics step not marked")
	}
}

func TestTagPickerAllowsNewTags(t *testing.T) {
	f := newFakes()
	existing := spec.Pipeline{Name: "x", Profile: "youtrack-xlsft", Source: spec.Source{Query: "project: YPC #Unresolved"}}
	d := start(t, f.deps, &existing)
	d.goStep(1)
	d.focus("needsInput.enter")
	d.keys("ctrl+f", "up", "enter")
	d.must("Needs input stage", "Pick a value of tag", "ai", "needs-input")
	d.dump("2-board-tag-picker")
	d.typ("waiting on human")
	d.must("new tag (YouTrack creates it on first use)", "command tag {waiting on human}")
	d.keys("enter")
	st := d.m.sh.p.Stages.NeedsInput
	if st.Enter != "tag {waiting on human}" || st.Exit != "untag {waiting on human}" || st.Match != "tag: {waiting on human}" {
		t.Fatalf("needs input = %+v", st)
	}
	if d.focused() != "needsInput.enter" {
		t.Fatalf("focus %s", d.focused())
	}
}

func TestPromptEditing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.md")
	if err := os.WriteFile(path, []byte("Edited {{issue.id}}\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	msg := readEdited(2, path, nil).(editedMsg)
	if msg.err != nil || msg.text != "Edited {{issue.id}}" || msg.which != 2 {
		t.Fatalf("got %+v", msg)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}

	d := start(t, newFakes().deps, nil)
	d.goStep(3)
	d.focus("prompts")
	d.keys("down", "down") // the answer prompt
	d.send(msg)
	d.must("based on fix, modified: answer", "Preview of answer", "Edited ABC-1")
	if d.m.sh.p.Prompts.Answer != "Edited {{issue.id}}" {
		t.Fatalf("answer = %q", d.m.sh.p.Prompts.Answer)
	}
	// An empty file keeps the old text.
	d.send(editedMsg{which: 2, text: ""})
	d.must("the answer prompt is empty")
	d.typ("r")
	d.must("based on fix")
	d.mustNot("modified")
	// Switching presets over edits asks first.
	d.send(editedMsg{which: 0, text: "Mine"})
	d.focus("preset")
	d.keys("right", "enter")
	d.must("Your edits to start will be replaced")
	if d.m.sh.p.Prompts.Start != "Mine" {
		t.Fatal("edits replaced without asking")
	}
	d.keys("enter")
	if d.m.sh.p.Prompts.Preset != "bugfix-test-first" || d.m.sh.p.Prompts.Start == "Mine" {
		t.Fatalf("prompts = %+v", d.m.sh.p.Prompts)
	}
}

func TestNoStrategyDetector(t *testing.T) {
	f := newFakes()
	f.deps.Strategy = nil
	existing := spec.Pipeline{Name: "x", Profile: "youtrack-xlsft", Source: spec.Source{Query: "q"}, Workspace: spec.Workspace{Dir: "/src/repo"}}
	d := start(t, f.deps, &existing)
	d.goStep(2)
	d.must("detection isn't available here")
}

func TestSuggestionApply(t *testing.T) {
	for _, c := range []struct {
		text  string
		s     Suggestion
		want  string
		caret int
	}{
		{"project: YPC Sta", Suggestion{Option: "State", Suffix: ": ", CompletionStart: 13, CompletionEnd: 16, Caret: 20}, "project: YPC State: ", 20},
		{"project: YPC Sta", Suggestion{Option: "Star", Prefix: "#", Suffix: " ", CompletionStart: 13, CompletionEnd: 16, Caret: 19}, "project: YPC #Star ", 19},
		{"State In", Suggestion{Option: "In Progress", Suffix: " ", CompletionStart: 6, CompletionEnd: 8, Caret: 18}, "State In Progress ", 18},
		{"ab", Suggestion{Option: "abc", CompletionStart: 0, CompletionEnd: 99}, "abc", 3},
		{"Задача", Suggestion{Option: "Задачи", CompletionStart: 0, CompletionEnd: 6}, "Задачи", 6},
	} {
		got, caret := c.s.Apply(c.text)
		if got != c.want || caret != c.caret {
			t.Errorf("%q + %+v = %q @%d, want %q @%d", c.text, c.s, got, caret, c.want, c.caret)
		}
	}
}

func TestPathSuggest(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"alpha/.git", "alps", ".hidden", "beta"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "alfile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.Separator)
	got, _ := pathSuggest(root+sep+"al", 0)(t.Context())
	var opts []string
	for _, s := range got {
		opts = append(opts, s.Option+"|"+s.Description)
	}
	want := []string{root + sep + "alpha" + sep + "|git repository", root + sep + "alps" + sep + "|"}
	if !slices.Equal(opts, want) {
		t.Fatalf("got %v, want %v", opts, want)
	}
	got, _ = pathSuggest(root+sep, 0)(t.Context())
	if len(got) != 3 {
		t.Fatalf("hidden directories should stay hidden: %v", got)
	}
	got, _ = pathSuggest(root+sep+".h", 0)(t.Context())
	if len(got) != 1 {
		t.Fatalf("a dot prefix shows hidden directories: %v", got)
	}
}

func TestSSHHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	cfg := "Host build-box\n  HostName 10.0.0.2\nHost *.internal gpu gpu\nhost staging\nMatch all\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := sshHosts(path); !slices.Equal(got, []string{"build-box", "gpu", "staging"}) {
		t.Fatalf("hosts = %v", got)
	}
	if got := sshHosts(filepath.Join(t.TempDir(), "none")); got != nil {
		t.Fatalf("hosts of a missing file = %v", got)
	}
}

// An ssh host is stored as typed; the path is not completed or expanded
// here, and the checks run there.
func TestSSHHost(t *testing.T) {
	f := newFakes()
	existing := spec.Pipeline{Name: "x", Profile: "youtrack-xlsft", Source: spec.Source{Query: "q"}}
	d := start(t, f.deps, &existing)
	d.goStep(2)
	d.focus("advanced")
	d.keys("enter", "tab")
	if d.focused() != "host" {
		t.Fatalf("focus %s", d.focused())
	}
	d.typ("build-box")
	d.focus("dir")
	d.must("on build-box")
	d.typ("~/src/repo")
	if w := d.m.sh.p.Workspace; w.Host != "build-box" || w.Dir != "~/src/repo" {
		t.Fatalf("workspace = %+v", w)
	}
	if dirs := f.inspect.dirs; len(dirs) == 0 || dirs[len(dirs)-1] != "~/src/repo" {
		t.Fatalf("inspected %v", dirs)
	}
	d.typ(" x")
	d.focus("host")
	d.send(ctrlU())
	d.typ("bad host")
	d.must("An ssh host alias has no spaces.")
}

func TestClause(t *testing.T) {
	for _, c := range []struct {
		f           Field
		v, cmd, qry string
	}{
		{Field{Name: "State", Type: "state"}, "In Progress", "State In Progress", "State: {In Progress}"},
		{Field{Name: "State", Type: "state"}, "Fixed", "State Fixed", "State: Fixed"},
		{Field{Name: "Fix versions", Type: "version"}, "1.0", "Fix versions 1.0", "{Fix versions}: 1.0"},
		{Field{Name: "Assignee", Type: "user"}, "me", "Assignee me", "Assignee: me"},
		{Field{Name: "tag", Type: "tag"}, "needs-input", "tag needs-input", "tag: needs-input"},
	} {
		cmd, q := clause(c.f, c.v)
		if cmd != c.cmd || q != c.qry {
			t.Errorf("clause(%s, %s) = %q, %q", c.f.Name, c.v, cmd, q)
		}
	}
}

func TestWorkingMustLeaveTheSource(t *testing.T) {
	existing := spec.Pipeline{
		Name: "x", Profile: "youtrack-xlsft", Source: spec.Source{Query: "project: YPC #Unresolved"},
		Stages: spec.Stages{Working: spec.Stage{Enter: "tag ai", Match: "tag: ai"}},
	}
	d := start(t, newFakes().deps, &existing)
	d.goStep(1)
	d.must("1 also match the source (YPC-3)", "ymcp would take them again", "Working !")
}

func TestPromptPresetsOfTheUser(t *testing.T) {
	f := newFakes()
	team := spec.Preset{Name: "team-fix", Rationale: "Our fix", Start: "Team start {{issue.id}}", Feedback: "Team feedback {{feedback}}", Answer: "Team answer {{answer}}"}
	f.presets.user = []spec.Preset{team}
	d := start(t, f.deps, nil)
	d.goStep(3)
	d.focus("preset")
	// The built-ins fill the line; the user preset is scrolled to.
	row := func() string { return d.m.steps[3].(*agentStep).presets.row(68, true) }
	if r := row(); !strings.Contains(r, " fix ") || !strings.Contains(r, "investigate") || !strings.HasSuffix(r, " ›") || strings.Contains(r, "team-fix") {
		t.Fatalf("preset row = %q", r)
	}
	d.keys("right", "right", "right", "right", "right", "right")
	d.must("your preset · Our fix")
	if r := row(); !strings.Contains(r, "‹") || !strings.Contains(r, "│") || !strings.Contains(r, "team-fix") || strings.HasSuffix(r, " ›") {
		t.Fatalf("preset row = %q", r)
	}
	d.dump("4-agent-user-preset")
	d.keys("enter")
	if pr := d.m.sh.p.Prompts; pr.Preset != "team-fix" || pr.Start != team.Start || pr.Answer != team.Answer {
		t.Fatalf("prompts = %+v", pr)
	}
	d.focus("prompts")
	d.must("based on team-fix", "Team start ABC-1")
	d.mustNot("modified")
}

func TestSaveAsPreset(t *testing.T) {
	f := newFakes()
	f.presets.user = []spec.Preset{{Name: "team-fix", Start: "s", Feedback: "f", Answer: "a"}}
	d := start(t, f.deps, nil)
	d.goStep(3)
	d.focus("prompts")
	d.send(editedMsg{which: 0, text: "My start {{issue.id}}"})
	d.must("based on fix, modified: start")

	// esc closes the form and does not ask to discard the draft.
	d.typ("s")
	d.must("Save these prompts as a preset", "become a preset of yours", "esc cancel")
	d.keys("esc")
	d.mustNot("become a preset of yours", "Discard your changes?")

	// From the preset too.
	d.focus("preset")
	d.typ("s")
	for name, want := range map[string]string{
		"fix":      `"fix" is a built-in preset`,
		"team-fix": `preset "team-fix" already exists`,
		"Bad Name": "invalid",
	} {
		d.send(ctrlU())
		d.typ(name)
		d.keys("enter")
		d.must(want, "become a preset of yours")
	}
	d.send(ctrlU())
	d.typ("mine")
	d.keys("enter")
	d.mustNot("invalid")
	d.typ("For my tests")
	d.dump("4-agent-save-preset")

	// A failing store keeps the form open with the error.
	f.presets.saveErr = errors.New("disk full")
	d.keys("enter")
	d.must("disk full", "become a preset of yours")
	f.presets.saveErr = nil
	d.keys("enter")
	d.must("Saved preset mine")
	d.mustNot("become a preset of yours")
	d.focus("prompts")
	d.must("based on mine")
	d.mustNot("modified")
	saved := f.presets.user[len(f.presets.user)-1]
	fix, _ := spec.LookupPreset("fix")
	if saved.Name != "mine" || saved.Rationale != "For my tests" || saved.Start != "My start {{issue.id}}" ||
		saved.Feedback != fix.Feedback || saved.Answer != fix.Answer {
		t.Fatalf("saved = %+v", saved)
	}
	if pr := d.m.sh.p.Prompts; pr.Preset != "mine" || pr.Start != saved.Start || pr.Feedback != saved.Feedback {
		t.Fatalf("prompts = %+v", pr)
	}

	// In the language field "s" is typed, not a command.
	d.focus("advanced")
	d.keys("enter")
	d.focus("language")
	d.typ("s")
	d.mustNot("become a preset of yours")
	if d.m.sh.p.Language != "Englishs" {
		t.Errorf("language = %q", d.m.sh.p.Language)
	}
}

func TestPresetListError(t *testing.T) {
	f := newFakes()
	f.presets.listErr = errors.New("preset file broken.json: unexpected EOF")
	d := start(t, f.deps, nil)
	d.goStep(3)
	d.focus("preset")
	d.must("broken.json", "bugfix-test-first")
}

// TestViews dumps every step of a filled-in pipeline at 120x34 and one at
// 100x30, and checks that they fit.
func TestViews(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := spec.Pipeline{
		Name: "backend", Profile: "youtrack-xlsft", Source: spec.Source{Query: "project: YPC #Unresolved"},
		Stages:    newPipeline().Stages,
		Workspace: spec.Workspace{Dir: repo},
		Agent:     claudeDefaults(),
		Strategy:  spec.Strategy{Kind: "github-pr"},
		Feedback:  spec.Feedback{YouTrack: []string{"ivan.petrov"}},
	}
	for _, size := range []struct{ w, h int }{{120, 34}, {100, 30}} {
		d := start(t, newFakes().deps, &existing)
		d.send(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		for i, s := range d.m.steps {
			d.goStep(i)
			d.dump(strings.ReplaceAll(strings.ToLower(s.title()), " ", "-") + fmt.Sprintf("-%dx%d", size.w, size.h))
		}
	}
}
