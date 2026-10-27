package tools

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/youtrack"
	"github.com/xlsft/ymcp/internal/youtrack/youtracktest"
)

// connectWith is connect with RegisterWith and opts.
func connectWith(t *testing.T, api http.Handler, opts Options) *mcp.ClientSession {
	t.Helper()
	f := youtracktest.New(t, api)
	yt, err := youtrack.New(youtrack.Options{BaseURL: f.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	RegisterWith(server, yt, nil, opts)

	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestMarkWrittenText(t *testing.T) {
	mark := func(s string) string { return "[agent] " + s }
	for _, tc := range []struct {
		tool   string
		args   map[string]any
		path   string // the POST whose body carries the text
		field  string
		plain  string // the text as given
		marked bool   // whether Mark applies
	}{
		{tool: "add_issue_comment", args: map[string]any{"issueId": "DEMO-7", "text": "hi"},
			path: "/api/issues/DEMO-7/comments", field: "text", plain: "hi", marked: true},
		{tool: "update_issue_comment", args: map[string]any{"issueId": "DEMO-7", "commentId": "4-1", "text": "fixed"},
			path: "/api/issues/DEMO-7/comments/4-1", field: "text", plain: "fixed", marked: true},
		{tool: "apply_command", args: map[string]any{"command": "State Fixed", "issueIds": []string{"DEMO-7"}, "comment": "done"},
			path: "/api/commands", field: "comment", plain: "done", marked: true},
		{tool: "create_issue", args: map[string]any{"project": "DEMO", "summary": "Bug", "description": "Steps"},
			path: "/api/issues", field: "description", plain: "Steps", marked: true},
		{tool: "update_issue", args: map[string]any{"issueId": "DEMO-7", "description": "New"},
			path: "/api/issues/DEMO-7", field: "description", plain: "New", marked: true},
		{tool: "update_issue", args: map[string]any{"issueId": "3-22", "description": "Draft text"},
			path: "/api/users/me/drafts/3-22", field: "description", plain: "Draft text", marked: true},
		{tool: "create_article", args: map[string]any{"project": "DEMO", "summary": "How to", "content": "# Steps"},
			path: "/api/articles", field: "content", plain: "# Steps", marked: true},
		{tool: "update_article", args: map[string]any{"articleId": "HELP-A-1", "content": "all new"},
			path: "/api/articles/HELP-A-1", field: "content", plain: "all new", marked: true},
		// Appends and find-and-replace edits keep the text as it is.
		{tool: "update_article", args: map[string]any{"articleId": "HELP-A-1", "content": "line two", "append": true},
			path: "/api/articles/HELP-A-1", field: "content", plain: "line one\n\nline two"},
		{tool: "edit_issue_description", args: map[string]any{"issueId": "DEMO-7", "edits": []map[string]any{{"oldText": "old", "newText": "new"}}},
			path: "/api/issues/DEMO-7", field: "description", plain: "the new text"},
		{tool: "edit_article_content", args: map[string]any{"articleId": "HELP-A-1", "edits": []map[string]any{{"oldText": "one", "newText": "1"}}},
			path: "/api/articles/HELP-A-1", field: "content", plain: "line 1\n"},
		// Summaries are not marked; drafts start without a description.
		{tool: "create_draft_issue", args: map[string]any{"project": "DEMO", "summary": "idea"},
			path: "/api/users/me/drafts", field: "summary", plain: "idea"},
	} {
		for _, withMark := range []bool{false, true} {
			name := tc.tool + "/plain"
			opts := Options{}
			if withMark {
				name = tc.tool + "/marked"
				opts.Mark = mark
			}
			t.Run(name, func(t *testing.T) {
				api := markAPI(t)
				cs := connectWith(t, api, opts)
				if isErr, text := call(t, cs, tc.tool, tc.args, nil); isErr {
					t.Fatal(text)
				}
				bodies := api.bodies(tc.path)
				if len(bodies) != 1 {
					t.Fatalf("POST %s: %d bodies, want 1: %v", tc.path, len(bodies), bodies)
				}
				want := tc.plain
				if withMark && tc.marked {
					want = mark(tc.plain)
				}
				if got := bodies[0][tc.field]; got != want {
					t.Errorf("%s = %q, want %q", tc.field, got, want)
				}
			})
		}
	}
}

// markAPI answers every write TestMarkWrittenText makes.
func markAPI(t *testing.T) *fakeAPI {
	api := newFakeAPI(t)
	api.json("POST /api/issues", `{"idReadable":"DEMO-8"}`)
	api.json("POST /api/issues/DEMO-7", `{"id":"2-7"}`)
	api.json("GET /api/issues/DEMO-7", `{"description":"the old text"}`)
	api.json("POST /api/issues/DEMO-7/comments", `{"id":"4-9","text":"x"}`)
	api.json("POST /api/issues/DEMO-7/comments/4-1", `{"id":"4-1","text":"x"}`)
	api.json("POST /api/users/me/drafts", `{"id":"3-22"}`)
	api.json("POST /api/users/me/drafts/3-22", `{"id":"3-22"}`)
	api.json("POST /api/commands/assist", `{"commands":[{"description":"State: Fixed"}],"suggestions":[]}`)
	api.json("POST /api/commands", `{}`)
	api.json("POST /api/articles", `{"idReadable":"HELP-A-3"}`)
	api.json("POST /api/articles/HELP-A-1", `{"idReadable":"HELP-A-1"}`)
	api.handle("GET /api/articles/HELP-A-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"137-1","content":"line one\n"}`)
	})
	return api
}
