package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/youtrack"
	"github.com/xlsft/ymcp/internal/youtrack/youtracktest"
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
		r.Body = io.NopCloser(bytes.NewReader(data))
		if len(data) > 0 && strings.Contains(r.Header.Get("Content-Type"), "json") {
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
	Register(server, yt, nil)

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
	api.handle("GET /api/issues/DEMO-7/comments", fakeComments(t, 8, 3))
	cs := connect(t, api)

	var out struct {
		Links          map[string][]string
		RecentComments []struct{ Text string }
	}
	if isErr, text := call(t, cs, "get_issue", map[string]any{"issueId": "DEMO-7"}, &out); isErr {
		t.Fatal(text)
	}
	var texts []string
	for _, c := range out.RecentComments {
		texts = append(texts, c.Text)
	}
	if strings.Join(texts, ",") != "c2,c4,c5,c6,c7" {
		t.Errorf("recent comments = %v", texts)
	}

	var page struct {
		Comments []struct{ Text string }
		Total    int
		HasMore  bool
	}
	if isErr, text := call(t, cs, "get_issue_comments", map[string]any{"issueId": "DEMO-7", "offset": 2, "limit": 2}, &page); isErr {
		t.Fatal(text)
	}
	if page.Total != 7 || !page.HasMore || len(page.Comments) != 2 || page.Comments[0].Text != "c2" || page.Comments[1].Text != "c4" {
		t.Errorf("page = %+v", page)
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

func TestGroupsHiddenFromMembersSayWhy(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("GET /api/groups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":"Forbidden","error_description":"HTTP 403 Forbidden"}`)
	})
	hub := http.NewServeMux()
	hub.HandleFunc("GET /hub/api/rest/usergroups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usergroups":[]}`)
	})
	cs := connectHub(t, api, hub)
	for tool, args := range map[string]map[string]any{
		"find_user_groups":       {"nameContains": "All"},
		"get_user_group_members": {"group": "All Users"},
	} {
		isErr, text := call(t, cs, tool, args, nil)
		if !isErr || !strings.Contains(text, "only admins can") {
			t.Errorf("%s: isErr=%v %q", tool, isErr, text)
		}
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

func TestDeletes(t *testing.T) {
	api := newFakeAPI(t)
	var mu sync.Mutex
	var deleted []string
	record := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deleted = append(deleted, r.URL.Path)
		mu.Unlock()
	}
	api.handle("DELETE /api/issues/DEMO-7", record)
	api.handle("DELETE /api/users/me/drafts/3-22", record)
	api.handle("DELETE /api/issues/DEMO-7/comments/4-9", record)
	api.json("POST /api/issues/DEMO-7/comments/4-8", `{"id":"4-8"}`)
	api.handle("DELETE /api/articles/HELP-A-1", record)
	cs := connect(t, api)

	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"delete_issue", map[string]any{"issueId": "DEMO-7"}},
		{"delete_issue", map[string]any{"issueId": "3-22"}},
		{"delete_issue_comment", map[string]any{"issueId": "DEMO-7", "commentId": "4-8"}},
		{"delete_issue_comment", map[string]any{"issueId": "DEMO-7", "commentId": "4-9", "permanent": true}},
		{"delete_article", map[string]any{"articleId": "HELP-A-1"}},
	} {
		if isErr, text := call(t, cs, c.tool, c.args, nil); isErr {
			t.Fatalf("%s: %s", c.tool, text)
		}
	}
	want := "/api/issues/DEMO-7,/api/users/me/drafts/3-22,/api/issues/DEMO-7/comments/4-9,/api/articles/HELP-A-1"
	if got := strings.Join(deleted, ","); got != want {
		t.Errorf("deleted = %s", got)
	}

	if b := api.bodies("/api/issues/DEMO-7/comments/4-8"); len(b) != 1 || b[0]["deleted"] != true {
		t.Errorf("soft delete body = %v", b)
	}

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if strings.HasPrefix(tool.Name, "delete_") && (tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint) {
			t.Errorf("%s is not marked destructive", tool.Name)
		}
	}
}

func TestEveryToolDeclaresAllHints(t *testing.T) {
	cs := connect(t, newFakeAPI(t))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		b, err := json.Marshal(tool.Annotations)
		if err != nil {
			t.Fatal(err)
		}
		var hints map[string]any
		if err := json.Unmarshal(b, &hints); err != nil {
			t.Fatal(err)
		}
		for _, h := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			if _, ok := hints[h].(bool); !ok {
				t.Errorf("%s: %s = %v, want a boolean", tool.Name, h, hints[h])
			}
		}
		if hints["readOnlyHint"] == true && hints["destructiveHint"] == true {
			t.Errorf("%s is both read-only and destructive", tool.Name)
		}
		if hints["openWorldHint"] == hints["readOnlyHint"] {
			t.Errorf("%s: openWorldHint = %v, want it on every write and only there", tool.Name, hints["openWorldHint"])
		}
	}
}

// fakeComments serves n comments c0..c{n-1} where the one at deleted is
// marked deleted, honoring $skip and $top like YouTrack.
func fakeComments(t *testing.T, n, deleted int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		skip, _ := strconv.Atoi(q.Get("$skip"))
		top, _ := strconv.Atoi(q.Get("$top"))
		if top < 0 || top == 0 && q.Get("$top") == "" {
			top = n
		}
		var out []map[string]any
		for i := skip; i < min(skip+top, n); i++ {
			c := map[string]any{"deleted": i == deleted}
			if q.Get("fields") != "deleted" {
				c["id"] = fmt.Sprintf("4-%d", i)
				c["text"] = fmt.Sprintf("c%d", i)
				if i == deleted {
					c["text"] = nil
				}
			}
			out = append(out, c)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}
}

