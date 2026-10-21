//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"
)

// TestInstance checks the sign-in and records what the instance looks like,
// which makes failures elsewhere easier to read.
func TestInstance(t *testing.T) {
	var me struct{ Login, FullName string }
	must(t, "get_current_user", nil, &me)
	if !strings.EqualFold(me.Login, os.Getenv("YOUTRACK_E2E_LOGIN")) {
		t.Errorf("signed in as %q, not the configured login", me.Login)
	}

	var cfg struct{ Version, Build string }
	api(t, "GET", "config", map[string]any{"fields": "version,build"}, nil, &cfg)
	t.Logf("YouTrack %s (build %s)", cfg.Version, cfg.Build)

	var projects []struct{ ShortName, Name string }
	api(t, "GET", "admin/projects", map[string]any{"fields": "shortName,name", "$top": -1}, nil, &projects)
	t.Logf("projects: %d %v", len(projects), projects)

	var boards []struct{ Name string }
	api(t, "GET", "agiles", map[string]any{"fields": "name", "$top": -1}, nil, &boards)
	t.Logf("agile boards: %v", boards)

	r := call(t, "call_api", map[string]any{"method": "GET", "path": "groups", "query": map[string]any{"fields": "name", "$top": 5}})
	t.Logf("can read /api/groups: %v", !r.isError)
}
