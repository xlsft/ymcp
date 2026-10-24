package tools

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type textEdit struct {
	OldText    string `json:"oldText" jsonschema:"Exact text to find, including whitespace and Markdown. Must occur once unless replaceAll is true; include surrounding lines to make it unique."`
	NewText    string `json:"newText" jsonschema:"Replacement text; empty deletes oldText"`
	ReplaceAll bool   `json:"replaceAll,omitempty" jsonschema:"Replace every occurrence instead of requiring exactly one. Default false."`
}

type editArticleContentIn struct {
	ArticleID string     `json:"articleId" jsonschema:"Article ID, e.g. HELP-A-1"`
	Edits     []textEdit `json:"edits" jsonschema:"Replacements applied in order, each to the result of the previous one"`
}

type editIssueDescriptionIn struct {
	IssueID string     `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123, or draft ID, e.g. 3-22"`
	Edits   []textEdit `json:"edits" jsonschema:"Replacements applied in order, each to the result of the previous one"`
}

func (t *toolset) registerTextEdit(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name: "edit_article_content",
		Description: "Changes part of an article's content by exact find-and-replace, without resending the whole text. " +
			"Read the article first and copy oldText from it. All edits apply or none do. Returns the article ID, URL and number of replacements.",
		Annotations: mutating("Edit article content", false),
	}, t.editArticleContent)

	add(s, &mcp.Tool{
		Name: "edit_issue_description",
		Description: "Changes part of an issue's description by exact find-and-replace, without resending the whole text. " +
			"Read the issue first and copy oldText from it. All edits apply or none do. Returns the issue ID, URL and number of replacements.",
		Annotations: mutating("Edit issue description", false),
	}, t.editIssueDescription)
}

func (t *toolset) editArticleContent(ctx context.Context, in editArticleContentIn) (any, error) {
	path := "articles/" + url.PathEscape(in.ArticleID)
	var cur struct {
		Content string `json:"content"`
	}
	if err := t.yt.Get(ctx, path, url.Values{"fields": {"content"}}, &cur); err != nil {
		return nil, err
	}
	content, n, err := applyEdits(cur.Content, in.Edits)
	if err != nil {
		return nil, err
	}
	var updated apiArticleRef
	if err := t.yt.Post(ctx, path, url.Values{"fields": {"idReadable"}}, map[string]any{"content": content}, &updated); err != nil {
		return nil, err
	}
	return map[string]any{"id": updated.IDReadable, "url": t.articleURL(updated.IDReadable), "replacements": n}, nil
}

func (t *toolset) editIssueDescription(ctx context.Context, in editIssueDescriptionIn) (any, error) {
	path := issuePath(in.IssueID)
	var cur struct {
		Description string `json:"description"`
	}
	if err := t.yt.Get(ctx, path, url.Values{"fields": {"description"}}, &cur); err != nil {
		return nil, err
	}
	description, n, err := applyEdits(cur.Description, in.Edits)
	if err != nil {
		return nil, err
	}
	if err := t.yt.Post(ctx, path, url.Values{"fields": {"id"}}, map[string]any{"description": description}, nil); err != nil {
		return nil, err
	}
	return map[string]any{"id": in.IssueID, "url": t.issueURL(in.IssueID), "replacements": n}, nil
}

// applyEdits runs the replacements in order and returns the new text and the
// number of replacements made. It fails without partial results if an oldText
// is missing, or occurs more than once without replaceAll.
func applyEdits(text string, edits []textEdit) (string, int, error) {
	if len(edits) == 0 {
		return "", 0, fmt.Errorf("no edits given")
	}
	total := 0
	for i, e := range edits {
		if e.OldText == "" {
			return "", 0, fmt.Errorf("edit %d: oldText is empty", i+1)
		}
		n := strings.Count(text, e.OldText)
		switch {
		case n == 0:
			return "", 0, fmt.Errorf("edit %d: oldText not found; read the current text and copy it exactly", i+1)
		case n > 1 && !e.ReplaceAll:
			return "", 0, fmt.Errorf("edit %d: oldText occurs %d times; add surrounding text to make it unique or set replaceAll", i+1, n)
		}
		text = strings.ReplaceAll(text, e.OldText, e.NewText)
		total += n
	}
	return text, total, nil
}
