package agenttest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/acp-go-sdk"
)

// runACP serves ACP on stdio until the client goes away.
func runACP(s Script) int {
	out := &idShifter{w: os.Stdout, state: s.State}
	if s.Banner != "" {
		out.raw(s.Banner)
	}
	a := &fakeACP{script: s, out: out}
	a.conn = acp.NewAgentSideConnection(a, out, shiftInput(os.Stdin))
	a.conn.SetLogger(slog.New(slog.DiscardHandler))
	<-a.conn.Done()
	return 0
}

// idShifter makes the fake's own requests to the client start at JSON-RPC
// id 0, as kimi and opencode do, while the SDK numbers them from 1:
// outgoing requests get id-1 and incoming responses id+1.
// Each request sent is logged as a Call with Method "request <method>"
// and the id as Params.
type idShifter struct {
	mu    sync.Mutex
	w     io.Writer
	state string
}

// Write takes one message per call, as the SDK writes them.
func (o *idShifter) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	line := shiftID(p, -1, true)
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(line, &m) == nil && m.Method != "" && m.ID != nil {
		record(o.state, Call{Method: "request " + m.Method, Params: m.ID})
	}
	_, err := o.w.Write(line)
	return len(p), err
}

// raw writes a line that is not JSON.
func (o *idShifter) raw(line string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	io.WriteString(o.w, line+"\n")
}

func shiftInput(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			if _, err := pw.Write(shiftID(sc.Bytes(), +1, false)); err != nil {
				return
			}
		}
		pw.CloseWithError(sc.Err())
	}()
	return pr
}

// shiftID adds delta to the numeric id of a request (withMethod) or a
// response (!withMethod) and returns the line with a newline.
func shiftID(line []byte, delta int64, withMethod bool) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(line, &m) != nil {
		return append(append([]byte(nil), line...), '\n')
	}
	_, hasMethod := m["method"]
	var id int64
	if raw, ok := m["id"]; ok && hasMethod == withMethod && json.Unmarshal(raw, &id) == nil {
		m["id"], _ = json.Marshal(id + delta)
	}
	b, _ := json.Marshal(m)
	return append(b, '\n')
}

type fakeACP struct {
	script Script
	conn   *acp.AgentSideConnection
	out    *idShifter
	nextID atomic.Int64

	mu       sync.Mutex
	permOpen bool // a permission request is open
}

var _ acp.Agent = (*fakeACP)(nil)

func (a *fakeACP) log(method string, params any) {
	b, _ := json.Marshal(params)
	record(a.script.State, Call{Method: method, Params: b})
}

func (a *fakeACP) Initialize(ctx context.Context, p acp.InitializeRequest) (acp.InitializeResponse, error) {
	a.log("initialize", p)
	caps := acp.AgentCapabilities{McpCapabilities: acp.McpCapabilities{Http: true}}
	if a.script.Flavor != "plain" {
		caps.SessionCapabilities.Resume = &acp.SessionResumeCapabilities{}
	}
	return acp.InitializeResponse{
		ProtocolVersion:   acp.ProtocolVersionNumber,
		AgentCapabilities: caps,
		AgentInfo:         &acp.Implementation{Name: "fake", Version: "1.0.0"},
		AuthMethods:       []acp.AuthMethod{},
	}, nil
}

func (a *fakeACP) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (a *fakeACP) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}

func (a *fakeACP) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}, nil
}

func (a *fakeACP) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}

func configOptions(s *session) []acp.SessionConfigOption {
	sel := func(id, cat, current string, values ...string) acp.SessionConfigOption {
		opts := acp.SessionConfigSelectOptionsUngrouped{}
		for _, v := range values {
			opts = append(opts, acp.SessionConfigSelectOption{Name: v, Value: acp.SessionConfigValueId(v)})
		}
		o := acp.NewSessionConfigOptionSelect(acp.SessionConfigValueId(current), acp.SessionConfigSelectOptions{Ungrouped: &opts})
		o.Select.Id, o.Select.Name = acp.SessionConfigId(id), id
		c := acp.SessionConfigOptionCategory(cat)
		o.Select.Category = &c
		return o
	}
	return []acp.SessionConfigOption{
		sel("model", "model", s.Model, "fake/default", "fake/k3"),
		sel("mode", "mode", s.Mode, "default", "plan", "auto", "yolo", "build"),
	}
}

