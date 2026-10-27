package spec

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("YMCP_CONFIG", filepath.Join(dir, "config.json"))
	return filepath.Join(dir, "pipelines")
}

// minimal is the smallest pipeline that passes Validate.
func minimal(name string) Pipeline {
	return Pipeline{
		Name:    name,
		Profile: "youtrack",
		Source:  Source{Query: "project: BE #Unresolved"},
		Stages: Stages{
			Working:    Stage{Enter: "State In Progress", Match: "State: {In Progress}"},
			Review:     Stage{Enter: "State Review", Match: "State: Review"},
			NeedsInput: Stage{Enter: "tag needs-input", Exit: "untag needs-input"},
			Closed:     Stage{Enter: "State Open"},
		},
		Workspace: Workspace{Dir: "~/code/backend"},
	}
}

func TestDirFollowsConfig(t *testing.T) {
	want := setup(t)
	got, err := Dir()
	if err != nil || got != want {
		t.Fatalf("Dir = %q, %v; want %q", got, err, want)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	dir := setup(t)
	os.MkdirAll(dir, 0o700)
	raw := `{"name":"ignored","profile":"p","source":{"query":"q"},"workspace":{"dir":"/r"}}`
	if err := os.WriteFile(filepath.Join(dir, "backend.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load("backend")
	if err != nil {
		t.Fatal(err)
	}
	off := false
	checks := []struct {
		field     string
		got, want any
	}{
		{"Name", p.Name, "backend"},
		{"PollInterval", p.Source.PollInterval.Duration, 60 * time.Second},
		{"Done.Enabled", p.Stages.Done.Enabled, &off},
		{"Done.On", p.Stages.Done.On(), false},
		{"Working.On", p.Stages.Working.On(), true},
		{"Branch", p.Workspace.Branch, "{{issue.id}}-{{issue.summary | slug}}"},
		{"CommitPrefix", p.Workspace.CommitPrefix, "{{issue.id}}"},
		{"Parallel", p.Workspace.Parallel, 1},
		{"Provider", p.Agent.Provider, "claude"},
		{"AgentName", p.Agent.Name, "Claude"},
		{"Permissions", p.Permissions, "ask"},
		{"Silence", p.Timeouts.Silence.Duration, 10 * time.Minute},
		{"Total", p.Timeouts.Total.Duration, 2 * time.Hour},
		{"Permission", p.Timeouts.Permission.Duration, 30 * time.Minute},
		{"Strategy", p.Strategy.Kind, "branch"},
		{"Debounce", p.Feedback.Debounce.Duration, 60 * time.Second},
		{"Language", p.Language, "English"},
		{"Preset", p.Prompts.Preset, "fix"},
		{"Service", p.ServiceEnabled(), true},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
}

func TestLoadKeepsExplicitValues(t *testing.T) {
	dir := setup(t)
	os.MkdirAll(dir, 0o700)
	raw := `{"profile":"p","source":{"query":"q","pollInterval":"15s"},"stages":{"done":{"enabled":true,"enter":"State Done"}},
	"workspace":{"dir":"/r","parallel":3,"branch":"b/{{issue.id}}"},"permissions":"allow","language":"Russian","service":false,
	"timeouts":{"silence":"1m","total":"90m"},"strategy":{"kind":"github-pr","draft":true}}`
	os.WriteFile(filepath.Join(dir, "x.json"), []byte(raw), 0o600)
	p, err := Load("x")
	if err != nil {
		t.Fatal(err)
	}
	if p.Source.PollInterval.Duration != 15*time.Second || !p.Stages.Done.On() || p.Workspace.Parallel != 3 ||
		p.Workspace.Branch != "b/{{issue.id}}" || p.Permissions != "allow" || p.Language != "Russian" ||
		p.ServiceEnabled() || p.Timeouts.Silence.Duration != time.Minute || p.Timeouts.Total.Duration != 90*time.Minute ||
		p.Timeouts.Permission.Duration != 30*time.Minute || p.Strategy.Kind != "github-pr" || !p.Strategy.Draft {
		t.Errorf("explicit values were overwritten: %+v", p)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := setup(t)
	if _, err := Load("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: err = %v, want ErrNotFound", err)
	}
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{"profile":`), 0o600)
	if _, err := Load("bad"); err == nil || !strings.Contains(err.Error(), "bad.json") {
		t.Errorf("broken JSON: err = %v", err)
	}
	os.WriteFile(filepath.Join(dir, "typo.json"), []byte(`{"profil":"x"}`), 0o600)
	if _, err := Load("typo"); err == nil || !strings.Contains(err.Error(), "profil") {
		t.Errorf("unknown field: err = %v", err)
	}
	os.WriteFile(filepath.Join(dir, "dur.json"), []byte(`{"timeouts":{"silence":"soon"}}`), 0o600)
	if _, err := Load("dur"); err == nil || !strings.Contains(err.Error(), "soon") {
		t.Errorf("bad duration: err = %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := setup(t)
	in := minimal("backend")
	in.Workspace.Host = "build-box"
	in.Agent = Agent{Provider: "acp", Name: "Kimi", Command: "kimi", Args: []string{"acp"}, Env: map[string]string{"A": "b"}, Mode: "yolo"}
	in.Feedback = Feedback{Debounce: Duration{2 * time.Minute}, YouTrack: []string{"ivan"}, GitHub: []string{"octocat"}}
	in.Messages = map[string]string{"placeholder": "working"}
	in.Prompts = Prompts{Preset: "docs", Start: "do {{issue.id}}"}
	off := false
	in.Service = &off

	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backend.json")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stat = %v, %v", info, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("leftover files after atomic write: %v", entries)
	}
	got, err := Load("backend")
	if err != nil {
		t.Fatal(err)
	}
	if want := in.WithDefaults(); !reflect.DeepEqual(got, want) {
		t.Errorf("round trip differs:\n got %+v\nwant %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// Saving again replaces the file.
	in.Language = "Russian"
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	if got, _ := Load("backend"); got.Language != "Russian" {
		t.Errorf("Language = %q after re-save", got.Language)
	}
}

func TestSaveWritesDurationsAsStrings(t *testing.T) {
	dir := setup(t)
	if err := Save(minimal("a")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a.json"))
	for _, want := range []string{`"pollInterval": "1m"`, `"silence": "10m"`, `"total": "2h"`, `"permission": "30m"`, `"debounce": "1m"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("file lacks %s:\n%s", want, data)
		}
	}
}

func TestListSortedAndTolerant(t *testing.T) {
	dir := setup(t)
	if list, err := List(); err != nil || len(list) != 0 {
		t.Fatalf("empty List = %v, %v", list, err)
	}
	for _, n := range []string{"zeta", "alpha", "mid-1"} {
		if err := Save(minimal(n)); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "Bad Name.json"), []byte("{}"), 0o600)
	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range list {
		names = append(names, p.Name)
	}
	if want := []string{"alpha", "mid-1", "zeta"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}

	os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o600)
	list, err = List()
	if err == nil || !strings.Contains(err.Error(), "broken.json") || len(list) != 3 {
		t.Errorf("with a broken file: %d pipelines, err = %v", len(list), err)
	}
}

func TestRenameAndDelete(t *testing.T) {
	setup(t)
	a := minimal("one")
	a.Language = "Russian"
	if err := Save(a); err != nil {
		t.Fatal(err)
	}
	if err := Save(minimal("two")); err != nil {
		t.Fatal(err)
	}

	if err := Rename("one", "two"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("rename onto existing: %v", err)
	}
	if err := Rename("one", "list"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("rename to reserved: %v", err)
	}
	if err := Rename("ghost", "three"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename missing: %v", err)
	}
	if err := Rename("one", "one"); err != nil {
		t.Errorf("rename to itself: %v", err)
	}
	if err := Rename("one", "three"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("one"); !errors.Is(err, ErrNotFound) {
		t.Errorf("old name still loads: %v", err)
	}
	got, err := Load("three")
	if err != nil || got.Name != "three" || got.Language != "Russian" {
		t.Errorf("renamed = %+v, %v", got, err)
	}

	if err := Delete("three"); err != nil {
		t.Fatal(err)
	}
	if err := Delete("three"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if list, _ := List(); len(list) != 1 || list[0].Name != "two" {
		t.Errorf("List after delete = %+v", list)
	}
}

func TestNames(t *testing.T) {
	setup(t)
	tests := []struct {
		name    string
		wantErr string // substring; empty = valid
	}{
		{"backend", ""},
		{"a", ""},
		{"my_pipe-2", ""},
		{strings.Repeat("a", 40), ""},
		{strings.Repeat("a", 41), "invalid"},
		{"", "empty"},
		{"list", "reserved"},
		{"new", "reserved"},
		{"presets", "reserved"},
		{"notify", "reserved"},
		{"Backend", "invalid"},
		{"has space", "invalid"},
		{"-dash", "invalid"},
		{"_under", "invalid"},
		{"../etc", "invalid"},
		{"a/b", "invalid"},
		{"a.json", "invalid"},
		{"имя", "invalid"},
	}
	for _, tc := range tests {
		err := CheckName(tc.name)
		if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("CheckName(%q) = %v, want %q", tc.name, err, tc.wantErr)
		}
		// The file functions refuse the same names, so nothing escapes the directory.
		if tc.wantErr != "" {
			if err := Save(Pipeline{Name: tc.name}); err == nil {
				t.Errorf("Save(%q) succeeded", tc.name)
			}
			if _, err := Load(tc.name); err == nil || errors.Is(err, ErrNotFound) {
				t.Errorf("Load(%q) = %v", tc.name, err)
			}
			if err := Delete(tc.name); err == nil || errors.Is(err, ErrNotFound) {
				t.Errorf("Delete(%q) = %v", tc.name, err)
			}
		}
	}
	for _, r := range []string{"list", "new", "presets", "notify"} {
		found := false
		for _, x := range Reserved {
			found = found || x == r
		}
		if !found {
			t.Errorf("Reserved lacks %q", r)
		}
	}
}

func TestValidate(t *testing.T) {
	no := false
	tests := []struct {
		name    string
		mutate  func(*Pipeline)
		wantErr []string // substrings, all must appear; empty = valid
	}{
		{"minimal is valid", func(p *Pipeline) {}, nil},
		{"zero optional fields are valid", func(p *Pipeline) { p.Workspace.Parallel = 0; p.Language = "" }, nil},
		{"all providers", func(p *Pipeline) { p.Agent.Provider = "opencode" }, nil},
		{"disabled stage needs nothing", func(p *Pipeline) { p.Stages.Closed = Stage{Enabled: &no} }, nil},
		{"closed needs no enter", func(p *Pipeline) { p.Stages.Closed = Stage{} }, nil},
		{"done enabled without enter", func(p *Pipeline) { yes := true; p.Stages.Done = Stage{Enabled: &yes} }, []string{"stages.done.enter"}},
		{"reserved name", func(p *Pipeline) { p.Name = "list" }, []string{"reserved"}},
		{"bad name", func(p *Pipeline) { p.Name = "A B" }, []string{"invalid"}},
		{"no profile", func(p *Pipeline) { p.Profile = " " }, []string{"profile is required"}},
		{"no query", func(p *Pipeline) { p.Source.Query = "" }, []string{"source.query is required"}},
		{"no working enter", func(p *Pipeline) { p.Stages.Working.Enter = "" }, []string{"stages.working.enter is required"}},
		{"no review match", func(p *Pipeline) { p.Stages.Review.Match = "" }, []string{"stages.review.match is required"}},
		{"needsInput needs no match", func(p *Pipeline) { p.Stages.NeedsInput.Match = "" }, nil},
		{"no dir", func(p *Pipeline) { p.Workspace.Dir = "" }, []string{"workspace.dir is required"}},
		{"host with space", func(p *Pipeline) { p.Workspace.Host = "my box" }, []string{"workspace.host"}},
		{"negative parallel", func(p *Pipeline) { p.Workspace.Parallel = -1 }, []string{"workspace.parallel must be at least 1"}},
		{"unknown provider", func(p *Pipeline) { p.Agent.Provider = "gpt" }, []string{`agent.provider "gpt" is unknown`, "claude, kimi, opencode, acp"}},
		{"acp without command", func(p *Pipeline) { p.Agent.Provider = "acp" }, []string{"agent.command is required"}},
		{"bad env name", func(p *Pipeline) { p.Agent.Env = map[string]string{"A=B": "x"} }, []string{"agent.env"}},
		{"unknown permissions", func(p *Pipeline) { p.Permissions = "maybe" }, []string{`permissions "maybe" is unknown`}},
		{"unknown strategy", func(p *Pipeline) { p.Strategy.Kind = "svn" }, []string{`strategy.kind "svn" is unknown`}},
		{"negative timeout", func(p *Pipeline) { p.Timeouts.Total.Duration = -time.Second }, []string{"timeouts.total must not be negative"}},
		{"negative poll", func(p *Pipeline) { p.Source.PollInterval.Duration = -1 }, []string{"source.pollInterval"}},
		{"empty login", func(p *Pipeline) { p.Feedback.GitHub = []string{"a", " "} }, []string{"feedback.github"}},
		{"unknown preset alone", func(p *Pipeline) { p.Prompts.Preset = "mine" }, []string{`prompts.preset "mine"`}},
		{"unknown preset with all prompts", func(p *Pipeline) {
			p.Prompts = Prompts{Preset: "mine", Start: "s", Feedback: "f", Answer: "a"}
		}, nil},
		{"several problems at once", func(p *Pipeline) { p.Profile = ""; p.Source.Query = ""; p.Workspace.Dir = "" },
			[]string{"profile is required", "source.query is required", "workspace.dir is required"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := minimal("backend")
			tc.mutate(&p)
			err := p.Validate()
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error type %T", err)
			}
			for _, s := range tc.wantErr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error lacks %q:\n%v", s, err)
				}
			}
		})
	}
}

