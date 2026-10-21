package tools

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type articleRef struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

type apiArticleRef struct {
	IDReadable string `json:"idReadable"`
	Summary    string `json:"summary"`
}

func (r *apiArticleRef) public() *articleRef {
	if r == nil {
		return nil
	}
	return &articleRef{r.IDReadable, r.Summary}
}

type apiArticle struct {
	ID            string          `json:"id"`
	IDReadable    string          `json:"idReadable"`
	Summary       string          `json:"summary"`
	Content       string          `json:"content"`
	Created       millis          `json:"created"`
	Updated       millis          `json:"updated"`
	Reporter      *userRef        `json:"reporter"`
	Project       apiProjectRef   `json:"project"`
	ParentArticle *apiArticleRef  `json:"parentArticle"`
	ChildArticles []apiArticleRef `json:"childArticles"`
	Tags          []apiTag        `json:"tags"`
	Visibility    *apiVisibility  `json:"visibility"`
	Attachments   []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		MimeType string `json:"mimeType,omitempty"`
	} `json:"attachments"`
}

const articleFields = "idReadable,summary,content,created,updated,reporter(login,fullName),project(shortName,name)," +
	"parentArticle(idReadable,summary),childArticles(idReadable,summary),tags(id,name)," +
	"attachments(id,name,size,mimeType)," + visibilityFields

type createArticleIn struct {
	Project         string   `json:"project" jsonschema:"Project key, e.g. HELP"`
	Summary         string   `json:"summary" jsonschema:"Article title"`
	Content         string   `json:"content,omitempty" jsonschema:"Article content in Markdown"`
	ParentArticle   string   `json:"parentArticle,omitempty" jsonschema:"ID of the parent article, e.g. HELP-A-1"`
	PermittedUsers  []string `json:"permittedUsers,omitempty" jsonschema:"Limit visibility to these user logins"`
	PermittedGroups []string `json:"permittedGroups,omitempty" jsonschema:"Limit visibility to these group names"`
}

type updateArticleIn struct {
	ArticleID       string  `json:"articleId" jsonschema:"Article ID, e.g. HELP-A-1"`
	Summary         *string `json:"summary,omitempty" jsonschema:"New title"`
	Content         *string `json:"content,omitempty" jsonschema:"New content in Markdown, or text to append when append is true"`
	Append          bool    `json:"append,omitempty" jsonschema:"Append content to the end instead of replacing it. Default false."`
	ParentArticleID *string `json:"parentArticleId,omitempty" jsonschema:"Move the article under this parent; empty string makes it top-level"`
}

type getArticleIn struct {
	ArticleID   string `json:"articleId" jsonschema:"Article ID, e.g. HELP-A-1"`
	LinesOffset int    `json:"linesOffset,omitempty" jsonschema:"First content line to return, 0-based. Default 0."`
	LinesCount  int    `json:"linesCount,omitempty" jsonschema:"Number of content lines to return. Default 500."`
}

type searchArticlesIn struct {
	Query  string `json:"query" jsonschema:"Knowledge base query, e.g. project: HELP, author: me, tag: faq, created: {This month}, \"exact phrase\", free text"`
	Offset int    `json:"offset,omitempty" jsonschema:"Number of articles to skip. Default 0."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Max articles to return. Default 10, max 20."`
}

func (t *toolset) registerArticles(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name:        "create_article",
		Description: "Creates a knowledge base article, optionally under a parent article and with limited visibility. Returns the article ID and URL.",
		Annotations: mutating("Create article", false),
	}, t.createArticle)

	add(s, &mcp.Tool{
		Name:        "update_article",
		Description: "Changes the title, content or parent of an article. With append, content is added to the end. Returns the article ID and URL.",
		Annotations: mutating("Update article", false),
	}, t.updateArticle)

	add(s, &mcp.Tool{
		Name: "get_article",
		Description: "Returns an article: title, content, parent and sub-articles, tags, attachments and visibility. " +
			"Long content can be read in segments with linesOffset and linesCount.",
		Annotations: readOnly("Get article"),
	}, t.getArticle)

	add(s, &mcp.Tool{
		Name:        "search_articles",
		Description: "Searches the knowledge base. Returns ID, title, URL, parent article and lines matching the free-text terms. Paginated.",
		Annotations: readOnly("Search articles"),
	}, t.searchArticles)
}

