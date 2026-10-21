package tools

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const searchQueryHelp = `YouTrack search query. Syntax:
- free text matches summary, description and comments; wildcards * and ?
- attribute filters: project: DEMO, for: me, reporter: jdoe, tag: {multi word}, <Field>: <Value> (e.g. State: Open, Priority: Critical)
- negation with -: State: -Fixed; multi-word names and values in braces: {Fix versions}: {2026.1 EAP}
- dates: created: {This month}, updated: {minus 2h} .. *, created: 2026-01-01 .. 2026-02-01
- shortcuts: #Unresolved, #Resolved, has: attachments, has: -comments
- links: <link type>: DEMO-1 (e.g. subtask of: DEMO-1), links: DEMO-1, mentions: DEMO-1
- terms are joined with AND by space; use "or" and parentheses for alternatives
- sort: sort by: updated desc`

var defaultSearchFields = []string{"Type", "State", "Assignee"}

type apiIssue struct {
	IDReadable    string        `json:"idReadable"`
	Summary       string        `json:"summary"`
	Description   string        `json:"description"`
	Created       millis        `json:"created"`
	Updated       millis        `json:"updated"`
	Resolved      millis        `json:"resolved"`
	Project       apiProjectRef `json:"project"`
	Reporter      *userRef      `json:"reporter"`
	Votes         int           `json:"votes"`
	CommentsCount int           `json:"commentsCount"`
	Tags          []struct {
		Name string `json:"name"`
	} `json:"tags"`
	CustomFields []customField `json:"customFields"`
	Links        []apiLink     `json:"links"`
	Attachments  []struct {
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		MimeType string `json:"mimeType"`
		URL      string `json:"url"`
	} `json:"attachments"`
}

type apiProjectRef struct {
	ID        string `json:"id,omitempty"`
	ShortName string `json:"shortName"`
	Name      string `json:"name,omitempty"`
}

type apiComment struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Created millis   `json:"created"`
	Updated millis   `json:"updated"`
	Author  *userRef `json:"author"`
}

const (
	issueListFields = "idReadable,summary,created,updated,resolved,project(shortName),reporter(login,fullName),customFields(" + customFieldValueFields + ")"
	issueFullFields = "idReadable,summary,description,created,updated,resolved,votes,commentsCount," +
		"project(id,shortName,name),reporter(login,fullName),tags(name)," +
		"customFields(" + customFieldValueFields + ")," +
		"links(" + linkFields + ")," +
		"attachments(name,size,mimeType,url)"
	commentFields = "id,text,created,updated,author(login,fullName)"
)

func (t *toolset) issueURL(id string) string {
	if isDraft(id) {
		return t.yt.BaseURL() + "/newIssue?draftId=" + url.QueryEscape(id)
	}
	return t.yt.BaseURL() + "/issue/" + id
}

type searchIssuesIn struct {
	Query                string   `json:"query" jsonschema:"YouTrack search query, see the tool description for syntax"`
	CustomFieldsToReturn []string `json:"customFieldsToReturn,omitempty" jsonschema:"Custom fields to include in results. Default: Type, State, Assignee. Pass [] for none."`
	Offset               int      `json:"offset,omitempty" jsonschema:"Number of issues to skip. Default 0."`
	Limit                int      `json:"limit,omitempty" jsonschema:"Max issues to return. Default 20, max 20."`
}

type issueBrief struct {
	ID           string         `json:"id"`
	Summary      string         `json:"summary"`
	Project      string         `json:"project"`
	Resolved     millis         `json:"resolved"`
	Reporter     string         `json:"reporter,omitempty"`
	Created      millis         `json:"created"`
	Updated      millis         `json:"updated"`
	CustomFields map[string]any `json:"customFields,omitempty"`
	URL          string         `json:"url"`
}

type getIssueIn struct {
	IssueID             string `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123, or draft ID, e.g. 3-22"`
	RecentCommentsCount *int   `json:"recentCommentsCount,omitempty" jsonschema:"How many latest comments to include. Default 5."`
}

type getIssueCommentsIn struct {
	IssueID string `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
	Offset  int    `json:"offset,omitempty" jsonschema:"Number of comments to skip. Default 0."`
	Limit   int    `json:"limit,omitempty" jsonschema:"Max comments to return. Default 10, max 10."`
}

type addIssueCommentIn struct {
	IssueID         string   `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
	Text            string   `json:"text" jsonschema:"Comment text, Markdown supported"`
	PermittedUsers  []string `json:"permittedUsers,omitempty" jsonschema:"Limit visibility to these user logins"`
	PermittedGroups []string `json:"permittedGroups,omitempty" jsonschema:"Limit visibility to these group names"`
}

func (t *toolset) registerIssues(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name: "search_issues",
		Description: "Searches issues with the YouTrack query language and returns id, summary, project, resolved, reporter, " +
			"created, updated and the requested custom fields. Use get_issue for full details. Paginated.\n\n" + searchQueryHelp,
		Annotations: readOnly("Search issues"),
	}, t.searchIssues)

	add(s, &mcp.Tool{
		Name: "get_issue",
		Description: "Returns full details of an issue: summary, description, URL, project, reporter, tags, votes, " +
			"custom fields, links, attachments and the most recent comments.",
		Annotations: readOnly("Get issue"),
	}, t.getIssue)

	add(s, &mcp.Tool{
		Name:        "get_issue_comments",
		Description: "Returns comments of an issue, oldest first. Paginated.",
		Annotations: readOnly("Get issue comments"),
	}, t.getIssueComments)

	add(s, &mcp.Tool{
		Name:        "add_issue_comment",
		Description: "Adds a comment to an issue, optionally visible only to given users and groups. Markdown is supported. Returns the comment ID and URL.",
		Annotations: mutating("Add issue comment", false),
	}, t.addIssueComment)
}

