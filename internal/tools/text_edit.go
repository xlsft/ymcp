package tools

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type textEdit struct {
	OldText    string `json:"oldText" jsonschema:"Exact text to find, including whitespace and Markdown, or a pattern when regexp is true. Must match once unless replaceAll is true; include surrounding lines to make it unique."`
	NewText    string `json:"newText" jsonschema:"Replacement text; empty deletes the match. With regexp, $1 or ${name} insert capture groups and $$ is a literal $."`
	ReplaceAll bool   `json:"replaceAll,omitempty" jsonschema:"Replace every match instead of requiring exactly one. Default false."`
	Regexp     bool   `json:"regexp,omitempty" jsonschema:"Treat oldText as a Go RE2 regular expression, e.g. (?m)^- \\[ \\] (.*)$. Flags: (?i) ignore case, (?m) ^ and $ match at each line, (?s) . matches newlines. Default false."`
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
			"Read the article first and copy oldText from it, or set regexp to match a pattern. All edits apply or none do. Returns the article ID, URL and number of replacements.",
		Annotations: mutating("Edit article content", false),
	}, t.editArticleContent)

	add(s, &mcp.Tool{
		Name: "edit_issue_description",
		Description: "Changes part of an issue's description by exact find-and-replace, without resending the whole text. " +
			"Read the issue first and copy oldText from it, or set regexp to match a pattern. All edits apply or none do. Returns the issue ID, URL and number of replacements.",
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
// is invalid or missing, or matches more than once without replaceAll.
func applyEdits(text string, edits []textEdit) (string, int, error) {
	if len(edits) == 0 {
		return "", 0, fmt.Errorf("no edits given")
	}
	total := 0
	for i, e := range edits {
		if e.OldText == "" {
			return "", 0, fmt.Errorf("edit %d: oldText is empty", i+1)
		}
		var re *regexp.Regexp
		var n int
		if e.Regexp {
			var err error
			if re, err = regexp.Compile(e.OldText); err != nil {
				return "", 0, fmt.Errorf("edit %d: %w", i+1, err)
			}
			n = len(re.FindAllStringIndex(text, -1))
		} else {
			n = strings.Count(text, e.OldText)
		}
		switch {
		case n == 0 && e.Regexp:
			return "", 0, fmt.Errorf("edit %d: pattern matches nothing", i+1)
		case n == 0:
			return "", 0, fmt.Errorf("edit %d: oldText not found; read the current text and copy it exactly", i+1)
		case n > 1 && !e.ReplaceAll:
			return "", 0, fmt.Errorf("edit %d: oldText matches %d times; make it more specific or set replaceAll", i+1, n)
		}
		if e.Regexp {
			text = re.ReplaceAllString(text, e.NewText)
		} else {
			text = strings.ReplaceAll(text, e.OldText, e.NewText)
		}
		total += n
	}
	return text, total, nil
}
