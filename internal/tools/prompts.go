package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// prompt is a ready-made task clients offer as a slash command. It is
// registered only when the tools it relies on are served.
type prompt struct {
	*mcp.Prompt
	needs []string
	text  func(t *toolset, args map[string]string) (string, error)
}

func (t *toolset) registerPrompts(s *mcp.Server, enabled map[string]bool) {
	served := func(tool string) bool { return enabled == nil || enabled[tool] }
	for _, p := range prompts() {
		if !allOf(p.needs, served) {
			continue
		}
		p := p
		s.AddPrompt(p.Prompt, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			args := req.Params.Arguments
			if args == nil {
				args = map[string]string{}
			}
			for _, a := range p.Arguments {
				if a.Required && strings.TrimSpace(args[a.Name]) == "" {
					return nil, fmt.Errorf("%s needs %s", p.Name, a.Name)
				}
			}
			text, err := p.text(t, args)
			if err != nil {
				return nil, err
			}
			text += optionalTools(text, served)
			return &mcp.GetPromptResult{
				Description: p.Description,
				Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}},
			}, nil
		})
	}
}

func allOf(tools []string, ok func(string) bool) bool {
	for _, tool := range tools {
		if !ok(tool) {
			return false
		}
	}
	return true
}

// optionalTools notes which of the tools a prompt mentions aren't served,
// so the model skips those steps instead of looking for them.
func optionalTools(text string, served func(string) bool) string {
	var missing []string
	for _, g := range Groups {
		for _, tool := range g.Tools {
			if !served(tool) && strings.Contains(text, tool) {
				missing = append(missing, tool)
			}
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "\n\nThese tools aren't available here, so skip the steps that need them: " + strings.Join(missing, ", ") + "."
}

// workdaysBack is the date n working days before now, skipping weekends.
func workdaysBack(now time.Time, n int) time.Time {
	d := now
	for n > 0 {
		d = d.AddDate(0, 0, -1)
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			n--
		}
	}
	return d
}

func prompts() []prompt {
	return []prompt{
		{
			Prompt: &mcp.Prompt{
				Name:        "standup",
				Title:       "Stand-up update",
				Description: "My stand-up from YouTrack: what I did since the last working day, what's next, what blocks me.",
				Arguments: []*mcp.PromptArgument{
					{Name: "days", Description: "Working days to look back. Default 1, so Friday on a Monday."},
				},
			},
			needs: []string{"get_current_user", "search_issues"},
			text: func(t *toolset, args map[string]string) (string, error) {
				days := 1
				if v := strings.TrimSpace(args["days"]); v != "" {
					n, err := strconv.Atoi(v)
					if err != nil || n < 1 || n > 30 {
						return "", fmt.Errorf("days must be a number from 1 to 30, got %q", v)
					}
					days = n
				}
				now := t.clock()
				from := workdaysBack(now, days).Format("2006-01-02")
				return fmt.Sprintf(`Prepare my stand-up update from YouTrack. Today is %s; look back to %s.

1. Call get_current_user to learn my login.
2. Find what I worked on: search_issues with "updated by: me updated: %s .. Today sort by: updated desc".
3. For the issues that matter, get_issue_activities shows what I changed since %s: state changes, comments, fields.
4. get_time_report with author "me" from %s to %s, grouped by issue, adds the time I logged.
5. Find what's next: search_issues with "for: me #Unresolved sort by: Priority", the top five.
6. Find blockers: my unresolved issues that depend on unresolved ones (get_issue shows links), or wait in states such as Blocked or On hold.

Answer in three short sections, Done, Next and Blockers, with one line per issue: its ID, summary and what happened.
Don't change anything in YouTrack.`, now.Format("2006-01-02"), from, from, from, from, now.Format("2006-01-02")), nil
			},
		},
		{
			Prompt: &mcp.Prompt{
				Name:        "triage",
				Title:       "Triage issues",
				Description: "Go through untriaged issues of a project and propose type, priority, assignee and duplicates; applies only what I approve.",
				Arguments: []*mcp.PromptArgument{
					{Name: "project", Description: "Project short name, e.g. DEMO", Required: true},
					{Name: "query", Description: "Which issues to triage. Default: unresolved and unassigned, newest first."},
				},
			},
			needs: []string{"search_issues", "get_issue", "get_issue_fields_schema", "update_issue"},
			text: func(t *toolset, args map[string]string) (string, error) {
				project := strings.TrimSpace(args["project"])
				query := strings.TrimSpace(args["query"])
				if query == "" {
					query = "#Unresolved Assignee: Unassigned sort by: created desc"
				}
				return fmt.Sprintf(`Triage issues of the YouTrack project %[1]s.

1. get_issue_fields_schema for %[1]s gives the fields and their allowed values; use only those.
2. search_issues with "project: %[1]s %[2]s", up to 20 issues.
3. Read each with get_issue, and get_issue_comments where the description isn't enough.
4. For each issue propose Type, Priority, Assignee (find_user checks a login) and State, and look for duplicates with search_issues on key words of the summary.
5. Show the proposals as a table: issue, summary, each proposed change, duplicate candidates, and a one-line reason.

Then ask me which to apply. Only after I confirm, apply them with update_issue, and mark duplicates with link_issues ("duplicates"). Change nothing I didn't approve.`, project, query), nil
			},
		},
		{
			Prompt: &mcp.Prompt{
				Name:        "sprint_report",
				Title:       "Sprint report",
				Description: "Status of a sprint on an agile board: done and open work, who has what, scope changes, time and risks.",
				Arguments: []*mcp.PromptArgument{
					{Name: "board", Description: "Agile board name or ID", Required: true},
					{Name: "sprint", Description: "Sprint name or ID. Default: the current one."},
				},
			},
			needs: []string{"get_agile_board", "get_sprint"},
			text: func(t *toolset, args map[string]string) (string, error) {
				board := strings.TrimSpace(args["board"])
				sprint := strings.TrimSpace(args["sprint"])
				if sprint == "" {
					sprint = "current"
				}
				return fmt.Sprintf(`Report on the sprint %[2]q of the YouTrack agile board %[1]q.

1. get_agile_board for %[1]q shows its sprints and dates; find_agile_boards helps if the name doesn't match.
2. get_sprint for that sprint lists its issues; page through all of them with offset.
3. Count issues by state and by assignee, and tell done from open by the board's columns.
4. Find scope changes: get_issue_activities on issues that look added late shows when they joined the sprint.
5. get_time_report over the sprint's dates, with a query for the sprint's issues, gives the time logged.
6. Name the risks: big open issues near the end, issues without an assignee, blocked issues.

Write a short report: one-line summary, numbers (done/total, by state), what's done, what's open and with whom, scope changes, time, risks.
Don't change anything in YouTrack.`, board, sprint), nil
			},
		},
		{
			Prompt: &mcp.Prompt{
				Name:        "issue_brief",
				Title:       "Issue brief",
				Description: "Catch up on an issue: what it is about, where it stands, what was decided and what's open.",
				Arguments: []*mcp.PromptArgument{
					{Name: "issue", Description: "Issue ID, e.g. DEMO-123", Required: true},
				},
			},
			needs: []string{"get_issue"},
			text: func(t *toolset, args map[string]string) (string, error) {
				issue := strings.TrimSpace(args["issue"])
				return fmt.Sprintf(`Brief me on the YouTrack issue %[1]s.

1. get_issue for %[1]s: summary, description, fields, links.
2. get_issue_comments for the discussion, and get_issue_activities for how its state, assignee and fields changed.
3. get_issue on the linked issues that matter: parents, subtasks, blockers, duplicates.
4. get_issue_vcs_changes for related commits and pull requests, if any.

Write: a two-sentence TL;DR, current state (state, assignee, priority, due), decisions made, open questions, and the next steps with who owns them.
Don't change anything in YouTrack.`, issue), nil
			},
		},
	}
}