func TestWorkItemsListAndDelete(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/issues/DEMO-7/timeTracking/workItems", `[
		{"id":"8-1","date":1767225600000,"duration":{"minutes":30,"presentation":"30m"},"type":{"name":"Testing"},"author":{"login":"alice"}},
		{"id":"8-2","date":1767225600000,"duration":{"minutes":60,"presentation":"1h"},"author":{"login":"bob"}}]`)
	var deleted string
	api.handle("DELETE /api/issues/DEMO-7/timeTracking/workItems/8-2", func(w http.ResponseWriter, r *http.Request) { deleted = r.URL.Path })
	cs := connect(t, api)

	var out struct {
		WorkItems    []map[string]any
		Total        int
		TotalMinutes int
	}
	if isErr, text := call(t, cs, "get_issue_work_items", map[string]any{"issueId": "DEMO-7", "offset": 1}, &out); isErr {
		t.Fatal(text)
	}
	if out.Total != 2 || out.TotalMinutes != 90 || len(out.WorkItems) != 1 || out.WorkItems[0]["id"] != "8-2" {
		t.Errorf("out = %+v", out)
	}
	if isErr, text := call(t, cs, "delete_work_item", map[string]any{"issueId": "DEMO-7", "workItemId": "8-2"}, nil); isErr {
		t.Fatal(text)
	}
	if deleted == "" {
		t.Error("work item was not deleted")
	}
}

func TestUnlinkIssues(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/issueLinkTypes", `[{"name":"Depend","sourceToTarget":"is required for","targetToSource":"depends on","directed":true}]`)
	var mu sync.Mutex
	linked := true
	api.handle("GET /api/issues/DEMO-1/links", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		issues := `[]`
		if linked {
			issues = `[{"id":"3-2","idReadable":"DEMO-2"}]`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"id":"163-1s","direction":"OUTWARD","linkType":{"name":"Depend","sourceToTarget":"is required for","targetToSource":"depends on"},"issues":[]},
			{"id":"163-1t","direction":"INWARD","linkType":{"name":"Depend","sourceToTarget":"is required for","targetToSource":"depends on"},"issues":%s}]`, issues)
	})
	api.handle("DELETE /api/issues/DEMO-1/links/163-1t/issues/3-2", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		linked = false
		mu.Unlock()
	})
	cs := connect(t, api)

	var out struct {
		Unlinked          string
		LinkedIssueCounts map[string]int
	}
	if isErr, text := call(t, cs, "unlink_issues", map[string]any{"targetIssueId": "DEMO-1", "linkType": "depends on", "issueToLinkId": "demo-2"}, &out); isErr {
		t.Fatal(text)
	}
	if out.Unlinked != "depends on DEMO-2" || len(out.LinkedIssueCounts) != 0 {
		t.Errorf("out = %+v", out)
	}
	isErr, text := call(t, cs, "unlink_issues", map[string]any{"targetIssueId": "DEMO-1", "linkType": "depends on", "issueToLinkId": "DEMO-2"}, nil)
	if !isErr || !strings.Contains(text, `has no link "depends on" to DEMO-2`) {
		t.Errorf("second unlink: isErr=%v %q", isErr, text)
	}
}

const fakeBoardJSON = `{"id":"195-13","name":"dev","projects":[{"shortName":"DEMO"}],
	"currentSprint":{"id":"209-2","name":"S2"},"sprintsSettings":{"disableSprints":false,"isExplicit":true},
	"columnSettings":{"field":{"name":"State"},"columns":[{"presentation":"Open"},{"presentation":"Fixed","isResolved":true}]},
	"estimationField":{"name":"Story points"},
	"sprints":[{"id":"209-1","name":"S1","start":1767225600000,"archived":true},{"id":"209-2","name":"S2","start":1768435200000}]}`

func newAgileAPI(t *testing.T) *fakeAPI {
	api := newFakeAPI(t)
	api.json("GET /api/agiles", `[{"id":"195-13","name":"dev","projects":[{"shortName":"DEMO"}],"sprintsSettings":{}},
		{"id":"195-16","name":"ops","projects":[{"shortName":"OPS"}],"sprintsSettings":{}},
		{"id":"195-17","name":"ops","projects":[{"shortName":"DEMO"}],"sprintsSettings":{"disableSprints":true}}]`)
	api.json("GET /api/agiles/195-13", fakeBoardJSON)
	return api
}

func TestAgileBoardsAndSprints(t *testing.T) {
	api := newAgileAPI(t)
	api.json("GET /api/agiles/195-13/sprints/209-2", `{"id":"209-2","name":"S2","goal":"ship","unresolvedIssuesCount":1}`)
	api.json("GET /api/agiles/195-13/sprints/209-2/issues", `[{"idReadable":"DEMO-1","summary":"a","customFields":[
		{"name":"State","$type":"StateIssueCustomField","value":{"name":"Open"}},
		{"name":"Assignee","$type":"SingleUserIssueCustomField","value":{"login":"bob"}},
		{"name":"Story points","$type":"SimpleIssueCustomField","value":3},
		{"name":"Priority","$type":"SingleEnumIssueCustomField","value":{"name":"Major"}}]}]`)
	cs := connect(t, api)

	var found struct {
		Boards []struct{ ID string }
		Total  int
	}
	if isErr, text := call(t, cs, "find_agile_boards", map[string]any{"nameContains": "demo"}, &found); isErr {
		t.Fatal(text)
	}
	if found.Total != 2 {
		t.Errorf("boards matching project DEMO = %+v", found)
	}

	var board struct {
		Sprints         []struct{ Name string }
		SprintsArchived int
		ColumnField     string
		Columns         []string
	}
	if isErr, text := call(t, cs, "get_agile_board", map[string]any{"board": "DEV", "includeArchived": true}, &board); isErr {
		t.Fatal(text)
	}
	if len(board.Sprints) != 2 || board.Sprints[0].Name != "S2" || board.SprintsArchived != 1 || board.ColumnField != "State" {
		t.Errorf("board = %+v", board)
	}

	isErr, text := call(t, cs, "get_agile_board", map[string]any{"board": "ops"}, nil)
	if !isErr || !strings.Contains(text, "pass an ID: 195-16 (OPS), 195-17 (DEMO)") {
		t.Errorf("ambiguous board: isErr=%v %q", isErr, text)
	}

	var sprint struct {
		Sprint struct{ Name, Goal string }
		Issues []struct {
			ID           string
			CustomFields map[string]any
		}
	}
	if isErr, text := call(t, cs, "get_sprint", map[string]any{"board": "195-13"}, &sprint); isErr {
		t.Fatal(text)
	}
	want := map[string]any{"State": "Open", "Assignee": "bob", "Story points": float64(3)}
	if sprint.Sprint.Goal != "ship" || len(sprint.Issues) != 1 || !reflect.DeepEqual(sprint.Issues[0].CustomFields, want) {
		t.Errorf("sprint = %+v", sprint)
	}
	if isErr, text := call(t, cs, "get_sprint", map[string]any{"board": "195-13", "sprint": "S9"}, nil); !isErr || !strings.Contains(text, `sprint "S9" not found`) {
		t.Errorf("unknown sprint: isErr=%v %q", isErr, text)
	}
}

