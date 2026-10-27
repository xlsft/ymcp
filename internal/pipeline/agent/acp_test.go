package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent/agenttest"
)

// TestACPTextAroundToolCalls: the text the agent writes before, between
// and after its tool calls comes back as paragraphs; the chunks of one
// message run together.
func TestACPTextAroundToolCalls(t *testing.T) {
	s, _ := fake(t, "acp", agenttest.Script{Turns: [][]agenttest.Step{{
		{Text: "Exploring "}, {Text: "the codebase."},
		{Tool: &agenttest.Tool{Title: "Read", Kind: "read"}},
		{Text: "The issue is about the list."},
		{Tool: &agenttest.Tool{Title: "Edit", Kind: "edit"}},
		{Tool: &agenttest.Tool{Title: "Bash", Kind: "execute"}},
		{Text: "\nDone."},
	}}}, StartOptions{})
	res := s.prompt(t, "go")
	if want := "Exploring the codebase.\n\nThe issue is about the list.\n\nDone."; res.Text != want {
		t.Errorf("text = %q, want %q", res.Text, want)
	}
	if res := s.prompt(t, "again"); res.Text != "ok" {
		t.Errorf("the next turn starts afresh: %q", res.Text)
	}
}

func TestACPStreamsEventsAndConfigures(t *testing.T) {
	script := agenttest.Script{Banner: "kimi: loading config…", Turns: [][]agenttest.Step{{
		{Thought: "Let me look."},
		{Garbage: "warning: not JSON"},
		{Text: "hello world. "},
		{Tool: &agenttest.Tool{Title: "Read", Kind: "read", Input: map[string]any{"path": "README.md"}}},
		{Usage: true},
		{Echo: true},
	}}}
	opts := StartOptions{
		Model: "fake/k3", Mode: "plan", SystemPrompt: "SERVICE",
		MCP: []MCPServer{{Name: "ymcp", URL: "http://127.0.0.1:1/work", Headers: map[string]string{"Authorization": "Bearer tok"}}},
	}
	s, state := fake(t, "acp", script, opts, "kimi")
	if !strings.HasPrefix(s.ID(), "fake_") {
		t.Fatalf("ID = %q", s.ID())
	}
	res := s.prompt(t, "do it")
	// The text after a tool call is a paragraph of its own.
	want := "hello world.\n\nprompt=SERVICE\n\ndo it history= mode=plan model=fake/k3"
	if res.StopReason != StopEndTurn || res.IsError || res.Text != want {
		t.Errorf("result = %+v\nwant text %q", res, want)
	}
	if e := s.wait(t, EventSession, nil); e.SessionID != s.ID() {
		t.Errorf("session event %q", e.SessionID)
	}
	s.wait(t, EventThought, func(e Event) bool { return e.Text == "Let me look." })
	call := s.wait(t, EventToolCall, nil)
	if call.Tool.Title != "Read" || call.Tool.Kind != "read" || call.Tool.Status != "pending" || string(call.Tool.Input) != `{"path":"README.md"}` {
		t.Errorf("tool call = %+v (%s)", call.Tool, call.Tool.Input)
	}
	upd := s.wait(t, EventToolUpdate, func(e Event) bool { return e.Tool.Status == "completed" })
	if upd.Tool.ID != call.Tool.ID || upd.Tool.Title != "Read" {
		t.Errorf("tool update = %+v", upd.Tool)
	}
	// The fake repeats its in_progress update with more output, as kimi
	// does; only the changes of status are events.
	var statuses []string
	for _, e := range s.all() {
		if e.Kind == EventToolUpdate {
			statuses = append(statuses, e.Tool.Status)
		}
	}
	if strings.Join(statuses, ",") != "in_progress,completed" {
		t.Errorf("tool updates = %q", statuses)
	}
	s.wait(t, EventUsage, func(e Event) bool { return e.Text == "used=1234 size=100000" })
	s.wait(t, EventUsage, func(e Event) bool { return e.Text == "in=10 out=20 total=30" })

	// The second turn has no service instruction in front.
	if res := s.prompt(t, "again"); res.Text != "ok" {
		t.Errorf("turn 2 = %+v", res)
	}
	prompts := calls(t, state, "session/prompt")
	if p := params(t, prompts[1])["prompt"].([]any)[0].(map[string]any)["text"]; p != "again" {
		t.Errorf("second prompt text %q", p)
	}

	init := params(t, calls(t, state, "initialize")[0])
	if init["protocolVersion"] != float64(1) {
		t.Errorf("initialize = %v", init)
	}
	if caps, _ := init["clientCapabilities"].(map[string]any); caps["terminal"] == true || caps["fs"] != nil && len(caps["fs"].(map[string]any)) > 0 {
		t.Errorf("client capabilities = %v", caps)
	}
	nw := params(t, calls(t, state, "session/new")[0])
	srv := nw["mcpServers"].([]any)[0].(map[string]any)
	hdr := srv["headers"].([]any)[0].(map[string]any)
	if nw["cwd"] == "" || srv["type"] != "http" || srv["name"] != "ymcp" || srv["url"] != "http://127.0.0.1:1/work" ||
		hdr["name"] != "Authorization" || hdr["value"] != "Bearer tok" {
		t.Errorf("session/new = %v", nw)
	}
	var set []string
	for _, c := range calls(t, state, "session/set_config_option") {
		p := params(t, c)
		set = append(set, p["configId"].(string)+"="+p["value"].(string))
	}
	if strings.Join(set, ",") != "model=fake/k3,mode=plan" {
		t.Errorf("config set = %q", set)
	}
}

