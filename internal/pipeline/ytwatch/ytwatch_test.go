package ytwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/youtrack"
	"github.com/xlsft/ymcp/internal/youtrack/youtracktest"
)

// fakeAPI serves canned YouTrack JSON and records requests.
type fakeAPI struct {
	mux *http.ServeMux

	mu       sync.Mutex
	requests []*http.Request
	bodies   map[string][]string // path -> raw POST bodies
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{mux: http.NewServeMux(), bodies: map[string][]string{}}
}

func (a *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.requests = append(a.requests, r)
	if r.Method == http.MethodPost {
		data, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(data))
		a.bodies[r.URL.Path] = append(a.bodies[r.URL.Path], string(data))
	}
	a.mu.Unlock()
	a.mux.ServeHTTP(w, r)
}

func (a *fakeAPI) handle(pattern string, h http.HandlerFunc) { a.mux.HandleFunc(pattern, h) }

func (a *fakeAPI) json(pattern string, status int, body string) {
	a.handle(pattern, func(w http.ResponseWriter, r *http.Request) { reply(w, status, body) })
}

func (a *fakeAPI) queries(path string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.requests {
		if r.URL.Path == path {
			out = append(out, r.URL.RawQuery)
		}
	}
	return out
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func newWatcher(t *testing.T, api http.Handler) (*Watcher, *youtracktest.Server) {
	t.Helper()
	srv := youtracktest.New(t, api)
	yt, err := youtrack.New(youtrack.Options{BaseURL: srv.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	return New(yt), srv
}

func TestURLs(t *testing.T) {
	if got := IssueURL("https://x.youtrack.cloud/", "YPC-1"); got != "https://x.youtrack.cloud/issue/YPC-1" {
		t.Errorf("IssueURL = %s", got)
	}
	if got := CommentURL("https://x.youtrack.cloud", "YPC-1", "7-152"); got != "https://x.youtrack.cloud/issue/YPC-1#focus=Comments-7-152.0-0" {
		t.Errorf("CommentURL = %s", got)
	}
}

func TestSearchPages(t *testing.T) {
	api := newFakeAPI()
	all := []string{"3-301", "3-302", "3-303", "3-304", "3-305"}
	api.handle("GET /api/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("query") != "project: YPC" || q.Get("fields") != issueLightFields {
			t.Errorf("query = %v", q)
		}
		skip, _ := strconv.Atoi(q.Get("$skip"))
		top, _ := strconv.Atoi(q.Get("$top"))
		var items []string
		for i := skip; i < min(skip+top, len(all)); i++ {
			items = append(items, fmt.Sprintf(`{"id":%q,"idReadable":"YPC-%d","summary":"S%d","updated":1793107630057,"$type":"Issue"}`, all[i], i+1, i+1))
		}
		reply(w, 200, "["+strings.Join(items, ",")+"]")
	})
	w, srv := newWatcher(t, api)
	w.pageSize = 2

	got, err := w.Search(context.Background(), "project: YPC")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d issues, want 5", len(got))
	}
	if n := len(api.queries("/api/issues")); n != 3 {
		t.Errorf("%d requests, want 3", n)
	}
	want := Issue{ID: "3-303", Key: "YPC-3", Summary: "S3", URL: srv.URL + "/issue/YPC-3", Updated: time.UnixMilli(1793107630057).UTC()}
	if !reflect.DeepEqual(got[2], want) {
		t.Errorf("issue = %+v, want %+v", got[2], want)
	}
}