func TestSprintWrites(t *testing.T) {
	api := newAgileAPI(t)
	api.json("GET /api/agiles/195-17", `{"id":"195-17","name":"ops","sprintsSettings":{"disableSprints":true}}`)
	api.json("POST /api/agiles/195-13/sprints", `{"id":"209-3","name":"S3"}`)
	api.json("POST /api/agiles/195-13/sprints/209-2", `{"id":"209-2","name":"S2","archived":true}`)
	api.json("GET /api/issues/DEMO-7", `{"id":"3-77"}`)
	api.json("POST /api/agiles/195-13/sprints/209-2/issues", `{"id":"3-77"}`)
	var mu sync.Mutex
	var deleted []string
	record := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deleted = append(deleted, r.URL.Path)
		mu.Unlock()
	}
	api.handle("DELETE /api/agiles/195-13/sprints/209-2/issues/3-77", record)
	api.handle("DELETE /api/agiles/195-13/sprints/209-1", record)
	cs := connect(t, api)

	steps := []struct {
		tool string
		args map[string]any
	}{
		{"create_sprint", map[string]any{"board": "dev", "name": "S3", "goal": "g", "start": "2026-01-01", "finish": "2026-01-14"}},
		{"update_sprint", map[string]any{"board": "dev", "sprint": "current", "archived": true, "goal": ""}},
		{"add_issue_to_sprint", map[string]any{"board": "dev", "issueId": "DEMO-7"}},
		{"remove_issue_from_sprint", map[string]any{"board": "dev", "sprint": "S2", "issueId": "DEMO-7"}},
		{"delete_sprint", map[string]any{"board": "195-13", "sprint": "209-1"}},
	}
	for _, st := range steps {
		if isErr, text := call(t, cs, st.tool, st.args, nil); isErr {
			t.Fatalf("%s: %s", st.tool, text)
		}
	}

	got, _ := json.Marshal(api.bodies("/api/agiles/195-13/sprints"))
	if string(got) != `[{"finish":1768435199999,"goal":"g","name":"S3","start":1767225600000}]` {
		t.Errorf("create body = %s", got)
	}
	got, _ = json.Marshal(api.bodies("/api/agiles/195-13/sprints/209-2"))
	if string(got) != `[{"archived":true,"goal":""}]` {
		t.Errorf("update body = %s", got)
	}
	if b := api.bodies("/api/agiles/195-13/sprints/209-2/issues"); len(b) != 1 || b[0]["id"] != "3-77" {
		t.Errorf("add body = %v", b)
	}
	if strings.Join(deleted, ",") != "/api/agiles/195-13/sprints/209-2/issues/3-77,/api/agiles/195-13/sprints/209-1" {
		t.Errorf("deleted = %v", deleted)
	}

	isErr, text := call(t, cs, "create_sprint", map[string]any{"board": "195-17", "name": "x"}, nil)
	if !isErr || !strings.Contains(text, "doesn't use sprints") {
		t.Errorf("sprints disabled: isErr=%v %q", isErr, text)
	}
}

