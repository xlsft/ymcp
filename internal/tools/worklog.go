package tools

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type logWorkIn struct {
	IssueID            string            `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
	DurationMinutes    int               `json:"durationMinutes" jsonschema:"Spent time in minutes, greater than 0"`
	Description        string            `json:"description,omitempty" jsonschema:"What was done"`
	Date               string            `json:"date,omitempty" jsonschema:"When the work was done: yyyy-MM-dd or yyyy-MM-dd HH:mm:ss in local time. Default: now."`
	WorkType           string            `json:"workType,omitempty" jsonschema:"Work type name, e.g. Development; see workTypes in get_project"`
	WorkItemAttributes map[string]string `json:"workItemAttributes,omitempty" jsonschema:"Work item attributes as {attribute name: value name}; see timeTrackingSettings in get_project"`
}

type namedRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type timeTrackingSettings struct {
	WorkItemTypes []namedRef `json:"workItemTypes"`
	Attributes    []struct {
		namedRef
		Values []namedRef `json:"values"`
	} `json:"attributes"`
}

const workItemFields = "id,date,text,duration(minutes,presentation),type(name),author(login),attributes(name,value(name))"

type apiWorkItem struct {
	ID       string `json:"id"`
	Date     millis `json:"date"`
	Text     string `json:"text"`
	Duration struct {
		Minutes      int    `json:"minutes"`
		Presentation string `json:"presentation"`
	} `json:"duration"`
	Type       *namedRef `json:"type"`
	Author     *userRef  `json:"author"`
	Attributes []struct {
		Name  string    `json:"name"`
		Value *namedRef `json:"value"`
	} `json:"attributes"`
}

func (w apiWorkItem) public() map[string]any {
	out := map[string]any{
		"id":          w.ID,
		"date":        w.Date,
		"duration":    w.Duration.Presentation,
		"minutes":     w.Duration.Minutes,
		"description": w.Text,
	}
	if w.Type != nil {
		out["workType"] = w.Type.Name
	}
	if w.Author != nil {
		out["author"] = w.Author.Login
	}
	if len(w.Attributes) > 0 {
		attrs := map[string]any{}
		for _, a := range w.Attributes {
			if a.Value != nil {
				attrs[a.Name] = a.Value.Name
			} else {
				attrs[a.Name] = nil
			}
		}
		out["attributes"] = attrs
	}
	return out
}

type getIssueWorkItemsIn struct {
	IssueID string `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
	Offset  int    `json:"offset,omitempty" jsonschema:"Number of work items to skip. Default 0."`
	Limit   int    `json:"limit,omitempty" jsonschema:"Max work items to return. Default 20, max 50."`
}

