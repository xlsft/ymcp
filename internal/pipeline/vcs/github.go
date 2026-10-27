package vcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// GitHub caps comment bodies at 65536 characters; bytes are a safe bound.
const githubMaxBody = 65000

// Comment ID prefixes, the same as GitHub's URL fragments.
const (
	ghIssueComment  = "issuecomment-"
	ghReviewComment = "discussion_r"
	ghReview        = "pullrequestreview-"
)

// github is the github-pr and github-issue-pr strategy.
type github struct {
	c    *client
	repo Repo
	kind Kind

	mu     sync.Mutex
	me     string
	def    string      // default branch
	issues map[int]int // PR number → mirror issue number
}

type ghUser struct {
	Login string `json:"login"`
}

type ghPull struct {
	Number   int     `json:"number"`
	HTMLURL  string  `json:"html_url"`
	State    string  `json:"state"`
	Merged   bool    `json:"merged"`
	MergedAt *stamp  `json:"merged_at"`
	Draft    bool    `json:"draft"`
	Body     *string `json:"body"`
	Base     struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type ghIssue struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
}

type ghComment struct {
	ID        int64           `json:"id"`
	User      ghUser          `json:"user"`
	Body      string          `json:"body"`
	HTMLURL   string          `json:"html_url"`
	CreatedAt stamp           `json:"created_at"`
	UpdatedAt stamp           `json:"updated_at"`
	Minimized json.RawMessage `json:"minimized"`
	// review comments
	InReplyTo    *int64 `json:"in_reply_to_id"`
	Path         string `json:"path"`
	Line         *int   `json:"line"`
	OriginalLine *int   `json:"original_line"`
	SubjectType  string `json:"subject_type"`
	DiffHunk     string `json:"diff_hunk"`
}

type ghReviewJSON struct {
	ID          int64  `json:"id"`
	User        ghUser `json:"user"`
	Body        string `json:"body"`
	State       string `json:"state"`
	HTMLURL     string `json:"html_url"`
	SubmittedAt *stamp `json:"submitted_at"`
}

func (g *github) Kind() Kind { return g.kind }

func (g *github) path(rest string) string {
	return "repos/" + url.PathEscape(g.repo.Owner) + "/" + url.PathEscape(g.repo.Name) + rest
}

func (g *github) get(ctx context.Context, path string, q url.Values, out any) error {
	resp, err := g.c.send(ctx, http.MethodGet, g.c.url(path, q), nil, "")
	if err != nil {
		return err
	}
	return decode(resp, out)
}

func (g *github) write(ctx context.Context, method, path string, in, out any) error {
	resp, err := g.c.send(ctx, method, g.c.url(path, nil), in, "")
	if err != nil {
		return err
	}
	return decode(resp, out)
}

func decode(resp response, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("vcs: reading the answer: %w", err)
	}
	return nil
}

func (g *github) Me(ctx context.Context) (string, error) {
	g.mu.Lock()
	me := g.me
	g.mu.Unlock()
	if me != "" {
		return me, nil
	}
	var u ghUser
	if err := g.get(ctx, "user", nil, &u); err != nil {
		return "", err
	}
	g.mu.Lock()
	g.me = u.Login
	g.mu.Unlock()
	return u.Login, nil
}

// defaultBranch reads the repository's default branch once.
func (g *github) defaultBranch(ctx context.Context) (string, error) {
	g.mu.Lock()
	def := g.def
	g.mu.Unlock()
	if def != "" {
		return def, nil
	}
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.get(ctx, g.path(""), nil, &r); err != nil {
		return "", err
	}
	if r.DefaultBranch == "" {
		return "", fmt.Errorf("vcs: %s has no default branch", g.repo.Path)
	}
	g.mu.Lock()
	g.def = r.DefaultBranch
	g.mu.Unlock()
	return r.DefaultBranch, nil
}

