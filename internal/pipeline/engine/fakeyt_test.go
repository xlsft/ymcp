package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/youtrack"
	"github.com/xlsft/ymcp/internal/youtrack/youtracktest"
)

// fakeYT is an in-memory YouTrack: issues with a State, comments with ids,
// the comment activity feed with cursors, and commands that set the State.
// Queries understand "State: X", "State: {X Y}", "project: P", parentheses
// and "and". No issue has a tag, and a query by a tag missing from tags is
// refused as YouTrack refuses it.
type fakeYT struct {
	srv *youtracktest.Server

	mu         sync.Mutex
	issues     []*fakeIssue
	comments   []*fakeComment
	activities []fakeActivity
	commands   []string          // "KEY: command"
	failing    map[string]string // command -> YouTrack's error description
	tags       map[string]bool   // tags that exist
	nextID     int
}

type fakeIssue struct {
	ID, Key, Summary, Description, State string
	Deleted                              bool
}

type fakeComment struct {
	ID, IssueID, Author, Text string
	Created, Updated          int64
	Deleted                   bool
}

type fakeActivity struct {
	seq       int
	at        int64
	category  string
	commentID string
}

func newFakeYT(t *testing.T) *fakeYT {
	f := &fakeYT{failing: map[string]string{}, tags: map[string]bool{}, nextID: 100}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/users/me", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]string{"id": "1-1", "login": youtracktest.Login})
	})
	mux.HandleFunc("GET /api/issues", f.search)
	mux.HandleFunc("GET /api/issues/{id}", f.getIssue)
	mux.HandleFunc("GET /api/issues/{id}/comments", f.listComments)
	mux.HandleFunc("POST /api/issues/{id}/comments", f.postComment)
	mux.HandleFunc("POST /api/issues/{id}/comments/{cid}", f.editComment)
	mux.HandleFunc("POST /api/commands", f.command)
	mux.HandleFunc("POST /api/commands/assist", f.assist)
	mux.HandleFunc("GET /api/activitiesPage", f.activitiesPage)
	mux.HandleFunc("GET /api/tags", f.listTags)
	f.srv = youtracktest.New(t, mux)
	return f
}

func (f *fakeYT) client(t *testing.T) *youtrack.Client {
	t.Helper()
	yt, err := youtrack.New(youtrack.Options{BaseURL: f.srv.URL, Login: youtracktest.Login, Password: youtracktest.Password, Proxy: "none"})
	if err != nil {
		t.Fatal(err)
	}
	return yt
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	reply(w, 404, map[string]string{"error": "Not Found", "error_description": "Entity with id not found"})
}

func nowMS() int64 { return time.Now().UnixMilli() }

// addIssue creates an issue in project YPC.
func (f *fakeYT) addIssue(summary, state string) *fakeIssue {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.issues) + 1
	is := &fakeIssue{ID: fmt.Sprintf("3-%d", n), Key: fmt.Sprintf("YPC-%d", n), Summary: summary, Description: "Description of " + summary, State: state}
	f.issues = append(f.issues, is)
	return is
}

func (f *fakeYT) issue(idOrKey string) *fakeIssue {
	for _, is := range f.issues {
		if (is.ID == idOrKey || is.Key == idOrKey) && !is.Deleted {
			return is
		}
	}
	return nil
}

func (f *fakeYT) setState(key, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issue(key).State = state
}

func (f *fakeYT) state(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if is := f.issue(key); is != nil {
		return is.State
	}
	return ""
}

func (f *fakeYT) deleteIssue(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issue(key).Deleted = true
}

func (f *fakeYT) fail(command, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing[command] = description
}

func (f *fakeYT) commandLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.commands)
}

// addComment writes a comment as a human would.
func (f *fakeYT) addComment(key, author, body string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newComment(f.issue(key), author, body)
}

func (f *fakeYT) newComment(is *fakeIssue, author, body string) string {
	f.nextID++
	c := &fakeComment{ID: fmt.Sprintf("4-%d", f.nextID), IssueID: is.ID, Author: author, Text: body, Created: nowMS()}
	f.comments = append(f.comments, c)
	f.activities = append(f.activities, fakeActivity{seq: len(f.activities) + 1, at: c.Created, category: "CommentsCategory", commentID: c.ID})
	return c.ID
}

// issueComments returns the texts of the issue's comments, oldest first.
func (f *fakeYT) issueComments(key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issue(key)
	var out []string
	for _, c := range f.comments {
		if is != nil && c.IssueID == is.ID && !c.Deleted {
			out = append(out, c.Text)
		}
	}
	return out
}

// agentComments returns the bodies (without header) of the marked comments.
func (f *fakeYT) agentComments(key string) []string {
	var out []string
	for _, c := range f.issueComments(key) {
		if _, ok := text.ParseMarker(c); ok {
			out = append(out, text.StripHeader(c))
		}
	}
	return out
}

