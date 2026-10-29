//go:build e2e

package e2e

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture is the project a run works in, made before the tests and deleted
// after them; deleting it removes its issues, articles and work items too.
// Tests share it but nothing in it: each makes the issues and other objects
// it needs and deletes them, so one can run, or fail, on its own.
type fixture struct {
	projectID, key string
	me             struct{ ID, Login string }
	schema         struct {
		Properties map[string]struct {
			Type  string
			Enum  []string
			Items struct{ Enum []string }
		}
		Required []string
	}
}

// project is the run's fixture.
var project *fixture

// enumValue returns the n-th allowed value of a field, skipping the test
// when the project has no such field.
func (f *fixture) enumValue(t *testing.T, field string, n int) string {
	t.Helper()
	p, ok := f.schema.Properties[field]
	if !ok || len(p.Enum) <= n {
		t.Skipf("project has no field %q with %d values", field, n+1)
	}
	return p.Enum[n]
}

// requiredFields fills every required field with its first allowed value.
func (f *fixture) requiredFields() map[string]any {
	fields := map[string]any{}
	for _, name := range f.schema.Required {
		if p := f.schema.Properties[name]; len(p.Enum) > 0 {
			fields[name] = p.Enum[0]
		}
	}
	return fields
}

// newProject creates the run's project, YE<run>, from the default template,
// which makes no agile board.
func newProject() (f *fixture, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	t := &sweepT{}
	f = &fixture{key: "YE" + runID}
	api(t, "GET", "users/me", map[string]any{"fields": "id,login"}, nil, &f.me)
	var created struct{ ID string }
	api(t, "POST", "admin/projects", map[string]any{"fields": "id"},
		map[string]any{"name": marker + " " + f.key, "shortName": f.key, "leader": map[string]any{"id": f.me.ID}}, &created)
	f.projectID = created.ID
	must(t, "get_issue_fields_schema", map[string]any{"projectKey": f.key}, &f.schema)
	if len(f.schema.Properties) == 0 {
		return f, fmt.Errorf("new project %s has no custom fields", f.key)
	}
	return f, nil
}

// delete removes the run's project; the sweep would catch it otherwise.
func (f *fixture) delete() {
	r := call(&sweepT{}, "call_api", map[string]any{"method": "DELETE", "path": "admin/projects/" + f.projectID})
	if r.isError {
		fmt.Printf("e2e: deleting project %s: %s\n", f.key, r.text)
	}
}

// newIssue creates an issue in the run's project with its required fields
// and the given ones, and deletes it when the test ends.
func newIssue(t *testing.T, summary string, fields map[string]any) string {
	t.Helper()
	args := map[string]any{"project": project.key, "summary": marker + " " + summary, "description": "Created by the ymcp E2E suite."}
	all := project.requiredFields()
	for k, v := range fields {
		all[k] = v
	}
	if len(all) > 0 {
		args["customFields"] = all
	}
	var out struct{ ID, URL string }
	must(t, "create_issue", args, &out)
	t.Cleanup(func() { call(t, "delete_issue", map[string]any{"issueId": out.ID}) })
	if !strings.HasPrefix(out.ID, project.key+"-") || !strings.Contains(out.URL, "/issue/"+out.ID) {
		t.Fatalf("create_issue returned %+v", out)
	}
	return out.ID
}

// onePNG is a 1×1 image.
var onePNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==")

func TestProjects(t *testing.T) {
	var found struct {
		Projects []struct{ ShortName string }
	}
	must(t, "find_projects", map[string]any{"nameContains": project.key}, &found)
	if len(found.Projects) != 1 || found.Projects[0].ShortName != project.key {
		t.Errorf("find_projects = %+v", found)
	}
	var p struct {
		Key          string
		CustomFields []string
	}
	must(t, "get_project", map[string]any{"projectKey": project.key}, &p)
	if p.Key != project.key || len(p.CustomFields) == 0 {
		t.Errorf("get_project = %+v", p)
	}
}