// answerEach answers every permission request with choose(request).
func answerEach(t *testing.T, s *recorder, choose func(n int, p *Permission) string) {
	go func() {
		seen := map[string]bool{}
		for n := 0; ; {
			select {
			case <-s.notify:
			case <-s.done:
				return
			case <-time.After(50 * time.Millisecond):
			}
			for _, e := range s.all() {
				if e.Kind != EventPermission || seen[e.Permission.RequestID] {
					continue
				}
				seen[e.Permission.RequestID] = true
				if err := s.Answer(e.Permission.RequestID, choose(n, e.Permission)); err != nil {
					t.Errorf("Answer: %v", err)
				}
				n++
			}
		}
	}()
}

func TestACPPermissionRoundTrip(t *testing.T) {
	script := agenttest.Script{Turns: [][]agenttest.Step{{
		{Permission: &agenttest.Tool{Title: "Bash", Kind: "execute", Input: map[string]any{"command": "git status"}}},
		{Permission: &agenttest.Tool{Title: "Bash", Kind: "execute", Input: map[string]any{"command": "rm -rf /"}}},
		{Text: " done"},
	}}}
	s, state := fake(t, "acp", script, StartOptions{}, "kimi")
	answerEach(t, s, func(n int, p *Permission) string {
		if n == 0 {
			return "approve_once"
		}
		return "reject"
	})
	res := s.prompt(t, "run things")
	if res.StopReason != StopEndTurn || res.Text != "allowed approve_once\n\nrejected done" {
		t.Errorf("result = %+v", res)
	}
	if len(res.Denied) != 1 || res.Denied[0].Status != "denied" || !strings.Contains(string(res.Denied[0].Input), "rm -rf") {
		t.Errorf("denied = %+v", res.Denied)
	}

	e := s.wait(t, EventPermission, nil)
	p := e.Permission
	// Kind is left out: the SDK may deliver the request before the tool_call
	// update that carries it, and kimi doesn't send one at all.
	if p.RequestID == "" || p.Tool.Title != "Bash" || !strings.Contains(string(p.Tool.Input), "git status") {
		t.Errorf("permission = %+v", p)
	}
	if len(p.Options) != 3 || p.Options[0] != (Option{ID: "approve_once", Name: "Approve once", Kind: "allow_once"}) || p.Options[2].Kind != "reject_once" {
		t.Errorf("options = %+v", p.Options)
	}
	if e.Text != "Requesting approval to Bash" {
		t.Errorf("permission text %q", e.Text)
	}
	// The fake numbers its requests from 0, as kimi and opencode do.
	reqs := calls(t, state, "request session/request_permission")
	if len(reqs) != 2 || string(reqs[0].Params) != "0" || string(reqs[1].Params) != "1" {
		t.Errorf("permission request ids = %+v", reqs)
	}
}

