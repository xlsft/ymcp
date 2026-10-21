package tools

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type createSprintIn struct {
	Board  string `json:"board" jsonschema:"Agile board name or ID, e.g. 195-13"`
	Name   string `json:"name" jsonschema:"Sprint name, e.g. 22.10.2026-04.11.2026"`
	Goal   string `json:"goal,omitempty" jsonschema:"Sprint goal"`
	Start  string `json:"start,omitempty" jsonschema:"First day, yyyy-MM-dd"`
	Finish string `json:"finish,omitempty" jsonschema:"Last day, yyyy-MM-dd"`
}

type updateSprintIn struct {
	Board    string  `json:"board" jsonschema:"Agile board name or ID, e.g. 195-13"`
	Sprint   string  `json:"sprint" jsonschema:"Sprint name or ID, or current"`
	Name     *string `json:"name,omitempty" jsonschema:"New name"`
	Goal     *string `json:"goal,omitempty" jsonschema:"New goal"`
	Start    *string `json:"start,omitempty" jsonschema:"New first day, yyyy-MM-dd"`
	Finish   *string `json:"finish,omitempty" jsonschema:"New last day, yyyy-MM-dd"`
	Archived *bool   `json:"archived,omitempty" jsonschema:"Archive (true) or restore (false) the sprint"`
}

