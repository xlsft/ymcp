package ytwatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xlsft/ymcp/internal/youtrack"
)

// Comment is a comment seen in the activity feed. Text, Updated, Deleted
// and Visibility are the comment's state when the feed was read, which may
// be newer than the activity that brought it.
type Comment struct {
	ID       string // e.g. 7-153
	IssueID  string // database id of the issue, e.g. 3-304
	IssueKey string // readable id, e.g. YPC-2
	Author   string // login
	Text     string
	URL      string
	Created  time.Time
	Updated  time.Time // zero when never edited
	// Deleted comments come with no text; callers drop them.
	Deleted bool
	// Edited is set when the feed brought only a text edit of the comment,
	// not its creation.
	Edited bool
	// Visibility is "" when everyone who sees the issue sees the comment,
	// otherwise the permitted groups and user logins, comma-separated
	// ("limited" when YouTrack lists none).
	Visibility string
}

// ErrBadCursor means YouTrack rejected the stored cursor. Start again from
// BootstrapCursor (or an earlier time) and rely on the seen-comment table.
var ErrBadCursor = errors.New("youtrack rejected the activity cursor")

// commentPageSize is $top of one activitiesPage request.
const commentPageSize = 100

// bootstrapLag is subtracted from the server clock for a fresh cursor, so
// a comment written in the same second is not missed; replays are
// deduplicated by the caller.
const bootstrapLag = 5 * time.Second

const commentActivityFields = "afterCursor,hasAfter,activities(id,timestamp,$type,category(id)," +
	"target(id,$type,deleted,text,created,updated,author(login),issue(id,idReadable)," +
	"visibility($type,permittedGroups(name),permittedUsers(login))))"

// BootstrapCursor returns a cursor at the server's current time, a few
// seconds back: "ERH.^+:<ms>". Comments returns activities after it.
func (w *Watcher) BootstrapCursor(ctx context.Context) (string, error) {
	now, err := w.ServerTime(ctx)
	if err != nil {
		return "", err
	}
	return timeCursor(now.Add(-bootstrapLag)), nil
}

func timeCursor(t time.Time) string {
	return "ERH.^+:" + strconv.FormatInt(t.UnixMilli(), 10)
}

// restartCursor matches a cursor that points before the start of the
// stream, like "ERH.^+:-1", which YouTrack returns for an empty first page.
// Used as a cursor it replays the whole history, so it is never kept.
var restartCursor = regexp.MustCompile(`:-\d+$`)

func usable(cursor string) bool {
	return cursor != "" && !restartCursor.MatchString(cursor)
}

type apiActivity struct {
	ID       string `json:"id"`
	Category struct {
		ID string `json:"id"`
	} `json:"category"`
	Target *apiComment `json:"target"`
}

// apiComment is a comment as the feed and the comments of an issue give it.
type apiComment struct {
	ID      string  `json:"id"`
	Type    string  `json:"$type"`
	Deleted bool    `json:"deleted"`
	Text    *string `json:"text"`
	Created *int64  `json:"created"`
	Updated *int64  `json:"updated"`
	Author  *struct {
		Login string `json:"login"`
	} `json:"author"`
	Issue *struct {
		ID         string `json:"id"`
		IDReadable string `json:"idReadable"`
	} `json:"issue"`
	Visibility *struct {
		Type            string `json:"$type"`
		PermittedGroups []struct {
			Name string `json:"name"`
		} `json:"permittedGroups"`
		PermittedUsers []struct {
			Login string `json:"login"`
		} `json:"permittedUsers"`
	} `json:"visibility"`
}

// Comments reads comment creations and text edits in the given projects
// after cursor, following pages while YouTrack has more, and returns them
// with the cursor to store for the next call.
//
// The query names projects, never issue ids, so a deleted issue can't
// break it; callers pick their issues by IssueID or IssueKey. A comment
// that appears several times (created, then edited) is returned once, at
// its first position, with its latest state; Edited is false if its
// creation was among them.
//
// An empty or restart cursor ("ERH.^+:-1") is replaced by BootstrapCursor.
// next is never a restart cursor: when YouTrack answers with one, the
// input cursor is kept. With no projects nothing is requested.
func (w *Watcher) Comments(ctx context.Context, projects []string, cursor string) (items []Comment, next string, err error) {
	if !usable(cursor) {
		if cursor, err = w.BootstrapCursor(ctx); err != nil {
			return nil, "", err
		}
	}
	if len(projects) == 0 {
		return nil, cursor, nil
	}
	q := url.Values{
		"categories": {"CommentsCategory,CommentTextCategory"},
		"issueQuery": {projectQuery(projects)},
		"fields":     {commentActivityFields},
		"$top":       {strconv.Itoa(commentPageSize)},
	}
	index := map[string]int{} // comment id -> position in items
	for page := 0; page < maxPages; page++ {
		q.Set("cursor", cursor)
		var res struct {
			Activities  []apiActivity `json:"activities"`
			AfterCursor string        `json:"afterCursor"`
			HasAfter    bool          `json:"hasAfter"`
		}
		if err := w.yt.Get(ctx, "activitiesPage", q, &res); err != nil {
			return nil, "", commentsError(err)
		}
		for _, a := range res.Activities {
			c, ok := w.comment(a)
			if !ok {
				continue
			}
			if i, seen := index[c.ID]; seen {
				c.Edited = items[i].Edited && c.Edited
				items[i] = c
				continue
			}
			index[c.ID] = len(items)
			items = append(items, c)
		}
		if !usable(res.AfterCursor) || res.AfterCursor == cursor {
			break
		}
		cursor = res.AfterCursor
		if !res.HasAfter {
			break
		}
	}
	return items, cursor, nil
}

