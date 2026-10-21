package tools

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type apiLinkType struct {
	Name           string `json:"name"`
	SourceToTarget string `json:"sourceToTarget"`
	TargetToSource string `json:"targetToSource"`
	Directed       bool   `json:"directed"`
}

type apiLink struct {
	ID        string      `json:"id"`
	Direction string      `json:"direction"`
	LinkType  apiLinkType `json:"linkType"`
	Issues    []struct {
		ID         string `json:"id"`
		IDReadable string `json:"idReadable"`
	} `json:"issues"`
}

const linkFields = "id,direction,linkType(name,sourceToTarget,targetToSource,directed),issues(id,idReadable)"

// name is the link phrase as read from the issue that owns the link.
func (l apiLink) name() string {
	if l.Direction == "INWARD" && l.LinkType.TargetToSource != "" {
		return l.LinkType.TargetToSource
	}
	if l.LinkType.SourceToTarget != "" {
		return l.LinkType.SourceToTarget
	}
	return l.LinkType.Name
}

// linkMap groups linked issue IDs by the link name as seen from the issue,
// e.g. {"subtask of": ["DEMO-1"], "relates to": ["DEMO-5"]}.
func linkMap(links []apiLink) map[string][]string {
	out := map[string][]string{}
	for _, l := range links {
		if len(l.Issues) == 0 {
			continue
		}
		name := l.name()
		for _, li := range l.Issues {
			out[name] = append(out[name], li.IDReadable)
		}
	}
	return out
}

type linkIssuesIn struct {
	TargetIssueID string `json:"targetIssueId" jsonschema:"Issue that gets the link, e.g. DEMO-1"`
	LinkType      string `json:"linkType" jsonschema:"Link name as read from the target issue, e.g. relates to, subtask of, parent for, depends on, is required for, duplicates, is duplicated by"`
	IssueToLinkID string `json:"issueToLinkId" jsonschema:"Issue to link to, e.g. DEMO-2"`
}

func (t *toolset) registerLinks(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name: "link_issues",
		Description: "Links two issues. {\"targetIssueId\":\"DEMO-1\",\"linkType\":\"subtask of\",\"issueToLinkId\":\"DEMO-2\"} " +
			"makes DEMO-1 a subtask of DEMO-2. Returns the number of linked issues per link type of the target.",
		Annotations: mutating("Link issues", false),
	}, t.linkIssues)

	add(s, &mcp.Tool{
		Name: "unlink_issues",
		Description: "Removes a link between two issues; arguments mirror link_issues. " +
			"{\"targetIssueId\":\"DEMO-1\",\"linkType\":\"subtask of\",\"issueToLinkId\":\"DEMO-2\"} makes DEMO-1 no longer a subtask of DEMO-2.",
		Annotations: mutating("Unlink issues", true),
	}, t.unlinkIssues)
}

func (t *toolset) linkIssues(ctx context.Context, in linkIssuesIn) (any, error) {
	phrase, err := t.resolveLinkPhrase(ctx, in.LinkType)
	if err != nil {
		return nil, err
	}
	if err := t.command(ctx, phrase+" "+in.IssueToLinkID, in.TargetIssueID); err != nil {
		return nil, err
	}
	links, err := t.issueLinks(ctx, in.TargetIssueID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"issueId": in.TargetIssueID, "linked": phrase + " " + in.IssueToLinkID, "linkedIssueCounts": linkCounts(links)}, nil
}

func (t *toolset) unlinkIssues(ctx context.Context, in linkIssuesIn) (any, error) {
	phrase, err := t.resolveLinkPhrase(ctx, in.LinkType)
	if err != nil {
		return nil, err
	}
	links, err := t.issueLinks(ctx, in.TargetIssueID)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if l.name() != phrase {
			continue
		}
		for _, li := range l.Issues {
			if !strings.EqualFold(li.IDReadable, in.IssueToLinkID) {
				continue
			}
			path := "issues/" + url.PathEscape(in.TargetIssueID) + "/links/" + url.PathEscape(l.ID) + "/issues/" + url.PathEscape(li.ID)
			if err := t.yt.Delete(ctx, path, nil); err != nil {
				return nil, err
			}
			if links, err = t.issueLinks(ctx, in.TargetIssueID); err != nil {
				return nil, err
			}
			return map[string]any{"issueId": in.TargetIssueID, "unlinked": phrase + " " + li.IDReadable, "linkedIssueCounts": linkCounts(links)}, nil
		}
	}
	return nil, fmt.Errorf("%s has no link %q to %s; its links: %v", in.TargetIssueID, phrase, in.IssueToLinkID, linkMap(links))
}

func (t *toolset) resolveLinkPhrase(ctx context.Context, input string) (string, error) {
	var types []apiLinkType
	if err := t.yt.Get(ctx, "issueLinkTypes", url.Values{"fields": {"name,sourceToTarget,targetToSource,directed"}}, &types); err != nil {
		return "", err
	}
	return linkPhrase(types, input)
}

func (t *toolset) issueLinks(ctx context.Context, issueID string) ([]apiLink, error) {
	var links []apiLink
	err := t.yt.Get(ctx, "issues/"+url.PathEscape(issueID)+"/links", url.Values{"fields": {linkFields}}, &links)
	return links, err
}

func linkCounts(links []apiLink) map[string]int {
	counts := map[string]int{}
	for name, ids := range linkMap(links) {
		counts[name] = len(ids)
	}
	return counts
}

// linkPhrase resolves user input to the command phrase of a link direction.
// The type name of a directed link maps to its outward phrase.
func linkPhrase(types []apiLinkType, input string) (string, error) {
	in := strings.TrimSpace(input)
	for _, lt := range types {
		for _, phrase := range []string{lt.SourceToTarget, lt.TargetToSource} {
			if phrase != "" && strings.EqualFold(phrase, in) {
				return phrase, nil
			}
		}
	}
	for _, lt := range types {
		if strings.EqualFold(lt.Name, in) && lt.SourceToTarget != "" {
			return lt.SourceToTarget, nil
		}
	}
	var options []string
	for _, lt := range types {
		for _, phrase := range []string{lt.SourceToTarget, lt.TargetToSource} {
			if phrase != "" && !slices.Contains(options, phrase) {
				options = append(options, phrase)
			}
		}
	}
	sort.Strings(options)
	return "", fmt.Errorf("unknown link type %q; available: %s", input, strings.Join(options, ", "))
}
