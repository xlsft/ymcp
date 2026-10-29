package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxRawResponse caps the response text call_api returns.
const maxRawResponse = 100_000

type callAPIIn struct {
	Method string         `json:"method" jsonschema:"HTTP method"`
	Path   string         `json:"path" jsonschema:"Path relative to the API root, e.g. issues/DEMO-1/comments or admin/projects; a query string may be included"`
	Query  map[string]any `json:"query,omitempty" jsonschema:"Query parameters, e.g. {\"fields\":\"idReadable,summary\",\"$top\":10}; arrays repeat the parameter"`
	Body   any            `json:"body,omitempty" jsonschema:"JSON request body for POST"`
	API    string         `json:"api,omitempty" jsonschema:"youtrack (/api/…, default) or hub (/hub/api/rest/…)"`
}

func (t *toolset) registerRawAPI(s *mcp.Server) {
	addGuarded(t, s, &mcp.Tool{
		Name: "call_api",
		Description: "Calls any YouTrack (or Hub) REST endpoint as the current user, for what no other tool covers. " +
			"YouTrack returns only entity IDs unless you pass a fields query, e.g. fields=idReadable,summary,customFields(name,value(name)). " +
			"Prefer GET; POST and DELETE change data. Reference: https://www.jetbrains.com/help/youtrack/devportal/api-entities.html",
		Annotations: anything("Call YouTrack REST API"),
		InputSchema: schemaFor[callAPIIn](func(p map[string]*jsonschema.Schema) {
			p["method"].Enum = enum("GET", "POST", "DELETE")
			p["api"].Enum = enum("youtrack", "hub")
		}),
	}, t.callAPI, t.callAPIGuard)
}

// callAPIGuard asks for confirmation of DELETE calls only.
func (t *toolset) callAPIGuard(_ context.Context, in callAPIIn) (string, bool, error) {
	if !strings.EqualFold(in.Method, "DELETE") {
		return "", false, nil
	}
	api := "YouTrack"
	if strings.EqualFold(in.API, "hub") {
		api = "Hub"
	}
	return "send DELETE " + in.Path + " to the " + api + " REST API", true, nil
}

func (t *toolset) callAPI(ctx context.Context, in callAPIIn) (any, error) {
	method := strings.ToUpper(in.Method)
	if !slices.Contains([]string{"GET", "POST", "DELETE"}, method) {
		return nil, fmt.Errorf("method must be GET, POST or DELETE, got %q", in.Method)
	}
	hub := strings.EqualFold(in.API, "hub")
	path, query, err := apiPath(in.Path, hub)
	if err != nil {
		return nil, err
	}
	for key, v := range in.Query {
		switch v := v.(type) {
		case []any:
			for _, item := range v {
				query.Add(key, fmt.Sprint(item))
			}
		case float64:
			query.Set(key, strconv.FormatFloat(v, 'f', -1, 64))
		default:
			query.Set(key, fmt.Sprint(v))
		}
	}
	if method != "POST" && in.Body != nil {
		return nil, fmt.Errorf("a body only goes with POST")
	}

	var raw json.RawMessage
	if hub {
		err = t.yt.HubDo(ctx, method, path, query, in.Body, &raw)
	} else {
		err = t.yt.Do(ctx, method, path, query, in.Body, &raw)
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return map[string]any{"ok": true, "method": method, "path": path}, nil
	}
	if len(raw) > maxRawResponse {
		return fmt.Sprintf("%s\n… truncated: the response has %d bytes; narrow it with fields, $top or $skip",
			raw[:maxRawResponse], len(raw)), nil
	}
	return raw, nil
}

// apiPath validates a relative API path and splits off its query string.
// Absolute URLs are refused so the token never goes to another host.
func apiPath(p string, hub bool) (string, url.Values, error) {
	p = strings.TrimSpace(p)
	if strings.Contains(p, "://") || strings.HasPrefix(p, "//") {
		return "", nil, fmt.Errorf("path must be relative to the API root, e.g. issues/DEMO-1, not a URL")
	}
	rawQuery := ""
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p, rawQuery = p[:i], p[i+1:]
	}
	p = strings.TrimLeft(p, "/")
	prefixes := []string{"api/"}
	if hub {
		prefixes = []string{"hub/api/rest/", "api/rest/"}
	}
	for _, prefix := range prefixes {
		p = strings.TrimPrefix(p, prefix)
	}
	if p == "" {
		return "", nil, fmt.Errorf("path is empty")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return "", nil, fmt.Errorf("path must not contain . or .. segments")
		}
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", nil, fmt.Errorf("query string: %w", err)
	}
	return p, query, nil
}