func (t *toolset) articleURL(id string) string {
	return t.yt.BaseURL() + "/articles/" + id
}

// articleDBID resolves a readable article ID to the database ID used in references.
func (t *toolset) articleDBID(ctx context.Context, id string) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := t.yt.Get(ctx, "articles/"+url.PathEscape(id), url.Values{"fields": {"id"}}, &a); err != nil {
		return "", fmt.Errorf("article %s: %w", id, err)
	}
	return a.ID, nil
}

func (t *toolset) createArticle(ctx context.Context, in createArticleIn) (any, error) {
	p, err := t.resolveProject(ctx, in.Project)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"project": map[string]any{"id": p.ID}, "summary": in.Summary}
	if in.Content != "" {
		body["content"] = in.Content
	}
	if in.ParentArticle != "" {
		parent, err := t.articleDBID(ctx, in.ParentArticle)
		if err != nil {
			return nil, err
		}
		body["parentArticle"] = map[string]any{"id": parent}
	}
	vis, err := t.limitedVisibility(ctx, in.PermittedUsers, in.PermittedGroups)
	if err != nil {
		return nil, err
	}
	if vis != nil {
		body["visibility"] = vis
	}

	var created apiArticleRef
	if err := t.yt.Post(ctx, "articles", url.Values{"fields": {"idReadable"}}, body, &created); err != nil {
		return nil, err
	}
	return map[string]any{"id": created.IDReadable, "url": t.articleURL(created.IDReadable)}, nil
}

func (t *toolset) updateArticle(ctx context.Context, in updateArticleIn) (any, error) {
	body := map[string]any{}
	var changed []string
	if in.Summary != nil {
		body["summary"] = *in.Summary
		changed = append(changed, "summary")
	}
	if in.Content != nil {
		content := *in.Content
		if in.Append {
			var cur struct {
				Content string `json:"content"`
			}
			if err := t.yt.Get(ctx, "articles/"+url.PathEscape(in.ArticleID), url.Values{"fields": {"content"}}, &cur); err != nil {
				return nil, err
			}
			if existing := strings.TrimRight(cur.Content, "\n"); existing != "" {
				content = existing + "\n\n" + content
			}
		}
		body["content"] = content
		changed = append(changed, "content")
	}
	if in.ParentArticleID != nil {
		if *in.ParentArticleID == "" {
			body["parentArticle"] = nil
		} else {
			parent, err := t.articleDBID(ctx, *in.ParentArticleID)
			if err != nil {
				return nil, err
			}
			body["parentArticle"] = map[string]any{"id": parent}
		}
		changed = append(changed, "parentArticle")
	}
	if len(changed) == 0 {
		return nil, fmt.Errorf("nothing to update")
	}
	var updated apiArticleRef
	if err := t.yt.Post(ctx, "articles/"+url.PathEscape(in.ArticleID), url.Values{"fields": {"idReadable"}}, body, &updated); err != nil {
		return nil, err
	}
	return map[string]any{"id": updated.IDReadable, "url": t.articleURL(updated.IDReadable), "changed": changed}, nil
}

func (t *toolset) getArticle(ctx context.Context, in getArticleIn) (any, error) {
	var a apiArticle
	if err := t.yt.Get(ctx, "articles/"+url.PathEscape(in.ArticleID), url.Values{"fields": {articleFields}}, &a); err != nil {
		return nil, err
	}
	lines := strings.Split(a.Content, "\n")
	if a.Content == "" {
		lines = nil
	}
	count := in.LinesCount
	if count <= 0 {
		count = 500
	}
	from := min(max(in.LinesOffset, 0), len(lines))
	to := min(from+count, len(lines))

	children := make([]*articleRef, 0, len(a.ChildArticles))
	for i := range a.ChildArticles {
		children = append(children, a.ChildArticles[i].public())
	}
	tags := make([]string, 0, len(a.Tags))
	for _, tg := range a.Tags {
		tags = append(tags, tg.Name)
	}
	var reporter string
	if a.Reporter != nil {
		reporter = a.Reporter.Login
	}
	return map[string]any{
		"id":            a.IDReadable,
		"summary":       a.Summary,
		"url":           t.articleURL(a.IDReadable),
		"project":       a.Project.ShortName,
		"reporter":      reporter,
		"created":       a.Created,
		"updated":       a.Updated,
		"parentArticle": a.ParentArticle.public(),
		"childArticles": children,
		"tags":          tags,
		"visibility":    a.Visibility.public(),
		"attachments":   a.Attachments,
		"content":       strings.Join(lines[from:to], "\n"),
		"linesOffset":   from,
		"totalLines":    len(lines),
		"hasMoreLines":  to < len(lines),
	}, nil
}