func TestIssues(t *testing.T) {
	parent := newIssue(t, "parent", nil)
	child := newIssue(t, "child", nil)
	must(t, "link_issues", map[string]any{"targetIssueId": child, "linkType": "subtask of", "issueToLinkId": parent}, nil)

	var got struct {
		ID, Summary string
		Links       map[string][]string
	}
	must(t, "get_issue", map[string]any{"issueId": child}, &got)
	if got.ID != child || !strings.HasPrefix(got.Summary, marker) || len(got.Links["subtask of"]) != 1 {
		t.Errorf("get_issue = %+v", got)
	}

	priority := project.enumValue(t, "Priority", 1)
	var upd struct{ Changed []string }
	must(t, "update_issue", map[string]any{
		"issueId": parent, "summary": marker + " parent (updated)", "description": "line 1\nline 2",
		"customFields": map[string]any{"Priority": priority}, "subscription": true,
	}, &upd)
	var after struct {
		Summary      string
		CustomFields map[string]any
	}
	must(t, "get_issue", map[string]any{"issueId": parent}, &after)
	if after.Summary != marker+" parent (updated)" || after.CustomFields["Priority"] != priority {
		t.Errorf("after update_issue: %+v", after)
	}
	mustFail(t, "update_issue", map[string]any{"issueId": parent, "customFields": map[string]any{"Priority": "no such value"}}, "not allowed")
	must(t, "update_issue", map[string]any{"issueId": parent, "subscription": false}, nil)

	// By ID, so other tests' issues, or the index catching up on their
	// deletion, don't change the answer.
	query := "issue id: " + parent + ", " + child
	var search struct {
		Issues []struct{ ID string }
	}
	must(t, "search_issues", map[string]any{"query": query + " sort by: created asc", "customFieldsToReturn": []string{}}, &search)
	if len(search.Issues) != 2 || search.Issues[0].ID != parent {
		t.Errorf("search_issues = %+v", search)
	}
	var count struct{ Count int }
	must(t, "count_issues", map[string]any{"query": query}, &count)
	if count.Count != 2 {
		t.Errorf("count_issues = %d", count.Count)
	}

	var assignee struct{ Assignee any }
	must(t, "change_issue_assignee", map[string]any{"issueId": parent, "assigneeLogin": project.me.Login}, &assignee)
	if assignee.Assignee != project.me.Login {
		t.Errorf("assigned to %v", assignee.Assignee)
	}
	must(t, "change_issue_assignee", map[string]any{"issueId": parent, "assigneeLogin": nil}, &assignee)
	if assignee.Assignee != nil {
		t.Errorf("not unassigned: %v", assignee.Assignee)
	}

	var acts struct {
		Activities []struct{ Kind, Field string }
	}
	must(t, "get_issue_activities", map[string]any{"issueId": parent, "categories": []string{"fields", "summary"}}, &acts)
	if len(acts.Activities) == 0 {
		t.Error("no activities after edits")
	}
	var vcs struct{ Changes []any }
	must(t, "get_issue_vcs_changes", map[string]any{"issueId": parent}, &vcs)

	// The search index lags behind deletions, so check each issue directly.
	for _, id := range []string{child, parent} {
		must(t, "delete_issue", map[string]any{"issueId": id}, nil)
		mustFail(t, "get_issue", map[string]any{"issueId": id}, "404")
	}
}

func TestComments(t *testing.T) {
	id := newIssue(t, "comments", nil)
	var c struct {
		Comment struct{ ID string }
	}
	must(t, "add_issue_comment", map[string]any{"issueId": id, "text": marker + " **comment**"}, &c)
	must(t, "update_issue_comment", map[string]any{"issueId": id, "commentId": c.Comment.ID, "text": marker + " edited"}, nil)
	must(t, "delete_issue_comment", map[string]any{"issueId": id, "commentId": c.Comment.ID}, nil)

	var list struct {
		Comments []struct {
			ID, Text string
			Deleted  bool
		}
		Total int
	}
	must(t, "get_issue_comments", map[string]any{"issueId": id}, &list)
	if list.Total != 0 {
		t.Errorf("soft-deleted comment still listed: %+v", list)
	}
	must(t, "get_issue_comments", map[string]any{"issueId": id, "includeDeleted": true}, &list)
	if len(list.Comments) != 1 || !list.Comments[0].Deleted {
		t.Errorf("includeDeleted = %+v", list)
	}
	must(t, "restore_issue_comment", map[string]any{"issueId": id, "commentId": c.Comment.ID}, nil)
	must(t, "get_issue_comments", map[string]any{"issueId": id}, &list)
	if list.Total != 1 || list.Comments[0].Text != marker+" edited" {
		t.Errorf("after restore: %+v", list)
	}
	must(t, "delete_issue_comment", map[string]any{"issueId": id, "commentId": c.Comment.ID, "permanent": true}, nil)
	must(t, "get_issue_comments", map[string]any{"issueId": id, "includeDeleted": true}, &list)
	if len(list.Comments) != 0 {
		t.Errorf("permanently deleted comment remains: %+v", list)
	}
}

