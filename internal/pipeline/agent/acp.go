package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/acp-go-sdk"
)

// cancelGrace is how long a cancelled ACP turn may take to answer
// "cancelled" before Prompt returns without it.
var cancelGrace = 10 * time.Second

// acpAgent is an agent speaking the Agent Client Protocol on stdio: one
// process per session.
type acpAgent struct {
	provider string
	command  string
	args     []string
	settings
}

func (a *acpAgent) Name() string { return a.provider }

func (a *acpAgent) Check(ctx context.Context) (Info, error) {
	info, err := a.checkBinary(ctx, a.command)
	if err != nil {
		return info, err
	}
	switch a.provider {
	case "kimi":
		switch a.mode {
		case "yolo":
			info.Warnings = append(info.Warnings, `kimi in mode "yolo" never asks for permission, not even for writes outside the worktree`)
		case "auto":
			info.Warnings = append(info.Warnings, `kimi in mode "auto" approves what it deems safe on its own and asks about the rest`)
		}
	case "opencode":
		if a.model == "" {
			info.Warnings = append(info.Warnings, "opencode has no model set: it uses its own default, which may not be configured")
		}
	}
	return info, nil
}

func (a *acpAgent) Start(ctx context.Context, opts StartOptions) (Session, error) {
	argv := append(append([]string{a.command}, a.args...), opts.Args...)
	s := &acpSession{
		provider: a.provider,
		command:  a.command,
		events:   newEventQueue(),
		pending:  map[string]*pendingPermission{},
		tools:    map[string]ToolCall{},
		exited:   make(chan struct{}),
		done:     make(chan struct{}),
	}
	p, err := a.launch(ctx, opts.Dir, opts.Env, argv, nil, true, &s.stderr, opts.OnGroup)
	if err != nil {
		s.events.close()
		return nil, fmt.Errorf("%s: start: %w", a.provider, err)
	}
	s.cmd, s.stdin, s.stdout, s.group = p.cmd, p.stdin, p.stdout, p.group
	stdin, stdout, g := p.stdin, p.stdout, p.group
	go func() {
		err := p.cmd.Wait()
		g.exited()
		g.kill() // children of a dead agent would keep the pipe open
		s.waitErr = err
		close(s.exited)
	}()

	s.conn = acp.NewClientSideConnection(acpClient{s}, stdin, stdout)
	s.conn.SetLogger(a.logger)
	if err := s.open(ctx, opts); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// open initializes the connection, creates or resumes the session and
// applies the model and mode.
func (s *acpSession) open(ctx context.Context, opts StartOptions) error {
	init, err := s.conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion:    acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{}, // no fs, no terminal
		ClientInfo:         &acp.Implementation{Name: "ymcp", Version: "1"},
	})
	if err != nil {
		return s.fail("initialize", err)
	}
	caps := init.AgentCapabilities
	if len(opts.MCP) > 0 && !caps.McpCapabilities.Http {
		return fmt.Errorf("%s: the agent doesn't support MCP servers over HTTP", s.provider)
	}
	servers := acpServers(opts.MCP)
	var cfg []acp.SessionConfigOption
	var modes *acp.SessionModeState
	if opts.SessionID != "" {
		if caps.SessionCapabilities.Resume == nil {
			return fmt.Errorf("%s: %w: no session/resume support", s.provider, ErrNoResume)
		}
		// session/resume, not session/load: load replays the whole history.
		r, err := s.conn.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: acp.SessionId(opts.SessionID), Cwd: opts.Dir, McpServers: servers})
		if err != nil {
			if s.gone() {
				return s.fail("session/resume", err)
			}
			return fmt.Errorf("%s: %w: %v", s.provider, ErrNoResume, err)
		}
		s.setID(opts.SessionID)
		cfg, modes = r.ConfigOptions, r.Modes
	} else {
		r, err := s.conn.NewSession(ctx, acp.NewSessionRequest{Cwd: opts.Dir, McpServers: servers})
		if err != nil {
			return s.fail("session/new", err)
		}
		s.setID(string(r.SessionId))
		cfg, modes = r.ConfigOptions, r.Modes
		s.systemPrompt = opts.SystemPrompt
	}
	s.events.emit(Event{Kind: EventSession, SessionID: s.ID()})
	// Set again after a resume too: kimi forgets the mode.
	if opts.Model != "" {
		if err := s.setOption(ctx, configID(cfg, "model"), opts.Model); err != nil {
			return fmt.Errorf("%s: set model %q: %w", s.provider, opts.Model, err)
		}
	}
	if opts.Mode != "" {
		var err error
		if id := configID(cfg, "mode"); id != "" || modes == nil {
			if id == "" {
				id = "mode"
			}
			err = s.setOption(ctx, id, opts.Mode)
		} else {
			_, err = s.conn.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: acp.SessionId(s.ID()), ModeId: acp.SessionModeId(opts.Mode)})
		}
		if err != nil {
			return fmt.Errorf("%s: set mode %q: %w", s.provider, opts.Mode, err)
		}
	}
	return nil
}