func (t *toolset) searchArticles(ctx context.Context, in searchArticlesIn) (any, error) {
	offset, limit := page(in.Offset, in.Limit, 10, 20)
	q := url.Values{
		"query":  {in.Query},
		"fields": {"idReadable,summary,content,updated,project(shortName),parentArticle(idReadable,summary)"},
		"$skip":  {strconv.Itoa(offset)},
		"$top":   {strconv.Itoa(limit + 1)},
	}
	var articles []apiArticle
	if err := t.yt.Get(ctx, "articles", q, &articles); err != nil {
		return nil, err
	}
	hasMore := len(articles) > limit
	if hasMore {
		articles = articles[:limit]
	}

	type result struct {
		ID            string      `json:"id"`
		Summary       string      `json:"summary"`
		URL           string      `json:"url"`
		Project       string      `json:"project"`
		Updated       millis      `json:"updated"`
		ParentArticle *articleRef `json:"parentArticle,omitempty"`
		MatchingLines []string    `json:"matchingLines,omitempty"`
	}
	terms := freeTextTerms(in.Query)
	out := make([]result, 0, len(articles))
	for _, a := range articles {
		out = append(out, result{
			ID:            a.IDReadable,
			Summary:       a.Summary,
			URL:           t.articleURL(a.IDReadable),
			Project:       a.Project.ShortName,
			Updated:       a.Updated,
			ParentArticle: a.ParentArticle.public(),
			MatchingLines: matchingLines(a.Content, terms, 3),
		})
	}
	return map[string]any{"articles": out, "offset": offset, "hasMore": hasMore}, nil
}

// freeTextTerms extracts the plain search words and quoted phrases of a
// query, skipping attribute filters such as "project: HELP" and #shortcuts.
func freeTextTerms(query string) []string {
	var terms []string
	rest := query
	for {
		start := strings.IndexByte(rest, '"')
		if start < 0 {
			break
		}
		end := strings.IndexByte(rest[start+1:], '"')
		if end < 0 {
			break
		}
		if phrase := strings.TrimSpace(rest[start+1 : start+1+end]); phrase != "" {
			terms = append(terms, strings.ToLower(phrase))
		}
		rest = rest[:start] + " " + rest[start+end+2:]
	}

	// Drop {braced values} and attribute filters with their values.
	words := strings.Fields(rest)
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case strings.HasPrefix(w, "{"):
			for i < len(words) && !strings.HasSuffix(words[i], "}") {
				i++
			}
		case strings.HasSuffix(w, ":"):
			i++ // the value follows
			if i < len(words) && strings.HasPrefix(words[i], "{") {
				for i < len(words) && !strings.HasSuffix(words[i], "}") {
					i++
				}
			}
		case strings.Contains(w, ":"), strings.HasPrefix(w, "#"), strings.HasPrefix(w, "-"),
			strings.EqualFold(w, "and"), strings.EqualFold(w, "or"):
		default:
			if w = strings.ToLower(strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })); w != "" {
				terms = append(terms, w)
			}
		}
	}
	return terms
}

func matchingLines(content string, terms []string, limit int) []string {
	if len(terms) == 0 {
		return nil
	}
	var out []string
	for _, line := range strings.Split(content, "\n") {
		lower := strings.ToLower(line)
		for _, term := range terms {
			if strings.Contains(lower, term) {
				out = append(out, strings.TrimSpace(line))
				break
			}
		}
		if len(out) == limit {
			break
		}
	}
	return out
}