func TestTags(t *testing.T) {
	id := newIssue(t, "tags", nil)
	name := marker + " tag " + runID
	var tag struct{ ID, Name string }
	must(t, "create_tag", map[string]any{"name": name}, &tag)
	t.Cleanup(func() { call(t, "delete_tag", map[string]any{"tag": tag.ID}) })

	var tags struct{ Tags []string }
	must(t, "manage_issue_tags", map[string]any{"issueId": id, "tag": tag.ID, "operation": "add"}, &tags)
	if !strings.Contains(strings.Join(tags.Tags, "|"), name) {
		t.Errorf("tag not added: %v", tags.Tags)
	}
	renamed := name + " renamed"
	must(t, "update_tag", map[string]any{"tag": tag.ID, "name": renamed}, nil)
	var found struct{ Total int }
	must(t, "find_tags", map[string]any{"nameContains": renamed}, &found)
	if found.Total != 1 {
		t.Errorf("find_tags after rename = %d", found.Total)
	}
	must(t, "manage_issue_tags", map[string]any{"issueId": id, "tag": renamed, "operation": "remove"}, &tags)
	if strings.Contains(strings.Join(tags.Tags, "|"), name) {
		t.Errorf("tag not removed: %v", tags.Tags)
	}
	must(t, "manage_issue_tags", map[string]any{"issueId": id, "tag": "Star", "operation": "add"}, &tags)
	must(t, "manage_issue_tags", map[string]any{"issueId": id, "tag": "Star", "operation": "remove"}, &tags)
	must(t, "delete_tag", map[string]any{"tag": tag.ID}, nil)
}

func TestLinks(t *testing.T) {
	id := newIssue(t, "links", nil)
	other := newIssue(t, "related", nil)
	var linked struct{ LinkedIssueCounts map[string]int }
	must(t, "link_issues", map[string]any{"targetIssueId": id, "linkType": "relates to", "issueToLinkId": other}, &linked)
	if linked.LinkedIssueCounts["relates to"] != 1 {
		t.Errorf("link counts = %v", linked.LinkedIssueCounts)
	}
	var unlinked struct{ LinkedIssueCounts map[string]int }
	must(t, "unlink_issues", map[string]any{"targetIssueId": id, "linkType": "relates to", "issueToLinkId": other}, &unlinked)
	if unlinked.LinkedIssueCounts["relates to"] != 0 {
		t.Errorf("after unlink = %v", unlinked.LinkedIssueCounts)
	}
	mustFail(t, "link_issues", map[string]any{"targetIssueId": id, "linkType": "no such link", "issueToLinkId": other}, "unknown link type")
}

func TestDeleteAfterConfirmation(t *testing.T) {
	asker, err := startServer("confirm", adminLogin, adminPassword, "YMCP_CONFIRM=on")
	if err != nil {
		t.Fatal(err)
	}
	defer asker.close()
	id := newIssue(t, "deleted after a confirmation", nil)
	var p struct {
		ConfirmationRequired bool
		OperationID, Confirm string
	}
	asker.must(t, "delete_issue", map[string]any{"issueId": id}, &p)
	if !p.ConfirmationRequired || p.Confirm == "" || p.OperationID == "" {
		t.Fatalf("delete_issue without confirm = %+v", p)
	}
	must(t, "get_issue", map[string]any{"issueId": id}, nil)
	asker.mustFail(t, "delete_issue", map[string]any{"issueId": id, "confirm": "wrong"}, "unknown")
	asker.must(t, "delete_issue", map[string]any{"issueId": id, "confirm": p.Confirm}, nil)
	mustFail(t, "get_issue", map[string]any{"issueId": id}, "404")
}

