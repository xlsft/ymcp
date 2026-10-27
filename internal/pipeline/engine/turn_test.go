package engine

import (
	"encoding/json"
	"testing"
)

// TestPermissionInput: a permission request shows people the command, or
// the text kimi describes the call with, not the JSON it came in.
func TestPermissionInput(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"command":"git status --short","description":"Status"}`, "git status --short"},
		{`[{"content":{"text":"Requesting approval to Running: git status --short --branch","type":"text"},"type":"content"}]`, "Running: git status --short --branch"},
		{`[{"content":{"text":"a","type":"text"},"type":"content"},{"content":{"text":"b","type":"text"},"type":"content"}]`, "a\nb"},
		{`{"path":"/tmp/x"}`, `{"path":"/tmp/x"}`},
		{``, ``},
	} {
		if got := permissionInput(json.RawMessage(c.in)); got != c.want {
			t.Errorf("permissionInput(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}
