// Package ytwatch polls YouTrack for the pipeline runner: the issues of a
// source query, stage membership of tracked issues, a comment feed with a
// cursor, stage commands and probes for deleted issues.
//
// Queries never list issue ids: one deleted issue makes YouTrack reject the
// whole query with 400. Result sets are fetched whole and intersected
// locally instead, and a single GET of the issue tells a deleted issue
// (404) from a transient failure.
package ytwatch

import (
	"context"
	"encoding/json"
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

// defaultPageSize is how many issues one search page asks for.
const defaultPageSize = 500

// maxPages stops a paging loop that never ends, e.g. a server that keeps
// answering hasAfter with the same cursor.
const maxPages = 1000

// Issue is a YouTrack issue as the runner sees it.
type Issue struct {
	// ID is the database id, e.g. 3-304; it survives a move to another project.
	ID string
	// Key is the readable id, e.g. YPC-2.
	Key         string
	Summary     string
	Description string // only from Watcher.Issue
	URL         string
	Updated     time.Time
	// Fields are the custom fields as plain strings, only from Watcher.Issue:
	// names for bundle values, logins for users, presentation for periods,
	// RFC 3339 for dates, comma-separated when there are several values.
	// An empty field is "".
	Fields map[string]string
}

// Watcher polls one YouTrack instance through a shared client.
type Watcher struct {
	yt       *youtrack.Client
	base     string
	pageSize int
}

// New returns a Watcher using yt. The runner should share one client so
// there is exactly one sign-in.
func New(yt *youtrack.Client) *Watcher {
	return &Watcher{
		yt:       yt,
		base:     yt.BaseURL(),
		pageSize: defaultPageSize,
	}
}

// IssueURL is the web link of an issue, e.g. https://x.youtrack.cloud/issue/YPC-1.
func IssueURL(base, key string) string {
	return strings.TrimRight(base, "/") + "/issue/" + key
}

// CommentURL is the web link of a comment, e.g.
// https://x.youtrack.cloud/issue/YPC-1#focus=Comments-7-152.0-0.
func CommentURL(base, key, commentID string) string {
	return IssueURL(base, key) + "#focus=Comments-" + commentID + ".0-0"
}

const (
	issueLightFields = "id,idReadable,summary,updated"
	issueFullFields  = "id,idReadable,summary,description,updated," +
		"customFields(name,$type,value(name,login,fullName,presentation,text,minutes))"
)

type apiIssue struct {
	ID           string  `json:"id"`
	IDReadable   string  `json:"idReadable"`
	Summary      string  `json:"summary"`
	Description  *string `json:"description"`
	Updated      *int64  `json:"updated"`
	CustomFields []struct {
		Name  string          `json:"name"`
		Type  string          `json:"$type"`
		Value json.RawMessage `json:"value"`
	} `json:"customFields"`
}

func (w *Watcher) issue(a apiIssue) Issue {
	is := Issue{ID: a.ID, Key: a.IDReadable, Summary: a.Summary, URL: IssueURL(w.base, a.IDReadable), Updated: msTime(a.Updated)}
	if a.Description != nil {
		is.Description = *a.Description
	}
	if a.CustomFields != nil {
		is.Fields = make(map[string]string, len(a.CustomFields))
		for _, f := range a.CustomFields {
			is.Fields[f.Name] = fieldString(f.Type, f.Value)
		}
	}
	return is
}

// Search returns every issue matching query, paging through the whole
// result set, with id, key, summary, URL and updated only.
func (w *Watcher) Search(ctx context.Context, query string) ([]Issue, error) {
	var out []Issue
	seen := map[string]bool{}
	for skip, page := 0, 0; page < maxPages; page++ {
		var batch []apiIssue
		q := url.Values{
			"query":  {query},
			"fields": {issueLightFields},
			"$skip":  {strconv.Itoa(skip)},
			"$top":   {strconv.Itoa(w.pageSize)},
		}
		if err := w.yt.Get(ctx, "issues", q, &batch); err != nil {
			return nil, fmt.Errorf("search %q: %w", query, err)
		}
		for _, a := range batch {
			// Issues may shift between pages while paging; keep the first copy.
			if seen[a.ID] {
				continue
			}
			seen[a.ID] = true
			out = append(out, w.issue(a))
		}
		if len(batch) < w.pageSize {
			break
		}
		skip += len(batch)
	}
	return out, nil
}

// Issue returns one issue with its description and custom fields.
// idOrKey is a database id (3-304) or a readable id (YPC-2).
func (w *Watcher) Issue(ctx context.Context, idOrKey string) (Issue, error) {
	var a apiIssue
	if err := w.yt.Get(ctx, "issues/"+url.PathEscape(idOrKey), youtrack.Fields(issueFullFields), &a); err != nil {
		return Issue{}, fmt.Errorf("get issue %s: %w", idOrKey, err)
	}
	return w.issue(a), nil
}

// Exists reports whether the issue is still there. Only a 404 means it is
// gone (deleted, or no longer visible to this account); any other failure
// is returned as an error and says nothing about the issue.
func (w *Watcher) Exists(ctx context.Context, idOrKey string) (bool, error) {
	err := w.yt.Get(ctx, "issues/"+url.PathEscape(idOrKey), youtrack.Fields("id"), nil)
	if err == nil {
		return true, nil
	}
	var ye *youtrack.Error
	if errors.As(err, &ye) && ye.Status == http.StatusNotFound {
		return false, nil
	}
	return false, fmt.Errorf("check issue %s: %w", idOrKey, err)
}

// Members reports which of ids match stageQuery. It runs the query once,
// parenthesised, and intersects locally; ids may be database or readable
// ids. Every id is in the result, true when it matched.
func (w *Watcher) Members(ctx context.Context, stageQuery string, ids []string) (map[string]bool, error) {
	if strings.TrimSpace(stageQuery) == "" {
		return nil, errors.New("stage query is empty")
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = false
	}
	if len(ids) == 0 {
		return out, nil
	}
	found, err := w.Search(ctx, "("+stageQuery+")")
	if err != nil {
		return nil, err
	}
	for _, is := range found {
		if _, ok := out[is.ID]; ok {
			out[is.ID] = true
		}
		if _, ok := out[is.Key]; ok {
			out[is.Key] = true
		}
	}
	return out, nil
}

// Me returns the login of the account the client signs in as.
func (w *Watcher) Me(ctx context.Context) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	if err := w.yt.Get(ctx, "users/me", youtrack.Fields("login"), &u); err != nil {
		return "", fmt.Errorf("current user: %w", err)
	}
	return u.Login, nil
}

