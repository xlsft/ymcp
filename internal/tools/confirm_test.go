package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// pending is what a guarded call returns instead of doing its work.
type pending struct {
	ConfirmationRequired bool
	OperationID          string
	Operation            string
	Confirm              string
	Message              string
}

func TestConfirmDeletesOnlyWithTheSecret(t *testing.T) {
	api := newFakeAPI(t)
	var deleted atomic.Int32
	api.handle("DELETE /api/issues/DEMO-7", func(w http.ResponseWriter, r *http.Request) { deleted.Add(1) })
	api.handle("DELETE /api/issues/DEMO-8", func(w http.ResponseWriter, r *http.Request) { deleted.Add(1) })
	cs := connectWith(t, api, Options{Confirm: true})

	var p pending
	if isErr, text := call(t, cs, "delete_issue", map[string]any{"issueId": "DEMO-7"}, &p); isErr {
		t.Fatal(text)
	}
	if !p.ConfirmationRequired || p.Confirm == "" || !strings.HasPrefix(p.OperationID, "op-") ||
		!strings.Contains(p.Operation, "DEMO-7") || !strings.Contains(p.Message, p.Confirm) {
		t.Fatalf("first call = %+v", p)
	}
	if deleted.Load() != 0 {
		t.Fatal("deleted before confirmation")
	}

	// The secret is bound to the arguments.
	if isErr, text := call(t, cs, "delete_issue", map[string]any{"issueId": "DEMO-8", "confirm": p.Confirm}, nil); !isErr || !strings.Contains(text, "other arguments") {
		t.Errorf("other issue = %v, %s", isErr, text)
	}
	if isErr, text := call(t, cs, "delete_issue", map[string]any{"issueId": "DEMO-7", "confirm": "nope"}, nil); !isErr || !strings.Contains(text, "unknown") {
		t.Errorf("wrong secret = %v, %s", isErr, text)
	}
	if deleted.Load() != 0 {
		t.Fatal("deleted with a bad secret")
	}

	var out struct{ Deleted string }
	if isErr, text := call(t, cs, "delete_issue", map[string]any{"issueId": "DEMO-7", "confirm": p.Confirm}, &out); isErr || out.Deleted != "DEMO-7" {
		t.Fatalf("confirmed = %v, %s", isErr, text)
	}
	if deleted.Load() != 1 {
		t.Fatalf("deleted %d times", deleted.Load())
	}
	// Once only.
	if isErr, _ := call(t, cs, "delete_issue", map[string]any{"issueId": "DEMO-7", "confirm": p.Confirm}, nil); !isErr || deleted.Load() != 1 {
		t.Errorf("second use = %v, deleted %d", isErr, deleted.Load())
	}
}

func TestConfirmOffDeletesAtOnce(t *testing.T) {
	api := newFakeAPI(t)
	api.handle("DELETE /api/issues/DEMO-7", func(w http.ResponseWriter, r *http.Request) {})
	cs := connectWith(t, api, Options{})
	var out struct{ Deleted string }
	if isErr, text := call(t, cs, "delete_issue", map[string]any{"issueId": "DEMO-7"}, &out); isErr || out.Deleted != "DEMO-7" {
		t.Fatalf("= %v, %s", isErr, text)
	}
	// Without confirmations the argument doesn't exist.
	if schema := toolSchema(t, cs, "delete_issue"); schema.Properties["confirm"] != nil {
		t.Error("confirm in the schema with confirmations off")
	}
}

