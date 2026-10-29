package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// claudeAgent is Claude Code driven as `claude -p … --output-format
// stream-json`, one process per turn.
type claudeAgent struct {
	provider string // claude, or anthropic and openai on a model API
	command  string
	args     []string
	settings
}

func (a *claudeAgent) Name() string { return a.provider }

// onEndpoint reports whether claude works with a model API through
// StartOptions.Endpoint rather than its own sign-in.
func (a *claudeAgent) onEndpoint() bool { return a.provider != "claude" }

// endpointEnv points claude at the gateway: every model it would pick
// (the main one, subagents, background work) is the endpoint's, and it
// sends nothing but model requests there.
func endpointEnv(e *Endpoint, extra map[string]string) map[string]string {
	env := map[string]string{
		"ANTHROPIC_BASE_URL":   e.URL,
		"ANTHROPIC_AUTH_TOKEN": e.Token,
		// Empty, not unset: claude would sign in with it otherwise.
		"ANTHROPIC_API_KEY":                        "",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS":   "1",
	}
	if e.Model != "" {
		for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL"} {
			env[k] = e.Model
		}
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func (a *claudeAgent) Check(ctx context.Context) (Info, error) {
	info, err := a.checkBinary(ctx, a.command)
	if err != nil {
		return info, err
	}
	if a.mode == "" && !hasFlag(a.args, "--permission-mode", "--allowedTools", "--allowed-tools", "--dangerously-skip-permissions") {
		info.Warnings = append(info.Warnings, "claude has no --permission-mode or --allowedTools: every tool call will need your approval")
	}
	if a.mode == "bypassPermissions" || hasFlag(a.args, "--dangerously-skip-permissions") {
		info.Warnings = append(info.Warnings, "claude runs every tool without asking (bypassPermissions)")
	}
	for _, f := range []string{"-p", "--print", "--output-format", "--session-id", "--resume", "--continue", "--mcp-config", "--input-format"} {
		if hasFlag(a.args, f) {
			info.Warnings = append(info.Warnings, fmt.Sprintf("claude args set %s, which ymcp sets itself", f))
		}
	}
	return info, nil
}

func (a *claudeAgent) Start(ctx context.Context, opts StartOptions) (Session, error) {
	if a.onEndpoint() {
		if opts.Endpoint == nil || opts.Endpoint.URL == "" {
			return nil, fmt.Errorf("%s: no model API to work with", a.provider)
		}
		opts.Env = endpointEnv(opts.Endpoint, opts.Env)
	}
	s := &claudeSession{agent: a, opts: opts}
	if opts.SessionID != "" {
		s.id, s.started = opts.SessionID, true
	} else {
		s.id = newUUID()
	}
	if a.host != nil {
		if err := s.remoteFiles(ctx); err != nil {
			return nil, err
		}
	} else if len(opts.MCP) > 0 {
		dir, err := os.MkdirTemp("", "ymcp-claude-")
		if err != nil {
			return nil, fmt.Errorf("claude: mcp config: %w", err)
		}
		s.tmp = dir
		s.mcpConfig = filepath.Join(dir, "mcp.json")
		if err := os.WriteFile(s.mcpConfig, claudeMCPConfig(opts.MCP), 0o600); err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("claude: mcp config: %w", err)
		}
	}
	s.events = newEventQueue()
	return s, nil
}

// claudeMCPConfig is the --mcp-config file for the servers.
func claudeMCPConfig(servers []MCPServer) []byte {
	type server struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	cfg := struct {
		MCPServers map[string]server `json:"mcpServers"`
	}{map[string]server{}}
	for _, m := range servers {
		cfg.MCPServers[m.Name] = server{Type: "http", URL: m.URL, Headers: m.Headers}
	}
	b, _ := json.Marshal(cfg)
	return b
}