func (g *github) Open(ctx context.Context, o OpenOptions) (Change, error) {
	if o.Branch == "" {
		return Change{}, errors.New("vcs: no branch to open a pull request for")
	}
	base := o.Base
	if base == "" {
		var err error
		if base, err = g.defaultBranch(ctx); err != nil {
			return Change{}, err
		}
	}
	// An open PR for the branch is the one an earlier attempt made.
	var open []ghPull
	q := url.Values{"head": {g.repo.Owner + ":" + o.Branch}, "state": {"open"}, "per_page": {"1"}}
	if err := g.get(ctx, g.path("/pulls"), q, &open); err != nil {
		return Change{}, err
	}
	if len(open) > 0 {
		return g.change(open[0]), nil
	}

	body := o.Body
	issue := 0
	if g.kind == KindGitHubIssuePR {
		title := o.IssueTitle
		if title == "" {
			title = o.Title
		}
		var is ghIssue
		in := map[string]string{"title": title, "body": clip(o.IssueBody, githubMaxBody)}
		if err := g.write(ctx, http.MethodPost, g.path("/issues"), in, &is); err != nil {
			return Change{}, fmt.Errorf("vcs: creating the issue: %w", err)
		}
		issue = is.Number
		// The keyword closes the issue on a merge into the default branch.
		body = strings.TrimRight(body, "\r\n \t") + "\n\nCloses #" + strconv.Itoa(issue)
	}
	in := map[string]any{"title": o.Title, "head": o.Branch, "base": base, "body": clip(body, githubMaxBody), "draft": o.Draft}
	var pr ghPull
	err := g.write(ctx, http.MethodPost, g.path("/pulls"), in, &pr)
	var he *HTTPError
	if err != nil && o.Draft && errors.As(err, &he) && he.Status == http.StatusUnprocessableEntity &&
		strings.Contains(strings.ToLower(he.Message), "draft") {
		// Draft PRs aren't available for private repositories on some plans.
		in["draft"] = false
		err = g.write(ctx, http.MethodPost, g.path("/pulls"), in, &pr)
	}
	if err != nil {
		if issue > 0 {
			return Change{Issue: issue, IssueURL: g.issueURL(issue)}, fmt.Errorf("vcs: creating the pull request (issue #%d was created): %w", issue, err)
		}
		return Change{}, fmt.Errorf("vcs: creating the pull request: %w", err)
	}
	if issue > 0 {
		g.remember(pr.Number, issue)
	}
	return g.change(pr), nil
}

func (g *github) issueURL(n int) string {
	return g.c.web + "/" + g.repo.Owner + "/" + g.repo.Name + "/issues/" + strconv.Itoa(n)
}

// closesRe finds a closing keyword for an issue of the same repository.
var closesRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?\s+#(\d+)\b`)

// closedIssue is the issue the body closes; the last keyword wins, as Open
// appends its own at the end.
func closedIssue(body string) int {
	m := closesRe.FindAllStringSubmatch(body, -1)
	if len(m) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(m[len(m)-1][1])
	return n
}

func (g *github) remember(pr, issue int) {
	g.mu.Lock()
	g.issues[pr] = issue
	g.mu.Unlock()
}

// change converts a pull request, learning its mirror issue on the way.
func (g *github) change(pr ghPull) Change {
	ch := Change{Number: pr.Number, URL: pr.HTMLURL, State: StateOpen, Draft: pr.Draft}
	switch {
	case pr.Merged || (pr.MergedAt != nil && !pr.MergedAt.IsZero()):
		ch.State, ch.Merged = StateMerged, true
	case pr.State == "closed":
		ch.State = StateClosed
	}
	if g.kind == KindGitHubIssuePR && pr.Body != nil {
		if n := closedIssue(*pr.Body); n > 0 {
			g.remember(pr.Number, n)
			ch.Issue, ch.IssueURL = n, g.issueURL(n)
		}
	}
	return ch
}

func (g *github) pull(ctx context.Context, number int) (ghPull, error) {
	var pr ghPull
	err := g.get(ctx, g.path("/pulls/"+strconv.Itoa(number)), nil, &pr)
	return pr, err
}

// Change reads the PR. For github-issue-pr, a PR merged into a branch other
// than the default one doesn't close its issue by itself, so Change closes it
// (best effort: the next call tries again).
func (g *github) Change(ctx context.Context, number int) (Change, error) {
	pr, err := g.pull(ctx, number)
	if err != nil {
		return Change{}, err
	}
	ch := g.change(pr)
	if ch.Merged && ch.Issue > 0 {
		if def, err := g.defaultBranch(ctx); err == nil && pr.Base.Ref != def {
			var is ghIssue
			if err := g.get(ctx, g.path("/issues/"+strconv.Itoa(ch.Issue)), nil, &is); err == nil && is.State == "open" {
				_ = g.write(ctx, http.MethodPatch, g.path("/issues/"+strconv.Itoa(ch.Issue)),
					map[string]string{"state": "closed", "state_reason": "completed"}, nil)
			}
		}
	}
	return ch, nil
}

// issueFor finds the mirror issue of a PR: from the cursor, the cache, or
// the PR body.
func (g *github) issueFor(ctx context.Context, number int, cur Cursor) (int, error) {
	if n, err := strconv.Atoi(cur["issue"]); err == nil && n > 0 {
		return n, nil
	}
	g.mu.Lock()
	n := g.issues[number]
	g.mu.Unlock()
	if n > 0 {
		return n, nil
	}
	pr, err := g.pull(ctx, number)
	if err != nil {
		return 0, err
	}
	if ch := g.change(pr); ch.Issue > 0 {
		return ch.Issue, nil
	}
	return 0, nil
}