func TestConfirmSchemas(t *testing.T) {
	cs := connectWith(t, newFakeAPI(t), Options{Confirm: true})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		destructive := tool.Annotations != nil && tool.Annotations.DestructiveHint != nil && *tool.Annotations.DestructiveHint
		if destructive != slices.Contains(Destructive, tool.Name) {
			t.Errorf("%s: destructive hint %v, in Destructive %v", tool.Name, destructive, !destructive)
		}
		_, has := toolSchema(t, cs, tool.Name).Properties["confirm"]
		if has != destructive {
			t.Errorf("%s: confirm argument %v", tool.Name, has)
		}
	}
	// A tool's own schema tweaks survive.
	if s := toolSchema(t, cs, "call_api"); len(s.Properties["method"].Enum) != 3 {
		t.Errorf("call_api method enum = %v", s.Properties["method"].Enum)
	}
}

func TestConfirmOnlyWhatRemoves(t *testing.T) {
	api := newFakeAPI(t)
	api.json("GET /api/issues/DEMO-1", `{"id":"2-1"}`)
	api.handle("POST /api/commands/assist", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := api.bodies("/api/commands/assist")
		if strings.Contains(body[len(body)-1]["query"].(string), "remove") {
			w.Write([]byte(`{"commands":[{"description":"Remove tag <b>x</b>","delete":true}]}`))
			return
		}
		w.Write([]byte(`{"commands":[{"description":"Set State"}]}`))
	})
	api.json("POST /api/commands", `{}`)
	cs := connectWith(t, api, Options{Confirm: true})

	var out struct{ Applied bool }
	if isErr, text := call(t, cs, "apply_command", map[string]any{"command": "State Fixed", "issueIds": []string{"DEMO-1"}}, &out); isErr || !out.Applied {
		t.Fatalf("set state = %v, %s", isErr, text)
	}
	var p pending
	if isErr, text := call(t, cs, "apply_command", map[string]any{"command": "remove tag x", "issueIds": []string{"DEMO-1"}}, &p); isErr || !p.ConfirmationRequired {
		t.Fatalf("remove tag = %v, %s", isErr, text)
	}
	if !strings.Contains(p.Operation, "Remove tag x") || len(api.bodies("/api/commands")) != 1 {
		t.Errorf("remove tag: %q, %d applied", p.Operation, len(api.bodies("/api/commands")))
	}
	if isErr, text := call(t, cs, "apply_command", map[string]any{"command": "remove tag x", "issueIds": []string{"DEMO-1"}, "confirm": p.Confirm}, &out); isErr || !out.Applied {
		t.Fatalf("confirmed = %v, %s", isErr, text)
	}

	// call_api: only DELETE asks.
	api.json("GET /api/issues/DEMO-1/comments", `[]`)
	if isErr, text := call(t, cs, "call_api", map[string]any{"method": "GET", "path": "issues/DEMO-1/comments"}, nil); isErr || strings.Contains(text, "confirmationRequired") {
		t.Errorf("GET = %v, %s", isErr, text)
	}
	if isErr, text := call(t, cs, "call_api", map[string]any{"method": "DELETE", "path": "issues/DEMO-1"}, &p); isErr || !p.ConfirmationRequired {
		t.Errorf("DELETE = %v, %s", isErr, text)
	}
}

func TestConfirmationsExpireAndStayBounded(t *testing.T) {
	var c confirmations
	now := time.Now()
	_, secret := c.issue(now, "delete_tag", `{"tag":"x"}`, "delete tag x")
	if err := c.take(now.Add(confirmFor+time.Second), secret, "delete_tag", `{"tag":"x"}`); err == nil {
		t.Error("an expired secret worked")
	}
	for i := range maxPending + 10 {
		c.issue(now.Add(time.Duration(i)*time.Millisecond), "delete_tag", `{}`, "")
	}
	if len(c.pending) != maxPending {
		t.Errorf("%d pending, want %d", len(c.pending), maxPending)
	}
}

func toolSchema(t *testing.T, cs *mcp.ClientSession, name string) *jsonschema.Schema {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name != name {
			continue
		}
		data, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var s jsonschema.Schema
		if err := s.UnmarshalJSON(data); err != nil {
			t.Fatal(err)
		}
		return &s
	}
	t.Fatalf("no tool %s", name)
	return nil
}
