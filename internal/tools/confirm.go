package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// With Options.Confirm, destructive tools take an optional confirm
// argument. A call without it changes nothing: it returns an operation id
// and a one-time secret, and the model asks the user. Called again with
// the same arguments and confirm set to the secret, the tool does its
// work. A secret is good for one call, with those arguments, for
// confirmFor.

// confirmFor is how long a confirmation secret stays valid.
const confirmFor = 10 * time.Minute

// maxPending caps the confirmations waiting at once; the oldest go first.
const maxPending = 100

// Destructive lists the tools that ask for confirmation when it is on.
// apply_command asks only for commands that remove something, call_api
// only for DELETE.
var Destructive = []string{
	"delete_issue", "delete_issue_comment", "delete_article", "delete_attachment", "delete_work_item",
	"delete_sprint", "delete_tag", "delete_saved_search", "unlink_issues", "apply_command", "call_api",
}

// pendingOp is a destructive call waiting for the user's yes.
type pendingOp struct {
	id      string
	tool    string
	args    string // the call's arguments as JSON, confirm left out
	summary string
	expires time.Time
}

// confirmations are the operations waiting for confirmation, by secret.
type confirmations struct {
	mu      sync.Mutex
	pending map[string]pendingOp
}

// issue stores an operation and returns it with its secret.
func (c *confirmations) issue(now time.Time, tool, args, summary string) (pendingOp, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending = map[string]pendingOp{}
	}
	var oldest string
	for secret, op := range c.pending {
		if now.After(op.expires) {
			delete(c.pending, secret)
		} else if oldest == "" || op.expires.Before(c.pending[oldest].expires) {
			oldest = secret
		}
	}
	if len(c.pending) >= maxPending {
		delete(c.pending, oldest)
	}
	op := pendingOp{id: "op-" + randomHex(4), tool: tool, args: args, summary: summary, expires: now.Add(confirmFor)}
	secret := randomHex(12)
	c.pending[secret] = op
	return op, secret
}

// take uses up the secret of a call with these arguments.
func (c *confirmations) take(now time.Time, secret, tool, args string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	op, ok := c.pending[secret]
	switch {
	case !ok || now.After(op.expires):
		delete(c.pending, secret)
		return fmt.Errorf("confirm %q is unknown, used or expired; nothing was done. Call %s without confirm to get a new secret, and ask the user again", secret, tool)
	case op.tool != tool || op.args != args:
		return fmt.Errorf("confirm %q was issued for %s (%s) with other arguments; nothing was done. Call %s again with exactly the arguments the user confirmed", secret, op.tool, op.summary, tool)
	}
	delete(c.pending, secret)
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// guard tells whether a call needs confirmation and what it does, in a
// phrase the user can say yes to: "delete issue DEMO-1".
type guard[In any] func(ctx context.Context, in In) (summary string, need bool, err error)

// always is the guard of a tool whose every call needs confirmation.
func always[In any](summary func(In) string) guard[In] {
	return func(_ context.Context, in In) (string, bool, error) { return summary(in), true, nil }
}

// confirmDescription is added to the input schema of a guarded tool.
const confirmDescription = "Leave out at first: the call then changes nothing and returns a secret. " +
	"Ask the user to confirm, then call again with the same arguments and confirm set to that secret."

// addGuarded registers a destructive tool: like add, but with
// confirmations on, a call goes through g first.
func addGuarded[In any](t *toolset, s *mcp.Server, tool *mcp.Tool, h handler[In], g guard[In]) {
	if t.confirm == nil {
		add(s, tool, h)
		return
	}
	var schema *jsonschema.Schema
	switch in := tool.InputSchema.(type) {
	case *jsonschema.Schema:
		schema = in.CloneSchemas()
	default:
		var err error
		if schema, err = jsonschema.For[In](nil); err != nil {
			panic(err)
		}
	}
	if schema.Properties == nil {
		schema.Properties = map[string]*jsonschema.Schema{}
	}
	schema.Properties["confirm"] = &jsonschema.Schema{Type: "string", Description: confirmDescription}
	tt := *tool
	tt.InputSchema = schema
	tt.Description += " Asks for the user's confirmation first: see the confirm argument."
	mcp.AddTool(s, &tt, func(ctx context.Context, _ *mcp.CallToolRequest, raw json.RawMessage) (*mcp.CallToolResult, any, error) {
		out, err := guarded(ctx, t, tool.Name, raw, h, g)
		if err != nil {
			return nil, nil, withHint(err)
		}
		return textResult(out), nil, nil
	})
}

// guarded runs a call of a guarded tool: with a valid confirm it does the
// work; without, it does it only when g says no confirmation is needed,
// and otherwise returns the secret to confirm with.
func guarded[In any](ctx context.Context, t *toolset, tool string, raw json.RawMessage, h handler[In], g guard[In]) (any, error) {
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
	}
	var secret string
	if c, ok := fields["confirm"]; ok {
		if err := json.Unmarshal(c, &secret); err != nil {
			return nil, fmt.Errorf("confirm must be a string: %w", err)
		}
		delete(fields, "confirm")
	}
	rest, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	var in In
	if err := json.Unmarshal(rest, &in); err != nil {
		return nil, err
	}
	args, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	if secret != "" {
		if err := t.confirm.take(t.clock(), secret, tool, string(args)); err != nil {
			return nil, err
		}
		return h(ctx, in)
	}
	summary, need, err := g(ctx, in)
	if err != nil {
		return nil, err
	}
	if !need {
		return h(ctx, in)
	}
	op, secret := t.confirm.issue(t.clock(), tool, string(args), summary)
	return map[string]any{
		"confirmationRequired": true,
		"operationId":          op.id,
		"operation":            summary,
		"confirm":              secret,
		"expiresAt":            op.expires.UTC().Format(time.RFC3339),
		"message": fmt.Sprintf("Nothing was changed yet. Ask the user to confirm this operation (%s): %s. "+
			"Only if they agree, call %s again with the same arguments and confirm set to %q. "+
			"The secret works once, for these arguments, for %d minutes.",
			op.id, summary, tool, secret, int(confirmFor.Minutes())),
	}, nil
}
