package tools

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// activityCategories maps tool-level names to YouTrack activity categories.
var activityCategories = map[string]string{
	"fields":      "CustomFieldCategory",
	"comments":    "CommentsCategory",
	"links":       "LinksCategory",
	"summary":     "SummaryCategory",
	"description": "DescriptionCategory",
	"created":     "IssueCreatedCategory",
	"tags":        "TagsCategory",
	"attachments": "AttachmentsCategory",
	"sprints":     "SprintCategory",
	"work":        "WorkItemCategory",
	"vcs":         "VcsChangeCategory",
	"project":     "ProjectCategory",
	"resolved":    "IssueResolvedCategory",
}

var activityNames = func() []string {
	names := make([]string, 0, len(activityCategories))
	for n := range activityCategories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}()

type getIssueActivitiesIn struct {
	IssueID     string   `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
	Categories  []string `json:"categories,omitempty" jsonschema:"Kinds of changes to include. Default: all."`
	OldestFirst bool     `json:"oldestFirst,omitempty" jsonschema:"Return the oldest changes first. Default: newest first."`
	Limit       int      `json:"limit,omitempty" jsonschema:"Max activities to return. Default 20, max 50."`
	Cursor      string   `json:"cursor,omitempty" jsonschema:"nextCursor from a previous call, to continue"`
}

type apiActivity struct {
	ID        string   `json:"id"`
	Timestamp millis   `json:"timestamp"`
	Author    *userRef `json:"author"`
	Category  struct {
		ID string `json:"id"`
	} `json:"category"`
	Field *struct {
		Name string `json:"name"`
	} `json:"field"`
	Added   json.RawMessage `json:"added"`
	Removed json.RawMessage `json:"removed"`
}

const activityValueFields = "name,login,text,presentation,idReadable,summary,minutes,duration(presentation)"

func (t *toolset) registerActivities(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name: "get_issue_activities",
		Description: "Returns the change history of an issue: who changed which field, comments, links, tags, " +
			"attachments, sprints and work items, with old and new values. Newest first by default; paginated with a cursor.",
		Annotations: readOnly("Get issue activities"),
		InputSchema: schemaFor[getIssueActivitiesIn](func(p map[string]*jsonschema.Schema) {
			p["categories"].Items.Enum = enum(activityNames...)
		}),
	}, t.getIssueActivities)
}

func (t *toolset) getIssueActivities(ctx context.Context, in getIssueActivitiesIn) (any, error) {
	_, limit := page(0, in.Limit, 20, 50)
	names := in.Categories
	if len(names) == 0 {
		names = activityNames
	}
	cats := make([]string, 0, len(names))
	for _, n := range names {
		if c, ok := activityCategories[strings.ToLower(n)]; ok {
			cats = append(cats, c)
		}
	}
	q := url.Values{
		"categories": {strings.Join(cats, ",")},
		"reverse":    {strconv.FormatBool(!in.OldestFirst)},
		"fields": {"hasAfter,afterCursor,hasBefore,beforeCursor,activities(id,timestamp,author(login),category(id),field(name)," +
			"added(" + activityValueFields + "),removed(" + activityValueFields + "))"},
		"$top": {strconv.Itoa(limit)},
	}
	if in.Cursor != "" {
		q.Set("cursor", in.Cursor)
	}
	var res struct {
		HasAfter     bool          `json:"hasAfter"`
		AfterCursor  string        `json:"afterCursor"`
		HasBefore    bool          `json:"hasBefore"`
		BeforeCursor string        `json:"beforeCursor"`
		Activities   []apiActivity `json:"activities"`
	}
	if err := t.yt.Get(ctx, "issues/"+url.PathEscape(in.IssueID)+"/activitiesPage", q, &res); err != nil {
		return nil, err
	}

	type activity struct {
		Time    millis `json:"time"`
		Author  string `json:"author,omitempty"`
		Kind    string `json:"kind"`
		Field   string `json:"field,omitempty"`
		Added   any    `json:"added,omitempty"`
		Removed any    `json:"removed,omitempty"`
	}
	kindOf := map[string]string{}
	for n, c := range activityCategories {
		kindOf[c] = n
	}
	out := make([]activity, 0, len(res.Activities))
	for _, a := range res.Activities {
		act := activity{Time: a.Timestamp, Kind: kindOf[a.Category.ID], Added: activityValue(a.Added), Removed: activityValue(a.Removed)}
		if act.Kind == "" {
			act.Kind = a.Category.ID
		}
		if a.Author != nil {
			act.Author = a.Author.Login
		}
		if a.Field != nil {
			act.Field = a.Field.Name
		}
		if strings.Contains(strings.ToLower(act.Field), "date") || act.Kind == "resolved" {
			act.Added, act.Removed = asDate(act.Added), asDate(act.Removed)
		}
		out = append(out, act)
	}
	result := map[string]any{"issueId": in.IssueID, "activities": out}
	// Newest-first pages continue towards older entries, i.e. "before".
	if !in.OldestFirst && res.HasBefore && res.BeforeCursor != "" {
		result["nextCursor"] = res.BeforeCursor
	} else if in.OldestFirst && res.HasAfter && res.AfterCursor != "" {
		result["nextCursor"] = res.AfterCursor
	}
	return result, nil
}

// activityValue reduces added/removed values to plain names, logins, IDs or
// shortened text. Empty lists become nil so they are omitted.
func activityValue(raw json.RawMessage) any {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return shorten(s, 300)
	}
	items, ok := v.([]any)
	if !ok {
		items = []any{v}
	}
	if len(items) == 0 {
		return nil
	}
	out := make([]any, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			out = append(out, it)
			continue
		}
		switch {
		case m["idReadable"] != nil:
			out = append(out, m["idReadable"])
		case m["login"] != nil:
			out = append(out, m["login"])
		case m["text"] != nil:
			out = append(out, shorten(m["text"].(string), 300))
		case m["presentation"] != nil:
			out = append(out, m["presentation"])
		case m["duration"] != nil:
			out = append(out, m["duration"].(map[string]any)["presentation"])
		case m["name"] != nil:
			out = append(out, m["name"])
		default:
			delete(m, "$type")
			if len(m) > 0 {
				out = append(out, m)
			}
		}
	}
	if len(out) == 1 {
		return out[0]
	}
	return out
}

// asDate renders a millisecond timestamp as RFC 3339 and leaves anything else.
func asDate(v any) any {
	if n, ok := v.(float64); ok {
		return formatMillis(int64(n))
	}
	return v
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
