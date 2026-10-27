package tools

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var htmlTag = regexp.MustCompile(`<[^>]*>`)

// maxCommandIssues caps how many issues one apply_command call may touch.
const maxCommandIssues = 100

type applyCommandIn struct {
	Command  string   `json:"command" jsonschema:"Command as typed in the YouTrack command dialog, e.g. State In Progress, for jdoe, tag backend, Priority Critical, project OPS, remove tag backend"`
	IssueIDs []string `json:"issueIds,omitempty" jsonschema:"Issues to apply the command to, e.g. [\"DEMO-1\",\"DEMO-2\"]"`
	Query    string   `json:"query,omitempty" jsonschema:"Apply to all issues matching this search query instead (at most 100)"`
	Comment  string   `json:"comment,omitempty" jsonschema:"Comment to add together with the command"`
	Silent   bool     `json:"silent,omitempty" jsonschema:"Don't send notifications about the change. Default false."`
	DryRun   bool     `json:"dryRun,omitempty" jsonschema:"Only parse the command and report what it would do or why it is invalid. Default false."`
}

type parsedCommand struct {
	Description string `json:"description"`
	Error       bool   `json:"error,omitempty"`
	Delete      bool   `json:"delete,omitempty"`
}

func (t *toolset) registerCommands(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name: "apply_command",
		Description: "Applies a YouTrack command to one or more issues, the same as the command dialog in the UI: " +
			"set fields, assign, tag, move to another project, add links, vote, star and more in one line. " +
			"The command is validated first and nothing is applied if any part is invalid; dryRun only validates and lists suggestions.",
		Annotations: anything("Apply command"),
	}, t.applyCommand)
}

func (t *toolset) applyCommand(ctx context.Context, in applyCommandIn) (any, error) {
	if strings.TrimSpace(in.Command) == "" {
		return nil, fmt.Errorf("command must not be empty")
	}
	ids, err := t.commandTargets(ctx, in)
	if err != nil {
		return nil, err
	}
	issues := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		issues = append(issues, map[string]string{"idReadable": id})
	}

	var assist struct {
		Commands    []parsedCommand `json:"commands"`
		Suggestions []struct {
			Option      string `json:"option"`
			Description string `json:"description"`
		} `json:"suggestions"`
	}
	body := map[string]any{"query": in.Command, "caret": len([]rune(in.Command)), "issues": issues}
	q := url.Values{"fields": {"commands(description,error,delete),suggestions(option,description)"}}
	if err := t.yt.Post(ctx, "commands/assist", q, body, &assist); err != nil {
		return nil, err
	}
	var invalid []string
	for i, c := range assist.Commands {
		// Descriptions carry HTML markup for the UI.
		assist.Commands[i].Description = html.UnescapeString(htmlTag.ReplaceAllString(c.Description, ""))
		c = assist.Commands[i]
		if c.Error {
			invalid = append(invalid, c.Description)
		}
	}

	out := map[string]any{"command": in.Command, "issues": ids, "parsed": assist.Commands}
	if in.DryRun {
		out["valid"] = len(invalid) == 0 && len(assist.Commands) > 0
		if len(assist.Suggestions) > 0 {
			out["suggestions"] = assist.Suggestions[:min(len(assist.Suggestions), 10)]
		}
		return out, nil
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("command %q is invalid, nothing applied: %s", in.Command, strings.Join(invalid, "; "))
	}
	if len(assist.Commands) == 0 {
		return nil, fmt.Errorf("command %q was not recognized, nothing applied; try dryRun for suggestions", in.Command)
	}

	apply := map[string]any{"query": in.Command, "issues": issues, "silent": in.Silent}
	if in.Comment != "" {
		apply["comment"] = t.marked(in.Comment)
	}
	if err := t.yt.Post(ctx, "commands", nil, apply, nil); err != nil {
		return nil, err
	}
	out["applied"] = true
	return out, nil
}

// commandTargets returns the readable IDs the command applies to.
func (t *toolset) commandTargets(ctx context.Context, in applyCommandIn) ([]string, error) {
	switch {
	case len(in.IssueIDs) > 0 && in.Query != "":
		return nil, fmt.Errorf("pass either issueIds or query, not both")
	case len(in.IssueIDs) > maxCommandIssues:
		return nil, fmt.Errorf("at most %d issues per command, got %d", maxCommandIssues, len(in.IssueIDs))
	case len(in.IssueIDs) > 0:
		return in.IssueIDs, nil
	case in.Query == "":
		return nil, fmt.Errorf("pass issueIds or query")
	}
	var found []struct {
		IDReadable string `json:"idReadable"`
	}
	q := url.Values{"query": {in.Query}, "fields": {"idReadable"}, "$top": {strconv.Itoa(maxCommandIssues + 1)}}
	if err := t.yt.Get(ctx, "issues", q, &found); err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no issues match %q", in.Query)
	}
	if len(found) > maxCommandIssues {
		return nil, fmt.Errorf("more than %d issues match %q; narrow the query (count_issues tells how many)", maxCommandIssues, in.Query)
	}
	ids := make([]string, 0, len(found))
	for _, f := range found {
		ids = append(ids, f.IDReadable)
	}
	return ids, nil
}
