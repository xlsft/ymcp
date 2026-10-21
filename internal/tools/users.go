package tools

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const userFields = "id,login,fullName,email,banned,profiles(general(timezone(id)))"

type apiUser struct {
	ID       string `json:"id"`
	Login    string `json:"login"`
	FullName string `json:"fullName"`
	Email    string `json:"email"`
	Banned   bool   `json:"banned"`
	Profiles struct {
		General struct {
			Timezone struct {
				ID string `json:"id"`
			} `json:"timezone"`
		} `json:"general"`
	} `json:"profiles"`
}

type user struct {
	Login    string `json:"login"`
	FullName string `json:"fullName"`
	Email    string `json:"email,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
	Banned   bool   `json:"banned,omitempty"`
}

func (u apiUser) public() user {
	return user{u.Login, u.FullName, u.Email, u.Profiles.General.Timezone.ID, u.Banned}
}

type getCurrentUserIn struct{}

type findUserIn struct {
	LoginOrEmail string `json:"loginOrEmail" jsonschema:"Login or email of the user; \"me\" returns the current user"`
}

type getSavedIssueSearchesIn struct {
	Offset int `json:"offset,omitempty" jsonschema:"Number of searches to skip. Default 0."`
	Limit  int `json:"limit,omitempty" jsonschema:"Max searches to return. Default 20, max 20."`
}

func (t *toolset) registerUsers(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name:        "get_current_user",
		Description: "Returns login, email, full name and time zone of the current user.",
		Annotations: readOnly("Get current user"),
	}, func(ctx context.Context, _ getCurrentUserIn) (any, error) {
		var u apiUser
		err := t.yt.Get(ctx, "users/me", url.Values{"fields": {userFields}}, &u)
		return u.public(), err
	})

	add(s, &mcp.Tool{
		Name:        "find_user",
		Description: "Finds a user by login or email. Returns login, full name, email and time zone.",
		Annotations: readOnly("Find user"),
	}, t.findUser)

	add(s, &mcp.Tool{
		Name:        "get_saved_issue_searches",
		Description: "Returns the current user's favorite saved issue searches. Their queries can be passed to search_issues.",
		Annotations: readOnly("Get saved issue searches"),
	}, t.getSavedIssueSearches)
}

func (t *toolset) findUser(ctx context.Context, in findUserIn) (any, error) {
	needle := strings.TrimSpace(in.LoginOrEmail)
	if needle == "" || strings.EqualFold(needle, "me") {
		var u apiUser
		err := t.yt.Get(ctx, "users/me", url.Values{"fields": {userFields}}, &u)
		return u.public(), err
	}

	var users []apiUser
	q := url.Values{"query": {needle}, "fields": {userFields}, "$top": {"20"}}
	if err := t.yt.Get(ctx, "users", q, &users); err != nil {
		return nil, err
	}
	for _, u := range users {
		if strings.EqualFold(u.Login, needle) || strings.EqualFold(u.Email, needle) {
			return u.public(), nil
		}
	}
	if len(users) == 1 {
		return users[0].public(), nil
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("no user matches %q", needle)
	}
	candidates := make([]string, 0, len(users))
	for _, u := range users {
		candidates = append(candidates, fmt.Sprintf("%s (%s)", u.Login, u.FullName))
	}
	return nil, fmt.Errorf("no exact match for %q; candidates: %s", needle, strings.Join(candidates, ", "))
}

func (t *toolset) getSavedIssueSearches(ctx context.Context, in getSavedIssueSearchesIn) (any, error) {
	offset, limit := page(in.Offset, in.Limit, 20, 20)
	// The sidebar favorites mix saved searches with tags and projects.
	var folders []struct {
		Type  string `json:"$type"`
		ID    string `json:"id"`
		Name  string `json:"name"`
		Query string `json:"query"`
	}
	q := url.Values{"fields": {"$type,id,name,query"}, "$top": {"-1"}}
	if err := t.yt.Get(ctx, "userIssueFolders", q, &folders); err != nil {
		return nil, err
	}
	type search struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Query string `json:"query"`
	}
	searches := []search{}
	for _, f := range folders {
		if f.Type == "SavedQuery" {
			searches = append(searches, search{f.ID, f.Name, f.Query})
		}
	}
	total := len(searches)
	return map[string]any{"searches": searches[min(offset, total):min(offset+limit, total)], "offset": offset, "total": total}, nil
}
