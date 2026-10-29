//go:build e2e

package e2e

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fixture is the project a run works in; deleting it removes its issues,
// articles and work items too.
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
func (f *fixture) requiredFields(t *testing.T) map[string]any {
	fields := map[string]any{}
	for _, name := range f.schema.Required {
		if p := f.schema.Properties[name]; len(p.Enum) > 0 {
			fields[name] = p.Enum[0]
		}
	}
	return fields
}

var fixtures atomic.Int32

func newFixture(t *testing.T) *fixture {
	f := &fixture{}
	api(t, "GET", "users/me", map[string]any{"fields": "id,login"}, nil, &f.me)

	// Each fixture gets its own project: YE<run><A, B, …>.
	n := fixtures.Add(1)
	f.key = fmt.Sprintf("YE%s%c", runID, 'A'+n-1)
	var created struct{ ID, ShortName string }
	api(t, "POST", "admin/projects", map[string]any{"fields": "id,shortName", "template": "scrum"},
		map[string]any{"name": marker + " " + f.key, "shortName": f.key, "leader": map[string]any{"id": f.me.ID}}, &created)
	f.projectID = created.ID
	t.Cleanup(func() {
		r := call(t, "call_api", map[string]any{"method": "DELETE", "path": "admin/projects/" + f.projectID})
		if r.isError {
			t.Errorf("cleanup: deleting project %s: %s", f.key, r.text)
		}
	})
	must(t, "get_issue_fields_schema", map[string]any{"projectKey": f.key}, &f.schema)
	if len(f.schema.Properties) == 0 {
		t.Fatal("new project has no custom fields")
	}
	return f
}

