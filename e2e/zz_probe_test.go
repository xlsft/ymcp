//go:build e2e

package e2e

import (
	"testing"
)

// Temporary: how to create a Hub user with a password and give it a project.
func TestProbeUsers(t *testing.T) {
	log := func(label string, args map[string]any) result {
		r := call(t, "call_api", args)
		text := r.text
		if len(text) > 1500 {
			text = text[:1500]
		}
		t.Logf("%s -> error=%v %s", label, r.isError, text)
		return r
	}
	log("authmodules", map[string]any{"method": "GET", "path": "authmodules", "api": "hub", "query": map[string]any{"fields": "id,name,type,disabled"}})
	log("roles", map[string]any{"method": "GET", "path": "roles", "api": "hub", "query": map[string]any{"fields": "id,key,name"}})

	login := marker + "-probe-" + runID
	pw := "Ymcp-e2e-" + runID + "-Pa55word!"
	r := log("create user", map[string]any{"method": "POST", "path": "users", "api": "hub", "query": map[string]any{"fields": "id,login,details(id,type)"},
		"body": map[string]any{"login": login, "name": login,
			"details": []any{map[string]any{"type": "LoginuserdetailsJSON", "login": login, "password": map[string]any{"type": "PlainpasswordJSON", "value": pw}}}}})
	if r.isError {
		return
	}
	var u struct{ ID string }
	must(t, "call_api", map[string]any{"method": "GET", "path": "users", "api": "hub", "query": map[string]any{"fields": "id,login", "query": "login: " + login}}, nil)
	log("hub user by login", map[string]any{"method": "GET", "path": "users", "api": "hub", "query": map[string]any{"fields": "id,login,details(type,login)", "query": "login: " + login}})
	log("youtrack user", map[string]any{"method": "GET", "path": "users", "query": map[string]any{"fields": "id,login,ringId", "query": login}})

	f := newFixture(t)
	log("project team", map[string]any{"method": "GET", "path": "admin/projects/" + f.projectID, "query": map[string]any{"fields": "team(id,name,ringId,usersCount),ringId"}})
	_ = u
	// Clean up the probe user.
	var users struct {
		Users []struct{ ID string }
	}
	must(t, "call_api", map[string]any{"method": "GET", "path": "users", "api": "hub", "query": map[string]any{"fields": "id", "query": "login: " + login}}, &users)
	for _, x := range users.Users {
		log("delete user", map[string]any{"method": "DELETE", "path": "users/" + x.ID, "api": "hub"})
	}
}
