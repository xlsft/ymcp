//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// TestRegularUser runs ymcp as a project member without admin rights, where
// ymcp has to work around the admin-only parts of the API: the field schema
// comes from issues, groups from Hub, and comments are marked deleted.
func TestRegularUser(t *testing.T) {
	login := marker + "-user-" + runID
	password := "Ymcp-e2e-" + runID + "-Pa55word!"
	var hubUser struct{ ID string }
	api(t, "POST", "users", map[string]any{"fields": "id,login"}, map[string]any{
		"login": login, "name": login,
		"details": []any{map[string]any{"type": "LoginuserdetailsJSON", "login": login,
			"password": map[string]any{"type": "PlainpasswordJSON", "value": password}}},
	}, &hubUser, "hub")
	t.Cleanup(func() {
		if r := call(t, "call_api", map[string]any{"api": "hub", "method": "DELETE", "path": "users/" + hubUser.ID}); r.isError {
			t.Errorf("cleanup: deleting user %s: %s", login, r.text)
		}
	})

	// Created after the user, so the project goes first on cleanup.
	f := newFixture(t)
	user, err := startServer("user", login, password)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(user.close)
	defer func() {
		if t.Failed() {
			user.dumpLog()
		}
	}()

	// The first sign-in creates the YouTrack user; then it gets access.
	var me struct{ Login string }
	user.must(t, "get_current_user", nil, &me)
	if me.Login != login {
		t.Fatalf("signed in as %q", me.Login)
	}
	grantAccess(t, user, f, hubUser.ID)

	// No issues yet: the schema can't be read without admin rights.
	r := user.call(t, "get_issue_fields_schema", map[string]any{"projectKey": f.key})
	if r.isError && !strings.Contains(r.text, "has no issues") {
		t.Errorf("schema of an empty project: %s", r.text)
	}
	var first struct{ ID string }
	user.must(t, "create_issue", map[string]any{"project": f.key, "summary": marker + " by a member"}, &first)

	// With an issue, the schema comes from it and fields can be set.
	var schema struct {
		Properties map[string]struct {
			Enum  []string
			Items struct{ Enum []string }
		}
	}
	user.must(t, "get_issue_fields_schema", map[string]any{"projectKey": f.key}, &schema)
	priority, ok := schema.Properties["Priority"]
	if !ok || len(priority.Enum) == 0 {
		t.Fatalf("member schema = %+v", schema)
	}
	value := priority.Enum[len(priority.Enum)-1]
	var second struct{ ID string }
	user.must(t, "create_issue", map[string]any{"project": f.key, "summary": marker + " with fields",
		"customFields": map[string]any{"Priority": value}}, &second)
	var got struct{ CustomFields map[string]any }
	user.must(t, "get_issue", map[string]any{"issueId": second.ID}, &got)
	if got.CustomFields["Priority"] != value {
		t.Errorf("Priority = %v, want %s", got.CustomFields["Priority"], value)
	}

	var project struct{ CustomFields []string }
	user.must(t, "get_project", map[string]any{"projectKey": f.key}, &project)
	if len(project.CustomFields) == 0 {
		t.Errorf("member get_project has no custom fields")
	}

	var groups struct {
		Groups []struct{ Name string }
	}
	user.must(t, "find_user_groups", nil, &groups)
	if len(groups.Groups) == 0 {
		t.Error("member sees no groups")
	}

	// Members can't erase comments; they are marked deleted and come back.
	var c struct {
		Comment struct{ ID string }
	}
	user.must(t, "add_issue_comment", map[string]any{"issueId": first.ID, "text": marker + " comment"}, &c)
	user.must(t, "delete_issue_comment", map[string]any{"issueId": first.ID, "commentId": c.Comment.ID}, nil)
	var comments struct {
		Comments []struct {
			ID      string
			Deleted bool
		}
	}
	user.must(t, "get_issue_comments", map[string]any{"issueId": first.ID, "includeDeleted": true}, &comments)
	if len(comments.Comments) != 1 || !comments.Comments[0].Deleted {
		t.Errorf("comments after delete = %+v", comments)
	}
	user.must(t, "restore_issue_comment", map[string]any{"issueId": first.ID, "commentId": c.Comment.ID}, nil)
	user.must(t, "get_issue_comments", map[string]any{"issueId": first.ID}, &comments)
	if len(comments.Comments) != 1 || comments.Comments[0].Deleted {
		t.Errorf("comments after restore = %+v", comments)
	}
}

// grantAccess gives a Hub user a Developer-like role in Hub's Global
// project, which covers the fixture project. YouTrack 2026 keeps projects
// out of Hub, and its project team API doesn't take new members.
func grantAccess(t *testing.T, user *server, f *fixture, hubID string) {
	t.Helper()
	var roles struct {
		Roles []struct{ ID, Key string }
	}
	api(t, "GET", "roles", map[string]any{"fields": "id,key", "$top": 1000}, nil, &roles, "hub")
	roleID := ""
	for _, want := range []string{"developer", "contributor"} {
		for _, r := range roles.Roles {
			if r.Key == want && roleID == "" {
				roleID = r.ID
			}
		}
	}
	if roleID == "" {
		t.Fatalf("no developer or contributor role among %+v", roles.Roles)
	}
	role := map[string]any{"role": map[string]any{"id": roleID}, "project": map[string]any{"id": "0"}}
	attempts := []map[string]any{
		{"api": "hub", "method": "POST", "path": "users/" + hubID + "/projectroles", "query": map[string]any{"fields": "id"}, "body": role},
		{"api": "hub", "method": "POST", "path": "projectroles", "query": map[string]any{"fields": "id"},
			"body": map[string]any{"role": role["role"], "project": role["project"], "owner": map[string]any{"id": hubID}}},
	}
	var tried []string
	for _, args := range attempts {
		r := call(t, "call_api", args)
		if !r.isError {
			if p := user.call(t, "get_project", map[string]any{"projectKey": f.key}); !p.isError {
				t.Logf("access granted with POST %s", args["path"])
				return
			} else {
				tried = append(tried, fmt.Sprintf("POST %s: ok, but get_project: %s", args["path"], p.text))
				continue
			}
		}
		tried = append(tried, fmt.Sprintf("POST %s: %s", args["path"], r.text))
	}
	t.Fatalf("can't give %s access to %s:\n%s", hubID, f.key, strings.Join(tried, "\n"))
}