func (a *fakeACP) NewSession(ctx context.Context, p acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	a.log("session/new", p)
	s := &session{ID: fmt.Sprintf("fake_%d_%d", os.Getpid(), a.nextID.Add(1)), Cwd: p.Cwd, Mode: "default", Model: "fake/default"}
	s.save(a.script.State)
	return acp.NewSessionResponse{SessionId: acp.SessionId(s.ID), ConfigOptions: configOptions(s)}, nil
}

func (a *fakeACP) ResumeSession(ctx context.Context, p acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	a.log("session/resume", p)
	s, ok := loadSession(a.script.State, string(p.SessionId))
	if !ok {
		return acp.ResumeSessionResponse{}, &acp.RequestError{Code: -32002, Message: "Resource not found", Data: map[string]any{"sessionId": p.SessionId}}
	}
	if s.Cwd != p.Cwd {
		return acp.ResumeSessionResponse{}, &acp.RequestError{Code: -32602, Message: "session belongs to another directory"}
	}
	if a.flavor() == "kimi" {
		s.Mode = "default" // kimi forgets the mode
		s.save(a.script.State)
	}
	return acp.ResumeSessionResponse{ConfigOptions: configOptions(s)}, nil
}

func (a *fakeACP) SetSessionConfigOption(ctx context.Context, p acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	a.log("session/set_config_option", p)
	v := p.ValueId
	if v == nil {
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams("boolean options are not supported")
	}
	s, ok := loadSession(a.script.State, string(v.SessionId))
	if !ok {
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams("unknown session")
	}
	switch v.ConfigId {
	case "mode":
		s.Mode = string(v.Value)
	case "model":
		if !strings.HasPrefix(string(v.Value), "fake/") {
			return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams(fmt.Sprintf("unknown model %q", v.Value))
		}
		s.Model = string(v.Value)
	default:
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams("unknown option " + string(v.ConfigId))
	}
	s.save(a.script.State)
	return acp.SetSessionConfigOptionResponse{ConfigOptions: configOptions(s)}, nil
}

func (a *fakeACP) SetSessionMode(ctx context.Context, p acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	a.log("session/set_mode", p)
	return acp.SetSessionModeResponse{}, nil
}

func (a *fakeACP) Cancel(ctx context.Context, p acp.CancelNotification) error {
	a.mu.Lock()
	open := a.permOpen
	a.mu.Unlock()
	a.log("session/cancel", map[string]any{"sessionId": p.SessionId, "permissionOpen": open})
	return nil
}

func (a *fakeACP) flavor() string {
	if a.script.Flavor == "" {
		return "kimi"
	}
	return a.script.Flavor
}

func (a *fakeACP) update(ctx context.Context, id acp.SessionId, u acp.SessionUpdate) {
	ctx = context.WithoutCancel(ctx) // report even a cancelled turn
	_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: id, Update: u})
}

// text sends text in two chunks, as agents stream it.
func (a *fakeACP) text(ctx context.Context, id acp.SessionId, t string) {
	half := len(t) / 2
	a.update(ctx, id, acp.UpdateAgentMessageText(t[:half]))
	a.update(ctx, id, acp.UpdateAgentMessageText(t[half:]))
}