func TestWork(t *testing.T) {
	id := newIssue(t, "work", nil)
	api(t, "POST", "admin/projects/"+project.projectID+"/timeTrackingSettings", map[string]any{"fields": "enabled"}, map[string]any{"enabled": true}, nil)
	var p struct {
		TimeTrackingSettings struct {
			WorkItemTypes []struct{ Name string }
		}
	}
	must(t, "get_project", map[string]any{"projectKey": project.key}, &p)
	args := map[string]any{"issueId": id, "durationMinutes": 90, "description": marker + " work", "date": time.Now().UTC().Format("2006-01-02")}
	if types := p.TimeTrackingSettings.WorkItemTypes; len(types) > 0 {
		args["workType"] = types[0].Name
	}
	var item struct{ ID, Duration string }
	must(t, "log_work", args, &item)
	if item.Duration != "1h 30m" {
		t.Errorf("log_work duration = %q", item.Duration)
	}
	var items struct{ TotalMinutes int }
	must(t, "get_issue_work_items", map[string]any{"issueId": id}, &items)
	if items.TotalMinutes != 90 {
		t.Errorf("work items total = %d", items.TotalMinutes)
	}
	var report struct{ TotalMinutes int }
	must(t, "get_time_report", map[string]any{"query": "issue id: " + id, "author": "me", "groupBy": "issue"}, &report)
	if report.TotalMinutes != 90 {
		t.Errorf("time report = %d", report.TotalMinutes)
	}
	must(t, "delete_work_item", map[string]any{"issueId": id, "workItemId": item.ID}, nil)
	must(t, "get_issue_work_items", map[string]any{"issueId": id}, &items)
	if items.TotalMinutes != 0 {
		t.Errorf("after delete_work_item total = %d", items.TotalMinutes)
	}
}

func TestAttachments(t *testing.T) {
	id := newIssue(t, "attachments", nil)
	var text struct{ ID string }
	must(t, "add_attachment", map[string]any{"issueId": id, "fileName": marker + ".md", "textContent": "# e2e\nhello"}, &text)
	path := filepath.Join(t.TempDir(), marker+".png")
	os.WriteFile(path, onePNG, 0o644)
	var image struct{ ID string }
	must(t, "add_attachment", map[string]any{"issueId": id, "filePath": path}, &image)

	if got := must(t, "get_attachment", map[string]any{"issueId": id, "attachment": text.ID}, nil); got != "# e2e\nhello" {
		t.Errorf("text attachment = %q", got)
	}
	var saved struct{ SavedTo string }
	must(t, "get_attachment", map[string]any{"issueId": id, "attachment": image.ID, "saveTo": t.TempDir()}, &saved)
	if data, _ := os.ReadFile(saved.SavedTo); string(data) != string(onePNG) {
		t.Error("downloaded image differs from the upload")
	}
	var list struct {
		Attachments []struct{ ID string }
	}
	must(t, "get_issue_attachments", map[string]any{"issueId": id}, &list)
	if len(list.Attachments) != 2 {
		t.Errorf("attachments = %+v", list)
	}
	must(t, "delete_attachment", map[string]any{"issueId": id, "attachment": text.ID}, nil)
	must(t, "delete_attachment", map[string]any{"issueId": id, "attachment": image.ID}, nil)
}

func TestCommands(t *testing.T) {
	id := newIssue(t, "commands", nil)
	state := project.enumValue(t, "State", 1)
	cmd := "State " + state // multi-word values go as is, without braces
	var dry struct{ Valid bool }
	must(t, "apply_command", map[string]any{"command": cmd, "issueIds": []string{id}, "dryRun": true}, &dry)
	if !dry.Valid {
		t.Fatalf("dry run says %q is invalid", cmd)
	}
	mustFail(t, "apply_command", map[string]any{"command": "State no-such-state", "issueIds": []string{id}}, "nothing applied")
	must(t, "apply_command", map[string]any{"command": cmd, "query": "issue id: " + id, "comment": marker + " via command"}, nil)
	var got struct{ CustomFields map[string]any }
	must(t, "get_issue", map[string]any{"issueId": id}, &got)
	if got.CustomFields["State"] != state {
		t.Errorf("State after command = %v, want %s", got.CustomFields["State"], state)
	}
}

func TestDrafts(t *testing.T) {
	var draft struct{ DraftID string }
	must(t, "create_draft_issue", map[string]any{"project": project.key, "summary": marker + " draft"}, &draft)
	t.Cleanup(func() { call(t, "delete_issue", map[string]any{"issueId": draft.DraftID}) })
	must(t, "update_issue", map[string]any{"issueId": draft.DraftID, "description": "draft body"}, nil)
	must(t, "edit_issue_description", map[string]any{"issueId": draft.DraftID,
		"edits": []map[string]any{{"oldText": "body", "newText": "text"}}}, nil)
	var got struct{ ID, Description string }
	must(t, "get_issue", map[string]any{"issueId": draft.DraftID}, &got)
	if got.ID != draft.DraftID || got.Description != "draft text" {
		t.Errorf("draft = %+v", got)
	}
	must(t, "delete_issue", map[string]any{"issueId": draft.DraftID}, nil)
}