func TestIssueActivities(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("GET /api/issues/DEMO-7/activitiesPage", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("categories") != "CommentsCategory,CustomFieldCategory" || q.Get("reverse") != "true" || q.Get("cursor") != "c+1" {
			t.Errorf("query = %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"hasBefore":true,"beforeCursor":"c+2","activities":[
			{"timestamp":1767225600000,"author":{"login":"bob"},"category":{"id":"CustomFieldCategory"},"field":{"name":"State"},
			 "added":[{"name":"Fixed"}],"removed":[{"name":"Open"}]},
			{"timestamp":1767225600000,"author":{"login":"bob"},"category":{"id":"CommentsCategory"},"field":{"name":"comments"},
			 "added":[{"text":"done"}],"removed":[]}]}`)
	})
	cs := connect(t, api)

	var out struct {
		Activities []map[string]any
		NextCursor string
	}
	if isErr, text := call(t, cs, "get_issue_activities", map[string]any{"issueId": "DEMO-7", "categories": []string{"comments", "fields"}, "cursor": "c+1"}, &out); isErr {
		t.Fatal(text)
	}
	if out.NextCursor != "c+2" || len(out.Activities) != 2 {
		t.Fatalf("out = %+v", out)
	}
	a := out.Activities[0]
	if a["kind"] != "fields" || a["added"] != "Fixed" || a["removed"] != "Open" || a["time"] != "2026-01-01T00:00:00Z" {
		t.Errorf("field change = %v", a)
	}
	if c := out.Activities[1]; c["added"] != "done" || c["removed"] != nil {
		t.Errorf("comment = %v", c)
	}
}

func TestCountIssuesWaitsForResult(t *testing.T) {
	countPoll = time.Millisecond
	api := newFakeAPI(t)
	var mu sync.Mutex
	polls := 0
	api.handle("POST /api/issuesGetter/count", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n < 3 {
			io.WriteString(w, `{"count":-1}`)
		} else {
			io.WriteString(w, `{"count":42}`)
		}
	})
	cs := connect(t, api)
	var out struct{ Count int }
	if isErr, text := call(t, cs, "count_issues", map[string]any{"query": "#Unresolved"}, &out); isErr {
		t.Fatal(text)
	}
	if out.Count != 42 || polls != 3 {
		t.Errorf("count=%d polls=%d", out.Count, polls)
	}
	if b := api.bodies("/api/issuesGetter/count"); b[0]["query"] != "#Unresolved" {
		t.Errorf("body = %v", b[0])
	}
}

func TestTimeReport(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("GET /api/workItems", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("author") != "alice" || q.Get("startDate") != "2026-01-01" || q.Get("query") != "project: DEMO" {
			t.Errorf("query = %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[
			{"id":"8-1","date":1767225600000,"duration":{"minutes":90},"type":{"name":"Testing"},"author":{"login":"alice"},"issue":{"idReadable":"DEMO-1","project":{"shortName":"DEMO"}}},
			{"id":"8-2","date":1767312000000,"duration":{"minutes":30},"author":{"login":"alice"},"issue":{"idReadable":"DEMO-2","project":{"shortName":"DEMO"}}},
			{"id":"8-3","date":1767312000000,"duration":{"minutes":60},"type":{"name":"Testing"},"author":{"login":"alice"},"issue":{"idReadable":"DEMO-1","project":{"shortName":"DEMO"}}}]`)
	})
	cs := connect(t, api)

	var out struct {
		Total  string
		Groups []struct {
			Key      string
			Duration string
			Items    int
		}
		Items []map[string]any
	}
	if isErr, text := call(t, cs, "get_time_report", map[string]any{
		"query": "project: DEMO", "author": "me", "from": "2026-01-01", "groupBy": "workType", "includeItems": true, "itemsLimit": 1,
	}, &out); isErr {
		t.Fatal(text)
	}
	got, _ := json.Marshal(out.Groups)
	if out.Total != "3h" || string(got) != `[{"Key":"Testing","Duration":"2h 30m","Items":2},{"Key":"(no type)","Duration":"30m","Items":1}]` || len(out.Items) != 1 {
		t.Errorf("total=%s groups=%s items=%d", out.Total, got, len(out.Items))
	}

	if isErr, text := call(t, cs, "get_time_report", map[string]any{"groupBy": "day", "author": "alice", "from": "2026-01-01", "query": "project: DEMO"}, &out); isErr {
		t.Fatal(text)
	}
	if len(out.Groups) != 2 || out.Groups[0].Key != "2026-01-01" || out.Groups[1].Duration != "1h 30m" {
		t.Errorf("by day = %+v", out.Groups)
	}
}

func TestApplyCommand(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("POST /api/commands/assist", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		bodies := api.bodies("/api/commands/assist")
		if bodies[len(bodies)-1]["query"] == "State Nope" {
			io.WriteString(w, `{"commands":[{"description":"State expected: Nope","error":true}],"suggestions":[{"option":"Open"}]}`)
			return
		}
		io.WriteString(w, `{"commands":[{"description":"State: Fixed"}],"suggestions":[]}`)
	})
	api.json("POST /api/commands", `{}`)
	api.json("GET /api/issues", `[{"idReadable":"DEMO-1"},{"idReadable":"DEMO-2"}]`)
	cs := connect(t, api)

	var dry struct {
		Valid       bool
		Suggestions []map[string]any
	}
	if isErr, text := call(t, cs, "apply_command", map[string]any{"command": "State Nope", "issueIds": []string{"DEMO-1"}, "dryRun": true}, &dry); isErr {
		t.Fatal(text)
	}
	if dry.Valid || len(dry.Suggestions) != 1 {
		t.Errorf("dry run = %+v", dry)
	}
	if isErr, text := call(t, cs, "apply_command", map[string]any{"command": "State Nope", "issueIds": []string{"DEMO-1"}}, nil); !isErr || !strings.Contains(text, "nothing applied") {
		t.Errorf("invalid apply: isErr=%v %q", isErr, text)
	}
	if n := len(api.bodies("/api/commands")); n != 0 {
		t.Fatalf("invalid command was applied %d times", n)
	}

	if isErr, text := call(t, cs, "apply_command", map[string]any{"command": "State Fixed", "query": "#Unresolved", "comment": "done", "silent": true}, nil); isErr {
		t.Fatal(text)
	}
	got, _ := json.Marshal(api.bodies("/api/commands"))
	if string(got) != `[{"comment":"done","issues":[{"idReadable":"DEMO-1"},{"idReadable":"DEMO-2"}],"query":"State Fixed","silent":true}]` {
		t.Errorf("applied = %s", got)
	}
}

