package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/youtrack"
	"github.com/xlsft/ymcp/internal/youtrack/youtracktest"
)

// fakeCallbacks records the pipeline tool calls.
type fakeCallbacks struct {
	mu        sync.Mutex
	questions []string // "<run>: <question>"
	reports   []string // "<run>: <text>"
	changes   []string // "<run>: <title> | <description>"
	err       error
}

func (f *fakeCallbacks) RequestInput(_ context.Context, run int64, question string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.questions = append(f.questions, fmt.Sprintf("%d: %s", run, question))
	return f.err
}

func (f *fakeCallbacks) Report(_ context.Context, run int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, fmt.Sprintf("%d: %s", run, text))
	return f.err
}

func (f *fakeCallbacks) SetChange(_ context.Context, run int64, title, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes = append(f.changes, fmt.Sprintf("%d: %s | %s", run, title, description))
	return f.err
}

func (f *fakeCallbacks) calls() (questions, reports []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.questions), slices.Clone(f.reports)
}

// fakeYouTrack answers comment posts and records their text.
type fakeYouTrack struct {
	mu       sync.Mutex
	comments []string
}

func (f *fakeYouTrack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/api/issues/DEMO-7/comments" {
		http.NotFound(w, r)
		return
	}
	var body struct{ Text string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.comments = append(f.comments, body.Text)
	n := len(f.comments)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"4-%d","text":%q}`, n, body.Text)
}

func (f *fakeYouTrack) posted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.comments)
}

func decorate(r RunInfo, body string) string {
	return fmt.Sprintf("[%s's agent #%d (%s)] %s", r.Mention, r.Number, r.Pipeline, body)
}

// start runs a Server for profile "work" against a fake YouTrack and
// returns it with its endpoint.
func start(t *testing.T, api http.Handler, enabled map[string]bool, cb Callbacks) (*Server, string) {
	t.Helper()
	f := youtracktest.New(t, api)
	yt, err := youtrack.New(youtrack.Options{BaseURL: f.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	s := New(yt, "work", enabled, decorate, cb, nil)
	base, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("base URL = %q", base)
	}
	return s, base + "/work"
}

// bearer adds the token to every request.
type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "0"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: bearer(token)},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callTool(cs *mcp.ClientSession, name string, args map[string]any) (text string, isError bool, err error) {
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", false, err
	}
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	return text, res.IsError, nil
}

// status posts an MCP initialize request with the given Authorization
// header and returns the HTTP status.
func status(t *testing.T, url, auth string) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"0"}}}`
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func TestRunsGetTheirOwnAccess(t *testing.T) {
	yt := &fakeYouTrack{}
	cb := &fakeCallbacks{}
	s, endpoint := start(t, yt, nil, cb)
	tokA := s.Grant(RunInfo{ID: 1, Number: 17, Pipeline: "backend", Mention: "@alice"})
	tokB := s.Grant(RunInfo{ID: 2, Number: 18, Pipeline: "frontend", Mention: "@alice"})
	if len(tokA) != 64 || tokA == tokB {
		t.Fatalf("tokens %q, %q: want two distinct 64-char hex strings", tokA, tokB)
	}

	for _, tc := range []struct {
		name, url, auth string
		want            int
	}{
		{"no token", endpoint, "", http.StatusUnauthorized},
		{"wrong token", endpoint, "Bearer " + strings.Repeat("0", 64), http.StatusUnauthorized},
		{"token without Bearer", endpoint, tokA, http.StatusUnauthorized},
		{"token prefix", endpoint, "Bearer " + tokA[:32], http.StatusUnauthorized},
		{"other profile", strings.TrimSuffix(endpoint, "/work") + "/home", "Bearer " + tokA, http.StatusNotFound},
		{"profile with slash", endpoint + "/", "Bearer " + tokA, http.StatusOK},
		{"valid", endpoint, "Bearer " + tokA, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := status(t, tc.url, tc.auth); got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}

	a, b := connect(t, endpoint, tokA), connect(t, endpoint, tokB)

	list, err := a.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
		if tool.Name == "request_input" || tool.Name == "report" || tool.Name == "set_change" {
			h := tool.Annotations
			if h == nil || h.ReadOnlyHint || h.DestructiveHint == nil || *h.DestructiveHint || h.IdempotentHint || h.OpenWorldHint == nil || !*h.OpenWorldHint {
				t.Errorf("%s annotations = %+v", tool.Name, h)
			}
		}
	}
	for _, want := range []string{"request_input", "report", "set_change", "add_issue_comment", "get_issue"} {
		if !slices.Contains(names, want) {
			t.Errorf("tool %s missing from %v", want, names)
		}
	}

	// Each run's comments carry its own mark.
	for _, cs := range []*mcp.ClientSession{a, b} {
		if text, isErr, err := callTool(cs, "add_issue_comment", map[string]any{"issueId": "DEMO-7", "text": "done"}); err != nil || isErr {
			t.Fatalf("add_issue_comment: %v %s", err, text)
		}
	}
	want := []string{"[@alice's agent #17 (backend)] done", "[@alice's agent #18 (frontend)] done"}
	if got := yt.posted(); !slices.Equal(got, want) {
		t.Errorf("posted comments = %q, want %q", got, want)
	}

	// Pipeline tools reach the callbacks with the run's ID.
	text, isErr, err := callTool(a, "request_input", map[string]any{"question": "Which DB?"})
	if err != nil || isErr || text != QuestionRecorded {
		t.Errorf("request_input = %q, isError %v, err %v", text, isErr, err)
	}
	text, isErr, err = callTool(b, "report", map[string]any{"text": "tests pass"})
	if err != nil || isErr || text != "Reported." {
		t.Errorf("report = %q, isError %v, err %v", text, isErr, err)
	}
	if _, isErr, _ := callTool(a, "report", map[string]any{"text": "  "}); !isErr {
		t.Error("an empty report should fail")
	}
	text, isErr, err = callTool(a, "set_change", map[string]any{"title": " fix(auth):\n keep the  cookie ", "description": "- Keeps it.\n"})
	if err != nil || isErr || text != ChangeSet {
		t.Errorf("set_change = %q, isError %v, err %v", text, isErr, err)
	}
	if _, isErr, _ := callTool(a, "set_change", map[string]any{"title": "x", "description": " "}); !isErr {
		t.Error("an empty description should fail")
	}
	cb.mu.Lock()
	if want := []string{"1: fix(auth): keep the cookie | - Keeps it."}; !slices.Equal(cb.changes, want) {
		t.Errorf("changes = %q, want %q", cb.changes, want)
	}
	cb.mu.Unlock()
	questions, reports := cb.calls()
	if !slices.Equal(questions, []string{"1: Which DB?"}) || !slices.Equal(reports, []string{"2: tests pass"}) {
		t.Errorf("questions = %q, reports = %q", questions, reports)
	}

	// Revoking A cuts it off, sessions included, and leaves B working.
	s.Revoke(tokA)
	if _, _, err := callTool(a, "get_current_user", nil); err == nil {
		t.Error("revoked run A can still call tools")
	}
	if got := status(t, endpoint, "Bearer "+tokA); got != http.StatusUnauthorized {
		t.Errorf("revoked token: status %d, want 401", got)
	}
	if text, isErr, err := callTool(b, "report", map[string]any{"text": "still here"}); err != nil || isErr {
		t.Errorf("run B after revoking A: %v %s", err, text)
	}
	s.Revoke(tokA) // twice is fine
}

