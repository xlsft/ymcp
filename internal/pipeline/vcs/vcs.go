// Package vcs publishes a pipeline run's branch on a code host and reads the
// feedback people leave there. A Strategy is one way of doing that: just the
// branch, a GitHub pull request, a GitHub issue that mirrors the YouTrack
// issue plus a pull request that closes it, or a GitLab merge request
// (gitlab.com or a self-hosted instance found from the origin URL).
//
// Everything is plain REST over net/http. Lists are paged with per_page=100
// and polled with ETag/If-None-Match, so an unchanged list costs a 304. Writes
// to one host are serialised and spaced at least a second apart, and rate
// limit answers (429, or GitHub's 403 "rate limit") are waited out when the
// wait is short. Timestamps are normalised to UTC.
//
// Comment IDs carry what is needed to edit the comment later, so Edit only
// needs the ID: "issuecomment-<id>" (a GitHub PR or issue conversation
// comment), "discussion_r<id>" (a GitHub review comment on a line),
// "pullrequestreview-<id>" (a GitHub review summary, read-only) and
// "!<iid>#note_<id>" (a GitLab merge request note). They match the URL
// fragments the hosts use.
package vcs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/text"
)

// Kind is a repository strategy.
type Kind string

// The strategies.
const (
	KindBranch        Kind = "branch"          // push the branch only
	KindGitHubPR      Kind = "github-pr"       // a pull request
	KindGitHubIssuePR Kind = "github-issue-pr" // an issue mirroring the YouTrack issue, and a PR that closes it
	KindGitLabMR      Kind = "gitlab-mr"       // a merge request
)

// Kinds lists every strategy.
var Kinds = []Kind{KindBranch, KindGitHubPR, KindGitHubIssuePR, KindGitLabMR}

// Comment kinds.
const (
	CommentConversation  = "conversation"   // a top-level PR/MR comment
	CommentReview        = "review"         // a GitHub review summary
	CommentReviewComment = "review_comment" // a comment on a line of the diff
	CommentIssue         = "issue"          // a comment on the mirror issue (github-issue-pr)
)

// Change states.
const (
	StateOpen   = "open"
	StateClosed = "closed"
	StateMerged = "merged"
)

// Change is a pull or merge request.
type Change struct {
	Number int
	URL    string
	State  string // open | closed | merged
	Merged bool
	Draft  bool
	// Issue and IssueURL are the mirror issue of a github-issue-pr change.
	Issue    int
	IssueURL string
}

// Comment is one piece of feedback, or a message the agent wrote.
type Comment struct {
	ID       string // see the package doc for the formats
	ThreadID string // the thread a reply goes to; "" for flat comments
	Author   string // login (GitHub) or username (GitLab)
	Body     string // raw text, with any hidden marker
	URL      string
	File     string // for line comments
	Line     int
	Hunk     string // the diff hunk a GitHub line comment is attached to
	Outdated bool   // the line is gone from the current diff
	Created  time.Time
	Updated  time.Time
	Kind     string // conversation | review | review_comment | issue
}

// Cursor is a strategy's polling state: since marks and ETags. Treat it as
// opaque and pass back what Comments returned.
type Cursor map[string]string

// OpenOptions describe the change to open.
type OpenOptions struct {
	Branch string // the pushed branch
	Base   string // the target branch; "" means the repository's default branch
	Title  string
	Body   string
	Draft  bool
	// IssueTitle and IssueBody are the mirror issue of github-issue-pr;
	// an empty title falls back to Title.
	IssueTitle string
	IssueBody  string
}

// Strategy publishes a run's work and reads the feedback on it.
type Strategy interface {
	Kind() Kind
	// Me is the login the strategy acts as ("" for the branch strategy).
	Me(ctx context.Context) (login string, err error)
	// Open creates the pull or merge request (and, for github-issue-pr, the
	// issue). An open one for the same branch is returned instead of a new
	// one. The branch strategy returns a zero Change.
	Open(ctx context.Context, o OpenOptions) (Change, error)
	// Change reads the state of a change.
	Change(ctx context.Context, number int) (Change, error)
	// Comments returns the feedback on a change that is new or changed since
	// the cursor, oldest first, and the cursor for the next call. Pages that
	// did not change are skipped; deduplicating by (ID, Updated) is the
	// caller's job, as the since windows overlap by a few seconds.
	Comments(ctx context.Context, number int, since Cursor) ([]Comment, Cursor, error)
	// Reply answers a comment: in its thread where the host has threads,
	// otherwise as a new conversation comment that links to and quotes it.
	Reply(ctx context.Context, number int, to Comment, body string) (Comment, error)
	// Edit replaces the body of a comment the strategy wrote; the whole new
	// body is sent. A comment that is gone gives ErrGone.
	Edit(ctx context.Context, c Comment, body string) (Comment, error)
	// Post writes a new top-level comment on the change.
	Post(ctx context.Context, number int, body string) (Comment, error)
	// Close closes the change without merging it, and the mirror issue of
	// github-issue-pr as not planned. The branch stays. A change that is
	// closed or merged already is left as it is.
	Close(ctx context.Context, number int) error
	// Hide folds a comment away on the page while it stays in the
	// history: GitHub minimizes it as outdated, GitLab resolves its
	// thread. A comment the host can't hide (a GitHub review summary, a
	// GitLab note outside a resolvable thread) gives ErrCantHide; one that
	// is gone gives ErrGone.
	Hide(ctx context.Context, c Comment) error
}

