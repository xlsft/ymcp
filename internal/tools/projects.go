package tools

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// resolveProject finds a project by short name (key) or database ID.
func (t *toolset) resolveProject(ctx context.Context, key string) (apiProjectRef, error) {
	var projects []apiProjectRef
	q := url.Values{"fields": {"id,shortName,name"}, "$top": {"-1"}}
	if err := t.yt.Get(ctx, "admin/projects", q, &projects); err != nil {
		return apiProjectRef{}, err
	}
	for _, p := range projects {
		if strings.EqualFold(p.ShortName, key) || p.ID == key {
			return p, nil
		}
	}
	return apiProjectRef{}, fmt.Errorf("project %q not found; use find_projects to list available projects", key)
}