func (s *acpSession) setOption(ctx context.Context, id, value string) error {
	if id == "" {
		id = "model"
	}
	_, err := s.conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{
		SessionId: acp.SessionId(s.ID()), ConfigId: acp.SessionConfigId(id), Value: acp.SessionConfigValueId(value),
	}})
	return err
}

// configID finds the select option with this id or category; "" if none.
func configID(cfg []acp.SessionConfigOption, want string) string {
	for _, o := range cfg {
		if o.Select == nil {
			continue
		}
		if string(o.Select.Id) == want || (o.Select.Category != nil && string(*o.Select.Category) == want) {
			return string(o.Select.Id)
		}
	}
	return ""
}

func acpServers(servers []MCPServer) []acp.McpServer {
	out := make([]acp.McpServer, 0, len(servers))
	for _, m := range servers {
		headers := make([]acp.HttpHeader, 0, len(m.Headers))
		for k, v := range m.Headers {
			headers = append(headers, acp.HttpHeader{Name: k, Value: v})
		}
		out = append(out, acp.McpServer{Http: &acp.McpServerHttpInline{Type: "http", Name: m.Name, Url: m.URL, Headers: headers}})
	}
	return out
}

type acpSession struct {
	provider     string
	command      string // the executable, named when it is not found
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	stdout       *os.File
	group        stopper
	conn         *acp.ClientSideConnection
	events       *eventQueue
	stderr       tail
	exited       chan struct{} // closed when the process has exited
	waitErr      error
	done         chan struct{} // closed by Close
	closeOnce    sync.Once
	turn         sync.Mutex // held by a running Prompt
	nextRequest  atomic.Int64
	systemPrompt string // sent in front of the first prompt, then cleared

	mu        sync.Mutex
	id        string
	pending   map[string]*pendingPermission
	tools     map[string]ToolCall // latest state of each tool call
	text      strings.Builder     // the running turn's answer
	afterTool bool                // a tool ran since the last text: the next text starts a new paragraph
	denied    []ToolCall
	cancelled bool
}

type pendingPermission struct {
	perm   Permission
	answer chan string // option ID; "" for cancelled
}

// ID is guarded: notifications may arrive while the session is created.
func (s *acpSession) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func (s *acpSession) setID(id string) {
	s.mu.Lock()
	s.id = id
	s.mu.Unlock()
}

func (s *acpSession) Events() <-chan Event { return s.events.out }

// gone reports whether the agent process has exited or closed its end
// of the connection.
func (s *acpSession) gone() bool {
	select {
	case <-s.exited:
		return true
	case <-s.conn.Done():
		return true
	default:
		return false
	}
}

// fail wraps an error of a protocol step, explaining a dead agent with
// its exit status and stderr.
func (s *acpSession) fail(step string, err error) error {
	select {
	case <-s.exited:
	case <-time.After(200 * time.Millisecond):
	}
	if s.gone() {
		msg := "agent closed the connection"
		select {
		case <-s.exited:
			msg = "agent exited"
			if s.waitErr != nil {
				msg += " (" + s.waitErr.Error() + ")"
			}
		default:
		}
		return fmt.Errorf("%s: %s: %s", s.provider, step, notFound(withStderr(msg, &s.stderr), s.waitErr, s.command))
	}
	return fmt.Errorf("%s: %s: %w", s.provider, step, err)
}

