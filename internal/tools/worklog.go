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

func (t *toolset) registerWorkItems(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name: "log_work",
		Description: "Adds a spent time work item to an issue. Use get_project to see the project's work types " +
			"and work item attributes. Returns the created work item.",
		Annotations: mutating("Log work", false),
		InputSchema: schemaFor[logWorkIn](func(p map[string]*jsonschema.Schema) {
			p["durationMinutes"].ExclusiveMinimum = new(float64)
		}),
	}, t.logWork)
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

	var item struct {
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
	path := "issues/" + url.PathEscape(in.IssueID) + "/timeTracking/workItems"
	if err := t.yt.Post(ctx, path, url.Values{"fields": {workItemFields}}, body, &item); err != nil {
		return nil, err
	}

	out := map[string]any{
		"id":          item.ID,
		"issueId":     in.IssueID,
		"date":        item.Date,
		"duration":    item.Duration.Presentation,
		"minutes":     item.Duration.Minutes,
		"description": item.Text,
	}
	if item.Type != nil {
		out["workType"] = item.Type.Name
	}
	if item.Author != nil {
		out["author"] = item.Author.Login
	}
	if len(item.Attributes) > 0 {
		attrs := map[string]any{}
		for _, a := range item.Attributes {
			if a.Value != nil {
				attrs[a.Name] = a.Value.Name
			} else {
				attrs[a.Name] = nil
			}
		}
		out["attributes"] = attrs
	}
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

func parseWorkDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse date %q, use yyyy-MM-dd or yyyy-MM-dd HH:mm:ss", s)
}