func TestArticles(t *testing.T) {
	newArticle := func(args map[string]any) string {
		t.Helper()
		var a struct{ ID string }
		must(t, "create_article", args, &a)
		t.Cleanup(func() { call(t, "delete_article", map[string]any{"articleId": a.ID}) })
		return a.ID
	}
	parent := newArticle(map[string]any{"project": project.key, "summary": marker + " article", "content": "# Title\n\nfirst"})
	child := newArticle(map[string]any{"project": project.key, "summary": marker + " child", "content": "child",
		"parentArticle": parent, "permittedUsers": []string{project.me.Login}})
	must(t, "update_article", map[string]any{"articleId": parent, "content": "appended " + runID, "append": true}, nil)
	must(t, "edit_article_content", map[string]any{"articleId": parent,
		"edits": []map[string]any{{"oldText": "first", "newText": "edited"}, {"oldText": `(?m)^# (\w+)$`, "newText": "# $1!", "regexp": true}}}, nil)
	mustFail(t, "edit_article_content", map[string]any{"articleId": parent,
		"edits": []map[string]any{{"oldText": "no such text", "newText": "x"}}}, "not found")

	var got struct {
		Content       string
		ChildArticles []struct{ ID string }
	}
	must(t, "get_article", map[string]any{"articleId": parent}, &got)
	if !strings.HasPrefix(got.Content, "# Title!\n\nedited") || !strings.HasSuffix(got.Content, "appended "+runID) || len(got.ChildArticles) != 1 {
		t.Errorf("article = %+v", got)
	}
	var childGot struct{ Visibility map[string][]string }
	must(t, "get_article", map[string]any{"articleId": child}, &childGot)
	if len(childGot.Visibility["permittedUsers"]) != 1 {
		t.Errorf("child visibility = %+v", childGot.Visibility)
	}
	var found struct {
		Articles []struct{ ID string }
	}
	must(t, "search_articles", map[string]any{"query": "project: " + project.key}, &found)
	if len(found.Articles) != 2 {
		t.Errorf("search_articles = %+v", found)
	}
	// Images for the article, uploaded in one call and referenced by name.
	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"diagram-1.png", "diagram-2.png"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, onePNG, 0o644)
		paths = append(paths, p)
	}
	var uploaded struct {
		Attachments []struct{ ID, Name string }
	}
	must(t, "add_attachment", map[string]any{"articleId": parent, "filePaths": paths}, &uploaded)
	if len(uploaded.Attachments) != 2 {
		t.Fatalf("article upload = %+v", uploaded)
	}
	var withFiles struct {
		Attachments []struct{ Name string }
	}
	must(t, "get_article", map[string]any{"articleId": parent}, &withFiles)
	if len(withFiles.Attachments) != 2 {
		t.Errorf("get_article attachments = %+v", withFiles.Attachments)
	}
	var saved struct{ SavedTo string }
	must(t, "get_attachment", map[string]any{"articleId": parent, "attachment": "diagram-2.png", "saveTo": t.TempDir()}, &saved)
	if data, _ := os.ReadFile(saved.SavedTo); string(data) != string(onePNG) {
		t.Error("article image differs from the upload")
	}
	must(t, "delete_attachment", map[string]any{"articleId": parent, "attachment": "diagram-1.png"}, nil)
	var left struct {
		Attachments []struct{ Name string }
	}
	must(t, "get_issue_attachments", map[string]any{"articleId": parent}, &left)
	if len(left.Attachments) != 1 || left.Attachments[0].Name != "diagram-2.png" {
		t.Errorf("after delete = %+v", left)
	}

	must(t, "delete_article", map[string]any{"articleId": child}, nil)
	must(t, "delete_article", map[string]any{"articleId": parent}, nil)
}

func TestSavedSearches(t *testing.T) {
	var s struct{ ID string }
	must(t, "create_saved_search", map[string]any{"name": marker + " search " + runID, "query": "project: " + project.key}, &s)
	t.Cleanup(func() { call(t, "delete_saved_search", map[string]any{"search": s.ID}) })
	var upd struct{ Query string }
	must(t, "update_saved_search", map[string]any{"search": s.ID, "query": "project: " + project.key + " #Unresolved"}, &upd)
	if !strings.HasSuffix(upd.Query, "#Unresolved") {
		t.Errorf("update_saved_search = %+v", upd)
	}
	must(t, "get_saved_issue_searches", nil, nil)
	must(t, "delete_saved_search", map[string]any{"search": s.ID}, nil)
}

