package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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
	a.json("GET /api/users/me", `{"id":"1-1","login":"alice","fullName":"Alice"}`)
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
	return connectHub(t, api, nil)
}

func connectHub(t *testing.T, api, hub http.Handler) *mcp.ClientSession {
	t.Helper()
	f := youtracktest.New(t, api)
	f.HubAPI = hub
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
	api.json("POST /api/issues/DEMO-7/watchers/issueWatchers", `{"id":"1-1"}`)
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
	got, _ := json.Marshal(api.bodies("/api/issues/DEMO-7/watchers/issueWatchers"))
	if string(got) != `[{"isStarred":true,"user":{"id":"1-1"}}]` {
		t.Errorf("watcher bodies = %s", got)
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

func TestLinkIssues(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/issueLinkTypes", `[
		{"name":"Relates","sourceToTarget":"relates to","targetToSource":"","directed":false},
		{"name":"Subtask","sourceToTarget":"parent for","targetToSource":"subtask of","directed":true}]`)
	api.json("POST /api/commands", `{}`)
	api.json("GET /api/issues/DEMO-1/links", `[
		{"direction":"INWARD","linkType":{"name":"Subtask","sourceToTarget":"parent for","targetToSource":"subtask of"},"issues":[{"idReadable":"DEMO-2"}]},
		{"direction":"BOTH","linkType":{"name":"Relates","sourceToTarget":"relates to"},"issues":[{"idReadable":"DEMO-3"},{"idReadable":"DEMO-4"}]}]`)
	cs := connect(t, api)

	var out struct {
		LinkedIssueCounts map[string]int
	}
	if isErr, text := call(t, cs, "link_issues", map[string]any{"targetIssueId": "DEMO-1", "linkType": "Subtask Of", "issueToLinkId": "DEMO-2"}, &out); isErr {
		t.Fatal(text)
	}
	if out.LinkedIssueCounts["subtask of"] != 1 || out.LinkedIssueCounts["relates to"] != 2 {
		t.Errorf("counts = %v", out.LinkedIssueCounts)
	}
	cmds := api.bodies("/api/commands")
	if len(cmds) != 1 || cmds[0]["query"] != "subtask of DEMO-2" {
		t.Errorf("commands = %v", cmds)
	}

	isErr, text := call(t, cs, "link_issues", map[string]any{"targetIssueId": "DEMO-1", "linkType": "blocks", "issueToLinkId": "DEMO-2"}, nil)
	if !isErr || !strings.Contains(text, "available: parent for, relates to, subtask of") {
		t.Errorf("isErr=%v %q", isErr, text)
	}
}

func TestLogWork(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/issues/DEMO-7", `{"project":{"id":"0-1","shortName":"DEMO"}}`)
	api.json("GET /api/admin/projects/0-1/timeTrackingSettings", `{
		"workItemTypes":[{"id":"w-1","name":"Development"},{"id":"w-2","name":"Testing"}],
		"attributes":[{"id":"a-1","name":"Billable","values":[{"id":"v-1","name":"Yes"},{"id":"v-2","name":"No"}]}]}`)
	api.json("POST /api/issues/DEMO-7/timeTracking/workItems", `{"id":"8-1","date":1767225600000,"text":"fix",
		"duration":{"minutes":90,"presentation":"1h 30m"},"type":{"name":"Testing"},"author":{"login":"alice"},
		"attributes":[{"name":"Billable","value":{"name":"No"}}]}`)
	cs := connect(t, api)

	var out struct {
		Duration, WorkType, Author string
		Attributes                 map[string]any
	}
	if isErr, text := call(t, cs, "log_work", map[string]any{
		"issueId": "DEMO-7", "durationMinutes": 90, "description": "fix", "date": "2026-01-01",
		"workType": "testing", "workItemAttributes": map[string]any{"billable": "no"},
	}, &out); isErr {
		t.Fatal(text)
	}
	if out.Duration != "1h 30m" || out.WorkType != "Testing" || out.Attributes["Billable"] != "No" {
		t.Errorf("out = %+v", out)
	}

	bodies := api.bodies("/api/issues/DEMO-7/timeTracking/workItems")
	if len(bodies) != 1 {
		t.Fatalf("POSTs = %d", len(bodies))
	}
	b := bodies[0]
	want := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	if b["date"] != float64(want) || b["text"] != "fix" {
		t.Errorf("body = %v", b)
	}
	got, _ := json.Marshal([]any{b["duration"], b["type"], b["attributes"]})
	if string(got) != `[{"minutes":90},{"id":"w-2"},[{"id":"a-1","name":"Billable","value":{"id":"v-2","name":"No"}}]]` {
		t.Errorf("body parts = %s", got)
	}

	isErr, text := call(t, cs, "log_work", map[string]any{"issueId": "DEMO-7", "durationMinutes": 5, "workType": "Design"}, nil)
	if !isErr || !strings.Contains(text, "available: Development, Testing") {
		t.Errorf("unknown type: isErr=%v %q", isErr, text)
	}
	if isErr, _ := call(t, cs, "log_work", map[string]any{"issueId": "DEMO-7", "durationMinutes": 0}, nil); !isErr {
		t.Error("zero duration should be rejected")
	}
}

func TestDraftIssue(t *testing.T) {
	api := newFakeAPI(t)
	api.json("POST /api/users/me/drafts", `{"id":"3-22"}`)
	api.json("GET /api/users/me/drafts/3-22", `{"idReadable":"Issue.Draft","project":{"id":"0-1","shortName":"DEMO"}}`)
	api.json("POST /api/users/me/drafts/3-22", `{"id":"3-22"}`)
	cs := connect(t, api)

	var draft struct{ DraftID, URL string }
	if isErr, text := call(t, cs, "create_draft_issue", map[string]any{"project": "DEMO", "summary": "idea"}, &draft); isErr {
		t.Fatal(text)
	}
	if draft.DraftID != "3-22" || !strings.HasSuffix(draft.URL, "/newIssue?draftId=3-22") {
		t.Errorf("draft = %+v", draft)
	}
	if b := api.bodies("/api/users/me/drafts"); len(b) != 1 || b[0]["summary"] != "idea" {
		t.Errorf("create body = %v", b)
	}

	if isErr, text := call(t, cs, "update_issue", map[string]any{"issueId": "3-22", "description": "more", "customFields": map[string]any{"State": "Open"}}, nil); isErr {
		t.Fatal(text)
	}
	if b := api.bodies("/api/users/me/drafts/3-22"); len(b) != 1 || b[0]["description"] != "more" {
		t.Errorf("update body = %v", b)
	}
	var got struct{ ID, URL string }
	if isErr, text := call(t, cs, "get_issue", map[string]any{"issueId": "3-22"}, &got); isErr {
		t.Fatal(text)
	}
	if got.ID != "3-22" || !strings.HasSuffix(got.URL, "/newIssue?draftId=3-22") {
		t.Errorf("draft issue = %+v", got)
	}
	if isErr, _ := call(t, cs, "update_issue", map[string]any{"issueId": "3-22", "vote": true}, nil); !isErr {
		t.Error("voting for a draft should fail")
	}
}

func TestGroups(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/groups", `[
		{"id":"3-0","name":"All Users","ringId":"r-all","usersCount":40},
		{"id":"3-1","name":"Backend devs","ringId":"r-1","usersCount":2},
		{"id":"3-2","name":"DEMO Team","ringId":"r-2","usersCount":1,"teamForProject":{"shortName":"DEMO","name":"Demo"}}]`)
	hub := http.NewServeMux()
	hub.HandleFunc("GET /hub/api/rest/usergroups/r-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"users":[{"login":"bob","name":"Bob","profile":{"email":{"email":"bob@x"}}},{"login":"eve","name":"Eve"}]}`)
	})
	hub.HandleFunc("GET /hub/api/rest/usergroups/r-2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"Not Found"}`)
	})
	hub.HandleFunc("GET /hub/api/rest/projectteams/r-2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"users":[{"login":"alice","name":"Alice"}]}`)
	})
	cs := connectHub(t, api, hub)

	var found struct {
		Groups []struct {
			Name, ProjectTeam string
		}
		Total int
	}
	if isErr, text := call(t, cs, "find_user_groups", map[string]any{"nameContains": "team"}, &found); isErr {
		t.Fatal(text)
	}
	if found.Total != 1 || found.Groups[0].ProjectTeam != "DEMO" {
		t.Errorf("found = %+v", found)
	}

	var members struct {
		Members []user
		Total   int
	}
	if isErr, text := call(t, cs, "get_user_group_members", map[string]any{"group": "backend devs", "limit": 1}, &members); isErr {
		t.Fatal(text)
	}
	if members.Total != 2 || len(members.Members) != 1 || members.Members[0] != (user{Login: "bob", FullName: "Bob", Email: "bob@x"}) {
		t.Errorf("members = %+v", members)
	}

	if isErr, text := call(t, cs, "get_user_group_members", map[string]any{"group": "DEMO Team"}, &members); isErr {
		t.Fatal(text)
	}
	if members.Total != 1 || members.Members[0].Login != "alice" {
		t.Errorf("team members = %+v", members)
	}
}

