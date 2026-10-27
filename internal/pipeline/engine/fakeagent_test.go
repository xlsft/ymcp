package engine

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
)

// fakeAgent is an in-process agent.Agent whose turns are Go functions.
// Turns are taken in order across sessions; past the list a turn answers
// "ok". It records every start and prompt.
type fakeAgent struct {
	mu       sync.Mutex
	turns    []func(*fakeTurn) agent.Result
	next     int
	starts   []agent.StartOptions
	prompts  []string
	sessions int
	noResume bool // Start with a SessionID fails with ErrNoResume
}

func (a *fakeAgent) Name() string { return "fake" }

func (a *fakeAgent) Check(context.Context) (agent.Info, error) {
	return agent.Info{Path: "/usr/bin/fake", Version: "fake 1.0", Warnings: []string{"fake warning"}}, nil
}

func (a *fakeAgent) script(turns ...func(*fakeTurn) agent.Result) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.turns = append(a.turns, turns...)
}

func (a *fakeAgent) Start(_ context.Context, opts agent.StartOptions) (agent.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.starts = append(a.starts, opts)
	if opts.SessionID != "" && a.noResume {
		return nil, fmt.Errorf("fake: %w", agent.ErrNoResume)
	}
	a.sessions++
	id := opts.SessionID
	if id == "" {
		id = fmt.Sprintf("session-%d", a.sessions)
	}
	return &fakeSession{a: a, opts: opts, id: id, events: make(chan agent.Event, 1024), answers: map[string]chan string{}}, nil
}

func (a *fakeAgent) startLog() []agent.StartOptions {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.starts)
}

func (a *fakeAgent) promptLog() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.prompts)
}

type fakeSession struct {
	a    *fakeAgent
	opts agent.StartOptions
	id   string

	mu      sync.Mutex
	events  chan agent.Event
	closed  bool
	cancel  chan struct{}
	answers map[string]chan string
	reqs    int
}

func (s *fakeSession) ID() string                 { return s.id }
func (s *fakeSession) Events() <-chan agent.Event { return s.events }

func (s *fakeSession) emit(e agent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.events <- e
	}
}

func (s *fakeSession) Prompt(ctx context.Context, text string) (agent.Result, error) {
	s.a.mu.Lock()
	s.a.prompts = append(s.a.prompts, text)
	var fn func(*fakeTurn) agent.Result
	if s.a.next < len(s.a.turns) {
		fn = s.a.turns[s.a.next]
	}
	s.a.next++
	s.a.mu.Unlock()

	s.mu.Lock()
	s.cancel = make(chan struct{})
	cancel := s.cancel
	s.mu.Unlock()
	t := &fakeTurn{s: s, prompt: text, cancel: cancel}
	if fn == nil {
		t.Say("ok")
		return agent.Result{StopReason: agent.StopEndTurn, Text: t.text}, nil
	}
	res := fn(t)
	if res.Text == "" {
		res.Text = t.text
	}
	if res.StopReason == "" {
		res.StopReason = agent.StopEndTurn
		if t.cancelled() {
			res.StopReason = agent.StopCancelled
		}
	}
	return res, nil
}

func (s *fakeSession) Answer(requestID, optionID string) error {
	s.mu.Lock()
	ch := s.answers[requestID]
	delete(s.answers, requestID)
	s.mu.Unlock()
	if ch == nil {
		return fmt.Errorf("no request %s", requestID)
	}
	ch <- optionID
	return nil
}

func (s *fakeSession) Cancel() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		select {
		case <-s.cancel:
		default:
			close(s.cancel)
		}
	}
	return nil
}

func (s *fakeSession) Close() error {
	s.Cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.events)
	}
	return nil
}

// fakeTurn is what a scripted turn can do.
type fakeTurn struct {
	s      *fakeSession
	prompt string
	text   string
	cancel chan struct{}
}

func (t *fakeTurn) cancelled() bool {
	select {
	case <-t.cancel:
		return true
	default:
		return false
	}
}

// Say streams text.
func (t *fakeTurn) Say(s string) {
	t.text += s
	t.s.emit(agent.Event{Kind: agent.EventText, Text: s})
}

// Hang waits until the turn is cancelled.
func (t *fakeTurn) Hang() { <-t.cancel }

// Ask asks for permission to run a tool and returns the chosen option id,
// or "" when the turn was cancelled.
func (t *fakeTurn) Ask(title string) string {
	s := t.s
	s.mu.Lock()
	s.reqs++
	id := fmt.Sprint(s.reqs)
	ch := make(chan string, 1)
	s.answers[id] = ch
	s.mu.Unlock()
	tool := agent.ToolCall{ID: "call-" + id, Title: title, Kind: "execute", Status: "pending", Input: []byte(`{"command":"make test"}`)}
	s.emit(agent.Event{Kind: agent.EventPermission, Text: title, Tool: &tool, Permission: &agent.Permission{
		RequestID: id, Tool: tool,
		Options: []agent.Option{{ID: "yes", Name: "Allow", Kind: "allow_once"}, {ID: "no", Name: "Reject", Kind: "reject_once"}},
	}})
	select {
	case o := <-ch:
		return o
	case <-t.cancel:
		return ""
	}
}

// Tool calls a tool of the ymcp MCP server the session was given.
func (t *fakeTurn) Tool(name string, args map[string]any) (string, error) {
	var srv agent.MCPServer
	for _, m := range t.s.opts.MCP {
		if m.Name == "ymcp" {
			srv = m
		}
	}
	if srv.URL == "" {
		return "", fmt.Errorf("no ymcp server")
	}
	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "fake-agent", Version: "0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   srv.URL,
		HTTPClient: &http.Client{Transport: headers(srv.Headers)},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		return "", err
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", err
	}
	var out string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out += tc.Text
		}
	}
	if res.IsError {
		return out, fmt.Errorf("tool error: %s", out)
	}
	return out, nil
}

// headers adds fixed headers to every request.
type headers map[string]string

func (h headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}
