package tools

import (
	"context"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Delete tools are not part of the built-in YouTrack MCP server.

type deleteIssueIn struct {
	IssueID string `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123, or draft ID, e.g. 3-22"`
}

type deleteIssueCommentIn struct {
	IssueID   string `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
	CommentID string `json:"commentId" jsonschema:"Comment ID as returned by get_issue_comments, e.g. 4-17"`
	Permanent bool   `json:"permanent,omitempty" jsonschema:"Erase the comment for good instead of marking it deleted (needs the Delete Comment permission). Default false."`
}

type deleteArticleIn struct {
	ArticleID string `json:"articleId" jsonschema:"Article ID, e.g. HELP-A-1"`
}

func (t *toolset) registerDeletes(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name: "delete_issue",
		Description: "Permanently deletes an issue or a draft, with its comments, attachments and work items. " +
			"This cannot be undone; confirm with the user first.",
		Annotations: mutating("Delete issue", true),
	}, t.deleteIssue)

	add(s, &mcp.Tool{
		Name: "delete_issue_comment",
		Description: "Deletes a comment from an issue. By default it is marked deleted, as in the UI, and can be restored there; " +
			"permanent erases it. Comment IDs come from get_issue_comments.",
		Annotations: mutating("Delete issue comment", true),
	}, t.deleteIssueComment)

	add(s, &mcp.Tool{
		Name: "delete_article",
		Description: "Permanently deletes a knowledge base article. If it has sub-articles, " +
			"delete or move them first (get_article lists them).",
		Annotations: mutating("Delete article", true),
	}, t.deleteArticle)
}

func (t *toolset) deleteIssue(ctx context.Context, in deleteIssueIn) (any, error) {
	if err := t.yt.Delete(ctx, issuePath(in.IssueID), nil); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": in.IssueID}, nil
}

func (t *toolset) deleteIssueComment(ctx context.Context, in deleteIssueCommentIn) (any, error) {
	path := "issues/" + url.PathEscape(in.IssueID) + "/comments/" + url.PathEscape(in.CommentID)
	var err error
	if in.Permanent {
		err = t.yt.Delete(ctx, path, nil)
	} else {
		err = t.yt.Post(ctx, path, url.Values{"fields": {"id"}}, map[string]any{"deleted": true}, nil)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"issueId": in.IssueID, "deletedComment": in.CommentID, "permanent": in.Permanent}, nil
}

func (t *toolset) deleteArticle(ctx context.Context, in deleteArticleIn) (any, error) {
	if err := t.yt.Delete(ctx, "articles/"+url.PathEscape(in.ArticleID), nil); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": in.ArticleID}, nil
}