func TestAttachments(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/issues/DEMO-7/attachments", `[
		{"id":"12-1","name":"log.txt","size":5,"mimeType":"text/plain","url":"/api/files/12-1?sign=s"},
		{"id":"12-2","name":"shot.png","size":4,"mimeType":"image/png","url":"/api/files/12-2?sign=s"},
		{"id":"12-3","name":"blob.bin","size":3,"mimeType":"application/octet-stream","url":"/api/files/12-3?sign=s"}]`)
	files := map[string]struct{ ct, body string }{
		"/api/files/12-1": {"text/plain", "hello"},
		"/api/files/12-2": {"image/png", "\x89PNG"},
		"/api/files/12-3": {"application/octet-stream", "\x00\x01\x02"},
	}
	api.handle("GET /api/files/", func(w http.ResponseWriter, r *http.Request) {
		f := files[r.URL.Path]
		w.Header().Set("Content-Type", f.ct)
		io.WriteString(w, f.body)
	})
	api.handle("POST /api/issues/DEMO-7/attachments", func(w http.ResponseWriter, r *http.Request) {
		f, h, err := r.FormFile("file0")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(f)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"id":"12-9","name":%q,"size":%d}]`, h.Filename, len(body))
	})
	var deleted string
	api.handle("DELETE /api/issues/DEMO-7/attachments/12-3", func(w http.ResponseWriter, r *http.Request) { deleted = r.URL.Path })
	cs := connect(t, api)

	if isErr, text := call(t, cs, "get_attachment", map[string]any{"issueId": "DEMO-7", "attachment": "log.txt"}, nil); isErr || text != "hello" {
		t.Errorf("text attachment: isErr=%v %q", isErr, text)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_attachment", Arguments: map[string]any{"issueId": "DEMO-7", "attachment": "12-2"}})
	if err != nil || res.IsError || len(res.Content) != 2 {
		t.Fatalf("image attachment: %v %+v", err, res)
	}
	if img, ok := res.Content[1].(*mcp.ImageContent); !ok || img.MIMEType != "image/png" || string(img.Data) != "\x89PNG" {
		t.Errorf("image content = %+v", res.Content[1])
	}
	if isErr, text := call(t, cs, "get_attachment", map[string]any{"issueId": "DEMO-7", "attachment": "blob.bin"}, nil); !isErr || !strings.Contains(text, "pass saveTo") {
		t.Errorf("binary inline: isErr=%v %q", isErr, text)
	}
	dir := t.TempDir()
	var saved struct{ SavedTo string }
	if isErr, text := call(t, cs, "get_attachment", map[string]any{"issueId": "DEMO-7", "attachment": "blob.bin", "saveTo": dir}, &saved); isErr {
		t.Fatal(text)
	}
	if data, _ := os.ReadFile(saved.SavedTo); string(data) != "\x00\x01\x02" {
		t.Errorf("saved %q = %q", saved.SavedTo, data)
	}

	var added struct {
		ID, Name string
		Size     int
	}
	if isErr, text := call(t, cs, "add_attachment", map[string]any{"issueId": "DEMO-7", "fileName": "note.md", "textContent": "# hi"}, &added); isErr {
		t.Fatal(text)
	}
	if added.ID != "12-9" || added.Name != "note.md" || added.Size != 4 {
		t.Errorf("added = %+v", added)
	}
	if isErr, text := call(t, cs, "delete_attachment", map[string]any{"issueId": "DEMO-7", "attachment": "blob.bin"}, nil); isErr || deleted == "" {
		t.Errorf("delete: isErr=%v %q", isErr, text)
	}
}

func TestCommentEditAndRestore(t *testing.T) {
	api := newFakeAPI(t)
	api.json("POST /api/issues/DEMO-7/comments/4-1", `{"id":"4-1","text":"fixed"}`)
	api.handle("GET /api/issues/DEMO-7/comments", fakeComments(t, 3, 1))
	cs := connect(t, api)

	if isErr, text := call(t, cs, "update_issue_comment", map[string]any{"issueId": "DEMO-7", "commentId": "4-1", "text": "fixed"}, nil); isErr {
		t.Fatal(text)
	}
	if isErr, text := call(t, cs, "restore_issue_comment", map[string]any{"issueId": "DEMO-7", "commentId": "4-1"}, nil); isErr {
		t.Fatal(text)
	}
	got, _ := json.Marshal(api.bodies("/api/issues/DEMO-7/comments/4-1"))
	if string(got) != `[{"text":"fixed"},{"deleted":false}]` {
		t.Errorf("bodies = %s", got)
	}

	var all struct {
		Comments []map[string]any
	}
	if isErr, text := call(t, cs, "get_issue_comments", map[string]any{"issueId": "DEMO-7", "includeDeleted": true}, &all); isErr {
		t.Fatal(text)
	}
	if len(all.Comments) != 3 || all.Comments[1]["deleted"] != true || all.Comments[0]["deleted"] != nil {
		t.Errorf("all comments = %v", all.Comments)
	}
}