func (a *fakeACP) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	a.log("session/prompt", p)
	s, ok := loadSession(a.script.State, string(p.SessionId))
	if !ok {
		return acp.PromptResponse{}, acp.NewInvalidParams("unknown session")
	}
	var prompt string
	for _, b := range p.Prompt {
		if b.Text != nil {
			prompt += b.Text.Text
		}
	}
	steps := turn(a.script, len(s.Prompts))
	echo := s.echo(prompt)
	s.Prompts = append(s.Prompts, prompt)
	s.save(a.script.State)

	id := p.SessionId
	cancelled := acp.PromptResponse{StopReason: acp.StopReasonCancelled}
	stop := acp.StopReasonEndTurn
	for n, st := range steps {
		if ctx.Err() != nil {
			return cancelled, nil
		}
		switch {
		case st.Exit != 0:
			os.Exit(st.Exit)
		case st.Text != "":
			a.text(ctx, id, st.Text)
		case st.Thought != "":
			a.update(ctx, id, acp.UpdateAgentThoughtText(st.Thought))
		case st.Echo:
			a.text(ctx, id, echo)
		case st.Env != "":
			a.text(ctx, id, envText(st.Env))
		case st.Tool != nil:
			tid := acp.ToolCallId(toolID(st.Tool, n))
			a.update(ctx, id, acp.StartToolCall(tid, st.Tool.Title, acp.WithStartKind(acp.ToolKind(st.Tool.Kind)), acp.WithStartStatus(acp.ToolCallStatusPending), acp.WithStartRawInput(st.Tool.Input)))
			a.update(ctx, id, acp.UpdateToolCall(tid, acp.WithUpdateStatus(acp.ToolCallStatusCompleted)))
		case st.Permission != nil:
			if end, done := a.permission(ctx, id, st.Permission, n); done {
				return end, nil
			}
		case st.Sleep > 0:
			select {
			case <-time.After(st.Sleep):
			case <-ctx.Done():
				return cancelled, nil
			}
		case st.Usage:
			a.update(ctx, id, acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{Used: 1234, Size: 100000}})
		case st.Spawn:
			spawn(a.script.State)
		case st.Garbage != "":
			a.out.raw(st.Garbage)
		case st.Stderr != "":
			fmt.Fprintln(os.Stderr, st.Stderr)
		case st.Error != "":
			return acp.PromptResponse{}, &acp.RequestError{Code: -32603, Message: st.Error}
		case st.Stop != "":
			stop = acp.StopReason(st.Stop)
		}
	}
	if ctx.Err() != nil {
		return cancelled, nil
	}
	return acp.PromptResponse{StopReason: stop, Usage: &acp.Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30}}, nil
}

// permission runs a Permission step. done means the turn ends with end.
func (a *fakeACP) permission(ctx context.Context, id acp.SessionId, t *Tool, n int) (end acp.PromptResponse, done bool) {
	cancelled := acp.PromptResponse{StopReason: acp.StopReasonCancelled}
	tid := acp.ToolCallId(toolID(t, n))
	a.update(ctx, id, acp.StartToolCall(tid, t.Title, acp.WithStartKind(acp.ToolKind(t.Kind)), acp.WithStartStatus(acp.ToolCallStatusPending)))
	options := []acp.PermissionOption{
		{OptionId: "approve_once", Name: "Approve once", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: "approve_always", Name: "Approve for this session", Kind: acp.PermissionOptionKindAllowAlways},
		{OptionId: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
	}
	rctx := ctx
	if a.flavor() == "opencode" {
		options[0].OptionId, options[1].OptionId = "once", "always"
		rctx = context.WithoutCancel(ctx) // opencode keeps waiting through session/cancel
	}
	a.mu.Lock()
	a.permOpen = true
	a.mu.Unlock()
	resp, err := a.conn.RequestPermission(rctx, acp.RequestPermissionRequest{
		SessionId: id,
		Options:   options,
		ToolCall: acp.ToolCallUpdate{ToolCallId: tid, Title: acp.Ptr(t.Title), RawInput: t.Input,
			Content: []acp.ToolCallContent{acp.ToolContent(acp.TextBlock("Requesting approval to " + t.Title))}},
	})
	a.mu.Lock()
	a.permOpen = false
	a.mu.Unlock()
	if err != nil || resp.Outcome.Selected == nil {
		a.update(ctx, id, acp.UpdateToolCall(tid, acp.WithUpdateStatus(acp.ToolCallStatusFailed)))
		return cancelled, true
	}
	choice := string(resp.Outcome.Selected.OptionId)
	if choice == "reject" {
		a.update(ctx, id, acp.UpdateToolCall(tid, acp.WithUpdateStatus(acp.ToolCallStatusFailed)))
		if a.flavor() == "opencode" {
			return cancelled, true // a reject ends opencode's turn
		}
		a.text(ctx, id, "rejected")
		return acp.PromptResponse{}, false
	}
	a.update(ctx, id, acp.UpdateToolCall(tid, acp.WithUpdateStatus(acp.ToolCallStatusInProgress), acp.WithUpdateRawInput(t.Input)))
	a.update(ctx, id, acp.UpdateToolCall(tid, acp.WithUpdateStatus(acp.ToolCallStatusCompleted)))
	a.text(ctx, id, "allowed "+choice)
	return acp.PromptResponse{}, false
}
