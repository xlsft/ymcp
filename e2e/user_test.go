//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// memberLogin is the one regular account the suite uses. The license limits
// the number of users and YouTrack keeps deleted ones, so it is created on
// the first run and kept; the sweep leaves it alone. Its password derives
// from the admin's, so it needs no secret of its own, and Hub has no API
// to reset it, so it must never reach a log (see scrub).
const memberLogin = marker + "-regular"

func memberPassword() string {
	sum := sha256.Sum256([]byte(memberLogin + "\x00" + adminPassword))
	return "Ym-" + hex.EncodeToString(sum[:12]) + "-Pw!"
}

// ensureMember creates the member account in Hub if it is missing and
// returns its Hub ID.
func ensureMember(t *testing.T) string {
	t.Helper()
	var found struct {
		Users []struct{ ID string }
	}
	api(t, "GET", "users", map[string]any{"fields": "id", "query": "login: " + memberLogin}, nil, &found, "hub")
	if len(found.Users) > 0 {
		return found.Users[0].ID
	}
	var created struct{ ID string }
	api(t, "POST", "users", map[string]any{"fields": "id"}, map[string]any{
		"login": memberLogin, "name": memberLogin,
		"details": []any{map[string]any{"type": "LoginuserdetailsJSON", "login": memberLogin,
			"password": map[string]any{"type": "PlainpasswordJSON", "value": memberPassword()}}},
	}, &created, "hub")
	t.Logf("created the member account %s", memberLogin)
	return created.ID
}

// TestRegularUser runs ymcp as a project member without admin rights, where
// ymcp has to work around the admin-only parts of the API: the field schema
// comes from issues, groups from Hub, and comments are marked deleted.
func TestRegularUser(t *testing.T) {
	login := memberLogin
	hubID := ensureMember(t)

	unbanMember(t)
	f := project
	user, err := startServer("user", login, memberPassword())
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
	grantAccess(t, user, f, hubID)

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

	// /api/groups is admin-only, and YouTrack 2026 shows members no groups
	// in Hub either; the error has to say so instead of an empty list.
	if r := user.call(t, "find_user_groups", map[string]any{"nameContains": "All Users"}); r.isError {
		if !strings.Contains(r.text, "only admins can") {
			t.Errorf("member groups error: %s", r.text)
		}
	} else {
		var groups struct {
			Groups []struct{ Name string }
		}
		json.Unmarshal([]byte(r.text), &groups)
		if len(groups.Groups) == 0 {
			t.Errorf("member got an empty group list instead of an explanation: %s", r.text)
		}
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
	var restored struct {
		Comments []struct {
			ID      string
			Deleted bool
		}
	}
	user.must(t, "get_issue_comments", map[string]any{"issueId": first.ID}, &restored)
	if len(restored.Comments) != 1 || restored.Comments[0].Deleted {
		t.Errorf("comments after restore = %+v", restored)
	}
}

// grantAccess makes sure the member can work in the fixture project. It
// gets a Developer-like role in Hub's Global project once, which covers
// every project; YouTrack 2026 keeps projects out of Hub, and its project
// team API doesn't take new members. The role takes a moment to apply.
func grantAccess(t *testing.T, user *server, f *fixture, hubID string) {
	t.Helper()
	canSee := func(tries int) string {
		var last string
		for i := range tries {
			if i > 0 {
				time.Sleep(time.Second)
			}
			r := user.call(t, "get_project", map[string]any{"projectKey": f.key})
			if !r.isError {
				return ""
			}
			last = r.text
		}
		return last
	}
	if canSee(3) == "" {
		return
	}
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
	r := call(t, "call_api", map[string]any{"api": "hub", "method": "POST", "path": "users/" + hubID + "/projectroles",
		"query": map[string]any{"fields": "id"}, "body": map[string]any{"role": map[string]any{"id": roleID}, "project": map[string]any{"id": "0"}}})
	t.Logf("granted the member role: error=%v %s", r.isError, shortText(r.text))
	if last := canSee(15); last != "" {
		t.Fatalf("the member still can't see %s: %s", f.key, last)
	}
}

func shortText(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
