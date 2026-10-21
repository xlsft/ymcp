// Package tools exposes YouTrack operations as MCP tools.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/youtrack"
)

// Register adds every tool to s, backed by yt.
func Register(s *mcp.Server, yt *youtrack.Client) {
	t := &toolset{yt: yt}
	t.registerIssues(s)
	t.registerIssueEdit(s)
	t.registerProjects(s)
	t.registerUsers(s)
	t.registerTags(s)
	t.registerLinks(s)
	t.registerWorkItems(s)
	t.registerGroups(s)
	t.registerArticles(s)
	t.registerDeletes(s)
	t.registerAgiles(s)
	t.registerSprints(s)
	t.registerActivities(s)
	t.registerReports(s)
}

type toolset struct {
	yt *youtrack.Client

	meMu sync.Mutex
	me   *apiUser
}

// currentUser returns the authenticated user, cached after the first call.
func (t *toolset) currentUser(ctx context.Context) (apiUser, error) {
	t.meMu.Lock()
	defer t.meMu.Unlock()
	if t.me != nil {
		return *t.me, nil
	}
	var u apiUser
	if err := t.yt.Get(ctx, "users/me", url.Values{"fields": {userFields}}, &u); err != nil {
		return apiUser{}, err
	}
	t.me = &u
	return u, nil
}

// handler is the shape every tool implements: decode In, return a JSON payload.
type handler[In any] func(ctx context.Context, in In) (any, error)

// add registers h as a tool whose result is rendered as indented JSON text.
func add[In any](s *mcp.Server, tool *mcp.Tool, h handler[In]) {
	mcp.AddTool(s, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		out, err := h(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})
}

func textResult(v any) *mcp.CallToolResult {
	var text string
	switch v := v.(type) {
	case string:
		text = v
	case json.RawMessage:
		var buf bytes.Buffer
		if json.Indent(&buf, v, "", "  ") == nil {
			text = buf.String()
		} else {
			text = string(v)
		}
	default:
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			text = err.Error()
		} else {
			text = string(b)
		}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true}
}

func mutating(title string, destructive bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: &destructive}
}

// schemaFor infers the input schema of In and lets tweak refine properties
// that struct tags can't express, such as enums and bounds.
func schemaFor[In any](tweak func(props map[string]*jsonschema.Schema)) *jsonschema.Schema {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		panic(err)
	}
	tweak(s.Properties)
	return s
}

func enum(values ...string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
