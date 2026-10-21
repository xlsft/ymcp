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
	Direction string      `json:"direction"`
	LinkType  apiLinkType `json:"linkType"`
	Issues    []struct {
		IDReadable string `json:"idReadable"`
	} `json:"issues"`
}

const linkFields = "direction,linkType(name,sourceToTarget,targetToSource,directed),issues(idReadable)"

// linkMap groups linked issue IDs by the link name as seen from the issue,
// e.g. {"subtask of": ["DEMO-1"], "relates to": ["DEMO-5"]}.
func linkMap(links []apiLink) map[string][]string {
	out := map[string][]string{}
	for _, l := range links {
		if len(l.Issues) == 0 {
			continue
		}
		name := l.LinkType.SourceToTarget
		if l.Direction == "INWARD" && l.LinkType.TargetToSource != "" {
			name = l.LinkType.TargetToSource
		}
		if name == "" {
			name = l.LinkType.Name
		}
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
}

func (t *toolset) linkIssues(ctx context.Context, in linkIssuesIn) (any, error) {
	var types []apiLinkType
	if err := t.yt.Get(ctx, "issueLinkTypes", url.Values{"fields": {"name,sourceToTarget,targetToSource,directed"}}, &types); err != nil {
		return nil, err
	}
	phrase, err := linkPhrase(types, in.LinkType)
	if err != nil {
		return nil, err
	}
	if err := t.command(ctx, phrase+" "+in.IssueToLinkID, in.TargetIssueID); err != nil {
		return nil, err
	}

	var links []apiLink
	if err := t.yt.Get(ctx, "issues/"+url.PathEscape(in.TargetIssueID)+"/links", url.Values{"fields": {linkFields}}, &links); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for name, ids := range linkMap(links) {
		counts[name] = len(ids)
	}
	return map[string]any{"issueId": in.TargetIssueID, "linked": phrase + " " + in.IssueToLinkID, "linkedIssueCounts": counts}, nil
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