func TestVcsChanges(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/issues/DEMO-7/vcsChanges", `[
		{"$type":"VcsChange","version":"abc123","text":"DEMO-7 fix","date":1767225600000,"files":3,"author":{"login":"bob"},"urls":["https://git/c/abc123"]},
		{"$type":"PullRequest","title":"Fix login","url":"https://git/pr/5","state":{"id":"MERGED"},"userName":"eve","date":1767225600000}]`)
	cs := connect(t, api)
	var out struct{ Changes []map[string]any }
	if isErr, text := call(t, cs, "get_issue_vcs_changes", map[string]any{"issueId": "DEMO-7"}, &out); isErr {
		t.Fatal(text)
	}
	got, _ := json.Marshal(out.Changes)
	want := `[{"author":"bob","date":"2026-01-01T00:00:00Z","files":3,"kind":"commit","message":"DEMO-7 fix","urls":["https://git/c/abc123"],"version":"abc123"},` +
		`{"author":"eve","date":"2026-01-01T00:00:00Z","kind":"pullRequest","state":"MERGED","title":"Fix login","urls":["https://git/pr/5"]}]`
	if string(got) != want {
		t.Errorf("changes =\n%s\nwant\n%s", got, want)
	}
}

func TestTagAndSavedSearchAdmin(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/tags", `[{"id":"6-1","name":"backend","owner":{"login":"bob"}},{"id":"6-2","name":"backend","owner":{"login":"alice"}}]`)
	api.json("POST /api/tags", `{"id":"6-3","name":"new"}`)
	api.json("POST /api/tags/6-2", `{"id":"6-2","name":"api"}`)
	api.json("GET /api/savedQueries", `[{"id":"11-1","name":"Mine","query":"for: me","owner":{"login":"bob"}},{"id":"11-2","name":"mine","query":"for: me","owner":{"login":"alice"}}]`)
	api.json("POST /api/savedQueries", `{"id":"11-3","name":"Bugs","query":"Type: Bug"}`)
	api.json("POST /api/savedQueries/11-2", `{"id":"11-2","name":"mine","query":"for: me #Unresolved"}`)
	var mu sync.Mutex
	var deleted []string
	record := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deleted = append(deleted, r.URL.Path)
		mu.Unlock()
	}
	api.handle("DELETE /api/tags/6-2", record)
	api.handle("DELETE /api/savedQueries/11-2", record)
	cs := connect(t, api)

	for _, st := range []struct {
		tool string
		args map[string]any
	}{
		{"create_tag", map[string]any{"name": "new"}},
		{"update_tag", map[string]any{"tag": "backend", "name": "api"}},
		{"delete_tag", map[string]any{"tag": "backend"}},
		{"create_saved_search", map[string]any{"name": "Bugs", "query": "Type: Bug"}},
		{"update_saved_search", map[string]any{"search": "MINE", "query": "for: me #Unresolved"}},
		{"delete_saved_search", map[string]any{"search": "mine"}},
	} {
		if isErr, text := call(t, cs, st.tool, st.args, nil); isErr {
			t.Fatalf("%s: %s", st.tool, text)
		}
	}
	if b := api.bodies("/api/tags/6-2"); len(b) != 1 || b[0]["name"] != "api" {
		t.Errorf("rename hit the wrong tag: %v", b)
	}
	if b := api.bodies("/api/savedQueries/11-2"); len(b) != 1 || b[0]["query"] != "for: me #Unresolved" {
		t.Errorf("saved search update = %v", b)
	}
	if strings.Join(deleted, ",") != "/api/tags/6-2,/api/savedQueries/11-2" {
		t.Errorf("deleted = %v", deleted)
	}
}

func TestCallAPI(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("GET /api/issues/DEMO-7", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("fields") != "idReadable,summary" || q.Get("$top") != "10" || strings.Join(q["customFields"], ",") != "State,Type" {
			t.Errorf("query = %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"idReadable":"DEMO-7","summary":"s"}`)
	})
	api.json("POST /api/issues/DEMO-7/comments", `{"id":"4-1"}`)
	api.handle("DELETE /api/issues/DEMO-7/tags/6-1", func(w http.ResponseWriter, r *http.Request) {})
	hub := http.NewServeMux()
	hub.HandleFunc("GET /hub/api/rest/usergroups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usergroups":[]}`)
	})
	cs := connectHub(t, api, hub)

	var issue struct{ IDReadable string }
	if isErr, text := call(t, cs, "call_api", map[string]any{
		"method": "GET", "path": "/api/issues/DEMO-7?fields=idReadable,summary",
		"query": map[string]any{"$top": 10, "customFields": []string{"State", "Type"}},
	}, &issue); isErr || issue.IDReadable != "DEMO-7" {
		t.Errorf("GET: isErr=%v %q", isErr, text)
	}
	if isErr, text := call(t, cs, "call_api", map[string]any{"method": "POST", "path": "issues/DEMO-7/comments", "body": map[string]any{"text": "hi"}}, nil); isErr {
		t.Errorf("POST: %s", text)
	}
	if b := api.bodies("/api/issues/DEMO-7/comments"); len(b) != 1 || b[0]["text"] != "hi" {
		t.Errorf("POST body = %v", b)
	}
	if isErr, text := call(t, cs, "call_api", map[string]any{"method": "DELETE", "path": "issues/DEMO-7/tags/6-1"}, nil); isErr || !strings.Contains(text, `"ok": true`) {
		t.Errorf("DELETE: isErr=%v %q", isErr, text)
	}
	if isErr, text := call(t, cs, "call_api", map[string]any{"method": "GET", "path": "/hub/api/rest/usergroups", "api": "hub"}, nil); isErr || !strings.Contains(text, "usergroups") {
		t.Errorf("hub GET: isErr=%v %q", isErr, text)
	}
	for _, bad := range []string{"https://evil.example/api/issues", "//evil.example/x", "issues/../admin/users", "../x"} {
		if isErr, text := call(t, cs, "call_api", map[string]any{"method": "GET", "path": bad}, nil); !isErr {
			t.Errorf("path %q accepted: %s", bad, text)
		}
	}
	if isErr, _ := call(t, cs, "call_api", map[string]any{"method": "GET", "path": "issues", "body": map[string]any{"x": 1}}, nil); !isErr {
		t.Error("GET with a body accepted")
	}
}

func TestChangeIssueAssignee(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/issues/DEMO-7", `{"project":{"id":"0-1","shortName":"DEMO"}}`)
	api.handle("POST /api/issues/DEMO-7", func(w http.ResponseWriter, r *http.Request) {
		bodies := api.bodies("/api/issues/DEMO-7")
		w.Header().Set("Content-Type", "application/json")
		v, _ := json.Marshal(bodies[len(bodies)-1]["customFields"].([]any)[0].(map[string]any)["value"])
		fmt.Fprintf(w, `{"customFields":[{"name":"Assignee","$type":"SingleUserIssueCustomField","value":%s}]}`, v)
	})
	cs := connect(t, api)

	var out struct{ Assignee any }
	if isErr, text := call(t, cs, "change_issue_assignee", map[string]any{"issueId": "DEMO-7", "assigneeLogin": "jdoe"}, &out); isErr || out.Assignee != "jdoe" {
		t.Errorf("assign: isErr=%v %q", isErr, text)
	}
	if isErr, text := call(t, cs, "change_issue_assignee", map[string]any{"issueId": "DEMO-7", "assigneeLogin": nil}, &out); isErr || out.Assignee != nil {
		t.Errorf("unassign: isErr=%v %q", isErr, text)
	}
	if isErr, text := call(t, cs, "change_issue_assignee", map[string]any{"issueId": "DEMO-7", "assigneeLogin": "mallory"}, nil); !isErr || !strings.Contains(text, "allowed: jdoe, admin") {
		t.Errorf("not allowed: isErr=%v %q", isErr, text)
	}
}

func TestGetProject(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/admin/projects/0-1", `{"id":"0-1","shortName":"DEMO","name":"Demo","leader":{"login":"bob"},"customFields":[{"field":{"name":"State"}}]}`)
	api.json("GET /api/admin/projects/0-1/timeTrackingSettings", `{"enabled":true,"workItemTypes":[{"id":"w-1","name":"Testing"}]}`)
	api.json("GET /api/admin/projects/0-2", `{"id":"0-2","shortName":"OPS","name":"Operations","customFields":[]}`)
	api.handle("GET /api/admin/projects/0-2/timeTrackingSettings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":"Forbidden"}`)
	})
	api.json("GET /api/admin/projects/0-2/customFields", `[]`)
	api.json("GET /api/issues", `[]`)
	cs := connect(t, api)

	var p struct {
		Key                  string
		CustomFields         []string
		TimeTrackingSettings map[string]any
	}
	if isErr, text := call(t, cs, "get_project", map[string]any{"projectKey": "demo"}, &p); isErr {
		t.Fatal(text)
	}
	if p.Key != "DEMO" || strings.Join(p.CustomFields, ",") != "State" || p.TimeTrackingSettings["enabled"] != true {
		t.Errorf("project = %+v", p)
	}
	if isErr, text := call(t, cs, "get_project", map[string]any{"projectKey": "OPS"}, &p); isErr || p.TimeTrackingSettings["error"] == nil {
		t.Errorf("forbidden time tracking: isErr=%v %q %+v", isErr, text, p)
	}
	if isErr, _ := call(t, cs, "get_project", map[string]any{"projectKey": "NOPE"}, nil); !isErr {
		t.Error("unknown project accepted")
	}
}