func TestACPAnswerErrors(t *testing.T) {
	s, _ := fake(t, "acp", agenttest.Script{Turns: [][]agenttest.Step{{{Permission: &agenttest.Tool{Title: "Bash"}}}}}, StartOptions{})
	done := make(chan Result, 1)
	go func() { done <- s.prompt(t, "x") }()
	p := s.wait(t, EventPermission, nil).Permission
	if err := s.Answer("nope", "approve_once"); err == nil {
		t.Error("Answer to an unknown request succeeded")
	}
	if err := s.Answer(p.RequestID, "nope"); err == nil {
		t.Error("Answer with an unknown option succeeded")
	}
	if err := s.Answer(p.RequestID, "approve_always"); err != nil {
		t.Fatal(err)
	}
	if err := s.Answer(p.RequestID, "approve_always"); err == nil {
		t.Error("second Answer succeeded")
	}
	if res := <-done; res.Text != "allowed approve_always" {
		t.Errorf("result = %+v", res)
	}
}

func TestACPCancel(t *testing.T) {
	tests := []struct {
		name, flavor string
		steps        []agenttest.Step
		viaContext   bool
	}{
		{name: "while working", steps: []agenttest.Step{{Text: "working"}, {Sleep: time.Minute}}},
		{name: "context while working", steps: []agenttest.Step{{Text: "working"}, {Sleep: time.Minute}}, viaContext: true},
		// opencode ignores session/cancel while a permission request is
		// open: Cancel must reject it first.
		{name: "opencode with permission open", flavor: "opencode", steps: []agenttest.Step{{Text: "working"}, {Permission: &agenttest.Tool{Title: "write"}}}},
		{name: "kimi with permission open", steps: []agenttest.Step{{Text: "working"}, {Permission: &agenttest.Tool{Title: "Bash"}}, {Sleep: time.Minute}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, state := fake(t, "acp", agenttest.Script{Flavor: tt.flavor, Turns: [][]agenttest.Step{tt.steps}}, StartOptions{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type out struct {
				res Result
				err error
			}
			done := make(chan out, 1)
			go func() {
				res, err := s.Prompt(ctx, "work")
				done <- out{res, err}
			}()
			s.wait(t, EventText, nil)
			if tt.steps[1].Permission != nil {
				s.wait(t, EventPermission, nil)
			}
			if tt.viaContext {
				cancel()
			} else if err := s.Cancel(); err != nil {
				t.Fatal(err)
			}
			select {
			case o := <-done:
				if o.err != nil || o.res.StopReason != StopCancelled || !strings.HasPrefix(o.res.Text, "working") || len(o.res.Denied) != 0 {
					t.Errorf("Prompt = %+v, %v", o.res, o.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Prompt did not return after Cancel")
			}
			// The fake may answer the prompt before it has logged the
			// cancel notification, so give the log a moment.
			deadline := time.Now().Add(5 * time.Second)
			for len(calls(t, state, "session/cancel")) == 0 {
				if time.Now().After(deadline) {
					t.Fatal("no session/cancel")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if res := s.prompt(t, "again"); res.Text != "ok" {
				t.Errorf("after cancel: %+v", res)
			}
		})
	}
}

func TestACPResume(t *testing.T) {
	state := t.TempDir()
	dir := t.TempDir()
	script := agenttest.Script{State: state, Turns: [][]agenttest.Step{{{Text: "OK"}}, {{Echo: true}}}}
	opts := StartOptions{Dir: dir, Mode: "plan", Model: "fake/k3", SystemPrompt: "SERVICE"}
	s1, _ := fake(t, "acp", script, opts, "kimi")
	id := s1.ID()
	s1.prompt(t, "Remember PELICAN.")
	s1.Close()

	opts.SessionID = id
	s2, _ := fake(t, "acp", script, opts, "kimi")
	if s2.ID() != id {
		t.Errorf("resumed ID %q, want %q", s2.ID(), id)
	}
	// kimi forgets the mode on resume; the adapter sets it again. No
	// service instruction in front of the first prompt after a resume.
	res := s2.prompt(t, "What word?")
	if want := "prompt=What word? history=SERVICE\n\nRemember PELICAN. mode=plan model=fake/k3"; res.Text != want {
		t.Errorf("resumed text %q, want %q", res.Text, want)
	}
	all, _ := agenttest.Calls(state)
	var seq []string
	for _, c := range all {
		if c.Method == "session/resume" || c.Method == "session/set_config_option" || c.Method == "session/load" {
			seq = append(seq, c.Method)
		}
	}
	if strings.Join(seq, " ") != "session/set_config_option session/set_config_option session/resume session/set_config_option session/set_config_option" {
		t.Errorf("calls = %q", seq)
	}
}

func TestACPResumeFailures(t *testing.T) {
	err := startErr(t, "acp", agenttest.Script{}, StartOptions{SessionID: "fake_unknown"})
	if !errors.Is(err, ErrNoResume) {
		t.Errorf("unknown session: %v", err)
	}
	err = startErr(t, "acp", agenttest.Script{Flavor: "plain"}, StartOptions{SessionID: "fake_1"})
	if !errors.Is(err, ErrNoResume) {
		t.Errorf("no resume support: %v", err)
	}
}

func TestACPStartErrors(t *testing.T) {
	err := startErr(t, "acp", agenttest.Script{}, StartOptions{Model: "gpt-9"})
	if !strings.Contains(err.Error(), `set model "gpt-9"`) {
		t.Errorf("bad model: %v", err)
	}
	a, _ := New("acp", "ymcp-no-such-agent", nil)
	if _, err := a.Start(context.Background(), StartOptions{Dir: t.TempDir()}); err == nil {
		t.Error("Start of a missing binary succeeded")
	}
}

func TestACPPromptErrors(t *testing.T) {
	s, _ := fake(t, "acp", agenttest.Script{Turns: [][]agenttest.Step{{{Text: "partial"}, {Error: "quota exceeded"}}}}, StartOptions{})
	res, err := s.Prompt(context.Background(), "x")
	if err != nil || !res.IsError || res.StopReason != StopError || !strings.Contains(res.Err, "quota exceeded") || res.Text != "partial" {
		t.Errorf("agent error: %+v, %v", res, err)
	}
	s.wait(t, EventError, nil)

	s, _ = fake(t, "acp", agenttest.Script{Turns: [][]agenttest.Step{{{Stderr: "fatal: out of memory"}, {Exit: 3}}}}, StartOptions{})
	res, err = s.Prompt(context.Background(), "x")
	if err == nil || !res.IsError || !strings.Contains(err.Error(), "agent exited") || !strings.Contains(err.Error(), "out of memory") {
		t.Errorf("crash: %+v, %v", res, err)
	}
}

func TestACPCloseKillsProcessGroup(t *testing.T) {
	s, state := fake(t, "acp", agenttest.Script{Turns: [][]agenttest.Step{{{Spawn: true}, {Text: "spawned"}}}}, StartOptions{})
	if res := s.prompt(t, "x"); res.Text != "spawned" {
		t.Errorf("result = %+v", res)
	}
	child := waitChild(t, state)
	closed := make(chan struct{})
	go func() {
		s.Close()
		s.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hangs")
	}
	gone(t, child)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("events not closed")
	}
	if _, err := s.Prompt(context.Background(), "x"); err == nil {
		t.Error("Prompt after Close succeeded")
	}
}

func TestACPEnvIsScrubbed(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("YOUTRACK_PASSWORD", "secret")
	s, _ := fake(t, "acp", agenttest.Script{Turns: [][]agenttest.Step{{
		{Env: "CLAUDECODE"}, {Text: ";"}, {Env: "YOUTRACK_PASSWORD"}, {Text: ";"}, {Env: "EXTRA"},
	}}}, StartOptions{Env: map[string]string{"EXTRA": "1"}})
	if res := s.prompt(t, "env"); res.Text != "CLAUDECODE unset;YOUTRACK_PASSWORD unset;EXTRA=1" {
		t.Errorf("text = %q", res.Text)
	}
}

func TestACPStopReasons(t *testing.T) {
	s, _ := fake(t, "acp", agenttest.Script{Turns: [][]agenttest.Step{{{Text: "no"}, {Stop: "refusal"}}}}, StartOptions{})
	if res := s.prompt(t, "x"); res.StopReason != StopRefusal || res.IsError {
		t.Errorf("result = %+v", res)
	}
}