func TestValidateDoesNotChangeReceiver(t *testing.T) {
	p := minimal("x")
	_ = p.Validate()
	if p.Workspace.Branch != "" || p.Stages.Done.Enabled != nil {
		t.Errorf("Validate applied defaults to the caller's copy: %+v", p)
	}
}

func TestDurationJSON(t *testing.T) {
	tests := []struct {
		in   time.Duration
		json string
	}{
		{0, `""`},
		{90 * time.Second, `"90s"`},
		{10 * time.Minute, `"10m"`},
		{2 * time.Hour, `"2h"`},
		{90 * time.Minute, `"90m"`},
		{1500 * time.Millisecond, `"1.5s"`},
	}
	for _, tc := range tests {
		data, err := json.Marshal(Duration{tc.in})
		if err != nil || string(data) != tc.json {
			t.Errorf("Marshal(%v) = %s, %v; want %s", tc.in, data, err, tc.json)
		}
		var back Duration
		if err := json.Unmarshal(data, &back); err != nil || back.Duration != tc.in {
			t.Errorf("Unmarshal(%s) = %v, %v", data, back.Duration, err)
		}
	}
	for in, want := range map[string]time.Duration{`"1h30m"`: 90 * time.Minute, `" 5s "`: 5 * time.Second, `null`: 0, `""`: 0} {
		var d Duration
		if err := json.Unmarshal([]byte(in), &d); err != nil || d.Duration != want {
			t.Errorf("Unmarshal(%s) = %v, %v; want %v", in, d.Duration, err, want)
		}
	}
	for _, in := range []string{`"soon"`, `"10"`, `600`, `true`} {
		var d Duration
		if err := json.Unmarshal([]byte(in), &d); err == nil {
			t.Errorf("Unmarshal(%s) succeeded", in)
		}
	}
}