func TestSearchExactPageBoundary(t *testing.T) {
	api := newFakeAPI()
	api.handle("GET /api/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("$skip") == "0" {
			reply(w, 200, `[{"id":"3-1","idReadable":"A-1"},{"id":"3-2","idReadable":"A-2"}]`)
			return
		}
		reply(w, 200, `[]`)
	})
	w, _ := newWatcher(t, api)
	w.pageSize = 2
	got, err := w.Search(context.Background(), "x")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestIssue(t *testing.T) {
	api := newFakeAPI()
	api.handle("GET /api/issues/YPC-2", func(w http.ResponseWriter, r *http.Request) {
		if f := r.URL.Query().Get("fields"); f != issueFullFields {
			t.Errorf("fields = %s", f)
		}
		reply(w, 200, `{"id":"3-304","idReadable":"YPC-2","summary":"Fix it","description":"Steps","updated":1793107800528,
		 "customFields":[
		  {"name":"State","$type":"StateIssueCustomField","value":{"name":"In Progress","$type":"StateBundleElement"}},
		  {"name":"Assignee","$type":"SingleUserIssueCustomField","value":{"login":"admin","fullName":"Admin","$type":"User"}},
		  {"name":"Fix versions","$type":"MultiVersionIssueCustomField","value":[{"name":"1.0"},{"name":"1.1"}]},
		  {"name":"Estimation","$type":"PeriodIssueCustomField","value":{"presentation":"2h","minutes":120}},
		  {"name":"Due Date","$type":"DateIssueCustomField","value":1793107800000},
		  {"name":"Points","$type":"SimpleIssueCustomField","value":3.5},
		  {"name":"Notes","$type":"TextIssueCustomField","value":{"text":"long text"}},
		  {"name":"Priority","$type":"SingleEnumIssueCustomField","value":null}
		 ]}`)
	})
	w, srv := newWatcher(t, api)
	got, err := w.Issue(context.Background(), "YPC-2")
	if err != nil {
		t.Fatal(err)
	}
	want := Issue{
		ID: "3-304", Key: "YPC-2", Summary: "Fix it", Description: "Steps", URL: srv.URL + "/issue/YPC-2",
		Updated: time.UnixMilli(1793107800528).UTC(),
		Fields: map[string]string{
			"State": "In Progress", "Assignee": "admin", "Fix versions": "1.0, 1.1", "Estimation": "2h",
			"Due Date": time.UnixMilli(1793107800000).UTC().Format(time.RFC3339), "Points": "3.5",
			"Notes": "long text", "Priority": "",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("issue =\n%+v\nwant\n%+v", got, want)
	}
}

func TestExists(t *testing.T) {
	api := newFakeAPI()
	api.json("GET /api/issues/YPC-1", 200, `{"id":"3-1","$type":"Issue"}`)
	api.json("GET /api/issues/YPC-4", 404, `{"error":"Not Found","error_description":"Entity with id YPC-4 not found"}`)
	api.json("GET /api/issues/YPC-5", 500, `{"error":"Internal Server Error","error_description":"boom"}`)
	w, _ := newWatcher(t, api)
	ctx := context.Background()

	for _, tc := range []struct {
		id      string
		want    bool
		wantErr bool
	}{
		{"YPC-1", true, false},
		{"YPC-4", false, false},
		{"YPC-5", false, true},
	} {
		got, err := w.Exists(ctx, tc.id)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("Exists(%s) = %v, %v", tc.id, got, err)
		}
	}
}

func TestMembers(t *testing.T) {
	api := newFakeAPI()
	api.handle("GET /api/issues", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("query"); q != "(State: {In Progress} or State: Review)" {
			t.Errorf("query = %q", q)
		}
		reply(w, 200, `[{"id":"3-302","idReadable":"YPC-2"},{"id":"3-303","idReadable":"YPC-3"},{"id":"3-309","idReadable":"YPC-9"}]`)
	})
	w, _ := newWatcher(t, api)
	got, err := w.Members(context.Background(), "State: {In Progress} or State: Review", []string{"3-302", "YPC-3", "3-304", "YPC-4"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"3-302": true, "YPC-3": true, "3-304": false, "YPC-4": false}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Members = %v, want %v", got, want)
	}
	for _, q := range api.queries("/api/issues") {
		if strings.Contains(q, "issue+id") || strings.Contains(q, "YPC-4") {
			t.Errorf("query lists ids: %s", q)
		}
	}

	if _, err := w.Members(context.Background(), " ", []string{"3-302"}); err == nil {
		t.Error("empty stage query: want error")
	}
	n := len(api.queries("/api/issues"))
	got, err = w.Members(context.Background(), "State: Open", nil)
	if err != nil || len(got) != 0 || len(api.queries("/api/issues")) != n {
		t.Errorf("no ids: %v, %v, requests %d -> %d", got, err, n, len(api.queries("/api/issues")))
	}
}