type sprintIssueIn struct {
	Board   string `json:"board" jsonschema:"Agile board name or ID, e.g. 195-13"`
	Sprint  string `json:"sprint,omitempty" jsonschema:"Sprint name or ID, or current. Default: current."`
	IssueID string `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
}

type deleteSprintIn struct {
	Board  string `json:"board" jsonschema:"Agile board name or ID, e.g. 195-13"`
	Sprint string `json:"sprint" jsonschema:"Sprint name or ID"`
}

func (t *toolset) registerSprints(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name:        "create_sprint",
		Description: "Creates a sprint on an agile board that uses sprints. Returns the sprint with its ID and URL.",
		Annotations: mutating("Create sprint", false),
	}, t.createSprint)

	add(s, &mcp.Tool{
		Name:        "update_sprint",
		Description: "Changes a sprint's name, goal or dates, or archives and restores it. Only the given properties change.",
		Annotations: mutating("Update sprint", false),
	}, t.updateSprint)

	add(s, &mcp.Tool{
		Name:        "add_issue_to_sprint",
		Description: "Adds an issue to a sprint (the current one by default) of an agile board.",
		Annotations: mutating("Add issue to sprint", false),
	}, t.addIssueToSprint)

	add(s, &mcp.Tool{
		Name:        "remove_issue_from_sprint",
		Description: "Removes an issue from a sprint (the current one by default) of an agile board. The issue itself is kept.",
		Annotations: mutating("Remove issue from sprint", false),
	}, t.removeIssueFromSprint)

	add(s, &mcp.Tool{
		Name: "delete_sprint",
		Description: "Permanently deletes a sprint from an agile board. Its issues are kept but leave the sprint. " +
			"To keep history, archive it with update_sprint instead.",
		Annotations: mutating("Delete sprint", true),
	}, t.deleteSprint)
}

// sprintBoard resolves a board and refuses boards that don't use sprints.
func (t *toolset) sprintBoard(ctx context.Context, ref string) (apiAgile, error) {
	b, err := t.resolveBoard(ctx, ref)
	if err != nil {
		return apiAgile{}, err
	}
	if b.SprintsSettings.DisableSprints {
		return apiAgile{}, fmt.Errorf("board %s doesn't use sprints", b.Name)
	}
	return b, nil
}

func sprintPath(b apiAgile, s apiSprint) string {
	return "agiles/" + url.PathEscape(b.ID) + "/sprints/" + url.PathEscape(s.ID)
}

// sprintDay parses yyyy-MM-dd as the start or the end of that day in UTC,
// the way sprint dates are stored on boards.
func sprintDay(s string, end bool) (int64, error) {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, fmt.Errorf("cannot parse date %q, use yyyy-MM-dd", s)
	}
	if end {
		d = d.Add(24*time.Hour - time.Millisecond)
	}
	return d.UnixMilli(), nil
}

func (t *toolset) createSprint(ctx context.Context, in createSprintIn) (any, error) {
	b, err := t.sprintBoard(ctx, in.Board)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"name": in.Name}
	if in.Goal != "" {
		body["goal"] = in.Goal
	}
	for key, val := range map[string]string{"start": in.Start, "finish": in.Finish} {
		if val == "" {
			continue
		}
		ms, err := sprintDay(val, key == "finish")
		if err != nil {
			return nil, err
		}
		body[key] = ms
	}
	var created apiSprint
	q := url.Values{"fields": {sprintFields}}
	if err := t.yt.Post(ctx, "agiles/"+url.PathEscape(b.ID)+"/sprints", q, body, &created); err != nil {
		return nil, err
	}
	return map[string]any{"board": b.Name, "sprint": created, "url": t.boardURL(b, created.ID)}, nil
}

func (t *toolset) updateSprint(ctx context.Context, in updateSprintIn) (any, error) {
	b, err := t.sprintBoard(ctx, in.Board)
	if err != nil {
		return nil, err
	}
	sprint, err := resolveSprint(b, in.Sprint)
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	if in.Name != nil {
		body["name"] = *in.Name
	}
	if in.Goal != nil {
		body["goal"] = *in.Goal
	}
	for key, val := range map[string]*string{"start": in.Start, "finish": in.Finish} {
		if val == nil {
			continue
		}
		ms, err := sprintDay(*val, key == "finish")
		if err != nil {
			return nil, err
		}
		body[key] = ms
	}
	if in.Archived != nil {
		body["archived"] = *in.Archived
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("nothing to update")
	}
	var updated apiSprint
	if err := t.yt.Post(ctx, sprintPath(b, sprint), url.Values{"fields": {sprintFields}}, body, &updated); err != nil {
		return nil, err
	}
	return map[string]any{"board": b.Name, "sprint": updated, "url": t.boardURL(b, updated.ID)}, nil
}

func (t *toolset) issueDBID(ctx context.Context, issueID string) (string, error) {
	var is struct {
		ID string `json:"id"`
	}
	if err := t.yt.Get(ctx, "issues/"+url.PathEscape(issueID), url.Values{"fields": {"id"}}, &is); err != nil {
		return "", err
	}
	return is.ID, nil
}

func (t *toolset) addIssueToSprint(ctx context.Context, in sprintIssueIn) (any, error) {
	b, sprint, dbid, err := t.sprintIssue(ctx, in)
	if err != nil {
		return nil, err
	}
	if err := t.yt.Post(ctx, sprintPath(b, sprint)+"/issues", url.Values{"fields": {"id"}}, map[string]any{"id": dbid}, nil); err != nil {
		return nil, err
	}
	return map[string]any{"board": b.Name, "sprint": sprint.Name, "added": in.IssueID}, nil
}

func (t *toolset) removeIssueFromSprint(ctx context.Context, in sprintIssueIn) (any, error) {
	b, sprint, dbid, err := t.sprintIssue(ctx, in)
	if err != nil {
		return nil, err
	}
	if err := t.yt.Delete(ctx, sprintPath(b, sprint)+"/issues/"+url.PathEscape(dbid), nil); err != nil {
		return nil, err
	}
	return map[string]any{"board": b.Name, "sprint": sprint.Name, "removed": in.IssueID}, nil
}

func (t *toolset) sprintIssue(ctx context.Context, in sprintIssueIn) (apiAgile, apiSprint, string, error) {
	b, err := t.sprintBoard(ctx, in.Board)
	if err != nil {
		return apiAgile{}, apiSprint{}, "", err
	}
	sprint, err := resolveSprint(b, in.Sprint)
	if err != nil {
		return apiAgile{}, apiSprint{}, "", err
	}
	dbid, err := t.issueDBID(ctx, in.IssueID)
	if err != nil {
		return apiAgile{}, apiSprint{}, "", err
	}
	return b, sprint, dbid, nil
}

func (t *toolset) deleteSprint(ctx context.Context, in deleteSprintIn) (any, error) {
	b, err := t.sprintBoard(ctx, in.Board)
	if err != nil {
		return nil, err
	}
	sprint, err := resolveSprint(b, in.Sprint)
	if err != nil {
		return nil, err
	}
	if err := t.yt.Delete(ctx, sprintPath(b, sprint), nil); err != nil {
		return nil, err
	}
	return map[string]any{"board": b.Name, "deletedSprint": sprint.Name}, nil
}