func (s *acpSession) Prompt(ctx context.Context, text string) (Result, error) {
	if !s.turn.TryLock() {
		return Result{}, fmt.Errorf("%s: a turn is already running", s.provider)
	}
	defer s.turn.Unlock()
	select {
	case <-s.done:
		return Result{}, fmt.Errorf("%s: session closed", s.provider)
	default:
	}
	if ctx.Err() != nil {
		return Result{StopReason: StopCancelled}, nil
	}
	s.mu.Lock()
	s.text.Reset()
	s.denied, s.cancelled, s.afterTool = nil, false, false
	s.mu.Unlock()
	if s.systemPrompt != "" {
		text = s.systemPrompt + "\n\n" + text
		s.systemPrompt = ""
	}

	// The turn runs on its own context: when ctx ends the agent is asked
	// to cancel and gets cancelGrace to say so.
	pctx, abort := context.WithCancel(context.Background())
	defer abort()
	stop := context.AfterFunc(ctx, func() {
		_ = s.Cancel()
		time.AfterFunc(cancelGrace, abort)
	})
	defer stop()
	resp, err := s.conn.Prompt(pctx, acp.PromptRequest{SessionId: acp.SessionId(s.ID()), Prompt: []acp.ContentBlock{acp.TextBlock(text)}})

	s.mu.Lock()
	res := Result{Text: s.text.String(), Denied: s.denied}
	cancelled := s.cancelled
	s.mu.Unlock()
	if err != nil {
		var re *acp.RequestError
		switch {
		case cancelled && !s.gone():
			res.StopReason = StopCancelled
			return res, nil
		case s.gone() || pctx.Err() != nil:
			err = s.fail("session/prompt", err)
			res.StopReason, res.IsError, res.Err = StopError, true, err.Error()
			return res, err
		case errors.As(err, &re):
			// The agent answered the prompt with an error (quota, model, …).
			res.StopReason, res.IsError, res.Err = StopError, true, err.Error()
			s.events.emit(Event{Kind: EventError, Text: res.Err})
			return res, nil
		}
		err = s.fail("session/prompt", err)
		res.StopReason, res.IsError, res.Err = StopError, true, err.Error()
		return res, err
	}
	res.StopReason = string(resp.StopReason)
	if res.StopReason == "" {
		res.StopReason = StopEndTurn
	}
	if u := resp.Usage; u != nil {
		raw, _ := json.Marshal(u)
		s.events.emit(Event{Kind: EventUsage, Raw: raw, Text: fmt.Sprintf("in=%d out=%d total=%d", u.InputTokens, u.OutputTokens, u.TotalTokens)})
	}
	return res, nil
}

func (s *acpSession) Answer(requestID, optionID string) error {
	s.mu.Lock()
	p, ok := s.pending[requestID]
	if ok {
		known := false
		for _, o := range p.perm.Options {
			known = known || o.ID == optionID
		}
		if !known {
			s.mu.Unlock()
			return fmt.Errorf("%s: permission request %s has no option %q", s.provider, requestID, optionID)
		}
		delete(s.pending, requestID)
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s: no pending permission request %q", s.provider, requestID)
	}
	p.answer <- optionID
	return nil
}

// Cancel answers pending permission requests with their reject option
// first (opencode ignores session/cancel while one is open), then sends
// session/cancel.
func (s *acpSession) Cancel() error {
	s.mu.Lock()
	s.cancelled = true
	pending := s.pending
	s.pending = map[string]*pendingPermission{}
	s.mu.Unlock()
	for _, p := range pending {
		p.answer <- rejectOption(p.perm.Options)
	}
	select {
	case <-s.exited:
		return nil
	default:
	}
	return s.conn.Cancel(context.Background(), acp.CancelNotification{SessionId: acp.SessionId(s.ID())})
}

// rejectOption is the option to reject once (or always); "" when there is
// none, which answers "cancelled".
func rejectOption(opts []Option) string {
	for _, kind := range []string{"reject_once", "reject_always"} {
		for _, o := range opts {
			if o.Kind == kind {
				return o.ID
			}
		}
	}
	return ""
}

func (s *acpSession) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.stdin.Close()
		s.group.kill()
		<-s.exited
		s.stdout.Close()
		s.group.release()
		s.events.close()
	})
	return nil
}

// acpClient answers the agent's requests to the client.
type acpClient struct{ s *acpSession }

func (c acpClient) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	s := c.s
	if string(n.SessionId) != s.ID() {
		return nil
	}
	u := n.Update
	raw, _ := json.Marshal(u)
	switch {
	case u.AgentMessageChunk != nil:
		if t := u.AgentMessageChunk.Content.Text; t != nil && t.Text != "" {
			// The chunks of one message run together; the text before and
			// after a tool call are separate paragraphs, as claude's
			// messages are.
			chunk := t.Text
			s.mu.Lock()
			if s.afterTool && s.text.Len() > 0 {
				prev := strings.TrimRight(s.text.String(), " \t\r\n")
				s.text.Reset()
				s.text.WriteString(prev)
				chunk = "\n\n" + strings.TrimLeft(chunk, " \t\r\n")
			}
			s.afterTool = false
			s.text.WriteString(chunk)
			s.mu.Unlock()
			s.events.emit(Event{Kind: EventText, Text: chunk, Raw: raw})
		}
	case u.AgentThoughtChunk != nil:
		if t := u.AgentThoughtChunk.Content.Text; t != nil {
			s.events.emit(Event{Kind: EventThought, Text: t.Text, Raw: raw})
		}
	case u.ToolCall != nil:
		t := u.ToolCall
		tc := ToolCall{ID: string(t.ToolCallId), Title: t.Title, Kind: string(t.Kind), Status: string(t.Status), Input: rawJSON(t.RawInput)}
		if tc.Status == "" {
			tc.Status = "pending"
		}
		s.mu.Lock()
		s.tools[tc.ID] = tc
		s.afterTool = true
		s.mu.Unlock()
		s.events.emit(Event{Kind: EventToolCall, Tool: &tc, Raw: raw})
	case u.ToolCallUpdate != nil:
		t := u.ToolCallUpdate
		tc, changed := s.mergeTool(string(t.ToolCallId), t.Title, t.Kind, t.Status, t.RawInput)
		s.mu.Lock()
		s.afterTool = true
		s.mu.Unlock()
		// Agents repeat updates while a tool streams its output, which
		// ymcp doesn't keep: only a change of what it records is an event.
		if changed {
			s.events.emit(Event{Kind: EventToolUpdate, Tool: &tc, Raw: raw})
		}
	case u.UsageUpdate != nil:
		s.events.emit(Event{Kind: EventUsage, Raw: raw, Text: fmt.Sprintf("used=%d size=%d", u.UsageUpdate.Used, u.UsageUpdate.Size)})
	}
	return nil
}