func (t *toolset) searchIssues(ctx context.Context, in searchIssuesIn) (any, error) {
	offset, limit := page(in.Offset, in.Limit, 20, 20)
	fieldNames := in.CustomFieldsToReturn
	if fieldNames == nil {
		fieldNames = defaultSearchFields
	}

	q := url.Values{
		"query":  {in.Query},
		"fields": {issueListFields},
		"$skip":  {strconv.Itoa(offset)},
		"$top":   {strconv.Itoa(limit + 1)}, // one extra to report hasMore
	}
	if len(fieldNames) == 0 {
		q.Set("fields", "idReadable,summary,created,updated,resolved,project(shortName),reporter(login,fullName)")
	}
	for _, name := range fieldNames {
		q.Add("customFields", name)
	}

	var issues []apiIssue
	if err := t.yt.Get(ctx, "issues", q, &issues); err != nil {
		return nil, err
	}
	hasMore := len(issues) > limit
	if hasMore {
		issues = issues[:limit]
	}

	out := make([]issueBrief, 0, len(issues))
	for _, is := range issues {
		b := issueBrief{
			ID:       is.IDReadable,
			Summary:  is.Summary,
			Project:  is.Project.ShortName,
			Resolved: is.Resolved,
			Created:  is.Created,
			Updated:  is.Updated,
			URL:      t.issueURL(is.IDReadable),
		}
		if is.Reporter != nil {
			b.Reporter = is.Reporter.Login
		}
		if len(fieldNames) > 0 {
			b.CustomFields = flattenFields(is.CustomFields)
		}
		out = append(out, b)
	}
	return map[string]any{"issues": out, "offset": offset, "hasMore": hasMore}, nil
}

func (t *toolset) getIssue(ctx context.Context, in getIssueIn) (any, error) {
	var is apiIssue
	if err := t.yt.Get(ctx, issuePath(in.IssueID), url.Values{"fields": {issueFullFields}}, &is); err != nil {
		return nil, err
	}

	recent := 5
	if in.RecentCommentsCount != nil {
		recent = max(*in.RecentCommentsCount, 0)
	}
	comments := []apiComment{}
	if recent > 0 && is.CommentsCount > 0 {
		q := url.Values{
			"fields": {commentFields},
			"$skip":  {strconv.Itoa(max(is.CommentsCount-recent, 0))},
			"$top":   {strconv.Itoa(recent)},
		}
		if err := t.yt.Get(ctx, "issues/"+url.PathEscape(in.IssueID)+"/comments", q, &comments); err != nil {
			return nil, err
		}
	}

	tags := make([]string, 0, len(is.Tags))
	for _, tg := range is.Tags {
		tags = append(tags, tg.Name)
	}

	type attachment struct {
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		MimeType string `json:"mimeType,omitempty"`
		URL      string `json:"url"`
	}
	attachments := make([]attachment, 0, len(is.Attachments))
	for _, a := range is.Attachments {
		attachments = append(attachments, attachment{a.Name, a.Size, a.MimeType, t.yt.BaseURL() + a.URL})
	}

	var reporter string
	if is.Reporter != nil {
		reporter = is.Reporter.Login
	}
	return map[string]any{
		"id":             is.IDReadable,
		"summary":        is.Summary,
		"description":    is.Description,
		"url":            t.issueURL(is.IDReadable),
		"project":        is.Project,
		"reporter":       reporter,
		"created":        is.Created,
		"updated":        is.Updated,
		"resolved":       is.Resolved,
		"votes":          is.Votes,
		"tags":           tags,
		"customFields":   flattenFields(is.CustomFields),
		"links":          linkMap(is.Links),
		"attachments":    attachments,
		"commentsCount":  is.CommentsCount,
		"recentComments": comments,
	}, nil
}

func (t *toolset) getIssueComments(ctx context.Context, in getIssueCommentsIn) (any, error) {
	offset, limit := page(in.Offset, in.Limit, 10, 10)
	q := url.Values{
		"fields": {commentFields},
		"$skip":  {strconv.Itoa(offset)},
		"$top":   {strconv.Itoa(limit + 1)},
	}
	var comments []apiComment
	if err := t.yt.Get(ctx, "issues/"+url.PathEscape(in.IssueID)+"/comments", q, &comments); err != nil {
		return nil, err
	}
	hasMore := len(comments) > limit
	if hasMore {
		comments = comments[:limit]
	}
	return map[string]any{"comments": comments, "offset": offset, "hasMore": hasMore}, nil
}

func (t *toolset) addIssueComment(ctx context.Context, in addIssueCommentIn) (any, error) {
	if in.Text == "" {
		return nil, fmt.Errorf("text must not be empty")
	}
	body := map[string]any{"text": in.Text}
	vis, err := t.limitedVisibility(ctx, in.PermittedUsers, in.PermittedGroups)
	if err != nil {
		return nil, err
	}
	if vis != nil {
		body["visibility"] = vis
	}
	var c apiComment
	err = t.yt.Post(ctx, "issues/"+url.PathEscape(in.IssueID)+"/comments",
		url.Values{"fields": {commentFields}}, body, &c)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"issueId": in.IssueID,
		"comment": c,
		"url":     fmt.Sprintf("%s#focus=Comments-%s.0-0", t.issueURL(in.IssueID), c.ID),
	}, nil
}