type claudeSession struct {
	agent     *claudeAgent
	opts      StartOptions
	tmp       string
	mcpConfig string
	// On a remote host: the directory of the files handed to claude, and
	// whether ymcp made it (rather than StartOptions.RunDir).
	remoteDir string
	remoteTmp bool
	events    *eventQueue
	turn      sync.Mutex // held by a running Prompt

	mu        sync.Mutex
	id        string
	started   bool // the session exists on claude's side: resume it
	group     stopper
	cancelled bool
	closed    bool
}

func (s *claudeSession) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func (s *claudeSession) Events() <-chan Event { return s.events.out }

func (s *claudeSession) Answer(requestID, optionID string) error {
	return fmt.Errorf("claude: no permission request %q: claude -p doesn't ask, denied calls come back in Result.Denied", requestID)
}

func (s *claudeSession) argv(text string) (args []string, resume bool) {
	s.mu.Lock()
	id, resume := s.id, s.started
	s.mu.Unlock()
	if strings.HasPrefix(text, "-") {
		text = " " + text // not a flag
	}
	args = []string{"-p", text, "--output-format", "stream-json", "--verbose"}
	if resume {
		args = append(args, "--resume", id)
	} else {
		args = append(args, "--session-id", id)
	}
	if s.mcpConfig != "" {
		args = append(args, "--mcp-config", s.mcpConfig, "--strict-mcp-config")
	}
	if len(s.opts.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(s.opts.AllowedTools, ","))
	}
	if s.opts.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", s.opts.SystemPrompt)
	}
	if s.opts.Model != "" {
		args = append(args, "--model", s.opts.Model)
	}
	if s.opts.Mode != "" {
		args = append(args, "--permission-mode", s.opts.Mode)
	}
	args = append(args, s.agent.args...)
	return append(args, s.opts.Args...), resume
}

// redactArgs replaces the prompt and the system prompt in a claude argv
// with their length, so the command line can be logged.
func redactArgs(args []string) []string {
	out := slices.Clone(args)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "-p" || out[i] == "--append-system-prompt" {
			out[i+1] = fmt.Sprintf("<%d chars>", len([]rune(out[i+1])))
			i++
		}
	}
	return out
}

func (s *claudeSession) Prompt(ctx context.Context, text string) (Result, error) {
	if !s.turn.TryLock() {
		return Result{}, errors.New("claude: a turn is already running")
	}
	defer s.turn.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Result{}, errors.New("claude: session closed")
	}
	s.cancelled = false
	s.mu.Unlock()
	if ctx.Err() != nil {
		return Result{StopReason: StopCancelled}, nil
	}

	args, resume := s.argv(text)
	s.agent.logger.Debug("starting claude", "command", s.agent.command, "args", redactArgs(args))
	argv := append([]string{s.agent.command}, args...)
	// stdin is /dev/null: claude -p would wait for input otherwise.
	var stderr tail
	proc, err := s.agent.launch(ctx, s.opts.Dir, s.opts.Env, argv, s.promptFiles(argv), false, &stderr, s.opts.OnGroup)
	if err != nil {
		return failed(err.Error()), fmt.Errorf("claude: start: %w", err)
	}
	cmd, stdout, g := proc.cmd, proc.stdout, proc.group
	defer stdout.Close()
	s.mu.Lock()
	s.group = g
	s.mu.Unlock()
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		g.exited()
		g.kill() // whatever claude left running; this also ends the output
		close(exited)
	}()
	stop := context.AfterFunc(ctx, func() { _ = s.Cancel() })
	defer stop()

	p := claudeParser{s: s, tools: map[string]ToolCall{}}
	r := bufio.NewReaderSize(stdout, 64<<10)
	for p.result == nil {
		line, err := r.ReadBytes('\n')
		p.line(line)
		if err != nil {
			break
		}
	}
	select {
	case <-exited:
	case <-time.After(killGrace): // a result but claude doesn't exit
		g.kill()
		<-exited
	}
	g.release()
	stderr.wait(killGrace) // its last lines, read after the exit

	s.mu.Lock()
	s.group = nil
	cancelled := s.cancelled
	if p.sawInit || p.result != nil {
		s.started = true
	}
	s.mu.Unlock()

	if p.result == nil {
		if cancelled {
			return Result{StopReason: StopCancelled, Text: p.text.String()}, nil
		}
		msg := "claude exited without a result"
		if waitErr != nil {
			msg += " (" + waitErr.Error() + ")"
		}
		msg = notFound(withStderr(msg, &stderr), waitErr, s.agent.command)
		if resume && strings.Contains(msg, "No conversation found") {
			return failed(msg), fmt.Errorf("claude: %w: %s", ErrNoResume, msg)
		}
		return failed(msg), errors.New(msg)
	}
	res := p.result.toResult(p.text.String())
	if cancelled && !res.IsError {
		res.StopReason = StopCancelled
	}
	if res.IsError {
		if res.Err == "" {
			res.Err = withStderr("claude reported an error", &stderr)
		}
		s.events.emit(Event{Kind: EventError, Text: res.Err})
		if resume && (strings.Contains(res.Err, "No conversation found") || strings.Contains(stderr.String(), "No conversation found")) {
			return res, fmt.Errorf("claude: %w: %s", ErrNoResume, res.Err)
		}
	}
	return res, nil
}

