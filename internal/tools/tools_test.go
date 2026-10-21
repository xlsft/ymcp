package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/youtrack-mcp/internal/youtrack"
	"github.com/xlsft/youtrack-mcp/internal/youtrack/youtracktest"
)

// fakeAPI serves canned YouTrack REST responses and records writes.
type fakeAPI struct {
	t   *testing.T
	mux *http.ServeMux

	mu     sync.Mutex
	posted map[string][]map[string]any // path -> bodies
}

func newFakeAPI(t *testing.T) *fakeAPI {
	a := &fakeAPI{t: t, mux: http.NewServeMux(), posted: map[string][]map[string]any{}}
	a.json("GET /api/admin/projects", `[{"id":"0-1","shortName":"DEMO","name":"Demo project"},{"id":"0-2","shortName":"OPS","name":"Operations"}]`)
	a.json("GET /api/admin/projects/0-1/customFields", projectFieldsJSON)
	return a
}

func (a *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		if len(data) > 0 {
			if err := json.Unmarshal(data, &body); err != nil {
				a.t.Errorf("POST %s: bad JSON: %v", r.URL.Path, err)
			}
		}
		a.mu.Lock()
		a.posted[r.URL.Path] = append(a.posted[r.URL.Path], body)
		a.mu.Unlock()
	}
	a.mux.ServeHTTP(w, r)
}

func (a *fakeAPI) handle(pattern string, h http.HandlerFunc) { a.mux.HandleFunc(pattern, h) }

func (a *fakeAPI) json(pattern, body string) {
	a.handle(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	})
}

func (a *fakeAPI) bodies(path string) []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.posted[path]
}

func connect(t *testing.T, api http.Handler) *mcp.ClientSession {
	t.Helper()
	f := youtracktest.New(t, api)
	yt, err := youtrack.New(youtrack.Options{BaseURL: f.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	Register(server, yt)

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

// call invokes a tool and decodes its JSON text result into out.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) (isError bool, text string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text = res.Content[0].(*mcp.TextContent).Text
	if !res.IsError && out != nil {
		if err := json.Unmarshal([]byte(text), out); err != nil {
			t.Fatalf("%s: decode %q: %v", name, text, err)
		}
	}
	return res.IsError, text
}

func TestCreateIssueSetsFieldsAndParent(t *testing.T) {
	api := newFakeAPI(t)
	api.json("POST /api/issues", `{"idReadable":"DEMO-7"}`)
	api.json("POST /api/commands", `{}`)
	cs := connect(t, api)

	var out struct{ ID, URL, ParentIssue string }
	if isErr, text := call(t, cs, "create_issue", map[string]any{
		"project":      "demo",
		"summary":      "Broken login",
		"description":  "Steps...",
		"customFields": map[string]any{"State": "Open", "Assignee": "jdoe"},
		"parentIssue":  "DEMO-1",
	}, &out); isErr {
		t.Fatal(text)
	}
	if out.ID != "DEMO-7" || !strings.HasSuffix(out.URL, "/issue/DEMO-7") || out.ParentIssue != "DEMO-1" {
		t.Errorf("out = %+v", out)
	}

	issues := api.bodies("/api/issues")
	if len(issues) != 1 {
		t.Fatalf("issue POSTs = %d", len(issues))
	}
	got, _ := json.Marshal(issues[0])
	want := `{"customFields":[{"$type":"SingleUserIssueCustomField","name":"Assignee","value":{"login":"jdoe"}},` +
		`{"$type":"StateIssueCustomField","name":"State","value":{"name":"Open"}}],` +
		`"description":"Steps...","project":{"id":"0-1"},"summary":"Broken login"}`
	if string(got) != want {
		t.Errorf("issue body =\n%s\nwant\n%s", got, want)
	}

	cmds := api.bodies("/api/commands")
	if len(cmds) != 1 {
		t.Fatalf("command POSTs = %d", len(cmds))
	}
	got, _ = json.Marshal(cmds[0])
	if string(got) != `{"issues":[{"idReadable":"DEMO-7"}],"query":"subtask of DEMO-1"}` {
		t.Errorf("command body = %s", got)
	}
}

func TestCreateIssueRejectsUnknownField(t *testing.T) {
	cs := connect(t, newFakeAPI(t))
	isErr, text := call(t, cs, "create_issue", map[string]any{
		"project": "DEMO", "summary": "x", "customFields": map[string]any{"Severity": "High"},
	}, nil)
	if !isErr || !strings.Contains(text, `unknown custom field "Severity"`) {
		t.Errorf("isErr=%v text=%q", isErr, text)
	}
}

func TestSearchIssuesDefaults(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("GET /api/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("query") != "#Unresolved" || q.Get("$top") != "3" || q.Get("$skip") != "0" {
			t.Errorf("query = %v", q)
		}
		if got := strings.Join(q["customFields"], ","); got != "Type,State,Assignee" {
			t.Errorf("customFields = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[
			{"idReadable":"DEMO-1","summary":"a","created":1767225600000,"project":{"shortName":"DEMO"},"reporter":{"login":"bob"},
			 "customFields":[{"name":"State","$type":"StateIssueCustomField","value":{"name":"Open"}}]},
			{"idReadable":"DEMO-2","summary":"b","project":{"shortName":"DEMO"},"customFields":[]},
			{"idReadable":"DEMO-3","summary":"c","project":{"shortName":"DEMO"},"customFields":[]}
		]`)
	})
	cs := connect(t, api)

	var out struct {
		Issues []struct {
			ID           string
			Created      string
			Reporter     string
			CustomFields map[string]any
		}
		HasMore bool
	}
	if isErr, text := call(t, cs, "search_issues", map[string]any{"query": "#Unresolved", "limit": 2}, &out); isErr {
		t.Fatal(text)
	}
	if len(out.Issues) != 2 || !out.HasMore {
		t.Fatalf("out = %+v", out)
	}
	first := out.Issues[0]
	if first.ID != "DEMO-1" || first.Created != "2026-01-01T00:00:00Z" || first.Reporter != "bob" || first.CustomFields["State"] != "Open" {
		t.Errorf("first = %+v", first)
	}
}

func TestGetIssueFetchesRecentComments(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/issues/DEMO-7", `{"idReadable":"DEMO-7","summary":"s","commentsCount":7,"project":{"id":"0-1","shortName":"DEMO"},
		"links":[{"direction":"INWARD","linkType":{"name":"Subtask","sourceToTarget":"parent for","targetToSource":"subtask of"},"issues":[{"idReadable":"DEMO-1"}]},
		         {"direction":"OUTWARD","linkType":{"name":"Relates","sourceToTarget":"relates to"},"issues":[]}],
		"customFields":[]}`)
	api.handle("GET /api/issues/DEMO-7/comments", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("$skip") != "2" || q.Get("$top") != "5" {
			t.Errorf("comments query = %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"id":"4-1","text":"hi","author":{"login":"bob"}}]`)
	})
	cs := connect(t, api)

	var out struct {
		Links          map[string][]string
		RecentComments []struct{ Text string }
	}
	if isErr, text := call(t, cs, "get_issue", map[string]any{"issueId": "DEMO-7"}, &out); isErr {
		t.Fatal(text)
	}
	if len(out.RecentComments) != 1 || out.RecentComments[0].Text != "hi" {
		t.Errorf("comments = %+v", out.RecentComments)
	}
	if got := out.Links["subtask of"]; len(got) != 1 || got[0] != "DEMO-1" || len(out.Links) != 1 {
		t.Errorf("links = %v", out.Links)
	}
}

