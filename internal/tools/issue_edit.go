package tools

import (
	"context"
	"fmt"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type getIssueFieldsSchemaIn struct {
	ProjectKey string `json:"projectKey" jsonschema:"Project key, e.g. DEMO"`
}

type createIssueIn struct {
	Project         string         `json:"project" jsonschema:"Project key, e.g. DEMO. Ask the user if unknown."`
	Summary         string         `json:"summary" jsonschema:"Issue summary (title)"`
	Description     string         `json:"description,omitempty" jsonschema:"Issue description, plain text or Markdown"`
	CustomFields    map[string]any `json:"customFields,omitempty" jsonschema:"Custom field values keyed by exact field name from get_issue_fields_schema, e.g. {\"Type\":\"Task\",\"Priority\":\"Minor\"}. Multi-value fields take arrays."`
	ParentIssue     string         `json:"parentIssue,omitempty" jsonschema:"ID of the parent issue; the new issue becomes its subtask"`
	PermittedUsers  []string       `json:"permittedUsers,omitempty" jsonschema:"Limit visibility to these user logins"`
	PermittedGroups []string       `json:"permittedGroups,omitempty" jsonschema:"Limit visibility to these group names"`
}

type createDraftIssueIn struct {
	Project string `json:"project" jsonschema:"Project key, e.g. DEMO"`
	Summary string `json:"summary" jsonschema:"Draft summary"`
}

type updateIssueIn struct {
	IssueID      string         `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123, or draft ID, e.g. 3-22"`
	Summary      *string        `json:"summary,omitempty" jsonschema:"New summary"`
	Description  *string        `json:"description,omitempty" jsonschema:"New description, plain text or Markdown"`
	CustomFields map[string]any `json:"customFields,omitempty" jsonschema:"Custom fields to change, keyed by exact field name from get_issue_fields_schema. null clears a field."`
	Subscription *bool          `json:"subscription,omitempty" jsonschema:"true stars the issue for the current user, false unstars it"`
	Vote         *bool          `json:"vote,omitempty" jsonschema:"true votes for the issue, false removes the vote"`
}

type changeIssueAssigneeIn struct {
	IssueID       string  `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
	AssigneeLogin *string `json:"assigneeLogin" jsonschema:"Login of the new assignee, or null to unassign"`
}

func (t *toolset) registerIssueEdit(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name: "get_issue_fields_schema",
		Description: "Returns a JSON Schema of the project's custom fields with allowed values. " +
			"Must be used before create_issue and update_issue: property names are the exact field names to pass.",
		Annotations: readOnly("Get issue fields schema"),
	}, t.getIssueFieldsSchema)

	add(s, &mcp.Tool{
		Name: "create_issue",
		Description: "Creates an issue. Call get_issue_fields_schema first: projects may require fields on creation. " +
			"If the project is not known, ask the user. Returns the issue ID and URL.",
		Annotations: mutating("Create issue", false),
	}, t.createIssue)

	add(s, &mcp.Tool{
		Name: "create_draft_issue",
		Description: "Creates an issue draft visible only to the current user. Fill it in with update_issue " +
			"using the returned draft ID. Returns the draft ID (e.g. 3-22) and URL.",
		Annotations: mutating("Create draft issue", false),
	}, t.createDraftIssue)

	add(s, &mcp.Tool{
		Name: "update_issue",
		Description: "Updates an issue: summary, description, a subset of custom fields, star (subscription) and vote. " +
			"Only the given properties change; to change part of the description, use edit_issue_description. Returns the issue ID and what was changed.",
		Annotations: mutating("Update issue", false),
	}, t.updateIssue)

	add(s, &mcp.Tool{
		Name:        "change_issue_assignee",
		Description: "Sets the Assignee of an issue. Pass null to unassign.",
		Annotations: mutating("Change issue assignee", false),
	}, t.changeIssueAssignee)
}

func (t *toolset) getIssueFieldsSchema(ctx context.Context, in getIssueFieldsSchemaIn) (any, error) {
	p, err := t.resolveProject(ctx, in.ProjectKey)
	if err != nil {
		return nil, err
	}
	schema, err := t.projectFields(ctx, p)
	if err != nil {
		return nil, err
	}
	return fieldsJSONSchema(schema), nil
}

func (t *toolset) createIssue(ctx context.Context, in createIssueIn) (any, error) {
	p, err := t.resolveProject(ctx, in.Project)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"project": map[string]any{"id": p.ID},
		"summary": in.Summary,
	}
	if in.Description != "" {
		body["description"] = in.Description
	}
	if len(in.CustomFields) > 0 {
		fields, err := t.customFieldsFor(ctx, p, in.CustomFields)
		if err != nil {
			return nil, err
		}
		body["customFields"] = fields
	}
	vis, err := t.limitedVisibility(ctx, in.PermittedUsers, in.PermittedGroups)
	if err != nil {
		return nil, err
	}
	if vis != nil {
		body["visibility"] = vis
	}

	var created struct {
		IDReadable string `json:"idReadable"`
	}
	if err := t.yt.Post(ctx, "issues", url.Values{"fields": {"idReadable"}}, body, &created); err != nil {
		return nil, err
	}
	out := map[string]any{"id": created.IDReadable, "url": t.issueURL(created.IDReadable)}
	if in.ParentIssue != "" {
		if err := t.command(ctx, "subtask of "+in.ParentIssue, created.IDReadable); err != nil {
			return nil, fmt.Errorf("issue %s created, but linking to parent failed: %w", created.IDReadable, err)
		}
		out["parentIssue"] = in.ParentIssue
	}
	return out, nil
}

func (t *toolset) createDraftIssue(ctx context.Context, in createDraftIssueIn) (any, error) {
	p, err := t.resolveProject(ctx, in.Project)
	if err != nil {
		return nil, err
	}
	var draft struct {
		ID string `json:"id"`
	}
	body := map[string]any{"project": map[string]any{"id": p.ID}, "summary": in.Summary}
	if err := t.yt.Post(ctx, "users/me/drafts", url.Values{"fields": {"id"}}, body, &draft); err != nil {
		return nil, err
	}
	return map[string]any{"draftId": draft.ID, "url": t.issueURL(draft.ID)}, nil
}

func (t *toolset) updateIssue(ctx context.Context, in updateIssueIn) (any, error) {
	if isDraft(in.IssueID) && (in.Subscription != nil || in.Vote != nil) {
		return nil, fmt.Errorf("drafts can't be starred or voted for")
	}
	body := map[string]any{}
	var changed []string
	if in.Summary != nil {
		body["summary"] = *in.Summary
		changed = append(changed, "summary")
	}
	if in.Description != nil {
		body["description"] = *in.Description
		changed = append(changed, "description")
	}
	if len(in.CustomFields) > 0 {
		fields, err := t.issueCustomFields(ctx, in.IssueID, in.CustomFields)
		if err != nil {
			return nil, err
		}
		body["customFields"] = fields
		for name := range in.CustomFields {
			changed = append(changed, name)
		}
	}
	if len(body) > 0 {
		if err := t.yt.Post(ctx, issuePath(in.IssueID), url.Values{"fields": {"id"}}, body, nil); err != nil {
			return nil, err
		}
	}
	if in.Subscription != nil {
		if err := t.setStar(ctx, in.IssueID, *in.Subscription); err != nil {
			return nil, err
		}
		changed = append(changed, pick(*in.Subscription, "starred", "unstarred"))
	}
	if in.Vote != nil {
		if err := t.command(ctx, pick(*in.Vote, "vote", "unvote"), in.IssueID); err != nil {
			return nil, err
		}
		changed = append(changed, pick(*in.Vote, "voted", "unvoted"))
	}
	if len(changed) == 0 {
		return nil, fmt.Errorf("nothing to update")
	}
	return map[string]any{"id": in.IssueID, "url": t.issueURL(in.IssueID), "changed": changed}, nil
}

func (t *toolset) changeIssueAssignee(ctx context.Context, in changeIssueAssigneeIn) (any, error) {
	var v any
	if in.AssigneeLogin != nil && *in.AssigneeLogin != "" {
		v = *in.AssigneeLogin
	}
	fields, err := t.issueCustomFields(ctx, in.IssueID, map[string]any{"Assignee": v})
	if err != nil {
		return nil, err
	}
	var updated struct {
		CustomFields []customField `json:"customFields"`
	}
	q := url.Values{"fields": {"customFields(" + customFieldValueFields + ")"}, "customFields": {"Assignee"}}
	if err := t.yt.Post(ctx, issuePath(in.IssueID), q, map[string]any{"customFields": fields}, &updated); err != nil {
		return nil, err
	}
	return map[string]any{"id": in.IssueID, "assignee": flattenFields(updated.CustomFields)["Assignee"]}, nil
}

// isDraft reports whether id is a draft's database ID rather than a readable issue ID.
func isDraft(id string) bool { return dbID.MatchString(id) }

// issuePath is the REST path of an issue or, for draft IDs, of the current user's draft.
func issuePath(id string) string {
	if isDraft(id) {
		return "users/me/drafts/" + url.PathEscape(id)
	}
	return "issues/" + url.PathEscape(id)
}

// issueProject returns the project an issue belongs to.
func (t *toolset) issueProject(ctx context.Context, issueID string) (apiProjectRef, error) {
	var is struct {
		Project apiProjectRef `json:"project"`
	}
	err := t.yt.Get(ctx, issuePath(issueID), url.Values{"fields": {"project(id,shortName)"}}, &is)
	return is.Project, err
}

// issueCustomFields builds the REST payload against the schema of the issue's project.
func (t *toolset) issueCustomFields(ctx context.Context, issueID string, values map[string]any) ([]map[string]any, error) {
	project, err := t.issueProject(ctx, issueID)
	if err != nil {
		return nil, err
	}
	return t.customFieldsFor(ctx, project, values)
}

// command applies a YouTrack command (as typed in the UI) to issues.
func (t *toolset) command(ctx context.Context, query string, issueIDs ...string) error {
	issues := make([]map[string]string, 0, len(issueIDs))
	for _, id := range issueIDs {
		issues = append(issues, map[string]string{"idReadable": id})
	}
	return t.yt.Post(ctx, "commands", nil, map[string]any{"query": query, "issues": issues}, nil)
}

func pick[T any](cond bool, yes, no T) T {
	if cond {
		return yes
	}
	return no
}