func TestArticles(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/users", `[{"id":"1-5","login":"bob"},{"id":"1-6","login":"bobby"}]`)
	api.json("GET /api/groups", `[{"id":"3-1","name":"Backend devs","ringId":"r-1"}]`)
	api.handle("GET /api/articles/HELP-A-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("fields") {
		case "id":
			io.WriteString(w, `{"id":"137-1"}`)
		case "content":
			io.WriteString(w, `{"content":"line one\n"}`)
		default:
			io.WriteString(w, `{"idReadable":"HELP-A-1","summary":"VPN","content":"a\nb\nc\nd","project":{"shortName":"HELP"},
				"childArticles":[{"idReadable":"HELP-A-2","summary":"macOS"}],"tags":[{"name":"faq"}],
				"visibility":{"$type":"LimitedVisibility","permittedUsers":[{"login":"bob"}],"permittedGroups":[]}}`)
		}
	})
	api.json("POST /api/articles", `{"idReadable":"HELP-A-3"}`)
	api.json("POST /api/articles/HELP-A-1", `{"idReadable":"HELP-A-1"}`)
	api.handle("GET /api/articles", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("query") != "project: HELP vpn" || q.Get("$top") != "11" {
			t.Errorf("search query = %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"idReadable":"HELP-A-1","summary":"VPN","content":"intro\nConnect to VPN first\nend","project":{"shortName":"HELP"}}]`)
	})
	cs := connect(t, api)

	var created struct{ ID, URL string }
	if isErr, text := call(t, cs, "create_article", map[string]any{
		"project": "DEMO", "summary": "How to", "content": "# Steps", "parentArticle": "HELP-A-1",
		"permittedUsers": []string{"bob"}, "permittedGroups": []string{"backend devs"},
	}, &created); isErr {
		t.Fatal(text)
	}
	if created.ID != "HELP-A-3" || !strings.HasSuffix(created.URL, "/articles/HELP-A-3") {
		t.Errorf("created = %+v", created)
	}
	got, _ := json.Marshal(api.bodies("/api/articles")[0])
	want := `{"content":"# Steps","parentArticle":{"id":"137-1"},"project":{"id":"0-1"},"summary":"How to",` +
		`"visibility":{"$type":"LimitedVisibility","permittedGroups":[{"id":"3-1"}],"permittedUsers":[{"id":"1-5"}]}}`
	if string(got) != want {
		t.Errorf("create body =\n%s\nwant\n%s", got, want)
	}

	if isErr, text := call(t, cs, "update_article", map[string]any{"articleId": "HELP-A-1", "content": "line two", "append": true}, nil); isErr {
		t.Fatal(text)
	}
	if b := api.bodies("/api/articles/HELP-A-1"); len(b) != 1 || b[0]["content"] != "line one\n\nline two" {
		t.Errorf("update body = %v", b)
	}

	var art struct {
		Content       string
		TotalLines    int
		HasMoreLines  bool
		ChildArticles []articleRef
		Visibility    map[string][]string
	}
	if isErr, text := call(t, cs, "get_article", map[string]any{"articleId": "HELP-A-1", "linesOffset": 1, "linesCount": 2}, &art); isErr {
		t.Fatal(text)
	}
	if art.Content != "b\nc" || art.TotalLines != 4 || !art.HasMoreLines || art.ChildArticles[0].ID != "HELP-A-2" ||
		art.Visibility["permittedUsers"][0] != "bob" {
		t.Errorf("article = %+v", art)
	}

	var found struct {
		Articles []struct {
			ID            string
			MatchingLines []string
		}
	}
	if isErr, text := call(t, cs, "search_articles", map[string]any{"query": "project: HELP vpn"}, &found); isErr {
		t.Fatal(text)
	}
	if len(found.Articles) != 1 || strings.Join(found.Articles[0].MatchingLines, "|") != "Connect to VPN first" {
		t.Errorf("found = %+v", found)
	}
}