func (w *Watcher) comment(a apiActivity) (Comment, bool) {
	c, ok := w.fromAPI(a.Target)
	c.Edited = a.Category.ID == "CommentTextCategory"
	return c, ok
}

// fromAPI makes a Comment of one YouTrack gave; false when it isn't an
// issue comment.
func (w *Watcher) fromAPI(t *apiComment) (Comment, bool) {
	if t == nil || t.ID == "" || (t.Type != "" && t.Type != "IssueComment") {
		return Comment{}, false
	}
	c := Comment{
		ID:      t.ID,
		Created: msTime(t.Created),
		Updated: msTime(t.Updated),
		Deleted: t.Deleted || t.Text == nil,
	}
	if t.Text != nil {
		c.Text = *t.Text
	}
	if t.Author != nil {
		c.Author = t.Author.Login
	}
	if t.Issue != nil {
		c.IssueID, c.IssueKey = t.Issue.ID, t.Issue.IDReadable
		c.URL = CommentURL(w.base, c.IssueKey, c.ID)
	}
	if v := t.Visibility; v != nil && v.Type != "" && v.Type != "UnlimitedVisibility" {
		var who []string
		for _, g := range v.PermittedGroups {
			who = append(who, g.Name)
		}
		for _, u := range v.PermittedUsers {
			who = append(who, u.Login)
		}
		c.Visibility = strings.Join(who, ", ")
		if c.Visibility == "" {
			c.Visibility = "limited"
		}
	}
	return c, true
}

// issueCommentFields are the fields of a comment read from its issue, the
// same as the feed's target.
const issueCommentFields = "id,$type,deleted,text,created,updated,author(login),issue(id,idReadable)," +
	"visibility($type,permittedGroups(name),permittedUsers(login))"

// issueCommentPage and issueCommentPages bound the comments IssueComments
// reads: pages of issueCommentPage, at most issueCommentPages of them.
const (
	issueCommentPage  = 200
	issueCommentPages = 25
)

// IssueComments lists the comments of an issue, oldest first, without the
// deleted ones. Unlike Comments it reads no feed: it is for an issue whose
// comments no cursor followed.
func (w *Watcher) IssueComments(ctx context.Context, idOrKey string) ([]Comment, error) {
	var out []Comment
	for page := range issueCommentPages {
		q := youtrack.Fields(issueCommentFields)
		q.Set("$top", strconv.Itoa(issueCommentPage))
		q.Set("$skip", strconv.Itoa(page*issueCommentPage))
		var raw []apiComment
		if err := w.yt.Get(ctx, "issues/"+url.PathEscape(idOrKey)+"/comments", q, &raw); err != nil {
			return nil, fmt.Errorf("comments of %s: %w", idOrKey, err)
		}
		for i := range raw {
			if c, ok := w.fromAPI(&raw[i]); ok && !c.Deleted {
				out = append(out, c)
			}
		}
		if len(raw) < issueCommentPage {
			break
		}
	}
	return out, nil
}

// projectQuery builds "project: A, B", bracing names that need it.
func projectQuery(projects []string) string {
	names := make([]string, 0, len(projects))
	for _, p := range projects {
		if strings.ContainsFunc(p, func(r rune) bool {
			return !(r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
		}) {
			p = "{" + p + "}"
		}
		names = append(names, p)
	}
	return "project: " + strings.Join(names, ", ")
}

// commentsError marks a rejected cursor: YouTrack answers 400
// `For input string: "..."` when it can't parse one.
func commentsError(err error) error {
	var ye *youtrack.Error
	if errors.As(err, &ye) && ye.Status == http.StatusBadRequest &&
		strings.Contains(ye.Description+ye.Body, "For input string") {
		return fmt.Errorf("%w: %w", ErrBadCursor, err)
	}
	return fmt.Errorf("comment feed: %w", err)
}
