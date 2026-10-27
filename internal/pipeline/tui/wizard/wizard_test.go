package wizard

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

// TestNewPipeline walks through every step of a new pipeline and checks
// the live help each one shows and the pipeline it ends with.
func TestNewPipeline(t *testing.T) {
	f := newFakes()
	d := start(t, f.deps, nil)

	// Name & profile: the first profile is picked and signed in to at once.
	d.must("New pipeline", "Pipeline name", "YouTrack profile", "YouTrack (xlsft)", "can't sign in with youtrack-beget")
	d.typ("backend")
	d.keys("tab", "down")
	d.must("signed in to https://xlsft.youtrack.cloud as admin")
	if d.m.sh.p.Name != "backend" || d.m.sh.p.Profile != "youtrack-xlsft" {
		t.Fatalf("name %q profile %q", d.m.sh.p.Name, d.m.sh.p.Profile)
	}
	d.dump("1-name")
	d.keys("enter")
	if d.step() != "Source" {
		t.Fatalf("enter on the last field should go on, at %s", d.step())
	}

	// Source: completions from search/assist, live count and samples.
	d.typ("project: YPC #Unr")
	d.must("#Unresolved", "#Unreviewed")
	d.dump("2-source-completions")
	d.keys("tab")
	if got := d.m.sh.p.Source.Query; got != "project: YPC #Unresolved" {
		t.Fatalf("query after completion = %q", got)
	}
	d.must("3 issues match right now", "YPC-3", "Fix login redirect", "Rate limit the API")
	d.dump("2-source")
	d.keys("ctrl+s")
	d.must("Saved searches", "Assigned to me", "AI backlog")
	d.keys("esc")
	d.keys("ctrl+n")

	// Stages: the default commands are checked on YPC-3 and the searches run.
	if d.step() != "Stages" {
		t.Fatalf("at %s", d.step())
	}
	d.must("Set State In Progress", "(on YPC-3)", "1 in this stage now: YPC-1", "! Needs input", "issues in work don't match the source query")
	if v := f.yt.called("validate State In Progress @YPC-3"); len(v) == 0 {
		t.Fatalf("working command not validated: %v", f.yt.calls)
	}
	d.dump("3-stages")

	// Field picker: Review becomes State Verified.
	d.keys("right", "ctrl+f")
	d.must("Pick a field", "State", "Priority", "Assignee", "Tag")
	d.keys("enter")
	d.typ("veri")
	d.must("Pick a value of State", "command State Verified", "search State: Verified")
	d.dump("3-stages-picker")
	d.keys("enter")
	if st := d.m.sh.p.Stages.Review; st.Enter != "State Verified" || st.Match != "State: Verified" {
		t.Fatalf("review stage = %+v", st)
	}
	d.must("Set State Verified")

	// Adding to a stage: Working also assigns the issue.
	d.keys("shift+tab", "left", "ctrl+f", "down", "down", "enter", "tab")
	if st := d.m.sh.p.Stages.Working; st.Enter != "State In Progress Assignee me" || st.Match != "State: {In Progress} Assignee: me" {
		t.Fatalf("working stage = %+v", st)
	}

	// Typing a command: completions from commands/assist and a live check.
	d.keys("shift+tab", "right", "tab")
	d.send(ctrlU())
	d.typ("State Bogus")
	d.must("invalid: State expected: Bogus")
	d.send(ctrlU())
	d.typ("State In")
	d.must("In Progress", "Incomplete")
	d.dump("3-stages-command-completions")
	d.keys("tab")
	if got := d.m.sh.p.Stages.Review.Enter; got != "State In Progress" {
		t.Fatalf("review enter after completion = %q", got)
	}
	d.send(ctrlU())
	d.typ("State Verified")

	// Done is off until turned on.
	d.keys("shift+tab", "right", "right")
	d.must("This stage is off")
	d.keys("ctrl+n")

	// Workspace: path completion and the repository checks.
	if d.step() != "Workspace" {
		t.Fatalf("at %s", d.step())
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	for _, p := range []string{filepath.Join(repo, ".git"), filepath.Join(root, "notes")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d.typ(root + string(filepath.Separator) + "re")
	d.must("repo"+string(filepath.Separator), "git repository")
	d.dump("4-workspace-path-completion")
	d.keys("tab")
	if got := d.m.sh.p.Workspace.Dir; got != repo {
		t.Fatalf("dir = %q, want %q", got, repo)
	}
	d.must("✓ git repository: ", "origin remote: git@github.com:xlsft/ymcp.git", "push access: allowed", "YPC-3-fix-login-redirect")
	if dirs := f.inspect.dirs; len(dirs) == 0 || dirs[len(dirs)-1] != repo {
		t.Fatalf("inspected %v", dirs)
	}
	d.dump("4-workspace")
	d.keys("ctrl+n")

	// Agent: every provider is checked; switching keeps each draft.
	d.must("2.1.0 (Claude Code)", "kimi: executable file not found", "0.15.3", "set Command to start it", "--allowedTools")
	d.dump("5-agent")
	d.keys("down")
	if d.m.sh.p.Agent.Provider != "kimi" {
		t.Fatalf("provider = %q", d.m.sh.p.Agent.Provider)
	}
	d.keys("up", "tab", "tab")
	d.send(ctrlU())
	d.must("claude has no --permission-mode")
	d.typ("acc")
	d.must("acceptEdits")
	d.keys("tab")
	if a := d.m.sh.p.Agent; a.Provider != "claude" || a.Mode != "acceptEdits" || len(a.Args) != 6 {
		t.Fatalf("agent = %+v", a)
	}
	d.keys("tab", "tab", "tab", "right")
	if d.m.sh.p.Permissions != "allow" {
		t.Fatalf("permissions = %q", d.m.sh.p.Permissions)
	}
	d.keys("left")
	d.keys("enter")

	// Strategy: detected from origin.
	if d.step() != "Strategy" {
		t.Fatalf("at %s", d.step())
	}
	d.must("github.com/xlsft/ymcp", "suggests github-pr", "token from `gh auth token`")
	d.typ("u")
	d.keys("space")
	if s := d.m.sh.p.Strategy; s.Kind != "github-pr" || !s.Draft {
		t.Fatalf("strategy = %+v", s)
	}
	d.dump("6-strategy")
	d.keys("down", "down")
	d.must("the strategy is for gitlab but origin is on github")
	d.keys("up", "up", "enter")

	// Feedback: YouTrack logins from the users API.
	d.typ("iv")
	d.must("ivan.petrov", "Ivan Petrov")
	d.dump("7-feedback-users")
	d.keys("tab")
	d.must("admin (you, always)", "ivan.petrov")
	d.keys("tab")
	d.typ("octocat, @hubot")
	d.dump("7-feedback")
	if fb := d.m.sh.p.Feedback; !slices.Equal(fb.YouTrack, []string{"ivan.petrov"}) || !slices.Equal(fb.GitHub, []string{"octocat", "hubot"}) {
		t.Fatalf("feedback = %+v", fb)
	}
	d.keys("ctrl+n")

	// Prompts: preview on the real issue, preset switch.
	d.must("Preview of start", "on YPC-3", "Resolve issue YPC-3: Fix login redirect", "Login redirects to /404.", "based on fix")
	d.dump("8-prompts")
	d.keys("right", "enter")
	if pr := d.m.sh.p.Prompts; pr.Preset != "bugfix-test-first" || !strings.HasPrefix(pr.Start, "Fix bug {{issue.id}}") {
		t.Fatalf("prompts = %+v", pr.Preset)
	}
	d.must("Fix bug YPC-3", "based on bugfix-test-first")
	d.keys("ctrl+n")

	// Notifications: only questions, from every provider.
	d.must("toast", "on here · needs_input, run_failed", "runner_stopped")
	d.keys("down", "space")
	d.dump("9-notifications")
	if n := d.m.sh.p.Notifications; n == nil || len(n.Providers) != 0 || !slices.Equal(n.Events, []string{"needs_input"}) {
		t.Fatalf("notifications = %+v", n)
	}
	d.keys("enter")

	// Review: the full check, then save & run.
	d.must("Check", "3 issues match: YPC-3, YPC-4, YPC-7", "tag needs-input doesn't exist yet", "Save & run in background")
	d.dump("10-review")
	d.typ("r")
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
		{"mode", p.Agent.Mode, "acceptEdits"},
		{"permissions", p.Permissions, "ask"},
		{"strategy", p.Strategy.Kind, "github-pr"},
		{"preset", p.Prompts.Preset, "bugfix-test-first"},
		{"language", p.Language, "English"},
		{"poll", p.Source.PollInterval.Duration, 60 * time.Second},
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
	d.keys("enter", "enter")
	if d.step() != "Source" {
		t.Fatalf("at %s:\n%s", d.step(), d.view())
	}
	d.must("project: YPC #Unresolved", "3 issues match")
	d.keys("ctrl+n")
	d.must("Set State In Progress", "Needs input")
	d.keys("ctrl+n")
	d.must("/src/repo")
	d.keys("ctrl+n")
	d.must("anthropic/claude-sonnet-4")
	d.keys("ctrl+n", "ctrl+n")
	d.must("ivan.petrov")
	d.keys("ctrl+n")
	d.must("based on docs, modified: start", "Custom start for YPC-3", "[ ] ymcp's service instruction")
	d.keys("ctrl+n")
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

func TestNameTaken(t *testing.T) {
	d := start(t, newFakes().deps, nil)
	d.typ("frontend")
	d.keys("enter")
	d.must("a pipeline named \"frontend\" already exists")
	if d.step() != "Name & profile" {
		t.Fatalf("moved on to %s with a taken name", d.step())
	}
}

func TestInvalidPipelineIsNotSaved(t *testing.T) {
	d := start(t, newFakes().deps, nil)
	d.send(navMsg{to: 9})
	d.must("Fix these before saving:", "pipeline name is empty", "source.query is required", "workspace.dir is required")
	d.keys("enter")
	d.typ("s")
	if len(d.out) != 0 {
		t.Fatalf("saved an invalid pipeline: %v", d.out)
	}
}

func TestCancel(t *testing.T) {
	d := start(t, newFakes().deps, nil)
	d.keys("esc")
	d.must("Discard this draft?")
	d.typ("n")
	d.mustNot("Discard this draft?")
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
	d.send(navMsg{to: 2})
	d.keys("ctrl+f")
	d.must("Pick a field")
	d.keys("esc")
	d.mustNot("Pick a field", "Discard this draft?")
	d.keys("esc")
	d.must("Discard this draft?")
}

func TestStaleSearchResultsAreDropped(t *testing.T) {
	f := newFakes()
	d := start(t, f.deps, nil)
	d.typ("x")
	d.keys("tab", "down", "enter")
	src := d.m.steps[1].(*sourceStep)
	old := src.deb.seq
	d.typ("project: YPC #Unresolved")
	d.send(sampleMsg{id: src.deb.id, seq: old, query: "old", err: os.ErrClosed})
	d.mustNot("file already closed")
	d.must("3 issues match")
}

func TestSourceErrors(t *testing.T) {
	d := start(t, newFakes().deps, nil)
	d.typ("x")
	d.keys("tab", "down", "enter")
	d.typ("bogus:")
	d.must("Could not parse query")
	if d.m.steps[1].mark() != markErr {
		t.Fatal("source step not marked failed")
	}
}

func TestTagPickerAllowsNewTags(t *testing.T) {
	f := newFakes()
	existing := spec.Pipeline{Name: "x", Profile: "youtrack-xlsft", Source: spec.Source{Query: "project: YPC #Unresolved"}}
	d := start(t, f.deps, &existing)
	d.send(navMsg{to: 2})
	d.keys("right", "right") // Needs input
	d.keys("ctrl+f", "up", "enter")
	d.must("Pick a value of tag", "ai", "needs-input")
	d.typ("waiting on human")
	d.must("new tag (YouTrack creates it on first use)", "command tag {waiting on human}")
	d.keys("enter")
	st := d.m.sh.p.Stages.NeedsInput
	if st.Enter != "tag {waiting on human}" || st.Exit != "untag {waiting on human}" || st.Match != "tag: {waiting on human}" {
		t.Fatalf("needs input = %+v", st)
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
	d.send(navMsg{to: 7})
	d.keys("tab", "tab", "tab", "tab") // the answer prompt
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
	d.keys("shift+tab", "shift+tab", "shift+tab", "shift+tab", "right", "enter")
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
	d := start(t, f.deps, nil)
	d.send(navMsg{to: 5})
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
	d.send(navMsg{to: 2})
	d.must("1 issues match both the source and this search (YPC-3)", "! Working")
}

func TestPromptPresetsOfTheUser(t *testing.T) {
	f := newFakes()
	team := spec.Preset{Name: "team-fix", Rationale: "Our fix", Start: "Team start {{issue.id}}", Feedback: "Team feedback {{feedback}}", Answer: "Team answer {{answer}}"}
	f.presets.user = []spec.Preset{team}
	d := start(t, f.deps, nil)
	d.send(navMsg{to: 7})
	// The built-ins fill the line; the user preset is scrolled to.
	d.must(" fix ", "investigate", " ›")
	d.mustNot("team-fix")
	d.dump("8-prompts-user-presets")
	d.keys("right", "right", "right", "right", "right", "right")
	d.must("‹", "│ team-fix", "your preset · Our fix")
	d.mustNot(" ›")
	d.dump("8-prompts-user-preset-selected")
	d.keys("enter")
	if pr := d.m.sh.p.Prompts; pr.Preset != "team-fix" || pr.Start != team.Start || pr.Answer != team.Answer {
		t.Fatalf("prompts = %+v", pr)
	}
	d.must("based on team-fix", "Team start ABC-1")
	d.mustNot("modified")
}

func TestSaveAsPreset(t *testing.T) {
	f := newFakes()
	f.presets.user = []spec.Preset{{Name: "team-fix", Start: "s", Feedback: "f", Answer: "a"}}
	d := start(t, f.deps, nil)
	d.send(navMsg{to: 7})
	d.keys("tab", "tab") // the start prompt
	d.send(editedMsg{which: 0, text: "My start {{issue.id}}"})
	d.must("based on fix, modified: start")

	// esc closes the form and does not ask to discard the draft.
	d.typ("s")
	d.must("Save these prompts as a preset", "esc cancel")
	d.keys("esc")
	d.mustNot("Save these prompts as a preset", "Discard this draft?")

	d.typ("s")
	for name, want := range map[string]string{
		"fix":      `"fix" is a built-in preset`,
		"team-fix": `preset "team-fix" already exists`,
		"Bad Name": "invalid",
	} {
		d.send(ctrlU())
		d.typ(name)
		d.keys("enter")
		d.must(want, "Save these prompts as a preset")
	}
	d.send(ctrlU())
	d.typ("mine")
	d.keys("enter")
	d.mustNot("invalid")
	d.typ("For my tests")
	d.dump("8-prompts-save-preset")

	// A failing store keeps the form open with the error.
	f.presets.saveErr = errors.New("disk full")
	d.keys("enter")
	d.must("disk full", "Save these prompts as a preset")
	f.presets.saveErr = nil
	d.keys("enter")
	d.must("Saved preset mine", "based on mine")
	d.mustNot("Save these prompts as a preset", "modified")
	saved := f.presets.user[len(f.presets.user)-1]
	fix, _ := spec.LookupPreset("fix")
	if saved.Name != "mine" || saved.Rationale != "For my tests" || saved.Start != "My start {{issue.id}}" ||
		saved.Feedback != fix.Feedback || saved.Answer != fix.Answer {
		t.Fatalf("saved = %+v", saved)
	}
	if pr := d.m.sh.p.Prompts; pr.Preset != "mine" || pr.Start != saved.Start || pr.Feedback != saved.Feedback {
		t.Fatalf("prompts = %+v", pr)
	}
	if it, _ := d.m.steps[7].(*promptsStep).presets.selected(); it.value != "mine" {
		t.Errorf("selected preset = %q", it.value)
	}

	// In the language field "s" is typed, not a command.
	d.keys("shift+tab")
	d.typ("s")
	d.mustNot("Save these prompts as a preset")
	if d.m.sh.p.Language != "Englishs" {
		t.Errorf("language = %q", d.m.sh.p.Language)
	}
}

func TestPresetListError(t *testing.T) {
	f := newFakes()
	f.presets.listErr = errors.New("preset file broken.json: unexpected EOF")
	d := start(t, f.deps, nil)
	d.send(navMsg{to: 7})
	d.must("broken.json", "bugfix-test-first")
}