type deleteWorkItemIn struct {
	IssueID    string `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
	WorkItemID string `json:"workItemId" jsonschema:"Work item ID from get_issue_work_items or log_work, e.g. 199-6"`
}

func (t *toolset) registerWorkItems(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name: "log_work",
		Description: "Adds a spent time work item to an issue. Use get_project to see the project's work types " +
			"and work item attributes. Returns the created work item.",
		Annotations: changes("Log work"),
		InputSchema: schemaFor[logWorkIn](func(p map[string]*jsonschema.Schema) {
			p["durationMinutes"].ExclusiveMinimum = new(float64)
		}),
	}, t.logWork)

	add(s, &mcp.Tool{
		Name:        "get_issue_work_items",
		Description: "Returns spent time work items of an issue, oldest first, with total minutes. Paginated.",
		Annotations: readOnly("Get issue work items"),
	}, t.getIssueWorkItems)

	add(s, &mcp.Tool{
		Name:        "delete_work_item",
		Description: "Permanently deletes a spent time work item. Work item IDs come from get_issue_work_items.",
		Annotations: removes("Delete work item"),
	}, t.deleteWorkItem)
}

func (t *toolset) getIssueWorkItems(ctx context.Context, in getIssueWorkItemsIn) (any, error) {
	offset, limit := page(in.Offset, in.Limit, 20, 50)
	var items []apiWorkItem
	path := "issues/" + url.PathEscape(in.IssueID) + "/timeTracking/workItems"
	if err := t.yt.Get(ctx, path, url.Values{"fields": {workItemFields}, "$top": {"-1"}}, &items); err != nil {
		return nil, err
	}
	totalMinutes := 0
	for _, w := range items {
		totalMinutes += w.Duration.Minutes
	}
	total := len(items)
	out := []map[string]any{}
	for _, w := range items[min(offset, total):min(offset+limit, total)] {
		out = append(out, w.public())
	}
	return map[string]any{"issueId": in.IssueID, "workItems": out, "offset": offset, "total": total, "totalMinutes": totalMinutes}, nil
}

func (t *toolset) deleteWorkItem(ctx context.Context, in deleteWorkItemIn) (any, error) {
	path := "issues/" + url.PathEscape(in.IssueID) + "/timeTracking/workItems/" + url.PathEscape(in.WorkItemID)
	if err := t.yt.Delete(ctx, path, nil); err != nil {
		return nil, err
	}
	return map[string]any{"issueId": in.IssueID, "deletedWorkItem": in.WorkItemID}, nil
}

func (t *toolset) logWork(ctx context.Context, in logWorkIn) (any, error) {
	if in.DurationMinutes <= 0 {
		return nil, fmt.Errorf("durationMinutes must be greater than 0")
	}
	date := time.Now()
	if in.Date != "" {
		var err error
		if date, err = parseWorkDate(in.Date); err != nil {
			return nil, err
		}
	}
	body := map[string]any{
		"duration": map[string]any{"minutes": in.DurationMinutes},
		"date":     date.UnixMilli(),
	}
	if in.Description != "" {
		body["text"] = in.Description
	}

	if in.WorkType != "" || len(in.WorkItemAttributes) > 0 {
		project, err := t.issueProject(ctx, in.IssueID)
		if err != nil {
			return nil, err
		}
		var tt timeTrackingSettings
		q := url.Values{"fields": {"workItemTypes(id,name),attributes(id,name,values(id,name))"}}
		if err := t.yt.Get(ctx, "admin/projects/"+url.PathEscape(project.ID)+"/timeTrackingSettings", q, &tt); err != nil {
			return nil, err
		}
		if in.WorkType != "" {
			wt, err := matchNamed(tt.WorkItemTypes, in.WorkType, "work type")
			if err != nil {
				return nil, err
			}
			body["type"] = map[string]any{"id": wt.ID}
		}
		if attrs, err := workItemAttributes(tt, in.WorkItemAttributes); err != nil {
			return nil, err
		} else if len(attrs) > 0 {
			body["attributes"] = attrs
		}
	}

	var item apiWorkItem
	path := "issues/" + url.PathEscape(in.IssueID) + "/timeTracking/workItems"
	if err := t.yt.Post(ctx, path, url.Values{"fields": {workItemFields}}, body, &item); err != nil {
		return nil, err
	}
	out := item.public()
	out["issueId"] = in.IssueID
	return out, nil
}

func workItemAttributes(tt timeTrackingSettings, values map[string]string) ([]map[string]any, error) {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []map[string]any
	for _, name := range names {
		var found bool
		for _, attr := range tt.Attributes {
			if !strings.EqualFold(attr.Name, name) {
				continue
			}
			found = true
			v, err := matchNamed(attr.Values, values[name], "value of attribute "+attr.Name)
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{
				"id":    attr.ID,
				"name":  attr.Name,
				"value": map[string]any{"id": v.ID, "name": v.Name},
			})
		}
		if !found {
			available := make([]namedRef, 0, len(tt.Attributes))
			for _, attr := range tt.Attributes {
				available = append(available, attr.namedRef)
			}
			return nil, fmt.Errorf("unknown work item attribute %q; available: %s", name, joinNames(available))
		}
	}
	return out, nil
}

func matchNamed(items []namedRef, name, what string) (namedRef, error) {
	for _, it := range items {
		if strings.EqualFold(it.Name, strings.TrimSpace(name)) {
			return it, nil
		}
	}
	return namedRef{}, fmt.Errorf("unknown %s %q; available: %s", what, name, joinNames(items))
}

func joinNames(items []namedRef) string {
	if len(items) == 0 {
		return "none"
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.Name)
	}
	return strings.Join(names, ", ")
}

// parseWorkDate reads a work item date. YouTrack keeps only the day, cut in
// the user's time zone, so a bare date is placed at noon UTC to land on the
// same day in any zone from UTC-11 to UTC+11.
func parseWorkDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Add(12 * time.Hour), nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse date %q, use yyyy-MM-dd or yyyy-MM-dd HH:mm:ss", s)
}