func TestSprints(t *testing.T) {
	id := newIssue(t, "sprints", nil)
	boardName := marker + " board " + runID
	var board struct{ ID string }
	api(t, "POST", "agiles", map[string]any{"fields": "id", "template": "scrum"},
		map[string]any{"name": boardName, "projects": []any{map[string]any{"id": project.projectID}}}, &board)
	t.Cleanup(func() { call(t, "call_api", map[string]any{"method": "DELETE", "path": "agiles/" + board.ID}) })
	api(t, "POST", "agiles/"+board.ID, map[string]any{"fields": "id"}, map[string]any{"sprintsSettings": map[string]any{"disableSprints": false, "isExplicit": true}}, nil)

	var found struct {
		Boards []struct{ ID string }
	}
	must(t, "find_agile_boards", map[string]any{"nameContains": boardName}, &found)
	if len(found.Boards) != 1 {
		t.Fatalf("find_agile_boards = %+v", found)
	}
	must(t, "get_agile_board", map[string]any{"board": board.ID}, nil)

	start := time.Now().UTC()
	var sprint struct {
		Sprint struct{ ID, Name string }
	}
	must(t, "create_sprint", map[string]any{"board": board.ID, "name": marker + " sprint", "goal": "e2e",
		"start": start.Format("2006-01-02"), "finish": start.AddDate(0, 0, 13).Format("2006-01-02")}, &sprint)
	must(t, "update_sprint", map[string]any{"board": board.ID, "sprint": sprint.Sprint.ID, "goal": "e2e updated"}, nil)
	must(t, "add_issue_to_sprint", map[string]any{"board": board.ID, "sprint": sprint.Sprint.ID, "issueId": id}, nil)

	var got struct {
		Sprint struct{ Goal string }
		Issues []struct{ ID string }
	}
	must(t, "get_sprint", map[string]any{"board": board.ID, "sprint": sprint.Sprint.ID}, &got)
	if got.Sprint.Goal != "e2e updated" || len(got.Issues) != 1 || got.Issues[0].ID != id {
		t.Errorf("get_sprint = %+v", got)
	}
	must(t, "remove_issue_from_sprint", map[string]any{"board": board.ID, "sprint": sprint.Sprint.ID, "issueId": id}, nil)
	must(t, "get_sprint", map[string]any{"board": board.ID, "sprint": sprint.Sprint.ID}, &got)
	if len(got.Issues) != 0 {
		t.Errorf("issue still in sprint: %+v", got.Issues)
	}
	must(t, "delete_sprint", map[string]any{"board": board.ID, "sprint": sprint.Sprint.ID}, nil)
}

func TestPeople(t *testing.T) {
	var me struct{ Login string }
	must(t, "find_user", map[string]any{"loginOrEmail": project.me.Login}, &me)
	if me.Login != project.me.Login {
		t.Errorf("find_user = %+v", me)
	}
	var groups struct {
		Groups []struct {
			Name       string
			UsersCount int
		}
	}
	must(t, "find_user_groups", map[string]any{"nameContains": "All Users"}, &groups)
	if len(groups.Groups) == 0 {
		t.Fatal("no All Users group")
	}
	var members struct{ Total int }
	must(t, "get_user_group_members", map[string]any{"group": groups.Groups[0].Name, "limit": 1}, &members)
	if members.Total == 0 {
		t.Errorf("group %q has no members", groups.Groups[0].Name)
	}
}

func TestCallAPI(t *testing.T) {
	id := newIssue(t, "call_api", nil)
	var issue struct{ IDReadable string }
	api(t, "GET", "issues/"+id, map[string]any{"fields": "idReadable"}, nil, &issue)
	if issue.IDReadable != id {
		t.Errorf("call_api GET = %+v", issue)
	}
	var comment struct{ ID string }
	api(t, "POST", "issues/"+id+"/comments", map[string]any{"fields": "id"}, map[string]any{"text": marker + " raw"}, &comment)
	api(t, "DELETE", "issues/"+id+"/comments/"+comment.ID, nil, nil, nil)
	mustFail(t, "call_api", map[string]any{"method": "GET", "path": "https://example.com/api/issues"}, "must be relative")
}