func TestApply(t *testing.T) {
	api := newFakeAPI()
	api.handle("POST /api/commands", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body struct {
			Query  string              `json:"query"`
			Issues []map[string]string `json:"issues"`
		}
		json.Unmarshal(data, &body)
		switch body.Query {
		case "State In Progress":
			reply(w, 200, `{"query":"State In Progress","issues":[{"idReadable":"YPC-2"}],"caret":0,"suggestions":null}`)
		case "State Bogus":
			reply(w, 400, `{"error":"Bad Request","error_description":"State expected: Bogus"}`)
		default:
			reply(w, 503, `{"error":"Service Unavailable"}`)
		}
	})
	w, _ := newWatcher(t, api)
	ctx := context.Background()

	if err := w.Apply(ctx, "State In Progress", "YPC-2"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := api.bodies["/api/commands"][0]; got != `{"issues":[{"idReadable":"YPC-2"}],"query":"State In Progress"}` {
		t.Errorf("body = %s", got)
	}
	if err := w.Apply(ctx, "State In Progress", "3-304"); err != nil {
		t.Fatalf("Apply by db id: %v", err)
	}
	if got := api.bodies["/api/commands"][1]; !strings.Contains(got, `"issues":[{"id":"3-304"}]`) {
		t.Errorf("body = %s", got)
	}

	err := w.Apply(ctx, "State Bogus", "YPC-2")
	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *CommandError", err)
	}
	if ce.Description != "State expected: Bogus" || ce.Command != "State Bogus" || ce.Issue != "YPC-2" || ce.Err.Status != 400 {
		t.Errorf("CommandError = %+v", ce)
	}
	var ye *youtrack.Error
	if !errors.As(err, &ye) {
		t.Error("CommandError does not unwrap to *youtrack.Error")
	}

	if err := w.Apply(ctx, "", "YPC-2"); err == nil {
		t.Error("empty command: want error")
	}
}

func TestApplyServerErrorIsNotCommandError(t *testing.T) {
	api := newFakeAPI()
	api.json("POST /api/commands", 500, `{"error":"Internal Server Error","error_description":"boom"}`)
	w, _ := newWatcher(t, api)
	err := w.Apply(context.Background(), "State Done", "YPC-2")
	var ce *CommandError
	if err == nil || errors.As(err, &ce) {
		t.Errorf("err = %v, want a plain error", err)
	}
}

