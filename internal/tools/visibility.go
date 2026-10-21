package tools

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// apiVisibility is the visibility of an issue, comment or article.
type apiVisibility struct {
	Type            string    `json:"$type"`
	PermittedUsers  []userRef `json:"permittedUsers"`
	PermittedGroups []struct {
		Name string `json:"name"`
	} `json:"permittedGroups"`
}

const visibilityFields = "visibility($type,permittedUsers(login),permittedGroups(name))"

// public renders visibility for tool output; nil means visible to everyone
// who can see the project.
func (v *apiVisibility) public() any {
	if v == nil || v.Type != "LimitedVisibility" {
		return nil
	}
	users := make([]string, 0, len(v.PermittedUsers))
	for _, u := range v.PermittedUsers {
		users = append(users, u.Login)
	}
	groups := make([]string, 0, len(v.PermittedGroups))
	for _, g := range v.PermittedGroups {
		groups = append(groups, g.Name)
	}
	return map[string]any{"permittedUsers": users, "permittedGroups": groups}
}

// limitedVisibility builds a LimitedVisibility payload from user logins and
// group names, or returns nil when both are empty.
func (t *toolset) limitedVisibility(ctx context.Context, logins, groupNames []string) (map[string]any, error) {
	if len(logins) == 0 && len(groupNames) == 0 {
		return nil, nil
	}
	users := []map[string]string{}
	for _, login := range logins {
		var found []struct {
			ID    string `json:"id"`
			Login string `json:"login"`
		}
		if err := t.yt.Get(ctx, "users", url.Values{"query": {login}, "fields": {"id,login"}, "$top": {"20"}}, &found); err != nil {
			return nil, err
		}
		id := ""
		for _, u := range found {
			if strings.EqualFold(u.Login, login) {
				id = u.ID
				break
			}
		}
		if id == "" {
			return nil, fmt.Errorf("permitted user %q not found", login)
		}
		users = append(users, map[string]string{"id": id})
	}

	groups := []map[string]string{}
	if len(groupNames) > 0 {
		all, err := t.listGroups(ctx)
		if err != nil {
			return nil, err
		}
	names:
		for _, name := range groupNames {
			for _, g := range all {
				if strings.EqualFold(g.Name, name) {
					if g.ID == "" {
						return nil, fmt.Errorf("permitted group %q: reading YouTrack group IDs needs permission to read user groups", name)
					}
					groups = append(groups, map[string]string{"id": g.ID})
					continue names
				}
			}
			return nil, fmt.Errorf("permitted group %q not found; use find_user_groups to list groups", name)
		}
	}
	return map[string]any{"$type": "LimitedVisibility", "permittedUsers": users, "permittedGroups": groups}, nil
}