func TestCommentWithLimitedVisibility(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/groups", `[{"id":"3-1","name":"Backend devs","ringId":"r-1"}]`)
	api.json("POST /api/issues/DEMO-7/comments", `{"id":"4-9","text":"secret"}`)
	cs := connect(t, api)

	var out struct{ URL string }
	if isErr, text := call(t, cs, "add_issue_comment", map[string]any{"issueId": "DEMO-7", "text": "secret", "permittedGroups": []string{"Backend devs"}}, &out); isErr {
		t.Fatal(text)
	}
	if !strings.HasSuffix(out.URL, "/issue/DEMO-7#focus=Comments-4-9.0-0") {
		t.Errorf("url = %q", out.URL)
	}
	got, _ := json.Marshal(api.bodies("/api/issues/DEMO-7/comments")[0])
	if string(got) != `{"text":"secret","visibility":{"$type":"LimitedVisibility","permittedGroups":[{"id":"3-1"}],"permittedUsers":[]}}` {
		t.Errorf("body = %s", got)
	}

	isErr, text := call(t, cs, "add_issue_comment", map[string]any{"issueId": "DEMO-7", "text": "x", "permittedGroups": []string{"QA"}}, nil)
	if !isErr || !strings.Contains(text, `permitted group "QA" not found`) {
		t.Errorf("isErr=%v %q", isErr, text)
	}
}