// restoreComment brings back a comment marked deleted, as the UI's
// restore does: it counts as touched.
func (f *fakeYT) restoreComment(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.comments {
		if c.ID == id {
			c.Deleted, c.Updated = false, nowMS()
			f.activities = append(f.activities, fakeActivity{seq: len(f.activities) + 1, at: c.Updated, category: "CommentsCategory", commentID: c.ID})
		}
	}
}

// editText edits a comment as a human would.
func (f *fakeYT) editText(id, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.comments {
		if c.ID == id {
			c.Text, c.Updated = body, nowMS()
			f.activities = append(f.activities, fakeActivity{seq: len(f.activities) + 1, at: c.Updated, category: "CommentTextCategory", commentID: c.ID})
		}
	}
}

// hiddenComments returns the texts of the issue's comments marked deleted.
func (f *fakeYT) hiddenComments(key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issue(key)
	var out []string
	for _, c := range f.comments {
		if is != nil && c.IssueID == is.ID && c.Deleted {
			out = append(out, c.Text)
		}
	}
	return out
}

func (f *fakeYT) deleteComment(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.comments {
		if c.ID == id {
			c.Deleted = true
		}
	}
}

// matches evaluates the tiny query language of the fake.
func matches(q string, is *fakeIssue) bool {
	q = strings.TrimSpace(q)
	for strings.HasPrefix(q, "(") && closing(q) == len(q)-1 {
		q = strings.TrimSpace(q[1 : len(q)-1])
	}
	depth := 0
	for i := 0; i < len(q); i++ {
		switch q[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && strings.HasPrefix(q[i:], " and ") {
			return matches(q[:i], is) && matches(q[i+5:], is)
		}
	}
	field, value, ok := strings.Cut(q, ":")
	if !ok {
		return false
	}
	value = strings.Trim(strings.TrimSpace(value), "{}")
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "state":
		return is.State == value
	case "project":
		for _, p := range strings.Split(value, ",") {
			if strings.HasPrefix(is.Key, strings.Trim(strings.TrimSpace(p), "{}")+"-") {
				return true
			}
		}
	}
	return false
}

// closing finds the parenthesis closing q[0].
func closing(q string) int {
	depth := 0
	for i := 0; i < len(q); i++ {
		switch q[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return -1
}

func issueJSON(is *fakeIssue, full bool) map[string]any {
	m := map[string]any{"id": is.ID, "idReadable": is.Key, "summary": is.Summary, "updated": nowMS()}
	if full {
		m["description"] = is.Description
		m["customFields"] = []map[string]any{{"name": "State", "$type": "StateIssueCustomField", "value": map[string]string{"name": is.State}}}
	}
	return m
}

var queryTag = regexp.MustCompile(`(?i)tag:\s*(\{[^}]*\}|[^\s()]+)`)

func (f *fakeYT) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	for _, m := range queryTag.FindAllStringSubmatch(q.Get("query"), -1) {
		if !f.tags[strings.Trim(m[1], "{}")] {
			f.mu.Unlock()
			reply(w, 400, map[string]string{"error": "invalid_query", "error_description": "Can't parse search query, please check and update query syntax"})
			return
		}
	}
	var all []map[string]any
	for _, is := range f.issues {
		if !is.Deleted && matches(q.Get("query"), is) {
			all = append(all, issueJSON(is, false))
		}
	}
	f.mu.Unlock()
	skip, _ := strconv.Atoi(q.Get("$skip"))
	top, _ := strconv.Atoi(q.Get("$top"))
	if skip > len(all) {
		skip = len(all)
	}
	all = all[skip:]
	if top > 0 && top < len(all) {
		all = all[:top]
	}
	if all == nil {
		all = []map[string]any{}
	}
	reply(w, 200, all)
}

func (f *fakeYT) listTags(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []map[string]string{}
	for name := range f.tags {
		out = append(out, map[string]string{"name": name})
	}
	reply(w, 200, out)
}

func (f *fakeYT) getIssue(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issue(r.PathValue("id"))
	if is == nil {
		notFound(w)
		return
	}
	reply(w, 200, issueJSON(is, true))
}

func commentJSON(c *fakeComment, is *fakeIssue) map[string]any {
	m := map[string]any{"id": c.ID, "$type": "IssueComment", "deleted": c.Deleted, "created": c.Created,
		"author": map[string]string{"login": c.Author}, "issue": map[string]string{"id": is.ID, "idReadable": is.Key}}
	if !c.Deleted {
		m["text"] = c.Text
	}
	if c.Updated != 0 {
		m["updated"] = c.Updated
	}
	return m
}

func (f *fakeYT) listComments(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issue(r.PathValue("id"))
	if is == nil {
		notFound(w)
		return
	}
	out := []map[string]any{}
	for _, c := range f.comments {
		if c.IssueID == is.ID && !c.Deleted {
			out = append(out, commentJSON(c, is))
		}
	}
	reply(w, 200, out)
}

func readText(r *http.Request) string {
	var body struct {
		Text string `json:"text"`
	}
	b, _ := io.ReadAll(r.Body)
	json.Unmarshal(b, &body)
	return body.Text
}

func (f *fakeYT) postComment(w http.ResponseWriter, r *http.Request) {
	body := readText(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issue(r.PathValue("id"))
	if is == nil {
		notFound(w)
		return
	}
	reply(w, 200, map[string]string{"id": f.newComment(is, youtracktest.Login, body)})
}

func (f *fakeYT) editComment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text    *string `json:"text"`
		Deleted *bool   `json:"deleted"`
	}
	raw, _ := io.ReadAll(r.Body)
	json.Unmarshal(raw, &in)
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issue(r.PathValue("id"))
	if is == nil {
		notFound(w)
		return
	}
	for _, c := range f.comments {
		if c.ID == r.PathValue("cid") && c.IssueID == is.ID && in.Deleted != nil {
			c.Deleted = *in.Deleted // as YouTrack: marked, kept, restorable
			reply(w, 200, map[string]string{"id": c.ID})
			return
		}
		if c.ID == r.PathValue("cid") && c.IssueID == is.ID && !c.Deleted && in.Text != nil {
			c.Text, c.Updated = *in.Text, nowMS()
			f.activities = append(f.activities, fakeActivity{seq: len(f.activities) + 1, at: c.Updated, category: "CommentTextCategory", commentID: c.ID})
			reply(w, 200, map[string]string{"id": c.ID})
			return
		}
	}
	notFound(w)
}

