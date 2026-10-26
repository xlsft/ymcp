// Package tools exposes YouTrack operations as MCP tools.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/youtrack"
)

// Register adds the tools to s, backed by yt: those in enabled, or all of
// them when enabled is nil (see Select).
func Register(s *mcp.Server, yt *youtrack.Client, enabled map[string]bool) {
	t := &toolset{yt: yt}
	defer func() {
		if enabled == nil {
			return
		}
		var off []string
		for _, g := range Groups {
			for _, tool := range g.Tools {
				if !enabled[tool] {
					off = append(off, tool)
				}
			}
		}
		s.RemoveTools(off...)
	}()
	t.registerIssues(s)
	t.registerIssueEdit(s)
	t.registerProjects(s)
	t.registerUsers(s)
	t.registerTags(s)
	t.registerLinks(s)
	t.registerWorkItems(s)
	t.registerGroups(s)
	t.registerArticles(s)
	t.registerTextEdit(s)
	t.registerDeletes(s)
	t.registerAgiles(s)
	t.registerSprints(s)
	t.registerActivities(s)
	t.registerReports(s)
	t.registerCommands(s)
	t.registerAttachments(s)
	t.registerVcs(s)
	t.registerTagAdmin(s)
	t.registerRawAPI(s)
	t.registerPrompts(s, enabled)
}

type toolset struct {
	yt *youtrack.Client

	meMu sync.Mutex
	me   *apiUser

	fieldsMu sync.Mutex
	fields   map[string]cachedFields // project ID → field schema
	now      func() time.Time        // for tests; nil means time.Now
}

func (t *toolset) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
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
			return nil, nil, withHint(err)
		}
		return textResult(out), nil, nil
	})
}

func textResult(v any) *mcp.CallToolResult {
	var text string
	switch v := v.(type) {
	case *mcp.CallToolResult:
		return v
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

// readOnly marks a tool that only reads.
func readOnly(title string) *mcp.ToolAnnotations {
	return hints(title, true, false, true)
}

// changes marks a tool whose every call changes data again, such as creating
// an issue or appending to an article.
func changes(title string) *mcp.ToolAnnotations {
	return hints(title, false, false, false)
}

// sets marks a tool that sets data to the given values, so repeating the call
// changes nothing. YouTrack keeps the history, so it is not destructive.
func sets(title string) *mcp.ToolAnnotations {
	return hints(title, false, false, true)
}

// removes marks a tool that deletes or unlinks; repeating the call changes nothing.
func removes(title string) *mcp.ToolAnnotations {
	return hints(title, false, true, true)
}

// anything marks a tool that can make any change, deletes included.
func anything(title string) *mcp.ToolAnnotations {
	return hints(title, false, true, false)
}

// hints sets all four hints explicitly, since some hosts reject a tool that
// leaves any out. No tool is open-world: each talks only to the configured
// YouTrack instance.
func hints(title string, readOnly, destructive, idempotent bool) *mcp.ToolAnnotations {
	openWorld := false
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    readOnly,
		DestructiveHint: &destructive,
		IdempotentHint:  idempotent,
		OpenWorldHint:   &openWorld,
	}
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