// mergeTool applies an update to the known state of a tool call; changed
// reports whether its title, kind, status or input changed (or the call
// was not known).
func (s *acpSession) mergeTool(id string, title *string, kind *acp.ToolKind, status *acp.ToolCallStatus, input any) (tc ToolCall, changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tc, ok := s.tools[id]
	if !ok {
		tc = ToolCall{ID: id, Status: "pending"}
	}
	before := tc
	if title != nil && *title != "" {
		tc.Title = *title
	}
	if kind != nil && *kind != "" {
		tc.Kind = string(*kind)
	}
	if status != nil && *status != "" {
		tc.Status = string(*status)
	}
	if in := rawJSON(input); in != nil {
		tc.Input = in
	}
	s.tools[id] = tc
	changed = !ok || tc.Title != before.Title || tc.Kind != before.Kind || tc.Status != before.Status || !bytes.Equal(tc.Input, before.Input)
	return tc, changed
}

func rawJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "{}" || string(b) == "null" {
		return nil
	}
	return b
}

func (c acpClient) RequestPermission(ctx context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	s := c.s
	cancelled := acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}
	if string(p.SessionId) != s.ID() {
		return cancelled, nil
	}
	t := p.ToolCall
	tool, _ := s.mergeTool(string(t.ToolCallId), nil, t.Kind, nil, t.RawInput)
	if t.Title != nil && *t.Title != "" {
		tool.Title = *t.Title // opencode puts the target path here
	}
	if tool.Input == nil {
		tool.Input = rawJSON(t.Content) // kimi describes the call only in content
	}
	perm := Permission{RequestID: strconv.FormatInt(s.nextRequest.Add(1), 10), Tool: tool}
	for _, o := range p.Options {
		perm.Options = append(perm.Options, Option{ID: string(o.OptionId), Name: o.Name, Kind: string(o.Kind)})
	}
	pp := &pendingPermission{perm: perm, answer: make(chan string, 1)}
	s.mu.Lock()
	cancelling := s.cancelled
	if !cancelling {
		s.pending[perm.RequestID] = pp
	}
	s.mu.Unlock()
	if cancelling {
		// Asked while the turn is being cancelled: reject at once.
		pp.answer <- rejectOption(perm.Options)
	} else {
		raw, _ := json.Marshal(p)
		s.events.emit(Event{Kind: EventPermission, Text: describe(t), Permission: &perm, Tool: &perm.Tool, Raw: raw})
	}

	var option string
	select {
	case option = <-pp.answer:
	case <-ctx.Done():
	case <-s.done:
	}
	s.mu.Lock()
	delete(s.pending, perm.RequestID)
	if option != "" && !s.cancelled {
		for _, o := range perm.Options {
			if o.ID == option && strings.HasPrefix(o.Kind, "reject") {
				d := perm.Tool
				d.Status = "denied"
				s.denied = append(s.denied, d)
			}
		}
	}
	s.mu.Unlock()
	if option == "" {
		return cancelled, nil
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: acp.PermissionOptionId(option)}}}, nil
}

// describe is a one-line text of what a permission request is about.
func describe(t acp.ToolCallUpdate) string {
	var parts []string
	for _, c := range t.Content {
		if c.Content != nil && c.Content.Content.Text != nil && c.Content.Content.Text.Text != "" {
			parts = append(parts, c.Content.Content.Text.Text)
		}
		if c.Diff != nil {
			parts = append(parts, "edit "+c.Diff.Path)
		}
	}
	if len(parts) == 0 && t.Title != nil {
		parts = append(parts, *t.Title)
	}
	return strings.Join(parts, "; ")
}

var errUnsupported = errors.New("not supported by ymcp")

func (acpClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errUnsupported
}

func (acpClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errUnsupported
}

func (acpClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errUnsupported
}

func (acpClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errUnsupported
}

func (acpClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errUnsupported
}

func (acpClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errUnsupported
}

func (acpClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errUnsupported
}