func failed(msg string) Result { return Result{StopReason: StopError, IsError: true, Err: msg} }

func (s *claudeSession) Cancel() error {
	s.mu.Lock()
	g := s.group
	if g != nil {
		s.cancelled = true
	}
	s.mu.Unlock()
	if g == nil {
		return nil
	}
	g.interrupt()
	time.AfterFunc(killGrace, func() {
		s.mu.Lock()
		running := s.group == g
		s.mu.Unlock()
		if running {
			g.kill()
		}
	})
	return nil
}

func (s *claudeSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	g := s.group
	if g != nil {
		s.cancelled = true
	}
	s.mu.Unlock()
	if g != nil {
		g.kill()
	}
	// Wait for a running Prompt to finish before cleaning up after it.
	s.turn.Lock()
	defer s.turn.Unlock()
	if s.tmp != "" {
		os.RemoveAll(s.tmp)
	}
	s.removeRemoteFiles()
	s.events.close()
	return nil
}

// claudeParser turns stream-json lines into events.
type claudeParser struct {
	s       *claudeSession
	tools   map[string]ToolCall
	text    strings.Builder
	sawInit bool
	result  *claudeResult
}

type claudeLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

type claudeResult struct {
	Subtype           string   `json:"subtype"`
	IsError           bool     `json:"is_error"`
	Result            string   `json:"result"`
	StopReason        *string  `json:"stop_reason"`
	Errors            []string `json:"errors"`
	APIErrorStatus    any      `json:"api_error_status"`
	TotalCostUSD      float64  `json:"total_cost_usd"`
	PermissionDenials []struct {
		ToolName  string          `json:"tool_name"`
		ToolUseID string          `json:"tool_use_id"`
		ToolInput json.RawMessage `json:"tool_input"`
	} `json:"permission_denials"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (p *claudeParser) line(raw []byte) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || raw[0] != '{' {
		return // not stream-json: a banner, a warning, a stray print
	}
	var l claudeLine
	if err := json.Unmarshal(raw, &l); err != nil {
		return
	}
	msg := json.RawMessage(raw)
	switch l.Type {
	case "system":
		if l.Subtype != "init" {
			return
		}
		p.sawInit = true
		if l.SessionID != "" {
			p.s.mu.Lock()
			p.s.id = l.SessionID
			p.s.mu.Unlock()
		}
		p.s.events.emit(Event{Kind: EventSession, SessionID: l.SessionID, Raw: msg})
	case "assistant":
		for _, b := range blocks(l.Message.Content) {
			switch b.Type {
			case "text":
				if b.Text != "" {
					if p.text.Len() > 0 {
						p.text.WriteString("\n\n")
					}
					p.text.WriteString(b.Text)
					p.s.events.emit(Event{Kind: EventText, Text: b.Text, Raw: msg})
				}
			case "thinking":
				if b.Thinking != "" {
					p.s.events.emit(Event{Kind: EventThought, Text: b.Thinking, Raw: msg})
				}
			case "tool_use":
				tc := ToolCall{ID: b.ID, Title: b.Name, Kind: claudeToolKind(b.Name), Status: "in_progress", Input: b.Input}
				p.tools[b.ID] = tc
				p.s.events.emit(Event{Kind: EventToolCall, Tool: &tc, Raw: msg})
			}
		}
	case "user":
		for _, b := range blocks(l.Message.Content) {
			if b.Type != "tool_result" {
				continue
			}
			tc, ok := p.tools[b.ToolUseID]
			if !ok {
				tc = ToolCall{ID: b.ToolUseID}
			}
			tc.Status = "completed"
			if b.IsError {
				tc.Status = "failed"
			}
			p.tools[b.ToolUseID] = tc
			p.s.events.emit(Event{Kind: EventToolUpdate, Tool: &tc, Raw: msg})
		}
	case "result":
		var r claudeResult
		if err := json.Unmarshal(raw, &r); err != nil {
			return
		}
		p.result = &r
		p.s.events.emit(Event{Kind: EventUsage, Raw: msg, Text: fmt.Sprintf("in=%d out=%d cost=$%.4f",
			r.Usage.InputTokens, r.Usage.OutputTokens, r.TotalCostUSD)})
	}
}

// blocks decodes message content, which is a string for plain prompts.
func blocks(raw json.RawMessage) []claudeBlock {
	var bs []claudeBlock
	if json.Unmarshal(raw, &bs) != nil {
		return nil
	}
	return bs
}

func (r *claudeResult) toResult(streamed string) Result {
	res := Result{StopReason: StopEndTurn, Text: r.Result}
	if res.Text == "" {
		res.Text = streamed
	}
	if r.StopReason != nil {
		switch *r.StopReason {
		case "max_tokens":
			res.StopReason = StopMaxTokens
		case "refusal":
			res.StopReason = StopRefusal
		}
	}
	for _, d := range r.PermissionDenials {
		res.Denied = append(res.Denied, ToolCall{ID: d.ToolUseID, Title: d.ToolName, Kind: claudeToolKind(d.ToolName), Status: "denied", Input: d.ToolInput})
	}
	// Failures may come as subtype "success" with is_error set (a failed
	// sign-in, an unknown model), or as an error_* subtype.
	if r.IsError || strings.HasPrefix(r.Subtype, "error") {
		res.IsError, res.StopReason = true, StopError
		var parts []string
		if r.Result != "" {
			parts = append(parts, r.Result)
		}
		parts = append(parts, r.Errors...)
		if r.APIErrorStatus != nil {
			parts = append(parts, fmt.Sprintf("API error %v", r.APIErrorStatus))
		}
		if len(parts) == 0 && r.Subtype != "" {
			parts = append(parts, r.Subtype)
		}
		res.Err = strings.Join(parts, "; ")
	}
	return res
}

// claudeToolKind maps a claude tool name to an ACP tool kind.
func claudeToolKind(name string) string {
	switch name {
	case "Read", "NotebookRead":
		return "read"
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return "edit"
	case "Grep", "Glob", "ToolSearch":
		return "search"
	case "Bash", "BashOutput", "KillShell":
		return "execute"
	case "WebFetch", "WebSearch":
		return "fetch"
	}
	return "other"
}

// newUUID returns a random version 4 UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// hasFlag reports whether args carry any of the flags, as "--flag" or
// "--flag=value".
func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		for _, f := range flags {
			if name == f {
				return true
			}
		}
	}
	return false
}

// checkBinary finds command, here or on the host, and reads `command
// --version`. A command that has no version flag only gets a warning.
func (s settings) checkBinary(ctx context.Context, command string) (Info, error) {
	if s.host != nil {
		return checkRemoteBinary(ctx, s.host, command)
	}
	return checkLocalBinary(ctx, command)
}

func checkLocalBinary(ctx context.Context, command string) (Info, error) {
	path, err := exec.LookPath(command)
	if err != nil {
		return Info{}, fmt.Errorf("agent: %w", err)
	}
	info := Info{Path: path}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.Command(path, "--version")
	cmd.Env = environ(nil)
	var stderr tail
	stdout, g, err := startPiped(cmd, &stderr)
	if err != nil {
		info.Warnings = append(info.Warnings, "could not read the version: "+err.Error())
		return info, nil
	}
	defer g.release()
	stop := context.AfterFunc(ctx, g.kill)
	var out tail
	io.Copy(&out, stdout)
	stdout.Close()
	err = cmd.Wait()
	stop()
	if err != nil {
		info.Warnings = append(info.Warnings, withStderr("could not read the version: "+err.Error(), &stderr))
		return info, nil
	}
	for _, l := range strings.Split(out.String(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			info.Version = l
			break
		}
	}
	return info, nil
}

// Files of a claude session on a remote host, in its remoteDir.
const (
	remoteMCPConfig    = "mcp.json"
	remotePrompt       = "prompt.txt"
	remoteSystemPrompt = "system-prompt.txt"
)

// remoteFiles prepares the directory of the files handed to claude on the
// host and writes the MCP config there.
func (s *claudeSession) remoteFiles(ctx context.Context) error {
	h := s.agent.host
	dir := strings.TrimRight(s.opts.RunDir, "/")
	if dir == "" {
		out, stderr, err := h.Run(ctx, "", []string{"sh", "-c", `mktemp -d "${TMPDIR:-/tmp}/ymcp-claude.XXXXXX"`}, nil)
		dir = strings.TrimSpace(string(out))
		if err != nil || !strings.HasPrefix(dir, "/") {
			return fmt.Errorf("claude: a temporary directory on %s: %v: %s", h.Name(), err, strings.TrimSpace(string(stderr)))
		}
		s.remoteTmp = true
	}
	s.remoteDir = dir
	if len(s.opts.MCP) == 0 {
		return nil
	}
	s.mcpConfig = dir + "/" + remoteMCPConfig
	if err := writeRemote(ctx, h, s.mcpConfig, string(claudeMCPConfig(s.opts.MCP))); err != nil {
		s.removeRemoteFiles()
		return fmt.Errorf("claude: mcp config: %w", err)
	}
	return nil
}

// promptFiles hands the prompt and the system prompt to a claude on a
// remote host in files, so the ssh command line stays short.
func (s *claudeSession) promptFiles(argv []string) map[int]string {
	if s.agent.host == nil {
		return nil
	}
	files := map[int]string{}
	names := map[string]string{"-p": remotePrompt, "--append-system-prompt": remoteSystemPrompt}
	for i := 1; i+1 < len(argv); i++ {
		if name, ok := names[argv[i]]; ok {
			files[i+1] = s.remoteDir + "/" + name
			delete(names, argv[i])
			i++
		}
	}
	return files
}

// removeRemoteFiles deletes what the session wrote on a remote host.
func (s *claudeSession) removeRemoteFiles() {
	h, dir := s.agent.host, s.remoteDir
	if h == nil || !strings.HasPrefix(dir, "/") || len(dir) < 2 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	argv := []string{"rm", "-f", "--", dir + "/" + remoteMCPConfig, dir + "/" + remotePrompt, dir + "/" + remoteSystemPrompt}
	if s.remoteTmp && strings.Contains(dir, "/ymcp-claude.") {
		argv = []string{"rm", "-rf", "--", dir}
	}
	_, _, _ = h.Run(ctx, "", argv, nil)
}
