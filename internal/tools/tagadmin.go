package tools

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type findTagsIn struct {
	NameContains string `json:"nameContains" jsonschema:"Case-insensitive substring of the tag name; empty string lists all visible tags"`
}

type createTagIn struct {
	Name string `json:"name" jsonschema:"Tag name"`
}

type updateTagIn struct {
	Tag  string `json:"tag" jsonschema:"Tag ID or name; among equal names your own tag wins"`
	Name string `json:"name" jsonschema:"New tag name"`
}

type deleteTagIn struct {
	Tag string `json:"tag" jsonschema:"Tag ID or name; among equal names your own tag wins"`
}

type apiSavedQuery struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Query string   `json:"query"`
	Owner *userRef `json:"owner,omitempty"`
}

type createSavedSearchIn struct {
	Name  string `json:"name" jsonschema:"Saved search name"`
	Query string `json:"query" jsonschema:"YouTrack search query"`
}

type updateSavedSearchIn struct {
	Search string  `json:"search" jsonschema:"Saved search ID or name; among equal names your own search wins"`
	Name   *string `json:"name,omitempty" jsonschema:"New name"`
	Query  *string `json:"query,omitempty" jsonschema:"New query"`
}

type deleteSavedSearchIn struct {
	Search string `json:"search" jsonschema:"Saved search ID or name; among equal names your own search wins"`
}

func (t *toolset) registerTagAdmin(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name:        "find_tags",
		Description: "Finds tags visible to the current user by name substring. Returns ID, name and owner.",
		Annotations: readOnly("Find tags"),
	}, t.findTags)
	add(s, &mcp.Tool{
		Name:        "create_tag",
		Description: "Creates a tag owned by the current user. Use manage_issue_tags to put it on issues.",
		Annotations: mutating("Create tag", false),
	}, t.createTag)
	add(s, &mcp.Tool{
		Name:        "update_tag",
		Description: "Renames a tag.",
		Annotations: mutating("Update tag", false),
	}, t.updateTag)
	add(s, &mcp.Tool{
		Name:        "delete_tag",
		Description: "Permanently deletes a tag and removes it from all issues.",
		Annotations: mutating("Delete tag", true),
	}, t.deleteTag)

	add(s, &mcp.Tool{
		Name:        "create_saved_search",
		Description: "Saves a search query under a name. It is not added to favorites; pin it in the UI to see it in get_saved_issue_searches.",
		Annotations: mutating("Create saved search", false),
	}, t.createSavedSearch)
	add(s, &mcp.Tool{
		Name:        "update_saved_search",
		Description: "Renames a saved search or changes its query.",
		Annotations: mutating("Update saved search", false),
	}, t.updateSavedSearch)
	add(s, &mcp.Tool{
		Name:        "delete_saved_search",
		Description: "Permanently deletes a saved search.",
		Annotations: mutating("Delete saved search", true),
	}, t.deleteSavedSearch)
}

func (t *toolset) findTags(ctx context.Context, in findTagsIn) (any, error) {
	tags, err := t.visibleTags(ctx)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(in.NameContains)
	out := []apiTag{}
	for _, tg := range tags {
		if strings.Contains(strings.ToLower(tg.Name), needle) {
			out = append(out, tg)
		}
	}
	return map[string]any{"tags": out, "total": len(out)}, nil
}

func (t *toolset) resolveTag(ctx context.Context, ref string) (apiTag, error) {
	tags, err := t.visibleTags(ctx)
	if err != nil {
		return apiTag{}, err
	}
	me, err := t.currentUser(ctx)
	if err != nil {
		return apiTag{}, err
	}
	return matchTag(tags, ref, me.Login)
}

func (t *toolset) createTag(ctx context.Context, in createTagIn) (any, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("name must not be empty")
	}
	var tag apiTag
	if err := t.yt.Post(ctx, "tags", url.Values{"fields": {"id,name,owner(login)"}}, map[string]any{"name": in.Name}, &tag); err != nil {
		return nil, err
	}
	return tag, nil
}

func (t *toolset) updateTag(ctx context.Context, in updateTagIn) (any, error) {
	tag, err := t.resolveTag(ctx, in.Tag)
	if err != nil {
		return nil, err
	}
	var updated apiTag
	if err := t.yt.Post(ctx, "tags/"+url.PathEscape(tag.ID), url.Values{"fields": {"id,name,owner(login)"}}, map[string]any{"name": in.Name}, &updated); err != nil {
		return nil, err
	}
	return updated, nil
}

func (t *toolset) deleteTag(ctx context.Context, in deleteTagIn) (any, error) {
	tag, err := t.resolveTag(ctx, in.Tag)
	if err != nil {
		return nil, err
	}
	if err := t.yt.Delete(ctx, "tags/"+url.PathEscape(tag.ID), nil); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": tag.Name, "id": tag.ID}, nil
}

func (t *toolset) resolveSavedSearch(ctx context.Context, ref string) (apiSavedQuery, error) {
	var all []apiSavedQuery
	if err := t.yt.Get(ctx, "savedQueries", url.Values{"fields": {"id,name,query,owner(login)"}, "$top": {"-1"}}, &all); err != nil {
		return apiSavedQuery{}, err
	}
	me, err := t.currentUser(ctx)
	if err != nil {
		return apiSavedQuery{}, err
	}
	var found *apiSavedQuery
	for i, q := range all {
		if q.ID == ref {
			return q, nil
		}
		if strings.EqualFold(q.Name, ref) {
			if q.Owner != nil && q.Owner.Login == me.Login {
				return q, nil
			}
			if found == nil {
				found = &all[i]
			}
		}
	}
	if found == nil {
		return apiSavedQuery{}, fmt.Errorf("saved search %q not found", ref)
	}
	return *found, nil
}

func (t *toolset) createSavedSearch(ctx context.Context, in createSavedSearchIn) (any, error) {
	var q apiSavedQuery
	body := map[string]any{"name": in.Name, "query": in.Query}
	if err := t.yt.Post(ctx, "savedQueries", url.Values{"fields": {"id,name,query,owner(login)"}}, body, &q); err != nil {
		return nil, err
	}
	return q, nil
}

func (t *toolset) updateSavedSearch(ctx context.Context, in updateSavedSearchIn) (any, error) {
	q, err := t.resolveSavedSearch(ctx, in.Search)
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	if in.Name != nil {
		body["name"] = *in.Name
	}
	if in.Query != nil {
		body["query"] = *in.Query
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("nothing to update")
	}
	var updated apiSavedQuery
	if err := t.yt.Post(ctx, "savedQueries/"+url.PathEscape(q.ID), url.Values{"fields": {"id,name,query,owner(login)"}}, body, &updated); err != nil {
		return nil, err
	}
	return updated, nil
}

func (t *toolset) deleteSavedSearch(ctx context.Context, in deleteSavedSearchIn) (any, error) {
	q, err := t.resolveSavedSearch(ctx, in.Search)
	if err != nil {
		return nil, err
	}
	if err := t.yt.Delete(ctx, "savedQueries/"+url.PathEscape(q.ID), nil); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": q.Name, "id": q.ID}, nil
}
