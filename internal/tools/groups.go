package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/youtrack-mcp/internal/youtrack"
)

type apiGroup struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	RingID         string         `json:"ringId"`
	UsersCount     int            `json:"usersCount"`
	TeamForProject *apiProjectRef `json:"teamForProject"`
}

type hubUser struct {
	Login   string `json:"login"`
	Name    string `json:"name"`
	Banned  bool   `json:"banned"`
	Profile struct {
		Email struct {
			Email string `json:"email"`
		} `json:"email"`
	} `json:"profile"`
}

type findUserGroupsIn struct {
	NameContains string `json:"nameContains" jsonschema:"Case-insensitive substring of the group name; empty string lists all groups"`
	Offset       int    `json:"offset,omitempty" jsonschema:"Number of groups to skip. Default 0."`
	Limit        int    `json:"limit,omitempty" jsonschema:"Max groups to return. Default 20, max 20."`
}

type getUserGroupMembersIn struct {
	Group  string `json:"group" jsonschema:"Group or project team name, as returned by find_user_groups"`
	Offset int    `json:"offset,omitempty" jsonschema:"Number of members to skip. Default 0."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Max members to return. Default 20, max 20."`
}

func (t *toolset) registerGroups(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name:        "find_user_groups",
		Description: "Finds user groups and project teams whose name contains a substring (case-insensitive). Paginated.",
		Annotations: readOnly("Find user groups"),
	}, t.findUserGroups)

	add(s, &mcp.Tool{
		Name:        "get_user_group_members",
		Description: "Returns members of a user group or project team: login, full name and email. Paginated.",
		Annotations: readOnly("Get user group members"),
	}, t.getUserGroupMembers)
}

func (t *toolset) listGroups(ctx context.Context) ([]apiGroup, error) {
	var groups []apiGroup
	q := url.Values{"fields": {"id,name,ringId,usersCount,teamForProject(shortName,name)"}, "$top": {"-1"}}
	err := t.yt.Get(ctx, "groups", q, &groups)
	return groups, err
}

func (t *toolset) findUserGroups(ctx context.Context, in findUserGroupsIn) (any, error) {
	offset, limit := page(in.Offset, in.Limit, 20, 20)
	groups, err := t.listGroups(ctx)
	if err != nil {
		return nil, err
	}
	type group struct {
		Name        string `json:"name"`
		ID          string `json:"id"`
		UsersCount  int    `json:"usersCount"`
		ProjectTeam string `json:"projectTeam,omitempty"`
	}
	needle := strings.ToLower(in.NameContains)
	matched := []group{}
	for _, g := range groups {
		if !strings.Contains(strings.ToLower(g.Name), needle) {
			continue
		}
		out := group{Name: g.Name, ID: g.ID, UsersCount: g.UsersCount}
		if g.TeamForProject != nil {
			out.ProjectTeam = g.TeamForProject.ShortName
		}
		matched = append(matched, out)
	}
	total := len(matched)
	return map[string]any{"groups": matched[min(offset, total):min(offset+limit, total)], "offset": offset, "total": total}, nil
}

func (t *toolset) getUserGroupMembers(ctx context.Context, in getUserGroupMembersIn) (any, error) {
	offset, limit := page(in.Offset, in.Limit, 20, 20)
	groups, err := t.listGroups(ctx)
	if err != nil {
		return nil, err
	}
	var group *apiGroup
	for i, g := range groups {
		if strings.EqualFold(g.Name, strings.TrimSpace(in.Group)) {
			group = &groups[i]
			break
		}
	}
	if group == nil {
		return nil, fmt.Errorf("group %q not found; use find_user_groups to list groups", in.Group)
	}

	// Membership lives in Hub. Project teams are separate Hub entities with the same ID.
	var hub struct {
		Users []hubUser `json:"users"`
	}
	q := url.Values{"fields": {"users(login,name,banned,profile(email(email)))"}}
	err = t.yt.HubGet(ctx, "usergroups/"+url.PathEscape(group.RingID), q, &hub)
	var ytErr *youtrack.Error
	if errors.As(err, &ytErr) && ytErr.Status == http.StatusNotFound {
		err = t.yt.HubGet(ctx, "projectteams/"+url.PathEscape(group.RingID), q, &hub)
	}
	if err != nil {
		return nil, fmt.Errorf("read members of %q from Hub: %w", group.Name, err)
	}

	members := make([]user, 0, len(hub.Users))
	for _, u := range hub.Users {
		members = append(members, user{Login: u.Login, FullName: u.Name, Email: u.Profile.Email.Email, Banned: u.Banned})
	}
	total := len(members)
	return map[string]any{
		"group":   group.Name,
		"members": members[min(offset, total):min(offset+limit, total)],
		"offset":  offset,
		"total":   total,
	}, nil
}