func TestFindUser(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("GET /api/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("query") {
		case "bob":
			io.WriteString(w, `[{"login":"bobby","fullName":"Bobby"},{"login":"bob","fullName":"Bob","email":"bob@x","profiles":{"general":{"timezone":{"id":"Europe/Berlin"}}}}]`)
		case "rob":
			io.WriteString(w, `[{"login":"robert","fullName":"Robert"},{"login":"robin","fullName":"Robin"}]`)
		case "solo":
			io.WriteString(w, `[{"login":"solo.han","fullName":"Han"}]`)
		default:
			io.WriteString(w, `[]`)
		}
	})
	cs := connect(t, api)

	var u user
	if isErr, text := call(t, cs, "find_user", map[string]any{"loginOrEmail": "bob"}, &u); isErr || u != (user{Login: "bob", FullName: "Bob", Email: "bob@x", TimeZone: "Europe/Berlin"}) {
		t.Errorf("exact: isErr=%v %q %+v", isErr, text, u)
	}
	if isErr, text := call(t, cs, "find_user", map[string]any{"loginOrEmail": "solo"}, &u); isErr || u.Login != "solo.han" {
		t.Errorf("single: isErr=%v %q", isErr, text)
	}
	if isErr, text := call(t, cs, "find_user", map[string]any{"loginOrEmail": "me"}, &u); isErr || u.Login != "alice" {
		t.Errorf("me: isErr=%v %q", isErr, text)
	}
	if isErr, text := call(t, cs, "find_user", map[string]any{"loginOrEmail": "rob"}, nil); !isErr || !strings.Contains(text, "robert (Robert), robin (Robin)") {
		t.Errorf("ambiguous: isErr=%v %q", isErr, text)
	}
	if isErr, text := call(t, cs, "find_user", map[string]any{"loginOrEmail": "ghost"}, nil); !isErr || !strings.Contains(text, "no user matches") {
		t.Errorf("missing: isErr=%v %q", isErr, text)
	}
}

func TestFindTagsAndAttachmentList(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/tags", `[{"id":"6-1","name":"Backend","owner":{"login":"bob"}},{"id":"6-2","name":"frontend"}]`)
	api.json("GET /api/issues/DEMO-7/attachments", `[{"id":"12-1","name":"a.txt","size":3,"mimeType":"text/plain","author":{"login":"bob"},"created":1767225600000}]`)
	cs := connect(t, api)

	var tags struct {
		Tags  []apiTag
		Total int
	}
	if isErr, text := call(t, cs, "find_tags", map[string]any{"nameContains": "END"}, &tags); isErr || tags.Total != 2 {
		t.Errorf("find_tags: isErr=%v %q", isErr, text)
	}
	var list struct {
		Attachments []map[string]any
	}
	if isErr, text := call(t, cs, "get_issue_attachments", map[string]any{"issueId": "DEMO-7"}, &list); isErr || len(list.Attachments) != 1 || list.Attachments[0]["author"] != "bob" {
		t.Errorf("attachments: isErr=%v %q", isErr, text)
	}
	if isErr, text := call(t, cs, "get_attachment", map[string]any{"issueId": "DEMO-7", "attachment": "zzz"}, nil); !isErr || !strings.Contains(text, "a.txt (12-1)") {
		t.Errorf("unknown attachment: isErr=%v %q", isErr, text)
	}
}