// ServerTime returns YouTrack's clock, read from the Date header of an
// authenticated request through the shared client. The header has second
// precision.
func (w *Watcher) ServerTime(ctx context.Context) (time.Time, error) {
	t, err := w.yt.ServerTime(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("server time: %w", err)
	}
	return t, nil
}

// msTime converts a YouTrack timestamp (ms since epoch, possibly null).
func msTime(ms *int64) time.Time {
	if ms == nil || *ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(*ms).UTC()
}

// fieldString reduces a custom field value to a plain string.
func fieldString(fieldType string, raw json.RawMessage) string {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return plain(fieldType, v)
}

func plain(fieldType string, v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if s := plain(fieldType, item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		for _, key := range []string{"login", "name", "presentation", "text", "fullName"} {
			if s, ok := v[key].(string); ok && s != "" {
				return s
			}
		}
		return ""
	case float64:
		if strings.HasPrefix(fieldType, "Date") {
			return time.UnixMilli(int64(v)).UTC().Format(time.RFC3339)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	default:
		return fmt.Sprint(v)
	}
}

// dbID matches a database id such as 3-304, as opposed to a readable id.
var dbID = regexp.MustCompile(`^\d+-\d+$`)

// issueRef is the JSON reference to an issue by either kind of id.
func issueRef(idOrKey string) map[string]string {
	if dbID.MatchString(idOrKey) {
		return map[string]string{"id": idOrKey}
	}
	return map[string]string{"idReadable": idOrKey}
}