func TestFindProjects(t *testing.T) {
	cs := connect(t, newFakeAPI(t))
	var out struct {
		Projects []struct{ ShortName string }
		Total    int
	}
	if isErr, text := call(t, cs, "find_projects", map[string]any{"nameContains": "OPERA"}, &out); isErr {
		t.Fatal(text)
	}
	if out.Total != 1 || out.Projects[0].ShortName != "OPS" {
		t.Errorf("out = %+v", out)
	}
}

func TestUpdateIssueStarsAndSetsFields(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/issues/DEMO-7", `{"project":{"id":"0-1","shortName":"DEMO"}}`)
	api.json("POST /api/issues/DEMO-7", `{"idReadable":"DEMO-7"}`)
	api.json("POST /api/commands", `{}`)
	cs := connect(t, api)

	var out struct{ Changed []string }
	if isErr, text := call(t, cs, "update_issue", map[string]any{
		"issueId": "DEMO-7", "summary": "new", "customFields": map[string]any{"Priority": "Critical"}, "subscription": true,
	}, &out); isErr {
		t.Fatal(text)
	}
	if strings.Join(out.Changed, ",") != "summary,Priority,starred" {
		t.Errorf("changed = %v", out.Changed)
	}
	if cmds := api.bodies("/api/commands"); len(cmds) != 1 || cmds[0]["query"] != "star" {
		t.Errorf("commands = %v", cmds)
	}
}

func TestManageIssueTags(t *testing.T) {
	api := newFakeAPI(t)
	var mu sync.Mutex
	onIssue := []apiTag{{ID: "6-1", Name: "Star"}}
	api.handle("GET /api/issues/DEMO-7", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"tags": onIssue})
	})
	api.json("GET /api/tags", `[{"id":"6-1","name":"Star"},{"id":"6-2","name":"backend"},{"id":"6-3","name":"backend-legacy"}]`)
	api.handle("POST /api/issues/DEMO-7/tags", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		onIssue = append(onIssue, apiTag{ID: "6-2", Name: "backend"})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"6-2"}`)
	})
	api.handle("DELETE /api/issues/DEMO-7/tags/6-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		onIssue = onIssue[1:]
		mu.Unlock()
	})
	cs := connect(t, api)

	var out struct{ Tags []string }
	if isErr, text := call(t, cs, "manage_issue_tags", map[string]any{"issueId": "DEMO-7", "tag": "Backend", "operation": "add"}, &out); isErr {
		t.Fatal(text)
	}
	if strings.Join(out.Tags, ",") != "Star,backend" {
		t.Errorf("after add: %v", out.Tags)
	}
	if body := api.bodies("/api/issues/DEMO-7/tags"); len(body) != 1 || body[0]["id"] != "6-2" {
		t.Errorf("add body = %v", body)
	}

	if isErr, text := call(t, cs, "manage_issue_tags", map[string]any{"issueId": "DEMO-7", "tag": "6-1", "operation": "remove"}, &out); isErr {
		t.Fatal(text)
	}
	if strings.Join(out.Tags, ",") != "backend" {
		t.Errorf("after remove: %v", out.Tags)
	}

	if isErr, text := call(t, cs, "manage_issue_tags", map[string]any{"issueId": "DEMO-7", "tag": "back", "operation": "add"}, nil); !isErr || !strings.Contains(text, "backend-legacy (6-3)") {
		t.Errorf("similar tags: isErr=%v %q", isErr, text)
	}
	isErr, text := call(t, cs, "manage_issue_tags", map[string]any{"issueId": "DEMO-7", "tag": "x", "operation": "toggle"}, nil)
	if !isErr || strings.Contains(text, "operation must be") {
		t.Errorf("operation outside enum should be rejected by the schema, got isErr=%v %q", isErr, text)
	}
}