func TestArticleAttachments(t *testing.T) {
	api := newFakeAPI(t)
	var mu sync.Mutex
	var uploaded []string
	api.handle("POST /api/articles/HELP-A-1/attachments", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		var out []map[string]any
		// One file per form field, file0, file1, …, as YouTrack expects.
		for i := range len(r.MultipartForm.File) {
			fh := r.MultipartForm.File[fmt.Sprintf("file%d", i)][0]
			mu.Lock()
			uploaded = append(uploaded, fh.Filename)
			mu.Unlock()
			out = append(out, map[string]any{"id": fmt.Sprintf("12-%d", i+1), "name": fh.Filename, "size": fh.Size, "mimeType": "image/png"})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
	api.json("GET /api/articles/HELP-A-1/attachments", `[{"id":"12-1","name":"one.png","size":3,"mimeType":"image/png","url":"/api/files/12-1?sign=s"}]`)
	var deleted string
	api.handle("DELETE /api/articles/HELP-A-1/attachments/12-1", func(w http.ResponseWriter, r *http.Request) { deleted = r.URL.Path })
	cs := connect(t, api)

	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"one.png", "two.png"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("png"), 0o644)
		paths = append(paths, p)
	}
	var added struct {
		ArticleID   string
		Attachments []struct{ ID, Name string }
	}
	if isErr, text := call(t, cs, "add_attachment", map[string]any{"articleId": "HELP-A-1", "filePaths": paths}, &added); isErr {
		t.Fatal(text)
	}
	if added.ArticleID != "HELP-A-1" || len(added.Attachments) != 2 || strings.Join(uploaded, ",") != "one.png,two.png" {
		t.Errorf("added = %+v, uploaded = %v", added, uploaded)
	}

	var list struct {
		Attachments []struct{ Name string }
	}
	if isErr, text := call(t, cs, "get_issue_attachments", map[string]any{"articleId": "HELP-A-1"}, &list); isErr || len(list.Attachments) != 1 {
		t.Errorf("list: isErr=%v %q", isErr, text)
	}
	if isErr, text := call(t, cs, "delete_attachment", map[string]any{"articleId": "HELP-A-1", "attachment": "one.png"}, nil); isErr || deleted == "" {
		t.Errorf("delete: isErr=%v %q", isErr, text)
	}
	if isErr, text := call(t, cs, "get_issue_attachments", map[string]any{}, nil); !isErr || !strings.Contains(text, "issueId or articleId") {
		t.Errorf("no owner: isErr=%v %q", isErr, text)
	}
	if isErr, text := call(t, cs, "add_attachment", map[string]any{"articleId": "HELP-A-1", "filePaths": paths, "fileName": "x.png"}, nil); !isErr || !strings.Contains(text, "single file") {
		t.Errorf("fileName with many files: isErr=%v %q", isErr, text)
	}
}

func TestFieldSchemaIsCachedAndRefreshedForNewValues(t *testing.T) {
	api := newFakeAPI(t)
	var schemaReads atomic.Int32
	priorities := `[{"name":"Minor"}]`
	var mu sync.Mutex
	api.handle("GET /api/admin/projects/0-2/customFields", func(w http.ResponseWriter, r *http.Request) {
		schemaReads.Add(1)
		mu.Lock()
		values := priorities
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"$type":"EnumProjectCustomField","canBeEmpty":true,"field":{"name":"Priority","fieldType":{"id":"enum[1]","isMultiValue":false,"valueType":"enum"}},"bundle":{"values":%s}}]`, values)
	})
	api.json("POST /api/issues", `{"idReadable":"OPS-9"}`)
	cs := connect(t, api)

	create := func(priority string) (bool, string) {
		return call(t, cs, "create_issue", map[string]any{"project": "OPS", "summary": "x", "customFields": map[string]any{"Priority": priority}}, nil)
	}
	for range 3 {
		if isErr, text := create("Minor"); isErr {
			t.Fatal(text)
		}
	}
	if n := schemaReads.Load(); n != 1 {
		t.Errorf("schema read %d times for three issues, want 1", n)
	}

	// An admin adds a value: the cached schema rejects it, so it is read again.
	mu.Lock()
	priorities = `[{"name":"Minor"},{"name":"Urgent"}]`
	mu.Unlock()
	if isErr, text := create("Urgent"); isErr {
		t.Fatalf("new value after the schema changed: %s", text)
	}
	if n := schemaReads.Load(); n != 2 {
		t.Errorf("schema read %d times, want 2", n)
	}
	if isErr, text := create("Nope"); !isErr || !strings.Contains(text, "allowed: Minor, Urgent") {
		t.Errorf("bad value: isErr=%v %s", isErr, text)
	}
}

func TestErrorsCarryHints(t *testing.T) {
	api := newFakeAPI(t)
	fail := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(body, "{") {
				w.Header().Set("Content-Type", "application/json")
			} else {
				w.Header().Set("Content-Type", "text/html")
			}
			w.WriteHeader(status)
			io.WriteString(w, body)
		}
	}
	api.handle("GET /api/issues/DEMO-404", fail(404, `{"error":"Not Found","error_description":"Entity with id DEMO-404 not found"}`))
	api.handle("GET /api/issues/DEMO-403", fail(403, `{"error":"Forbidden","error_description":"You have no access"}`))
	api.handle("GET /api/articles/DEMO-A-9", fail(404, `{"error":"Not Found","error_description":"Entity not found"}`))
	api.handle("GET /api/issues/DEMO-500", fail(500, "<!DOCTYPE html><html><head><title>500 Internal Server Error</title></head><body><h1>Oops</h1>"+strings.Repeat("x", 2000)+"</body></html>"))
	cs := connect(t, api)

	for _, tc := range []struct {
		tool string
		args map[string]any
		want []string
		not  string
	}{
		{"get_issue", map[string]any{"issueId": "DEMO-404"}, []string{"Entity with id DEMO-404 not found", "Hint: It doesn't exist", "search_issues finds"}, ""},
		{"get_issue", map[string]any{"issueId": "DEMO-403"}, []string{"Hint: This account lacks the YouTrack permission"}, ""},
		{"get_article", map[string]any{"articleId": "DEMO-A-9"}, []string{"search_articles finds"}, ""},
		{"get_issue", map[string]any{"issueId": "DEMO-500"}, []string{"(HTML page) 500 Internal Server Error", "Hint: YouTrack failed on its side"}, "<html"},
		{"get_issue_fields_schema", map[string]any{"projectKey": "NOPE"}, []string{"not found"}, "Hint:"},
	} {
		isErr, text := call(t, cs, tc.tool, tc.args, nil)
		if !isErr {
			t.Errorf("%s %v succeeded", tc.tool, tc.args)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s %v: no %q in %q", tc.tool, tc.args, w, text)
			}
		}
		if tc.not != "" && strings.Contains(text, tc.not) {
			t.Errorf("%s %v: %q in %q", tc.tool, tc.args, tc.not, text)
		}
		if len(text) > 900 {
			t.Errorf("%s %v: %d characters", tc.tool, tc.args, len(text))
		}
	}
}