func TestGroupsFallBackToHubWhenForbidden(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("GET /api/groups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":"Forbidden","error_description":"HTTP 403 Forbidden"}`)
	})
	hub := http.NewServeMux()
	hub.HandleFunc("GET /hub/api/rest/usergroups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"UserGroupsPage","usergroups":[{"id":"h-all","name":"All Users"},{"id":"h-1","name":"Backend devs"}]}`)
	})
	hub.HandleFunc("GET /hub/api/rest/projectteams", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"ProjectTeamsPage","projectteams":[{"id":"h-t","name":"DEMO Team","project":{"key":"DEMO","name":"Demo"}}]}`)
	})
	hub.HandleFunc("GET /hub/api/rest/usergroups/h-all", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"users":[{"login":"a"},{"login":"b"},{"login":"c"},{"login":"d"}]}`)
	})
	cs := connectHub(t, api, hub)

	var found struct {
		Groups []struct{ Name, ID, ProjectTeam string }
		Total  int
	}
	if isErr, text := call(t, cs, "find_user_groups", map[string]any{"nameContains": ""}, &found); isErr {
		t.Fatal(text)
	}
	if found.Total != 3 || found.Groups[0].ID != "h-all" || found.Groups[2].ProjectTeam != "DEMO" {
		t.Errorf("found = %+v", found)
	}

	var members struct {
		Members []user
		Total   int
	}
	if isErr, text := call(t, cs, "get_user_group_members", map[string]any{"group": "All Users", "limit": 3}, &members); isErr {
		t.Fatal(text)
	}
	if members.Total != 4 || len(members.Members) != 3 {
		t.Errorf("members = %+v", members)
	}

	isErr, text := call(t, cs, "add_issue_comment", map[string]any{"issueId": "DEMO-7", "text": "x", "permittedGroups": []string{"Backend devs"}}, nil)
	if !isErr || !strings.Contains(text, "needs permission to read user groups") {
		t.Errorf("visibility without group IDs: isErr=%v %q", isErr, text)
	}
}