func TestPresets(t *testing.T) {
	want := []string{"fix", "bugfix-test-first", "feature", "refactor", "docs", "investigate"}
	var got []string
	for _, p := range Presets() {
		got = append(got, p.Name)
		if !p.Builtin {
			t.Errorf("preset %s is not marked built-in", p.Name)
		}
		if p.Rationale == "" || p.Start == "" || p.Feedback == "" || p.Answer == "" {
			t.Errorf("preset %s has an empty text", p.Name)
		}
		if !strings.Contains(p.Start, "{{issue.id}}") || !strings.Contains(p.Feedback, "{{issue.id}}") || !strings.Contains(p.Answer, "{{answer}}") {
			t.Errorf("preset %s lacks its template variables", p.Name)
		}
		if !strings.Contains(p.Feedback, "{{feedback}}") {
			t.Errorf("preset %s feedback lacks {{feedback}}", p.Name)
		}
		for _, s := range []string{p.Start, p.Feedback, p.Answer} {
			if strings.Count(s, "{{") != strings.Count(s, "}}") {
				t.Errorf("preset %s has unbalanced braces", p.Name)
			}
		}
		if p2, ok := LookupPreset(p.Name); !ok || p2 != p {
			t.Errorf("LookupPreset(%s) = %v, %v", p.Name, p2, ok)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("preset names = %v, want %v", got, want)
	}
	if _, ok := LookupPreset("nope"); ok {
		t.Error("LookupPreset found a missing preset")
	}
	// Presets returns a copy.
	Presets()[0].Name = "changed"
	if Presets()[0].Name != "fix" {
		t.Error("Presets exposes its internal slice")
	}
}

func TestResolvedPrompts(t *testing.T) {
	fix, _ := LookupPreset("fix")
	docs, _ := LookupPreset("docs")

	got := Pipeline{}.ResolvedPrompts()
	if got.Start != fix.Start || got.Feedback != fix.Feedback || got.Answer != fix.Answer {
		t.Error("empty prompts do not fall back to the fix preset")
	}
	got = Pipeline{Prompts: Prompts{Preset: "docs", Start: "mine"}}.ResolvedPrompts()
	if got.Start != "mine" || got.Feedback != docs.Feedback || got.Answer != docs.Answer {
		t.Errorf("mixed prompts = %+v", got)
	}
	if got := (Pipeline{Prompts: Prompts{Preset: "custom"}}).ResolvedPrompts(); got.Start != "" {
		t.Errorf("unknown preset resolved to %q", got.Start)
	}
}

func TestServiceInstruction(t *testing.T) {
	if !strings.Contains(ServiceInstruction, "every commit message starts with {{commit.prefix}}.") {
		t.Error("the commit line must use {{commit.prefix}}")
	}
	if strings.Contains(ServiceInstruction, "starts with {{issue.id}}") {
		t.Error("the commit line still uses {{issue.id}}")
	}
	for _, v := range []string{"{{pipeline.name}}", "{{run.number}}", "{{issue.id}}", "{{branch}}", "{{base}}", "{{language}}", "request_input", "report(text)", "`ymcp`"} {
		if !strings.Contains(ServiceInstruction, v) {
			t.Errorf("ServiceInstruction lacks %s", v)
		}
	}
	if !strings.Contains(LanguageRule, "{{language}}") || !strings.Contains(ServiceInstruction, LanguageRule) {
		t.Error("LanguageRule is not the language line of the service instruction")
	}
}