func TestCallbackErrorsReachTheAgent(t *testing.T) {
	cb := &fakeCallbacks{err: errors.New("store is closed")}
	s, endpoint := start(t, &fakeYouTrack{}, nil, cb)
	cs := connect(t, endpoint, s.Grant(RunInfo{ID: 3, Number: 1, Pipeline: "p"}))
	text, isErr, err := callTool(cs, "request_input", map[string]any{"question": "?"})
	if err != nil || !isErr || !strings.Contains(text, "store is closed") {
		t.Errorf("request_input = %q, isError %v, err %v", text, isErr, err)
	}
}

func TestEnabledToolsKeepPipelineTools(t *testing.T) {
	s, endpoint := start(t, &fakeYouTrack{}, map[string]bool{"add_issue_comment": true}, &fakeCallbacks{})
	cs := connect(t, endpoint, s.Grant(RunInfo{ID: 1, Number: 1, Pipeline: "p"}))
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if want := []string{"add_issue_comment", "report", "request_input", "set_change"}; !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}

func TestNoDecoratorLeavesTextAlone(t *testing.T) {
	yt := &fakeYouTrack{}
	f := youtracktest.New(t, yt)
	client, err := youtrack.New(youtrack.Options{BaseURL: f.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	s := New(client, "work", nil, nil, &fakeCallbacks{}, nil)
	base, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Start("127.0.0.1:0"); err == nil {
		t.Error("a second Start should fail")
	}
	cs := connect(t, base+"/work", s.Grant(RunInfo{ID: 1, Number: 1, Pipeline: "p"}))
	if text, isErr, err := callTool(cs, "add_issue_comment", map[string]any{"issueId": "DEMO-7", "text": "plain"}); err != nil || isErr {
		t.Fatalf("add_issue_comment: %v %s", err, text)
	}
	if got := yt.posted(); !slices.Equal(got, []string{"plain"}) {
		t.Errorf("posted = %q", got)
	}
}