func TestFieldSchemaFromIssuesForNonAdmin(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/admin/projects/0-2/customFields", `[]`)
	api.handle("GET /api/issues", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("query"); q != "project: {OPS} sort by: updated desc" {
			t.Errorf("query = %q", q)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[
			{"customFields":[
				{"projectCustomField":{"$type":"StateProjectCustomField","canBeEmpty":false,"field":{"name":"Stage","fieldType":{"id":"state[1]","valueType":"state"}},
				 "bundle":{"values":[{"name":"Backlog"},{"name":"Done"}]}}}]},
			{"customFields":[
				{"projectCustomField":{"$type":"StateProjectCustomField","canBeEmpty":false,"field":{"name":"Stage","fieldType":{"id":"state[1]","valueType":"state"}},
				 "bundle":{"values":[{"name":"Backlog"},{"name":"Done"}]}}},
				{"projectCustomField":{"$type":"EnumProjectCustomField","canBeEmpty":true,"field":{"name":"flow","fieldType":{"id":"enum[*]","isMultiValue":true,"valueType":"enum"}},
				 "bundle":{"values":[{"name":"infra"},{"name":"app"}]}}}]}]`)
	})
	api.json("POST /api/issues", `{"idReadable":"OPS-9"}`)
	cs := connect(t, api)

	var schema struct {
		Properties map[string]map[string]any
		Required   []string
	}
	if isErr, text := call(t, cs, "get_issue_fields_schema", map[string]any{"projectKey": "OPS"}, &schema); isErr {
		t.Fatal(text)
	}
	if len(schema.Properties) != 2 || schema.Properties["flow"]["type"] != "array" || strings.Join(schema.Required, ",") != "Stage" {
		t.Errorf("schema = %+v", schema)
	}

	if isErr, text := call(t, cs, "create_issue", map[string]any{
		"project": "OPS", "summary": "x", "customFields": map[string]any{"Stage": "Backlog", "flow": []string{"infra"}},
	}, nil); isErr {
		t.Fatal(text)
	}
	got, _ := json.Marshal(api.bodies("/api/issues")[0]["customFields"])
	want := `[{"$type":"StateIssueCustomField","name":"Stage","value":{"name":"Backlog"}},{"$type":"MultiEnumIssueCustomField","name":"flow","value":[{"name":"infra"}]}]`
	if string(got) != want {
		t.Errorf("customFields =\n%s\nwant\n%s", got, want)
	}
}

func TestSavedSearchesAreFavorites(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/userIssueFolders", `[
		{"$type":"Tag","id":"10-32","name":"Star","query":"tag: Star"},
		{"$type":"SavedQuery","id":"11-0","name":"Assigned to me","query":"for: me"},
		{"$type":"Project","id":"0-0","name":"Demo","query":"project: Demo"},
		{"$type":"SavedQuery","id":"11-1","name":"Commented by me","query":"commenter: me"}]`)
	cs := connect(t, api)

	var out struct {
		Searches []struct{ Name, Query string }
		Total    int
	}
	if isErr, text := call(t, cs, "get_saved_issue_searches", map[string]any{"offset": 1}, &out); isErr {
		t.Fatal(text)
	}
	if out.Total != 2 || len(out.Searches) != 1 || out.Searches[0].Query != "commenter: me" {
		t.Errorf("out = %+v", out)
	}
}

func TestStarTagGoesThroughWatchers(t *testing.T) {
	api := newFakeAPI(t)
	var mu sync.Mutex
	starred := false
	api.handle("POST /api/issues/DEMO-7/watchers/issueWatchers", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starred = !starred
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"1-1"}`)
	})
	api.handle("GET /api/issues/DEMO-7", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"tags": []apiTag{{Name: "backend"}}, "watchers": map[string]any{"hasStar": starred}})
	})
	cs := connect(t, api)

	var out struct{ Tags []string }
	if isErr, text := call(t, cs, "manage_issue_tags", map[string]any{"issueId": "DEMO-7", "tag": "star", "operation": "add"}, &out); isErr {
		t.Fatal(text)
	}
	if strings.Join(out.Tags, ",") != "Star,backend" {
		t.Errorf("after star: %v", out.Tags)
	}
	if isErr, text := call(t, cs, "manage_issue_tags", map[string]any{"issueId": "DEMO-7", "tag": "Star", "operation": "remove"}, &out); isErr {
		t.Fatal(text)
	}
	if strings.Join(out.Tags, ",") != "backend" {
		t.Errorf("after unstar: %v", out.Tags)
	}
	bodies := api.bodies("/api/issues/DEMO-7/watchers/issueWatchers")
	if len(bodies) != 2 || bodies[0]["isStarred"] != true || bodies[1]["isStarred"] != false {
		t.Errorf("watcher bodies = %v", bodies)
	}
}
