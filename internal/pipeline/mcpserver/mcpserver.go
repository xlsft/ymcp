// Package mcpserver serves the ymcp tools to pipeline agents over MCP
// streamable HTTP on a loopback port. Each run gets its own bearer token
// and MCP server: YouTrack text the agent writes is marked as the run's,
// and two pipeline tools, request_input and report, reach the runner.
package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/tools"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// Callbacks receive the pipeline tool calls of a run.
type Callbacks interface {
	// RequestInput records a question for a human; the run waits for the reply.
	RequestInput(ctx context.Context, run int64, question string) error
	// Report publishes a brief interim status.
	Report(ctx context.Context, run int64, text string) error
}

// RunInfo identifies the run behind a token.
type RunInfo struct {
	ID       int64
	Number   int
	Pipeline string
	Mention  string // "@login" in YouTrack
}

// Decorator marks text a run's agent writes to YouTrack.
type Decorator func(r RunInfo, body string) string

// QuestionRecorded is what request_input returns to the agent.
const QuestionRecorded = "Question recorded. End your turn; the reply arrives as your next message."

// Server is the per-runner MCP endpoint. Its methods are safe for
// concurrent use.
type Server struct {
	yt       *youtrack.Client
	profile  string
	enabled  map[string]bool
	decorate Decorator
	cb       Callbacks
	logger   *slog.Logger

	mu     sync.Mutex
	grants map[string]*grant // token → run
	srv    *http.Server
}

// grant is a run's access: its MCP server and HTTP handler, built on the
// first request.
type grant struct {
	run     RunInfo
	token   []byte
	once    sync.Once
	server  *mcp.Server
	handler http.Handler
}

// New returns a server for the profile's YouTrack client yt, serving the
// tools in enabled (all when nil, see tools.Select). decorate may be nil
// to leave text unmarked.
func New(yt *youtrack.Client, profile string, enabled map[string]bool, decorate Decorator, cb Callbacks, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Server{
		yt:       yt,
		profile:  profile,
		enabled:  enabled,
		decorate: decorate,
		cb:       cb,
		logger:   logger,
		grants:   map[string]*grant{},
	}
}

// Start listens on addr, e.g. "127.0.0.1:0", and serves /<profile> in the
// background. It returns the base URL, e.g. http://127.0.0.1:41234.
func (s *Server) Start(addr string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return "", errors.New("mcpserver: already started")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("mcpserver: %w", err)
	}
	s.srv = &http.Server{Handler: http.HandlerFunc(s.serveHTTP), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("pipeline MCP server stopped", "err", err)
		}
	}()
	return "http://" + ln.Addr().String(), nil
}

// Grant gives run r access and returns its bearer token: 32 random bytes
// in hex.
func (s *Server) Grant(r RunInfo) string {
	b := make([]byte, 32)
	rand.Read(b) // never fails
	token := hex.EncodeToString(b)
	s.mu.Lock()
	s.grants[token] = &grant{run: r, token: []byte(token)}
	s.mu.Unlock()
	s.logger.Debug("pipeline MCP access granted", "pipeline", r.Pipeline, "run", r.Number)
	return token
}

// Revoke ends the token's access and closes the run's open MCP sessions.
func (s *Server) Revoke(token string) {
	s.mu.Lock()
	g := s.grants[token]
	delete(s.grants, token)
	s.mu.Unlock()
	if g == nil {
		return
	}
	g.closeSessions()
	s.logger.Debug("pipeline MCP access revoked", "pipeline", g.run.Pipeline, "run", g.run.Number)
}

// Close stops the HTTP server and drops every grant.
func (s *Server) Close() error {
	s.mu.Lock()
	srv, grants := s.srv, s.grants
	s.grants = map[string]*grant{}
	s.mu.Unlock()
	for _, g := range grants {
		g.closeSessions()
	}
	if srv == nil {
		return nil
	}
	return srv.Close()
}

// serveHTTP hands a request for /<profile> to the MCP handler of the run
// its bearer token selects. Without a valid token it answers 401.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	g := s.lookup(r.Header.Get("Authorization"))
	if g != nil {
		g.once.Do(func() {
			g.server = s.newMCP(g.run)
			g.handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return g.server }, nil)
		})
	}
	if g == nil || g.handler == nil { // nil handler: revoked meanwhile
		s.logger.Warn("rejected pipeline MCP request without a valid token", "remote", r.RemoteAddr, "path", r.URL.Path)
		w.Header().Set("WWW-Authenticate", `Bearer realm="ymcp"`)
		http.Error(w, "missing or wrong bearer token", http.StatusUnauthorized)
		return
	}
	if base := "/" + s.profile; r.URL.Path != base && !strings.HasPrefix(r.URL.Path, base+"/") {
		http.Error(w, "no such profile; served: "+base, http.StatusNotFound)
		return
	}
	g.handler.ServeHTTP(w, r)
}