type commandBody struct {
	Query  string              `json:"query"`
	Issues []map[string]string `json:"issues"`
}

func (f *fakeYT) command(w http.ResponseWriter, r *http.Request) {
	var body commandBody
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if desc, bad := f.failing[body.Query]; bad {
		reply(w, 400, map[string]string{"error": "bad_request", "error_description": desc})
		return
	}
	for _, ref := range body.Issues {
		is := f.issue(ref["idReadable"] + ref["id"])
		if is == nil {
			reply(w, 400, map[string]string{"error": "bad_request", "error_description": "Issue not found"})
			return
		}
		f.commands = append(f.commands, is.Key+": "+body.Query)
		if v, ok := strings.CutPrefix(body.Query, "State "); ok {
			is.State = v
		}
	}
	reply(w, 200, map[string]any{})
}

func (f *fakeYT) assist(w http.ResponseWriter, r *http.Request) {
	var body commandBody
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	desc, bad := f.failing[body.Query]
	f.mu.Unlock()
	if bad {
		reply(w, 200, map[string]any{"commands": []map[string]any{{"description": desc, "error": true}}, "suggestions": []any{}})
		return
	}
	reply(w, 200, map[string]any{"commands": []map[string]any{{"description": "Set " + body.Query, "error": false}}})
}

// activitiesPage serves the comment feed. Cursors are "ERH.^+:<ms>" (from
// BootstrapCursor) or "c:<seq>" (from the fake).
func (f *fakeYT) activitiesPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cursor := q.Get("cursor")
	afterSeq, afterMS := 0, int64(-1)
	switch {
	case strings.HasPrefix(cursor, "c:"):
		afterSeq, _ = strconv.Atoi(cursor[2:])
	case strings.HasPrefix(cursor, "ERH.^+:"):
		afterMS, _ = strconv.ParseInt(cursor[len("ERH.^+:"):], 10, 64)
	default:
		reply(w, 400, map[string]string{"error": "bad_request", "error_description": `For input string: "` + cursor + `"`})
		return
	}
	issueQuery := q.Get("issueQuery")
	f.mu.Lock()
	defer f.mu.Unlock()
	acts := []map[string]any{}
	last := 0
	for _, a := range f.activities {
		if a.seq <= afterSeq || (afterMS >= 0 && a.at <= afterMS) {
			continue
		}
		var c *fakeComment
		for _, x := range f.comments {
			if x.ID == a.commentID {
				c = x
			}
		}
		var is *fakeIssue
		for _, x := range f.issues {
			if x.ID == c.IssueID {
				is = x
			}
		}
		if !matches(issueQuery, is) {
			continue
		}
		last = a.seq
		acts = append(acts, map[string]any{"id": fmt.Sprintf("act-%d", a.seq), "timestamp": a.at,
			"category": map[string]string{"id": a.category}, "target": commentJSON(c, is)})
	}
	next := cursor
	if last > 0 {
		next = "c:" + strconv.Itoa(last)
	}
	reply(w, 200, map[string]any{"activities": acts, "afterCursor": next, "hasAfter": false})
}
