package endpoint

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// The Messages API, as far as the gateway reads it.

type msgRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []message       `json:"messages"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []tool          `json:"tools,omitempty"`
	ToolChoice    *toolChoice     `json:"tool_choice,omitempty"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// block is a content block: text, image, tool_use, tool_result, or one
// the gateway drops (thinking, documents, …).
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *imageSource    `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"` // base64 or url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type tool struct {
	Type        string          `json:"type,omitempty"` // empty or "custom"; others are server tools
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type toolChoice struct {
	Type string `json:"type"` // auto, any, tool, none
	Name string `json:"name,omitempty"`
}

// blocks reads content, a string or an array of blocks.
func blocks(content json.RawMessage) ([]block, error) {
	content = json.RawMessage(strings.TrimSpace(string(content)))
	if len(content) == 0 || string(content) == "null" {
		return nil, nil
	}
	if content[0] == '"' {
		var s string
		if err := json.Unmarshal(content, &s); err != nil {
			return nil, err
		}
		return []block{{Type: "text", Text: s}}, nil
	}
	var out []block
	err := json.Unmarshal(content, &out)
	return out, err
}

// The Chat Completions API.

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	Tools         []chatTool     `json:"tools,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"` // string, []chatPart or nil
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatPart struct {
	Type     string    `json:"type"` // text or image_url
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatTool struct {
	Type     string       `json:"type"` // function
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type chatToolCall struct {
	Index    *int         `json:"index,omitempty"` // in stream deltas
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function chatCallFunc `json:"function"`
}

type chatCallFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
	Error   *chatError   `json:"error"`
}

type chatChoice struct {
	Message      chatOut `json:"message"`
	Delta        chatOut `json:"delta"`
	FinishReason string  `json:"finish_reason"`
}

type chatOut struct {
	Content   json.RawMessage `json:"content"`
	ToolCalls []chatToolCall  `json:"tool_calls"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type chatError struct {
	Message string `json:"message"`
	Code    any    `json:"code"`
}

func (e chatError) text() string {
	if e.Code != nil && e.Code != "" {
		return fmt.Sprintf("%s (%v)", e.Message, e.Code)
	}
	return e.Message
}

// text reads content, a string or an array of text parts.
func (o chatOut) text() string {
	c := strings.TrimSpace(string(o.Content))
	if c == "" || c == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(o.Content, &s) == nil {
		return s
	}
	var parts []chatPart
	json.Unmarshal(o.Content, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// toChat translates a Messages request. Thinking blocks are dropped, as
// are server tools such as web search, which the upstream doesn't run.
func toChat(req msgRequest) (chatRequest, error) {
	out := chatRequest{
		Model: req.Model, MaxTokens: req.MaxTokens, Temperature: req.Temperature, TopP: req.TopP,
		Stop: req.StopSequences, Stream: req.Stream,
	}
	if req.Stream {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	sys, err := blocks(req.System)
	if err != nil {
		return out, fmt.Errorf("system: %w", err)
	}
	var texts []string
	for _, b := range sys {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	if len(texts) > 0 {
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: strings.Join(texts, "\n\n")})
	}
	for i, m := range req.Messages {
		bs, err := blocks(m.Content)
		if err != nil {
			return out, fmt.Errorf("messages[%d]: %w", i, err)
		}
		switch m.Role {
		case "user":
			out.Messages = append(out.Messages, userMessages(bs)...)
		case "assistant":
			if am, ok := assistantMessage(bs); ok {
				out.Messages = append(out.Messages, am)
			}
		case "system":
			// Claude Code puts instructions between the turns too.
			var texts []string
			for _, b := range bs {
				if b.Type == "text" && b.Text != "" {
					texts = append(texts, b.Text)
				}
			}
			if len(texts) > 0 {
				out.Messages = append(out.Messages, chatMessage{Role: "system", Content: strings.Join(texts, "\n\n")})
			}
		default:
			return out, fmt.Errorf("messages[%d]: unknown role %q", i, m.Role)
		}
	}
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "custom" {
			continue
		}
		params := t.InputSchema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, chatTool{Type: "function", Function: chatFunction{Name: t.Name, Description: t.Description, Parameters: params}})
	}
	if tc := req.ToolChoice; tc != nil && len(out.Tools) > 0 {
		switch tc.Type {
		case "auto":
			out.ToolChoice = "auto"
		case "any":
			out.ToolChoice = "required"
		case "none":
			out.ToolChoice = "none"
		case "tool":
			out.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
		}
	}
	return out, nil
}

// userMessages are the tool results of a user turn, one tool message
// each, then the rest of it. Images in tool results go to the user
// message: tool messages carry only text.
func userMessages(bs []block) []chatMessage {
	var out []chatMessage
	var parts []chatPart
	for _, b := range bs {
		switch b.Type {
		case "tool_result":
			inner, _ := blocks(b.Content)
			var texts []string
			for _, ib := range inner {
				switch ib.Type {
				case "text":
					texts = append(texts, ib.Text)
				case "image":
					if p, ok := imagePart(ib); ok {
						parts = append(parts, p)
					}
				}
			}
			text := strings.Join(texts, "\n")
			if b.IsError && !strings.HasPrefix(text, "Error") {
				text = "Error: " + text
			}
			if text == "" {
				text = "(no output)"
			}
			out = append(out, chatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: text})
		case "text":
			if b.Text != "" {
				parts = append(parts, chatPart{Type: "text", Text: b.Text})
			}
		case "image":
			if p, ok := imagePart(b); ok {
				parts = append(parts, p)
			}
		}
	}
	if len(parts) > 0 {
		out = append(out, chatMessage{Role: "user", Content: partsContent(parts)})
	}
	return out
}

// partsContent is a plain string when every part is text: not every
// service takes an array.
func partsContent(parts []chatPart) any {
	var texts []string
	for _, p := range parts {
		if p.Type != "text" {
			return parts
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n\n")
}

func imagePart(b block) (chatPart, bool) {
	s := b.Source
	switch {
	case s == nil:
		return chatPart{}, false
	case s.Type == "base64" && s.Data != "":
		return chatPart{Type: "image_url", ImageURL: &imageURL{URL: "data:" + s.MediaType + ";base64," + s.Data}}, true
	case s.Type == "url" && s.URL != "":
		return chatPart{Type: "image_url", ImageURL: &imageURL{URL: s.URL}}, true
	}
	return chatPart{}, false
}

// assistantMessage joins the text of an assistant turn and turns its
// tool uses into tool calls; false when nothing is left.
func assistantMessage(bs []block) (chatMessage, bool) {
	m := chatMessage{Role: "assistant"}
	var texts []string
	for _, b := range bs {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "tool_use":
			args := "{}"
			var c bytes.Buffer
			if json.Compact(&c, b.Input) == nil && c.String() != "null" {
				args = c.String()
			}
			m.ToolCalls = append(m.ToolCalls, chatToolCall{ID: b.ID, Type: "function", Function: chatCallFunc{Name: b.Name, Arguments: args}})
		}
	}
	if len(texts) > 0 {
		m.Content = strings.Join(texts, "\n\n")
	}
	return m, len(texts) > 0 || len(m.ToolCalls) > 0
}

// The answer, back in the shape of the Messages API.

type msgResponse struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	Role         string     `json:"role"`
	Model        string     `json:"model"`
	Content      []outBlock `json:"content"`
	StopReason   *string    `json:"stop_reason"`
	StopSequence *string    `json:"stop_sequence"`
	Usage        msgUsage   `json:"usage"`
}

type outBlock struct {
	Type  string          `json:"type"`
	Text  *string         `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type msgUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func textBlock(s string) outBlock { return outBlock{Type: "text", Text: &s} }

// toolBlock is a tool_use block; arguments that aren't a JSON object
// become an empty input.
func toolBlock(id, name, args string) outBlock {
	if id == "" {
		id = newID("toolu_")
	}
	input := json.RawMessage(strings.TrimSpace(args))
	var obj map[string]json.RawMessage
	if json.Unmarshal(input, &obj) != nil || obj == nil {
		input = json.RawMessage("{}")
	}
	return outBlock{Type: "tool_use", ID: id, Name: name, Input: input}
}

// stopReason maps a finish reason; tool calls always mean tool_use, as
// some services finish them with "stop".
func stopReason(finish string, tools bool) string {
	switch {
	case tools || finish == "tool_calls" || finish == "function_call":
		return "tool_use"
	case finish == "length":
		return "max_tokens"
	case finish == "content_filter":
		return "refusal"
	}
	return "end_turn"
}

func fromChat(cr chatResponse, model string) msgResponse {
	out := msgResponse{ID: "msg_" + cr.ID, Type: "message", Role: "assistant", Model: model, Content: []outBlock{}}
	if cr.ID == "" {
		out.ID = newID("msg_")
	}
	finish := ""
	if len(cr.Choices) > 0 {
		c := cr.Choices[0]
		finish = c.FinishReason
		if t := c.Message.text(); t != "" {
			out.Content = append(out.Content, textBlock(t))
		}
		for _, tc := range c.Message.ToolCalls {
			out.Content = append(out.Content, toolBlock(tc.ID, tc.Function.Name, tc.Function.Arguments))
		}
	}
	reason := stopReason(finish, len(out.Content) > 0 && out.Content[len(out.Content)-1].Type == "tool_use")
	out.StopReason = &reason
	if cr.Usage != nil {
		out.Usage = msgUsage{InputTokens: cr.Usage.PromptTokens, OutputTokens: cr.Usage.CompletionTokens}
	}
	return out
}

func newID(prefix string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// sse writes the events of a Messages stream.
type sse struct {
	w   io.Writer
	f   http.Flusher
	err error
}

func (s *sse) event(name string, data any) {
	if s.err != nil {
		return
	}
	b, _ := json.Marshal(data)
	_, s.err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b)
	if s.f != nil {
		s.f.Flush()
	}
}

// pendingCall is a streamed tool call: its arguments come in pieces.
type pendingCall struct {
	id, name string
	args     strings.Builder
}

// streamMessages turns a Chat Completions stream into a Messages one.
// Text streams as it comes; tool calls are collected and sent whole at the
// end, as services interleave and number their pieces differently.
func streamMessages(w http.ResponseWriter, body io.Reader, model string, logger *slog.Logger) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	s := &sse{w: w, f: f}
	s.event("message_start", map[string]any{"type": "message_start", "message": msgResponse{
		ID: newID("msg_"), Type: "message", Role: "assistant", Model: model, Content: []outBlock{},
	}})

	var (
		index    int
		textOpen bool
		calls    []*pendingCall
		byIndex  = map[int]*pendingCall{}
		finish   string
		usage    msgUsage
	)
	closeText := func() {
		if textOpen {
			s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
			index++
			textOpen = false
		}
	}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() && s.err == nil {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var chunk chatResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			logger.Debug("skipped a stream chunk that is not JSON", "chunk", clip(data, 200))
			continue
		}
		if chunk.Error != nil {
			closeText()
			s.event("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "model API: " + chunk.Error.text()}})
			return
		}
		if chunk.Usage != nil {
			usage = msgUsage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		c := chunk.Choices[0]
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
		if t := c.Delta.text(); t != "" {
			if !textOpen {
				s.event("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": textBlock("")})
				textOpen = true
			}
			s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]string{"type": "text_delta", "text": t}})
		}
		for i, tc := range c.Delta.ToolCalls {
			key := i
			if tc.Index != nil {
				key = *tc.Index
			}
			pc := byIndex[key]
			if pc == nil || (tc.ID != "" && pc.id != "" && tc.ID != pc.id) {
				pc = &pendingCall{}
				byIndex[key] = pc
				calls = append(calls, pc)
			}
			if tc.ID != "" {
				pc.id = tc.ID
			}
			if tc.Function.Name != "" {
				pc.name = tc.Function.Name
			}
			pc.args.WriteString(tc.Function.Arguments)
		}
	}
	if err := sc.Err(); err != nil && s.err == nil && !errors.Is(err, io.EOF) {
		logger.Warn("model API stream broke", "err", err)
		closeText()
		s.event("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "the model API's stream broke: " + err.Error()}})
		return
	}
	closeText()
	for _, pc := range calls {
		if pc.name == "" {
			continue
		}
		b := toolBlock(pc.id, pc.name, pc.args.String())
		s.event("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": outBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: json.RawMessage("{}")}})
		s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]string{"type": "input_json_delta", "partial_json": string(b.Input)}})
		s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	tools := false
	for _, pc := range calls {
		tools = tools || pc.name != ""
	}
	s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason(finish, tools), "stop_sequence": nil},
		"usage": usage,
	})
	s.event("message_stop", map[string]any{"type": "message_stop"})
}
