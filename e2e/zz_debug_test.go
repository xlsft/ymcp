//go:build e2e

package e2e

import (
	"encoding/json"
	"testing"
)

// Temporary: how undirected links look and which DELETE removes them.
func TestDebugLinks(t *testing.T) {
	f := newFixture(t)
	mk := func(s string) (string, string) {
		var out struct{ ID string }
		args := map[string]any{"project": f.key, "summary": marker + " " + s}
		if fields := f.requiredFields(t); len(fields) > 0 {
			args["customFields"] = fields
		}
		must(t, "create_issue", args, &out)
		var db struct{ ID string }
		api(t, "GET", "issues/"+out.ID, map[string]any{"fields": "id"}, nil, &db)
		return out.ID, db.ID
	}
	a, _ := mk("a")
	b, bdb := mk("b")
	links := func() string {
		var raw json.RawMessage
		api(t, "GET", "issues/"+a+"/links", map[string]any{"fields": "id,direction,linkType(name,sourceToTarget,targetToSource,directed),issues(id,idReadable)"}, nil, &raw)
		return string(raw)
	}
	must(t, "link_issues", map[string]any{"targetIssueId": a, "linkType": "relates to", "issueToLinkId": b}, nil)
	t.Logf("links after relate: %s", links())

	var parsed []struct {
		ID     string
		Issues []struct{ ID string }
	}
	var raw json.RawMessage
	api(t, "GET", "issues/"+a+"/links", map[string]any{"fields": "id,issues(id)"}, nil, &raw)
	json.Unmarshal(raw, &parsed)
	for _, l := range parsed {
		if len(l.Issues) == 0 {
			continue
		}
		for _, path := range []string{
			"issues/" + a + "/links/" + l.ID + "/issues/" + bdb,
			"issues/" + a + "/links/" + l.ID + "s/issues/" + bdb,
			"issues/" + a + "/links/" + l.ID + "t/issues/" + bdb,
		} {
			r := call(t, "call_api", map[string]any{"method": "DELETE", "path": path})
			t.Logf("DELETE %s -> error=%v %s | links now: %s", path, r.isError, r.text, links())
		}
	}
	r := call(t, "apply_command", map[string]any{"command": "remove relates to " + b, "issueIds": []string{a}, "dryRun": true})
	t.Logf("command dry run: %s", r.text)
}
