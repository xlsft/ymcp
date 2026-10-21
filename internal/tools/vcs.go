package tools

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type getIssueVcsChangesIn struct {
	IssueID string `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Max changes to return. Default 20, max 50."`
}

type apiVcsChange struct {
	Type     string   `json:"$type"`
	ID       string   `json:"id"`
	Date     millis   `json:"date"`
	Version  string   `json:"version"`
	Text     string   `json:"text"`
	Title    string   `json:"title"`
	URL      string   `json:"url"`
	URLs     []string `json:"urls"`
	Files    int      `json:"files"`
	Author   *userRef `json:"author"`
	UserName string   `json:"userName"`
	State    *struct {
		ID string `json:"id"`
	} `json:"state"`
}

// vcsFields covers commits (VcsChange) and pull requests; YouTrack ignores
// the fields a type doesn't have.
const vcsFields = "$type,id,date,version,text,title,url,urls,files,author(login),userName,state(id)"

func (t *toolset) registerVcs(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name: "get_issue_vcs_changes",
		Description: "Returns commits and pull requests linked to an issue through a VCS integration " +
			"(GitHub, GitLab, Bitbucket, Gitea, TeamCity): version, message, author, date and links. " +
			"Empty when the project has no VCS integration.",
		Annotations: readOnly("Get issue VCS changes"),
	}, t.getIssueVcsChanges)
}

func (t *toolset) getIssueVcsChanges(ctx context.Context, in getIssueVcsChangesIn) (any, error) {
	_, limit := page(0, in.Limit, 20, 50)
	var changes []apiVcsChange
	q := url.Values{"fields": {vcsFields}, "$top": {strconv.Itoa(limit)}}
	if err := t.yt.Get(ctx, "issues/"+url.PathEscape(in.IssueID)+"/vcsChanges", q, &changes); err != nil {
		return nil, err
	}
	type change struct {
		Kind    string   `json:"kind"`
		Version string   `json:"version,omitempty"`
		Title   string   `json:"title,omitempty"`
		Message string   `json:"message,omitempty"`
		State   string   `json:"state,omitempty"`
		Author  string   `json:"author,omitempty"`
		Date    millis   `json:"date"`
		Files   int      `json:"files,omitempty"`
		URLs    []string `json:"urls,omitempty"`
	}
	out := make([]change, 0, len(changes))
	for _, c := range changes {
		ch := change{Kind: "commit", Version: c.Version, Title: c.Title, Message: shorten(c.Text, 500), Date: c.Date, Files: c.Files, URLs: c.URLs}
		if strings.Contains(c.Type, "PullRequest") {
			ch.Kind = "pullRequest"
		}
		if c.URL != "" {
			ch.URLs = append([]string{c.URL}, ch.URLs...)
		}
		if c.State != nil {
			ch.State = c.State.ID
		}
		switch {
		case c.Author != nil:
			ch.Author = c.Author.Login
		case c.UserName != "":
			ch.Author = c.UserName
		}
		out = append(out, ch)
	}
	return map[string]any{"issueId": in.IssueID, "changes": out}, nil
}