// lookup finds the grant for an Authorization header, comparing every
// token in constant time.
func (s *Server) lookup(header string) *grant {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *grant
	for _, g := range s.grants {
		if subtle.ConstantTimeCompare([]byte(token), g.token) == 1 {
			found = g
		}
	}
	return found
}

// closeSessions ends the run's MCP sessions, if its server was built.
func (g *grant) closeSessions() {
	g.once.Do(func() {}) // a request still in flight must not build one now
	if g.server == nil {
		return
	}
	for ss := range g.server.Sessions() {
		ss.Close()
	}
}

// newMCP builds the MCP server of run r: the ymcp tools with marked text,
// plus the pipeline tools.
func (s *Server) newMCP(r RunInfo) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "ymcp", Version: version()}, &mcp.ServerOptions{
		Instructions: "Tools for the YouTrack instance at " + s.yt.BaseURL() + ". Issues are addressed by readable ID (e.g. PRJ-123). " +
			"request_input and report talk to the ymcp pipeline running this session.",
	})
	var opts tools.Options
	if s.decorate != nil {
		opts.Mark = func(text string) string { return s.decorate(r, text) }
	}
	tools.RegisterWith(srv, s.yt, s.enabled, opts)
	s.addPipelineTools(srv, r)
	srv.AddReceivingMiddleware(s.logCalls(r))
	return srv
}

type requestInputIn struct {
	Question string `json:"question" jsonschema:"What you need decided: the question, the options and your recommendation. Markdown is supported."`
}

type reportIn struct {
	Text string `json:"text" jsonschema:"A brief status update, Markdown supported"`
}

func (s *Server) addPipelineTools(srv *mcp.Server, r RunInfo) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "request_input",
		Description: "Asks a human for a decision you can't make yourself, e.g. when the issue is ambiguous or contradicts the code. " +
			"Say what you need, the options and your recommendation. The question is posted for you and returns at once: " +
			"end your turn right after the call; the reply arrives as your next message.",
		Annotations: pipelineHints("Request input"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in requestInputIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.Question) == "" {
			return nil, nil, errors.New("question must not be empty")
		}
		if err := s.cb.RequestInput(ctx, r.ID, in.Question); err != nil {
			return nil, nil, fmt.Errorf("question not recorded: %w", err)
		}
		return textResult(QuestionRecorded), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "report",
		Description: "Posts a brief interim status of your work to the issue. Use it rarely, for progress worth telling mid-turn; " +
			"your final message is published for you, so don't repeat it here.",
		Annotations: pipelineHints("Report status"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in reportIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.Text) == "" {
			return nil, nil, errors.New("text must not be empty")
		}
		if err := s.cb.Report(ctx, r.ID, in.Text); err != nil {
			return nil, nil, fmt.Errorf("report not posted: %w", err)
		}
		return textResult("Reported."), nil, nil
	})
}

// pipelineHints declares all four hints, like the ymcp tools: each call
// publishes something new to a tracker other people read.
func pipelineHints(title string) *mcp.ToolAnnotations {
	destructive, openWorld := false, true
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: &destructive,
		IdempotentHint:  false,
		OpenWorldHint:   &openWorld,
	}
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// logCalls logs the run's tool calls at debug level and failures as warnings.
func (s *Server) logCalls(r RunInfo) mcp.Middleware {
	logger := s.logger.With("pipeline", r.Pipeline, "run", r.Number)
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			start := time.Now()
			res, err := next(ctx, method, req)
			p, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok {
				return res, err
			}
			took := time.Since(start).Round(time.Millisecond)
			if tr, _ := res.(*mcp.CallToolResult); err != nil || (tr != nil && tr.IsError) {
				logger.Warn("agent tool failed", "tool", p.Name, "took", took, "err", errText(err, tr))
			} else {
				logger.Debug("agent tool", "tool", p.Name, "took", took)
			}
			return res, err
		}
	}
}

func errText(err error, r *mcp.CallToolResult) string {
	if err != nil {
		return err.Error()
	}
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			return t.Text
		}
	}
	return ""
}

// version is the module version of the binary, "(devel)" in local builds.
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "dev"
}
