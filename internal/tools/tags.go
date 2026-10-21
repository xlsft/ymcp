package tools

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// dbID matches YouTrack database IDs such as 6-12.
var dbID = regexp.MustCompile(`^\d+-\d+$`)

type apiTag struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Owner *userRef `json:"owner,omitempty"`
}

func (tg apiTag) ownedBy(login string) bool { return tg.Owner != nil && tg.Owner.Login == login }

type manageIssueTagsIn struct {
	IssueID   string `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
	Tag       string `json:"tag" jsonschema:"Tag ID (preferred, e.g. 6-12) or tag name; a name matches the first tag with that name"`
	Operation string `json:"operation" jsonschema:"add or remove"`
}

func (t *toolset) registerTags(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name: "manage_issue_tags",
		Description: "Adds a tag to an issue or removes it. The tag is given by ID or name; when no tag matches, " +
			"the error lists similar tags. Returns the issue ID and its tags after the change.",
		Annotations: mutating("Manage issue tags", false),
		InputSchema: schemaFor[manageIssueTagsIn](func(p map[string]*jsonschema.Schema) {
			p["operation"].Enum = enum("add", "remove")
		}),
	}, t.manageIssueTags)
}

func (t *toolset) manageIssueTags(ctx context.Context, in manageIssueTagsIn) (any, error) {
	op := strings.ToLower(in.Operation)
	if op != "add" && op != "remove" {
		return nil, fmt.Errorf("operation must be add or remove, got %q", in.Operation)
	}
	issueTags, err := t.issueTags(ctx, in.IssueID)
	if err != nil {
		return nil, err
	}

	// Removal matches against the issue's own tags; adding against all visible tags.
	candidates := issueTags
	if op == "add" {
		if candidates, err = t.visibleTags(ctx); err != nil {
			return nil, err
		}
	}
	me, err := t.currentUser(ctx)
	if err != nil {
		return nil, err
	}
	tag, err := matchTag(candidates, in.Tag, me.Login)
	if err != nil {
		if op == "remove" {
			return nil, fmt.Errorf("issue %s has no tag %q; its tags: %s", in.IssueID, in.Tag, tagNames(issueTags))
		}
		return nil, err
	}

	path := "issues/" + url.PathEscape(in.IssueID) + "/tags"
	if op == "add" {
		err = t.yt.Post(ctx, path, url.Values{"fields": {"id"}}, map[string]any{"id": tag.ID}, nil)
	} else {
		err = t.yt.Delete(ctx, path+"/"+url.PathEscape(tag.ID), nil)
	}
	if err != nil {
		return nil, err
	}

	after, err := t.issueTags(ctx, in.IssueID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(after))
	for _, tg := range after {
		names = append(names, tg.Name)
	}
	return map[string]any{"issueId": in.IssueID, "tags": names}, nil
}

func (t *toolset) issueTags(ctx context.Context, issueID string) ([]apiTag, error) {
	var is struct {
		Tags []apiTag `json:"tags"`
	}
	err := t.yt.Get(ctx, "issues/"+url.PathEscape(issueID), url.Values{"fields": {"tags(id,name,owner(login))"}}, &is)
	return is.Tags, err
}

func (t *toolset) visibleTags(ctx context.Context) ([]apiTag, error) {
	var tags []apiTag
	err := t.yt.Get(ctx, "tags", url.Values{"fields": {"id,name,owner(login)"}, "$top": {"-1"}}, &tags)
	return tags, err
}

// matchTag finds a tag by ID, then by exact name, then case-insensitively.
// Several users may have tags with the same name (everyone has a Star), so
// among equal names the one owned by me wins.
func matchTag(tags []apiTag, ref, me string) (apiTag, error) {
	ref = strings.TrimSpace(ref)
	if dbID.MatchString(ref) {
		for _, tg := range tags {
			if tg.ID == ref {
				return tg, nil
			}
		}
	}
	for _, same := range []func(string) bool{
		func(name string) bool { return name == ref },
		func(name string) bool { return strings.EqualFold(name, ref) },
	} {
		var found *apiTag
		for i, tg := range tags {
			if !same(tg.Name) {
				continue
			}
			if tg.ownedBy(me) {
				return tg, nil
			}
			if found == nil {
				found = &tags[i]
			}
		}
		if found != nil {
			return *found, nil
		}
	}
	var similar []apiTag
	for _, tg := range tags {
		if strings.Contains(strings.ToLower(tg.Name), strings.ToLower(ref)) ||
			strings.Contains(strings.ToLower(ref), strings.ToLower(tg.Name)) {
			similar = append(similar, tg)
		}
	}
	if len(similar) == 0 {
		return apiTag{}, fmt.Errorf("tag %q not found; available tags: %s", ref, tagNames(tags[:min(len(tags), 20)]))
	}
	return apiTag{}, fmt.Errorf("tag %q not found; similar tags: %s", ref, tagNames(similar))
}

func tagNames(tags []apiTag) string {
	if len(tags) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(tags))
	for _, tg := range tags {
		parts = append(parts, fmt.Sprintf("%s (%s)", tg.Name, tg.ID))
	}
	return strings.Join(parts, ", ")
}