func (g *github) Comments(ctx context.Context, number int, since Cursor) ([]Comment, Cursor, error) {
	next := Cursor{}
	for k, v := range since {
		if strings.HasPrefix(k, "since:") || k == "issue" {
			next[k] = v
		}
	}
	var out []Comment
	n := strconv.Itoa(number)

	conversation := func(stream, path, kind string) error {
		q := url.Values{"per_page": {strconv.Itoa(perPage)}}
		if s := sinceParam(since["since:"+stream]); s != "" {
			q.Set("since", s)
		}
		return g.c.pages(ctx, stream, g.c.url(path, q), since, next, func(b []byte) (int, error) {
			var list []ghComment
			if err := json.Unmarshal(b, &list); err != nil {
				return 0, err
			}
			for _, c := range list {
				advance(next, "since:"+stream, c.UpdatedAt.Time)
				if minimized(c.Minimized) {
					continue
				}
				out = append(out, fromIssueComment(c, kind))
			}
			return len(list), nil
		})
	}
	if err := conversation("conversation", g.path("/issues/"+n+"/comments"), CommentConversation); err != nil {
		return nil, since, err
	}

	err := g.c.pages(ctx, "reviews", g.c.url(g.path("/pulls/"+n+"/reviews"), url.Values{"per_page": {strconv.Itoa(perPage)}}), since, next,
		func(b []byte) (int, error) {
			var list []ghReviewJSON
			if err := json.Unmarshal(b, &list); err != nil {
				return 0, err
			}
			for _, r := range list {
				// Reviews made of line comments come with an empty body: the
				// comments arrive on their own. Pending ones aren't sent yet.
				if r.State == "PENDING" || strings.TrimSpace(r.Body) == "" || r.SubmittedAt == nil {
					continue
				}
				out = append(out, Comment{
					ID: ghReview + strconv.FormatInt(r.ID, 10), Author: r.User.Login, Body: r.Body, URL: r.HTMLURL,
					Created: r.SubmittedAt.Time, Updated: r.SubmittedAt.Time, Kind: CommentReview,
				})
			}
			return len(list), nil
		})
	if err != nil {
		return nil, since, err
	}

	q := url.Values{"per_page": {strconv.Itoa(perPage)}, "sort": {"updated"}, "direction": {"asc"}}
	if s := sinceParam(since["since:review_comments"]); s != "" {
		q.Set("since", s)
	}
	err = g.c.pages(ctx, "review_comments", g.c.url(g.path("/pulls/"+n+"/comments"), q), since, next, func(b []byte) (int, error) {
		var list []ghComment
		if err := json.Unmarshal(b, &list); err != nil {
			return 0, err
		}
		for _, c := range list {
			advance(next, "since:review_comments", c.UpdatedAt.Time)
			out = append(out, fromReviewComment(c))
		}
		return len(list), nil
	})
	if err != nil {
		return nil, since, err
	}

	if g.kind == KindGitHubIssuePR {
		issue, err := g.issueFor(ctx, number, since)
		if err != nil {
			return nil, since, err
		}
		if issue > 0 {
			next["issue"] = strconv.Itoa(issue)
			if err := conversation("issue", g.path("/issues/"+strconv.Itoa(issue)+"/comments"), CommentIssue); err != nil {
				return nil, since, err
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, next, nil
}

// minimized reports a comment hidden as spam, off-topic, outdated and so on.
func minimized(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	switch s {
	case "", "null", "false", "{}":
		return false
	case "true":
		return true
	}
	var v struct {
		IsMinimized  *bool `json:"is_minimized"`
		IsMinimized2 *bool `json:"isMinimized"`
	}
	if json.Unmarshal(raw, &v) == nil {
		if v.IsMinimized != nil {
			return *v.IsMinimized
		}
		if v.IsMinimized2 != nil {
			return *v.IsMinimized2
		}
	}
	return true
}

func fromIssueComment(c ghComment, kind string) Comment {
	return Comment{
		ID: ghIssueComment + strconv.FormatInt(c.ID, 10), Author: c.User.Login, Body: c.Body, URL: c.HTMLURL,
		Created: c.CreatedAt.Time, Updated: c.UpdatedAt.Time, Kind: kind,
	}
}

func fromReviewComment(c ghComment) Comment {
	root := c.ID
	if c.InReplyTo != nil && *c.InReplyTo != 0 {
		root = *c.InReplyTo
	}
	out := Comment{
		ID:       ghReviewComment + strconv.FormatInt(c.ID, 10),
		ThreadID: ghReviewComment + strconv.FormatInt(root, 10),
		Author:   c.User.Login, Body: c.Body, URL: c.HTMLURL, File: c.Path, Hunk: c.DiffHunk,
		Created: c.CreatedAt.Time, Updated: c.UpdatedAt.Time, Kind: CommentReviewComment,
	}
	switch {
	case c.Line != nil:
		out.Line = *c.Line
	case c.SubjectType != "file":
		// The line left the diff: GitHub keeps the comment on its old line.
		out.Outdated = true
		if c.OriginalLine != nil {
			out.Line = *c.OriginalLine
		}
	}
	return out
}

var issueOfURL = regexp.MustCompile(`/issues/(\d+)(?:[#?/]|$)`)

func (g *github) Reply(ctx context.Context, number int, to Comment, body string) (Comment, error) {
	switch to.Kind {
	case CommentReviewComment:
		root := strings.TrimPrefix(to.ThreadID, ghReviewComment)
		if root == "" {
			root = strings.TrimPrefix(to.ID, ghReviewComment)
		}
		if _, err := strconv.ParseInt(root, 10, 64); err != nil {
			return Comment{}, fmt.Errorf("vcs: %q is not a GitHub review comment", to.ID)
		}
		var c ghComment
		path := g.path("/pulls/" + strconv.Itoa(number) + "/comments/" + root + "/replies")
		err := g.write(ctx, http.MethodPost, path, map[string]string{"body": clip(body, githubMaxBody)}, &c)
		var he *HTTPError
		if err == nil {
			return fromReviewComment(c), nil
		}
		if !errors.As(err, &he) || he.Status != http.StatusNotFound {
			return Comment{}, err
		}
		// The thread is gone: answer in the conversation instead.
	case CommentIssue:
		issue := 0
		if m := issueOfURL.FindStringSubmatch(to.URL); m != nil {
			issue, _ = strconv.Atoi(m[1])
		}
		if issue == 0 {
			var err error
			if issue, err = g.issueFor(ctx, number, nil); err != nil {
				return Comment{}, err
			}
		}
		if issue > 0 {
			return g.postIssueComment(ctx, issue, withRef(body, refBlock(to)), CommentIssue)
		}
	}
	return g.postIssueComment(ctx, number, withRef(body, refBlock(to)), CommentConversation)
}

func (g *github) Post(ctx context.Context, number int, body string) (Comment, error) {
	return g.postIssueComment(ctx, number, body, CommentConversation)
}

func (g *github) postIssueComment(ctx context.Context, number int, body, kind string) (Comment, error) {
	var c ghComment
	path := g.path("/issues/" + strconv.Itoa(number) + "/comments")
	if err := g.write(ctx, http.MethodPost, path, map[string]string{"body": clip(body, githubMaxBody)}, &c); err != nil {
		return Comment{}, err
	}
	return fromIssueComment(c, kind), nil
}

func (g *github) Edit(ctx context.Context, c Comment, body string) (Comment, error) {
	if id, ok := strings.CutPrefix(c.ID, ghIssueComment); ok && isNumber(id) {
		path := g.path("/issues/comments/" + id)
		// A fallback reply's link to what it answers survives the edit.
		if refOf(body) == "" {
			var old ghComment
			if err := g.get(ctx, path, nil, &old); err != nil {
				return Comment{}, gone(err)
			}
			body = withRef(body, refOf(old.Body))
		}
		var out ghComment
		if err := g.write(ctx, http.MethodPatch, path, map[string]string{"body": clip(body, githubMaxBody)}, &out); err != nil {
			return Comment{}, gone(err)
		}
		kind := c.Kind
		if kind == "" {
			kind = CommentConversation
		}
		return fromIssueComment(out, kind), nil
	}
	if id, ok := strings.CutPrefix(c.ID, ghReviewComment); ok && isNumber(id) {
		var out ghComment
		if err := g.write(ctx, http.MethodPatch, g.path("/pulls/comments/"+id), map[string]string{"body": clip(body, githubMaxBody)}, &out); err != nil {
			return Comment{}, gone(err)
		}
		return fromReviewComment(out), nil
	}
	if strings.HasPrefix(c.ID, ghReview) {
		return Comment{}, errors.New("vcs: a GitHub review summary can't be edited")
	}
	return Comment{}, fmt.Errorf("vcs: %q is not a GitHub comment ID", c.ID)
}

func isNumber(s string) bool {
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

// gone turns a 404 into ErrGone.
func gone(err error) error {
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		return fmt.Errorf("%w: %w", ErrGone, err)
	}
	return err
}