func TestValidate(t *testing.T) {
	// Answers recorded from example.youtrack.cloud (commands/assist on YPC-1).
	answers := map[string]string{
		"State In Progress": `{"suggestions":[{"option":"In Progress","description":"State"},{"option":"State In Progress","description":"at 13:34"}],
			"commands":[{"error":false,"delete":false,"description":"Set State <span class=\"bold\">In Progress</span>"}]}`,
		"State": `{"suggestions":[{"option":"State","description":"change State"},{"option":"State Submitted","description":"at 13:34"}],
			"commands":[{"error":true,"delete":false,"description":"State expected"}]}`,
		"State In Progress Assignee": `{"suggestions":[{"option":"Assignee","description":"change Assignee"}],
			"commands":[{"error":false,"description":"Set State <span class=\"bold\">In Progress</span>"},{"error":true,"description":"Assignee expected"}]}`,
		"State Bogus":     `{"suggestions":[],"commands":[{"error":true,"delete":false,"description":"State expected: Bogus"}]}`,
		"Nonexistent foo": `{"suggestions":[],"commands":[{"error":true,"delete":false,"description":"Unknown command: Nonexistent foo"}]}`,
		"":                `{"suggestions":[{"option":"+1","description":"command"}],"commands":[]}`,
	}
	api := newFakeAPI()
	api.handle("POST /api/commands/assist", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query  string              `json:"query"`
			Caret  int                 `json:"caret"`
			Issues []map[string]string `json:"issues"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Caret != len([]rune(body.Query)) || len(body.Issues) != 1 || body.Issues[0]["idReadable"] != "YPC-1" {
			t.Errorf("body = %+v", body)
		}
		if body.Query == "missing issue" {
			reply(w, 400, `{"error":"Bad Request","error_description":"YouTrack is unable to locate an Issue-type entity with the specified ID"}`)
			return
		}
		reply(w, 200, answers[body.Query])
	})
	w, _ := newWatcher(t, api)
	ctx := context.Background()

	for _, tc := range []struct {
		command string
		state   CommandState
		text    string
	}{
		{"State In Progress", Valid, "Set State In Progress"},
		{"State", Incomplete, "State expected"},
		{"State In Progress Assignee", Incomplete, "Assignee expected"},
		{"State Bogus", Invalid, "State expected: Bogus"},
		{"Nonexistent foo", Invalid, "Unknown command: Nonexistent foo"},
		{"", Incomplete, "empty command"},
	} {
		got, err := w.Validate(ctx, tc.command, "YPC-1")
		if err != nil {
			t.Errorf("Validate(%q): %v", tc.command, err)
			continue
		}
		if got.State != tc.state || got.Text != tc.text {
			t.Errorf("Validate(%q) = %+v, want %s %q", tc.command, got, tc.state, tc.text)
		}
	}
	got, _ := w.Validate(ctx, "State", "YPC-1")
	if want := []string{"State", "State Submitted"}; !reflect.DeepEqual(got.Suggestions, want) {
		t.Errorf("suggestions = %v, want %v", got.Suggestions, want)
	}
	if _, err := w.Validate(ctx, "missing issue", "YPC-1"); err == nil {
		t.Error("400 from assist: want error")
	}
	if _, err := w.Validate(ctx, "State Done", ""); err == nil {
		t.Error("no issue: want error")
	}
}

func TestMe(t *testing.T) {
	api := newFakeAPI()
	api.json("GET /api/users/me", 200, `{"login":"admin","$type":"Me"}`)
	w, _ := newWatcher(t, api)
	got, err := w.Me(context.Background())
	if err != nil || got != "admin" {
		t.Errorf("Me = %q, %v", got, err)
	}
}

func TestServerTime(t *testing.T) {
	// A server whose clock is off: the Date header wins over the local clock.
	date := time.Date(2026, 10, 27, 13, 27, 10, 0, time.UTC)
	api := newFakeAPI()
	api.handle("GET /api/users/me", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Date", date.Format(http.TimeFormat))
		reply(rw, 200, `{"id":"1-1","login":"admin"}`)
	})
	w, _ := newWatcher(t, api)
	got, err := w.ServerTime(context.Background())
	if err != nil || !got.Equal(date) {
		t.Errorf("ServerTime = %v, %v; want %v", got, err, date)
	}
	cursor, err := w.BootstrapCursor(context.Background())
	if want := "ERH.^+:" + strconv.FormatInt(date.Add(-bootstrapLag).UnixMilli(), 10); err != nil || cursor != want {
		t.Errorf("BootstrapCursor = %q, %v; want %q", cursor, err, want)
	}
}

// Activities shaped like the real feed (see the research notes).
const (
	actCreated153 = `{"id":"7-153.0-0","$type":"CommentActivityItem","category":{"id":"CommentsCategory"},"timestamp":1793107695000,
		"target":{"id":"7-153","$type":"IssueComment","deleted":false,"text":"comment A1","created":1793107695000,"updated":null,
		 "author":{"login":"admin"},"issue":{"id":"3-302","idReadable":"YPC-2"},"visibility":{"$type":"UnlimitedVisibility"}}}`
	actEdited153 = `{"id":"0-0.14-2302","$type":"TextMarkupActivityItem","category":{"id":"CommentTextCategory"},"timestamp":1793107723582,
		"target":{"id":"7-153","$type":"IssueComment","deleted":false,"text":"comment A1 (EDITED twice)","created":1793107695000,"updated":1793107723582,
		 "author":{"login":"admin"},"issue":{"id":"3-302","idReadable":"YPC-2"},"visibility":{"$type":"UnlimitedVisibility"}}}`
	actEdited154 = `{"id":"0-0.14-2303","$type":"TextMarkupActivityItem","category":{"id":"CommentTextCategory"},"timestamp":1793107781000,
		"target":{"id":"7-154","$type":"IssueComment","deleted":false,"text":"B1 edited","created":1793107600000,"updated":1793107781000,
		 "author":{"login":"jdoe"},"issue":{"id":"3-303","idReadable":"YPC-3"},"visibility":{"$type":"UnlimitedVisibility"}}}`
	actDeleted161 = `{"id":"7-161.0-0","$type":"CommentActivityItem","category":{"id":"CommentsCategory"},"timestamp":1793108662744,
		"target":{"id":"7-161","$type":"IssueComment","deleted":true,"text":null,"created":1793108662744,"updated":null,
		 "author":{"login":"admin"},"issue":{"id":"3-305","idReadable":"YPC-4"}}}`
	actPrivate162 = `{"id":"7-162.0-0","$type":"CommentActivityItem","category":{"id":"CommentsCategory"},"timestamp":1793108700000,
		"target":{"id":"7-162","$type":"IssueComment","deleted":false,"text":"team only","created":1793108700000,"updated":null,
		 "author":{"login":"admin"},"issue":{"id":"3-305","idReadable":"YPC-4"},
		 "visibility":{"$type":"LimitedVisibility","permittedGroups":[{"name":"ymcp pipeline checks Team","$type":"ProjectTeam"}],"permittedUsers":[{"login":"jdoe"}]}}}`
)

func page(acts []string, after string, hasAfter bool) string {
	return fmt.Sprintf(`{"activities":[%s],"afterCursor":%q,"hasAfter":%v,"$type":"ActivityCursorPage"}`,
		strings.Join(acts, ","), after, hasAfter)
}

// feed serves activitiesPage answers keyed by the cursor parameter.
func feed(t *testing.T, pages map[string]string) *fakeAPI {
	api := newFakeAPI()
	api.handle("GET /api/activitiesPage", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("categories") != "CommentsCategory,CommentTextCategory" {
			t.Errorf("categories = %s", q.Get("categories"))
		}
		if q.Get("fields") != commentActivityFields {
			t.Errorf("fields = %s", q.Get("fields"))
		}
		if strings.Contains(q.Get("issueQuery"), "issue id") {
			t.Errorf("issueQuery lists ids: %s", q.Get("issueQuery"))
		}
		body, ok := pages[q.Get("cursor")]
		if !ok {
			t.Errorf("unexpected cursor %q", q.Get("cursor"))
			reply(w, 400, `{"error":"bad_request","error_description":"For input string: \"x\""}`)
			return
		}
		reply(w, 200, body)
	})
	return api
}

func TestCommentsPagesAndEdits(t *testing.T) {
	api := feed(t, map[string]string{
		"ERH.14-2300+:1793107684593": page([]string{actCreated153, actEdited153}, "ERH.14-2302+:1793107723582", true),
		"ERH.14-2302+:1793107723582": page([]string{actEdited154, actDeleted161, actPrivate162}, "ERH.14-2324+:1793108700000", false),
	})
	w, srv := newWatcher(t, api)
	items, next, err := w.Comments(context.Background(), []string{"YPC", "My Project"}, "ERH.14-2300+:1793107684593")
	if err != nil {
		t.Fatal(err)
	}
	if next != "ERH.14-2324+:1793108700000" {
		t.Errorf("next = %s", next)
	}
	qs := api.queries("/api/activitiesPage")
	if len(qs) != 2 {
		t.Fatalf("%d requests, want 2", len(qs))
	}
	if !strings.Contains(qs[0], "issueQuery=project%3A+YPC%2C+%7BMy+Project%7D") {
		t.Errorf("issueQuery not project: YPC, {My Project}: %s", qs[0])
	}

	want := []Comment{
		{ID: "7-153", IssueID: "3-302", IssueKey: "YPC-2", Author: "admin", Text: "comment A1 (EDITED twice)",
			URL: srv.URL + "/issue/YPC-2#focus=Comments-7-153.0-0", Created: time.UnixMilli(1793107695000).UTC(),
			Updated: time.UnixMilli(1793107723582).UTC()},
		{ID: "7-154", IssueID: "3-303", IssueKey: "YPC-3", Author: "jdoe", Text: "B1 edited",
			URL: srv.URL + "/issue/YPC-3#focus=Comments-7-154.0-0", Created: time.UnixMilli(1793107600000).UTC(),
			Updated: time.UnixMilli(1793107781000).UTC(), Edited: true},
		{ID: "7-161", IssueID: "3-305", IssueKey: "YPC-4", Author: "admin", Deleted: true,
			URL: srv.URL + "/issue/YPC-4#focus=Comments-7-161.0-0", Created: time.UnixMilli(1793108662744).UTC()},
		{ID: "7-162", IssueID: "3-305", IssueKey: "YPC-4", Author: "admin", Text: "team only",
			URL: srv.URL + "/issue/YPC-4#focus=Comments-7-162.0-0", Created: time.UnixMilli(1793108700000).UTC(),
			Visibility: "ymcp pipeline checks Team, jdoe"},
	}
	if !reflect.DeepEqual(items, want) {
		t.Errorf("items =\n%+v\nwant\n%+v", items, want)
	}
}

func TestCommentsEmptyPageKeepsCursor(t *testing.T) {
	for _, after := range []string{"ERH.14-2324+:1793108700000", "ERH.^+:-1", ""} {
		api := feed(t, map[string]string{"ERH.14-2324+:1793108700000": page(nil, after, false)})
		w, _ := newWatcher(t, api)
		items, next, err := w.Comments(context.Background(), []string{"YPC"}, "ERH.14-2324+:1793108700000")
		if err != nil || len(items) != 0 || next != "ERH.14-2324+:1793108700000" {
			t.Errorf("afterCursor %q: items %v, next %q, err %v", after, items, next, err)
		}
	}
}

func TestCommentsRestartCursorIgnored(t *testing.T) {
	date := time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)
	boot := timeCursor(date.Add(-bootstrapLag))
	// The bootstrap page is empty and comes back with the restart sentinel.
	api := feed(t, map[string]string{boot: `{"activities":[],"beforeCursor":"ERH.^+:-1","afterCursor":"ERH.^+:-1","hasAfter":false}`})
	api.handle("GET /api/users/me", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Date", date.Format(http.TimeFormat))
		reply(rw, 200, `{"id":"1-1"}`)
	})
	w, _ := newWatcher(t, api)
	ctx := context.Background()

	cursor, err := w.BootstrapCursor(ctx)
	if err != nil || cursor != boot {
		t.Fatalf("BootstrapCursor = %q, %v", cursor, err)
	}
	for _, in := range []string{"", "ERH.^+:-1"} {
		items, next, err := w.Comments(ctx, []string{"YPC"}, in)
		if err != nil || len(items) != 0 || next != boot {
			t.Errorf("cursor %q: items %v, next %q, err %v; want next %q", in, items, next, err, boot)
		}
	}
	for _, q := range api.queries("/api/activitiesPage") {
		if strings.Contains(q, "%3A-1") {
			t.Errorf("restart cursor sent: %s", q)
		}
	}
}

func TestCommentsRestartCursorMidStream(t *testing.T) {
	// hasAfter with a restart cursor must not loop or replay history.
	api := feed(t, map[string]string{"C1": page([]string{actCreated153}, "ERH.^+:-1", true)})
	w, _ := newWatcher(t, api)
	items, next, err := w.Comments(context.Background(), []string{"YPC"}, "C1")
	if err != nil || len(items) != 1 || next != "C1" {
		t.Errorf("items %v, next %q, err %v", items, next, err)
	}
	if n := len(api.queries("/api/activitiesPage")); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

func TestCommentsNoProjects(t *testing.T) {
	api := newFakeAPI()
	w, _ := newWatcher(t, api)
	items, next, err := w.Comments(context.Background(), nil, "ERH.14-1+:1")
	if err != nil || items != nil || next != "ERH.14-1+:1" || len(api.queries("/api/activitiesPage")) != 0 {
		t.Errorf("items %v, next %q, err %v", items, next, err)
	}
}

func TestCommentsErrors(t *testing.T) {
	api := newFakeAPI()
	api.handle("GET /api/activitiesPage", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cursor") {
		case "garbage":
			reply(w, 400, `{"error":"bad_request","error_description":"For input string: \"garbage\""}`)
		case "dead-project":
			reply(w, 400, `{"error":"Bad Request","error_description":"Could not parse query [project: GONE]"}`)
		default:
			reply(w, 500, `{"error":"Internal Server Error"}`)
		}
	})
	w, _ := newWatcher(t, api)
	ctx := context.Background()

	_, next, err := w.Comments(ctx, []string{"YPC"}, "garbage")
	if !errors.Is(err, ErrBadCursor) || next != "" {
		t.Errorf("garbage cursor: next %q, err %v", next, err)
	}
	var ye *youtrack.Error
	if !errors.As(err, &ye) || ye.Status != 400 {
		t.Errorf("garbage cursor: err %v does not wrap *youtrack.Error", err)
	}
	for _, c := range []string{"dead-project", "other"} {
		if _, _, err := w.Comments(ctx, []string{"GONE"}, c); err == nil || errors.Is(err, ErrBadCursor) {
			t.Errorf("cursor %s: err %v", c, err)
		}
	}
}

func TestProjectQuery(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{"YPC"}, "project: YPC"},
		{[]string{"YPC", "BE_2", "ops-x"}, "project: YPC, BE_2, ops-x"},
		{[]string{"My Project"}, "project: {My Project}"},
	} {
		if got := projectQuery(tc.in); got != tc.want {
			t.Errorf("projectQuery(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
