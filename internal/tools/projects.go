package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type findProjectsIn struct {
	NameContains string `json:"nameContains" jsonschema:"Case-insensitive substring of the project name or key; empty string lists all projects"`
	Offset       int    `json:"offset,omitempty" jsonschema:"Number of projects to skip. Default 0."`
	Limit        int    `json:"limit,omitempty" jsonschema:"Max projects to return. Default 20, max 20."`
}

type getProjectIn struct {
	ProjectKey string `json:"projectKey" jsonschema:"Project key, e.g. DEMO"`
}

const projectFields = "id,shortName,name,description,archived,leader(login,fullName),createdBy(login,fullName)," +
	"customFields(field(name))"

const timeTrackingFields = "enabled,estimate(field(name)),timeSpent(field(name))," +
	"workItemTypes(id,name),attributes(id,name,values(id,name))"

func (t *toolset) registerProjects(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name:        "find_projects",
		Description: "Finds projects whose name or key contains a substring (case-insensitive). Returns key, name and ID. Paginated.",
		Annotations: readOnly("Find projects"),
	}, t.findProjects)

	add(s, &mcp.Tool{
		Name: "get_project",
		Description: "Returns project details: key, name, description, leader, custom field names and time tracking " +
			"settings including work types and work item attributes (for log_work).",
		Annotations: readOnly("Get project"),
	}, t.getProject)
}

func (t *toolset) listProjects(ctx context.Context) ([]apiProjectRef, error) {
	var projects []apiProjectRef
	q := url.Values{"fields": {"id,shortName,name"}, "$top": {"-1"}}
	err := t.yt.Get(ctx, "admin/projects", q, &projects)
	return projects, err
}

// resolveProject finds a project by short name (key) or database ID.
func (t *toolset) resolveProject(ctx context.Context, key string) (apiProjectRef, error) {
	projects, err := t.listProjects(ctx)
	if err != nil {
		return apiProjectRef{}, err
	}
	for _, p := range projects {
		if strings.EqualFold(p.ShortName, key) || p.ID == key {
			return p, nil
		}
	}
	return apiProjectRef{}, fmt.Errorf("project %q not found; use find_projects to list available projects", key)
}

func (t *toolset) findProjects(ctx context.Context, in findProjectsIn) (any, error) {
	offset, limit := page(in.Offset, in.Limit, 20, 20)
	projects, err := t.listProjects(ctx)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(in.NameContains)
	matched := []apiProjectRef{}
	for _, p := range projects {
		if strings.Contains(strings.ToLower(p.Name), needle) || strings.Contains(strings.ToLower(p.ShortName), needle) {
			matched = append(matched, p)
		}
	}
	total := len(matched)
	matched = matched[min(offset, total):min(offset+limit, total)]
	return map[string]any{"projects": matched, "offset": offset, "total": total}, nil
}

func (t *toolset) getProject(ctx context.Context, in getProjectIn) (any, error) {
	ref, err := t.resolveProject(ctx, in.ProjectKey)
	if err != nil {
		return nil, err
	}
	var p struct {
		ID           string   `json:"id"`
		ShortName    string   `json:"shortName"`
		Name         string   `json:"name"`
		Description  string   `json:"description"`
		Archived     bool     `json:"archived"`
		Leader       *userRef `json:"leader"`
		CreatedBy    *userRef `json:"createdBy"`
		CustomFields []struct {
			Field struct {
				Name string `json:"name"`
			} `json:"field"`
		} `json:"customFields"`
	}
	if err := t.yt.Get(ctx, "admin/projects/"+url.PathEscape(ref.ID), url.Values{"fields": {projectFields}}, &p); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(p.CustomFields))
	for _, f := range p.CustomFields {
		names = append(names, f.Field.Name)
	}
	if len(names) == 0 {
		// Without admin rights the project settings list no fields.
		if schema, err := t.projectFields(ctx, ref); err == nil {
			names = fieldNames(schema)
		}
	}

	out := map[string]any{
		"id":           p.ID,
		"key":          p.ShortName,
		"name":         p.Name,
		"description":  p.Description,
		"archived":     p.Archived,
		"leader":       p.Leader,
		"createdBy":    p.CreatedBy,
		"customFields": names,
		"url":          t.yt.BaseURL() + "/projects/" + p.ShortName,
	}
	// Time tracking needs extra permissions; report the failure instead of failing the tool.
	var tt json.RawMessage
	if err := t.yt.Get(ctx, "admin/projects/"+url.PathEscape(ref.ID)+"/timeTrackingSettings",
		url.Values{"fields": {timeTrackingFields}}, &tt); err != nil {
		out["timeTrackingSettings"] = map[string]any{"error": err.Error()}
	} else {
		out["timeTrackingSettings"] = tt
	}
	return out, nil
}