func TestScenarios(t *testing.T) {
	f := newFixture(t)
	issue := func(summary string, fields map[string]any) string {
		t.Helper()
		args := map[string]any{"project": f.key, "summary": marker + " " + summary, "description": "Created by the ymcp E2E suite."}
		all := f.requiredFields(t)
		for k, v := range fields {
			all[k] = v
		}
		if len(all) > 0 {
			args["customFields"] = all
		}
		var out struct{ ID, URL string }
		must(t, "create_issue", args, &out)
		if !strings.HasPrefix(out.ID, f.key+"-") || !strings.Contains(out.URL, "/issue/"+out.ID) {
			t.Fatalf("create_issue returned %+v", out)
		}
		return out.ID
	}

	var parent, child string

	t.Run("projects", func(t *testing.T) {
		var found struct {
			Projects []struct{ ShortName string }
		}
		must(t, "find_projects", map[string]any{"nameContains": f.key}, &found)
		if len(found.Projects) != 1 || found.Projects[0].ShortName != f.key {
			t.Errorf("find_projects = %+v", found)
		}
		var p struct {
			Key          string
			CustomFields []string
		}
		must(t, "get_project", map[string]any{"projectKey": f.key}, &p)
		if p.Key != f.key || len(p.CustomFields) == 0 {
			t.Errorf("get_project = %+v", p)
		}
	})

	t.Run("issues", func(t *testing.T) {
		parent = issue("parent", nil)
		child = issue("child", nil)
		must(t, "link_issues", map[string]any{"targetIssueId": child, "linkType": "subtask of", "issueToLinkId": parent}, nil)

		var got struct {
			ID, Summary string
			Links       map[string][]string
		}
		must(t, "get_issue", map[string]any{"issueId": child}, &got)
		if got.ID != child || !strings.HasPrefix(got.Summary, marker) || len(got.Links["subtask of"]) != 1 {
			t.Errorf("get_issue = %+v", got)
		}

		priority := f.enumValue(t, "Priority", 1)
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

		var search struct {
			Issues []struct{ ID string }
		}
		must(t, "search_issues", map[string]any{"query": "project: " + f.key + " sort by: created asc", "customFieldsToReturn": []string{}}, &search)
		if len(search.Issues) < 2 || search.Issues[0].ID != parent {
			t.Errorf("search_issues = %+v", search)
		}
		var count struct{ Count int }
		must(t, "count_issues", map[string]any{"query": "project: " + f.key}, &count)
		if count.Count < 2 {
			t.Errorf("count_issues = %d", count.Count)
		}

		var assignee struct{ Assignee any }
		must(t, "change_issue_assignee", map[string]any{"issueId": parent, "assigneeLogin": f.me.Login}, &assignee)
		if assignee.Assignee != f.me.Login {
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
	})

	t.Run("comments", func(t *testing.T) {
		var c struct {
			Comment struct{ ID string }
		}
		must(t, "add_issue_comment", map[string]any{"issueId": parent, "text": marker + " **comment**"}, &c)
		must(t, "update_issue_comment", map[string]any{"issueId": parent, "commentId": c.Comment.ID, "text": marker + " edited"}, nil)
		must(t, "delete_issue_comment", map[string]any{"issueId": parent, "commentId": c.Comment.ID}, nil)

		var list struct {
			Comments []struct {
				ID, Text string
				Deleted  bool
			}
			Total int
		}
		must(t, "get_issue_comments", map[string]any{"issueId": parent}, &list)
		if list.Total != 0 {
			t.Errorf("soft-deleted comment still listed: %+v", list)
		}
		must(t, "get_issue_comments", map[string]any{"issueId": parent, "includeDeleted": true}, &list)
		if len(list.Comments) != 1 || !list.Comments[0].Deleted {
			t.Errorf("includeDeleted = %+v", list)
		}
		must(t, "restore_issue_comment", map[string]any{"issueId": parent, "commentId": c.Comment.ID}, nil)
		must(t, "get_issue_comments", map[string]any{"issueId": parent}, &list)
		if list.Total != 1 || list.Comments[0].Text != marker+" edited" {
			t.Errorf("after restore: %+v", list)
		}
		must(t, "delete_issue_comment", map[string]any{"issueId": parent, "commentId": c.Comment.ID, "permanent": true}, nil)
		must(t, "get_issue_comments", map[string]any{"issueId": parent, "includeDeleted": true}, &list)
		if len(list.Comments) != 0 {
			t.Errorf("permanently deleted comment remains: %+v", list)
		}
	})

	t.Run("tags", func(t *testing.T) {
		name := marker + " tag " + runID
		var tag struct{ ID, Name string }
		must(t, "create_tag", map[string]any{"name": name}, &tag)
		t.Cleanup(func() { call(t, "delete_tag", map[string]any{"tag": tag.ID}) })

		var tags struct{ Tags []string }
		must(t, "manage_issue_tags", map[string]any{"issueId": parent, "tag": tag.ID, "operation": "add"}, &tags)
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
		must(t, "manage_issue_tags", map[string]any{"issueId": parent, "tag": renamed, "operation": "remove"}, &tags)
		if strings.Contains(strings.Join(tags.Tags, "|"), name) {
			t.Errorf("tag not removed: %v", tags.Tags)
		}
		must(t, "manage_issue_tags", map[string]any{"issueId": parent, "tag": "Star", "operation": "add"}, &tags)
		must(t, "manage_issue_tags", map[string]any{"issueId": parent, "tag": "Star", "operation": "remove"}, &tags)
		must(t, "delete_tag", map[string]any{"tag": tag.ID}, nil)
	})

	t.Run("links", func(t *testing.T) {
		other := issue("related", nil)
		var linked struct{ LinkedIssueCounts map[string]int }
		must(t, "link_issues", map[string]any{"targetIssueId": parent, "linkType": "relates to", "issueToLinkId": other}, &linked)
		if linked.LinkedIssueCounts["relates to"] != 1 {
			t.Errorf("link counts = %v", linked.LinkedIssueCounts)
		}
		var unlinked struct{ LinkedIssueCounts map[string]int }
		must(t, "unlink_issues", map[string]any{"targetIssueId": parent, "linkType": "relates to", "issueToLinkId": other}, &unlinked)
		if unlinked.LinkedIssueCounts["relates to"] != 0 {
			t.Errorf("after unlink = %v", unlinked.LinkedIssueCounts)
		}
		mustFail(t, "link_issues", map[string]any{"targetIssueId": parent, "linkType": "no such link", "issueToLinkId": other}, "unknown link type")
		must(t, "delete_issue", map[string]any{"issueId": other}, nil)
		mustFail(t, "get_issue", map[string]any{"issueId": other}, "404")
	})

	t.Run("delete only after a confirmation", func(t *testing.T) {
		asker, err := startServer("confirm", adminLogin, adminPassword, "YMCP_CONFIRM=on")
		if err != nil {
			t.Fatal(err)
		}
		defer asker.close()
		id := issue("deleted after a confirmation", nil)
		t.Cleanup(func() { call(t, "delete_issue", map[string]any{"issueId": id}) })
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
	})

	t.Run("work", func(t *testing.T) {
		api(t, "POST", "admin/projects/"+f.projectID+"/timeTrackingSettings", map[string]any{"fields": "enabled"}, map[string]any{"enabled": true}, nil)
		var project struct {
			TimeTrackingSettings struct {
				WorkItemTypes []struct{ Name string }
			}
		}
		must(t, "get_project", map[string]any{"projectKey": f.key}, &project)
		args := map[string]any{"issueId": parent, "durationMinutes": 90, "description": marker + " work", "date": time.Now().UTC().Format("2006-01-02")}
		if types := project.TimeTrackingSettings.WorkItemTypes; len(types) > 0 {
			args["workType"] = types[0].Name
		}
		var item struct{ ID, Duration string }
		must(t, "log_work", args, &item)
		if item.Duration != "1h 30m" {
			t.Errorf("log_work duration = %q", item.Duration)
		}
		var items struct{ TotalMinutes int }
		must(t, "get_issue_work_items", map[string]any{"issueId": parent}, &items)
		if items.TotalMinutes != 90 {
			t.Errorf("work items total = %d", items.TotalMinutes)
		}
		var report struct{ TotalMinutes int }
		must(t, "get_time_report", map[string]any{"query": "project: " + f.key, "author": "me", "groupBy": "issue"}, &report)
		if report.TotalMinutes != 90 {
			t.Errorf("time report = %d", report.TotalMinutes)
		}
		must(t, "delete_work_item", map[string]any{"issueId": parent, "workItemId": item.ID}, nil)
		must(t, "get_issue_work_items", map[string]any{"issueId": parent}, &items)
		if items.TotalMinutes != 0 {
			t.Errorf("after delete_work_item total = %d", items.TotalMinutes)
		}
	})

	t.Run("attachments", func(t *testing.T) {
		var text struct{ ID string }
		must(t, "add_attachment", map[string]any{"issueId": parent, "fileName": marker + ".md", "textContent": "# e2e\nhello"}, &text)
		png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==")
		path := filepath.Join(t.TempDir(), marker+".png")
		os.WriteFile(path, png, 0o644)
		var image struct{ ID string }
		must(t, "add_attachment", map[string]any{"issueId": parent, "filePath": path}, &image)

		if got := must(t, "get_attachment", map[string]any{"issueId": parent, "attachment": text.ID}, nil); got != "# e2e\nhello" {
			t.Errorf("text attachment = %q", got)
		}
		dir := t.TempDir()
		var saved struct{ SavedTo string }
		must(t, "get_attachment", map[string]any{"issueId": parent, "attachment": image.ID, "saveTo": dir}, &saved)
		if data, _ := os.ReadFile(saved.SavedTo); string(data) != string(png) {
			t.Error("downloaded image differs from the upload")
		}
		var list struct {
			Attachments []struct{ ID string }
		}
		must(t, "get_issue_attachments", map[string]any{"issueId": parent}, &list)
		if len(list.Attachments) != 2 {
			t.Errorf("attachments = %+v", list)
		}
		must(t, "delete_attachment", map[string]any{"issueId": parent, "attachment": text.ID}, nil)
		must(t, "delete_attachment", map[string]any{"issueId": parent, "attachment": image.ID}, nil)
	})

	t.Run("commands", func(t *testing.T) {
		state := f.enumValue(t, "State", 1)
		cmd := "State " + state // multi-word values go as is, without braces
		var dry struct{ Valid bool }
		must(t, "apply_command", map[string]any{"command": cmd, "issueIds": []string{child}, "dryRun": true}, &dry)
		if !dry.Valid {
			t.Fatalf("dry run says %q is invalid", cmd)
		}
		mustFail(t, "apply_command", map[string]any{"command": "State no-such-state", "issueIds": []string{child}}, "nothing applied")
		must(t, "apply_command", map[string]any{"command": cmd, "query": "issue id: " + child, "comment": marker + " via command"}, nil)
		var got struct{ CustomFields map[string]any }
		must(t, "get_issue", map[string]any{"issueId": child}, &got)
		if got.CustomFields["State"] != state {
			t.Errorf("State after command = %v, want %s", got.CustomFields["State"], state)
		}
	})

	t.Run("drafts", func(t *testing.T) {
		var draft struct{ DraftID string }
		must(t, "create_draft_issue", map[string]any{"project": f.key, "summary": marker + " draft"}, &draft)
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
	})

	t.Run("articles", func(t *testing.T) {
		var parentArticle struct{ ID string }
		must(t, "create_article", map[string]any{"project": f.key, "summary": marker + " article", "content": "# Title\n\nfirst"}, &parentArticle)
		var childArticle struct{ ID string }
		must(t, "create_article", map[string]any{"project": f.key, "summary": marker + " child", "content": "child",
			"parentArticle": parentArticle.ID, "permittedUsers": []string{f.me.Login}}, &childArticle)
		must(t, "update_article", map[string]any{"articleId": parentArticle.ID, "content": "appended " + runID, "append": true}, nil)
		must(t, "edit_article_content", map[string]any{"articleId": parentArticle.ID,
			"edits": []map[string]any{{"oldText": "first", "newText": "edited"}, {"oldText": `(?m)^# (\w+)$`, "newText": "# $1!", "regexp": true}}}, nil)
		mustFail(t, "edit_article_content", map[string]any{"articleId": parentArticle.ID,
			"edits": []map[string]any{{"oldText": "no such text", "newText": "x"}}}, "not found")

		var got struct {
			Content       string
			ChildArticles []struct{ ID string }
		}
		must(t, "get_article", map[string]any{"articleId": parentArticle.ID}, &got)
		if !strings.HasPrefix(got.Content, "# Title!\n\nedited") || !strings.HasSuffix(got.Content, "appended "+runID) || len(got.ChildArticles) != 1 {
			t.Errorf("article = %+v", got)
		}
		var childGot struct{ Visibility map[string][]string }
		must(t, "get_article", map[string]any{"articleId": childArticle.ID}, &childGot)
		if len(childGot.Visibility["permittedUsers"]) != 1 {
			t.Errorf("child visibility = %+v", childGot.Visibility)
		}
		var found struct {
			Articles []struct{ ID string }
		}
		must(t, "search_articles", map[string]any{"query": "project: " + f.key}, &found)
		if len(found.Articles) != 2 {
			t.Errorf("search_articles = %+v", found)
		}
		// Images for the article, uploaded in one call and referenced by name.
		png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==")
		dir := t.TempDir()
		var paths []string
		for _, name := range []string{"diagram-1.png", "diagram-2.png"} {
			p := filepath.Join(dir, name)
			os.WriteFile(p, png, 0o644)
			paths = append(paths, p)
		}
		var uploaded struct {
			Attachments []struct{ ID, Name string }
		}
		must(t, "add_attachment", map[string]any{"articleId": parentArticle.ID, "filePaths": paths}, &uploaded)
		if len(uploaded.Attachments) != 2 {
			t.Fatalf("article upload = %+v", uploaded)
		}
		var withFiles struct {
			Attachments []struct{ Name string }
		}
		must(t, "get_article", map[string]any{"articleId": parentArticle.ID}, &withFiles)
		if len(withFiles.Attachments) != 2 {
			t.Errorf("get_article attachments = %+v", withFiles.Attachments)
		}
		var saved struct{ SavedTo string }
		must(t, "get_attachment", map[string]any{"articleId": parentArticle.ID, "attachment": "diagram-2.png", "saveTo": t.TempDir()}, &saved)
		if data, _ := os.ReadFile(saved.SavedTo); string(data) != string(png) {
			t.Error("article image differs from the upload")
		}
		must(t, "delete_attachment", map[string]any{"articleId": parentArticle.ID, "attachment": "diagram-1.png"}, nil)
		var left struct {
			Attachments []struct{ Name string }
		}
		must(t, "get_issue_attachments", map[string]any{"articleId": parentArticle.ID}, &left)
		if len(left.Attachments) != 1 || left.Attachments[0].Name != "diagram-2.png" {
			t.Errorf("after delete = %+v", left)
		}

		must(t, "delete_article", map[string]any{"articleId": childArticle.ID}, nil)
		must(t, "delete_article", map[string]any{"articleId": parentArticle.ID}, nil)
	})

	t.Run("saved searches", func(t *testing.T) {
		var s struct{ ID string }
		must(t, "create_saved_search", map[string]any{"name": marker + " search " + runID, "query": "project: " + f.key}, &s)
		t.Cleanup(func() { call(t, "delete_saved_search", map[string]any{"search": s.ID}) })
		var upd struct{ Query string }
		must(t, "update_saved_search", map[string]any{"search": s.ID, "query": "project: " + f.key + " #Unresolved"}, &upd)
		if !strings.HasSuffix(upd.Query, "#Unresolved") {
			t.Errorf("update_saved_search = %+v", upd)
		}
		must(t, "get_saved_issue_searches", nil, nil)
		must(t, "delete_saved_search", map[string]any{"search": s.ID}, nil)
	})

	t.Run("sprints", func(t *testing.T) {
		boardName := marker + " board " + runID
		var board struct{ ID string }
		api(t, "POST", "agiles", map[string]any{"fields": "id", "template": "scrum"},
			map[string]any{"name": boardName, "projects": []any{map[string]any{"id": f.projectID}}}, &board)
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
		must(t, "add_issue_to_sprint", map[string]any{"board": board.ID, "sprint": sprint.Sprint.ID, "issueId": parent}, nil)

		var got struct {
			Sprint struct{ Goal string }
			Issues []struct{ ID string }
		}
		must(t, "get_sprint", map[string]any{"board": board.ID, "sprint": sprint.Sprint.ID}, &got)
		if got.Sprint.Goal != "e2e updated" || len(got.Issues) != 1 || got.Issues[0].ID != parent {
			t.Errorf("get_sprint = %+v", got)
		}
		must(t, "remove_issue_from_sprint", map[string]any{"board": board.ID, "sprint": sprint.Sprint.ID, "issueId": parent}, nil)
		must(t, "get_sprint", map[string]any{"board": board.ID, "sprint": sprint.Sprint.ID}, &got)
		if len(got.Issues) != 0 {
			t.Errorf("issue still in sprint: %+v", got.Issues)
		}
		must(t, "delete_sprint", map[string]any{"board": board.ID, "sprint": sprint.Sprint.ID}, nil)
	})

	t.Run("people", func(t *testing.T) {
		var me struct{ Login string }
		must(t, "find_user", map[string]any{"loginOrEmail": f.me.Login}, &me)
		if me.Login != f.me.Login {
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
	})

	t.Run("call_api", func(t *testing.T) {
		var issue struct{ IDReadable string }
		api(t, "GET", "issues/"+parent, map[string]any{"fields": "idReadable"}, nil, &issue)
		if issue.IDReadable != parent {
			t.Errorf("call_api GET = %+v", issue)
		}
		var comment struct{ ID string }
		api(t, "POST", "issues/"+parent+"/comments", map[string]any{"fields": "id"}, map[string]any{"text": marker + " raw"}, &comment)
		api(t, "DELETE", "issues/"+parent+"/comments/"+comment.ID, nil, nil, nil)
		mustFail(t, "call_api", map[string]any{"method": "GET", "path": "https://example.com/api/issues"}, "must be relative")
	})

	t.Run("delete issues", func(t *testing.T) {
		// The search index lags behind deletions, so check each issue directly.
		for _, id := range []string{child, parent} {
			must(t, "delete_issue", map[string]any{"issueId": id}, nil)
			mustFail(t, "get_issue", map[string]any{"issueId": id}, "404")
		}
	})
}