// Auth is how a strategy signs in to the host.
type Auth struct {
	Token  string
	Source string // where TokenFor found the token: "gh", "glab" or "keyring"
}

var (
	// ErrGone means the comment or change no longer exists.
	ErrGone = errors.New("vcs: gone")
	// ErrNoChange is what the branch strategy answers to anything that needs
	// a pull request.
	ErrNoChange = errors.New("vcs: the branch strategy has no pull or merge request")
	// ErrNoToken means TokenFor found no credentials for the host.
	ErrNoToken = errors.New("vcs: no token")
	// ErrCantHide means the host has no way to hide the comment.
	ErrCantHide = errors.New("vcs: the comment can't be hidden")
)

// New returns the strategy kind for repo. An empty repo.API is derived from
// the host; the token may be empty for public read-only use.
func New(kind Kind, repo Repo, auth Auth) (Strategy, error) {
	switch kind {
	case KindBranch:
		return branch{}, nil
	case KindGitHubPR, KindGitHubIssuePR:
		if repo.Kind == KindGitLabMR {
			return nil, fmt.Errorf("vcs: %s looks like a GitLab host; strategy %s needs GitHub", repo.Host, kind)
		}
		if repo.Owner == "" || repo.Name == "" || strings.Contains(repo.Owner, "/") {
			return nil, fmt.Errorf("vcs: %q is not an owner/name GitHub repository", repo.Path)
		}
		c, err := newClient(familyGitHub, repo, auth.Token)
		if err != nil {
			return nil, err
		}
		return &github{c: c, repo: repo, kind: kind, issues: map[int]int{}}, nil
	case KindGitLabMR:
		if repo.Kind == KindGitHubPR || repo.Kind == KindGitHubIssuePR {
			return nil, fmt.Errorf("vcs: %s looks like a GitHub host; strategy %s needs GitLab", repo.Host, kind)
		}
		if repo.Path == "" {
			return nil, errors.New("vcs: the GitLab project path is empty")
		}
		c, err := newClient(familyGitLab, repo, auth.Token)
		if err != nil {
			return nil, err
		}
		return &gitlab{c: c, repo: repo}, nil
	}
	return nil, fmt.Errorf("vcs: unknown strategy %q", kind)
}

// branch is the strategy that only pushes the branch.
type branch struct{}

func (branch) Kind() Kind                                        { return KindBranch }
func (branch) Me(context.Context) (string, error)                { return "", nil }
func (branch) Open(context.Context, OpenOptions) (Change, error) { return Change{}, nil }
func (branch) Change(context.Context, int) (Change, error)       { return Change{}, nil }
func (branch) Comments(_ context.Context, _ int, since Cursor) ([]Comment, Cursor, error) {
	return nil, since, nil
}
func (branch) Reply(context.Context, int, Comment, string) (Comment, error) {
	return Comment{}, ErrNoChange
}
func (branch) Edit(context.Context, Comment, string) (Comment, error) { return Comment{}, ErrNoChange }
func (branch) Post(context.Context, int, string) (Comment, error)     { return Comment{}, ErrNoChange }
func (branch) Close(context.Context, int) error                       { return ErrNoChange }
func (branch) Hide(context.Context, Comment) error                    { return ErrNoChange }

// refIntro starts the block a fallback reply puts under the agent header to
// say what it answers. Edit keeps the block when the new body lacks it.
const refIntro = "> In reply to "

// refBlock links to and quotes the comment a reply answers, for hosts where
// the reply can't go into a thread.
func refBlock(to Comment) string {
	what := "comment"
	if to.Kind == CommentReview {
		what = "review"
	}
	who := "the " + what
	if to.Author != "" {
		who = "@" + to.Author + "'s " + what
	}
	if to.URL != "" {
		who = "[" + who + "](" + to.URL + ")"
	}
	lines := []string{refIntro + who + ":"}
	quote := strings.TrimSpace(text.StripHeader(to.Body))
	n := 0
	for l := range strings.SplitSeq(quote, "\n") {
		l = strings.TrimRight(l, " \t\r")
		if n == 3 {
			lines = append(lines, "> …")
			break
		}
		if r := []rune(l); len(r) > 200 {
			l = string(r[:200]) + "…"
		}
		lines = append(lines, strings.TrimRight("> "+l, " "))
		n++
	}
	if quote == "" {
		lines = lines[:1]
	}
	return strings.Join(lines, "\n")
}

// withRef puts ref under the agent header of body (or at the top when there
// is no header).
func withRef(body, ref string) string {
	if ref == "" {
		return body
	}
	rest := text.StripHeader(body)
	head := strings.TrimRight(body[:len(body)-len(rest)], "\r\n")
	if head == "" {
		return ref + "\n\n" + rest
	}
	return head + "\n\n" + ref + "\n\n" + rest
}

// refOf returns the reference block of a body withRef built, or "".
func refOf(body string) string {
	rest := text.StripHeader(body)
	if !strings.HasPrefix(rest, refIntro) {
		return ""
	}
	var lines []string
	for l := range strings.SplitSeq(rest, "\n") {
		l = strings.TrimRight(l, "\r")
		if !strings.HasPrefix(l, ">") {
			break
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

// clip cuts body to at most max bytes on a rune boundary, saying so.
func clip(body string, max int) string {
	if len(body) <= max {
		return body
	}
	const tail = "\n\n… [trimmed]"
	cut := max - len(tail)
	for cut > 0 && !utf8RuneStart(body[cut]) {
		cut--
	}
	return body[:cut] + tail
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
