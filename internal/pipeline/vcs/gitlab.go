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
	"time"
)

// GitLab caps note bodies at 1,000,000 characters.
const gitlabMaxBody = 1_000_000

// gitlab is the gitlab-mr strategy.
type gitlab struct {
	c    *client
	repo Repo

	mu  sync.Mutex
	me  string
	def string
}

type glUser struct {
	Username string `json:"username"`
}

type glMR struct {
	IID    int    `json:"iid"`
	WebURL string `json:"web_url"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	WIP    bool   `json:"work_in_progress"`
}

type glNote struct {
	ID        int64       `json:"id"`
	Body      string      `json:"body"`
	Author    glUser      `json:"author"`
	CreatedAt stamp       `json:"created_at"`
	UpdatedAt stamp       `json:"updated_at"`
	System    bool        `json:"system"`
	Internal  bool        `json:"internal"`
	Type      string      `json:"type"`
	Position  *glPosition `json:"position"`
}

type glPosition struct {
	NewPath string `json:"new_path"`
	OldPath string `json:"old_path"`
	NewLine *int   `json:"new_line"`
	OldLine *int   `json:"old_line"`
}

type glDiscussion struct {
	ID       string   `json:"id"`
	Resolved bool     `json:"resolved"`
	Notes    []glNote `json:"notes"`
}

func (l *gitlab) Kind() Kind { return KindGitLabMR }

func (l *gitlab) path(rest string) string {
	return "projects/" + url.PathEscape(l.repo.Path) + rest
}

func (l *gitlab) mrPath(iid int, rest string) string {
	return l.path("/merge_requests/" + strconv.Itoa(iid) + rest)
}

func (l *gitlab) get(ctx context.Context, path string, q url.Values, out any) error {
	resp, err := l.c.send(ctx, http.MethodGet, l.c.url(path, q), nil, "")
	if err != nil {
		return err
	}
	return decode(resp, out)
}

func (l *gitlab) write(ctx context.Context, method, path string, in, out any) error {
	resp, err := l.c.send(ctx, method, l.c.url(path, nil), in, "")
	if err != nil {
		return err
	}
	return decode(resp, out)
}

func (l *gitlab) Me(ctx context.Context) (string, error) {
	l.mu.Lock()
	me := l.me
	l.mu.Unlock()
	if me != "" {
		return me, nil
	}
	var u glUser
	if err := l.get(ctx, "user", nil, &u); err != nil {
		return "", err
	}
	l.mu.Lock()
	l.me = u.Username
	l.mu.Unlock()
	return u.Username, nil
}

func (l *gitlab) defaultBranch(ctx context.Context) (string, error) {
	l.mu.Lock()
	def := l.def
	l.mu.Unlock()
	if def != "" {
		return def, nil
	}
	var p struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := l.get(ctx, l.path(""), nil, &p); err != nil {
		return "", err
	}
	if p.DefaultBranch == "" {
		return "", fmt.Errorf("vcs: %s has no default branch", l.repo.Path)
	}
	l.mu.Lock()
	l.def = p.DefaultBranch
	l.mu.Unlock()
	return p.DefaultBranch, nil
}

var draftRe = regexp.MustCompile(`(?i)^\s*(?:\[draft\]|\(draft\)|draft:|draft\s*-)`)

func (l *gitlab) Open(ctx context.Context, o OpenOptions) (Change, error) {
	if o.Branch == "" {
		return Change{}, errors.New("vcs: no branch to open a merge request for")
	}
	base := o.Base
	if base == "" {
		var err error
		if base, err = l.defaultBranch(ctx); err != nil {
			return Change{}, err
		}
	}
	var open []glMR
	q := url.Values{"source_branch": {o.Branch}, "state": {"opened"}, "per_page": {"1"}}
	if err := l.get(ctx, l.path("/merge_requests"), q, &open); err != nil {
		return Change{}, err
	}
	if len(open) > 0 {
		return mrChange(open[0]), nil
	}
	title := o.Title
	if o.Draft && !draftRe.MatchString(title) {
		title = "Draft: " + title
	}
	in := map[string]any{
		"source_branch": o.Branch, "target_branch": base, "title": title,
		"description": clip(o.Body, gitlabMaxBody), "remove_source_branch": false,
	}
	var mr glMR
	if err := l.write(ctx, http.MethodPost, l.path("/merge_requests"), in, &mr); err != nil {
		return Change{}, fmt.Errorf("vcs: creating the merge request: %w", err)
	}
	return mrChange(mr), nil
}

func mrChange(mr glMR) Change {
	ch := Change{Number: mr.IID, URL: mr.WebURL, State: StateOpen, Draft: mr.Draft || mr.WIP}
	switch mr.State {
	case "merged":
		ch.State, ch.Merged = StateMerged, true
	case "closed":
		ch.State = StateClosed
	}
	return ch
}

func (l *gitlab) Change(ctx context.Context, number int) (Change, error) {
	var mr glMR
	if err := l.get(ctx, l.mrPath(number, ""), nil, &mr); err != nil {
		return Change{}, err
	}
	return mrChange(mr), nil
}

// Close closes an open merge request.
func (l *gitlab) Close(ctx context.Context, number int) error {
	ch, err := l.Change(ctx, number)
	if err != nil || ch.State != StateOpen {
		return err
	}
	return l.write(ctx, http.MethodPut, l.mrPath(number, ""), map[string]string{"state_event": "close"}, nil)
}

// noteURL is the web address of a note on merge request iid.
func (l *gitlab) noteURL(iid int, id int64) string {
	return l.c.web + "/" + l.repo.Path + "/-/merge_requests/" + strconv.Itoa(iid) + "#note_" + strconv.FormatInt(id, 10)
}

func (l *gitlab) noteID(iid int, id int64) string {
	return "!" + strconv.Itoa(iid) + "#note_" + strconv.FormatInt(id, 10)
}

// parseNoteID splits "!<iid>#note_<id>".
func parseNoteID(s string) (iid int, note int64, ok bool) {
	rest, ok := strings.CutPrefix(s, "!")
	if !ok {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(rest, "#note_")
	if !ok {
		return 0, 0, false
	}
	iid, err1 := strconv.Atoi(a)
	note, err2 := strconv.ParseInt(b, 10, 64)
	return iid, note, err1 == nil && err2 == nil && iid > 0
}

// Comments reads the merge request's discussions. GitLab has no since filter
// here, so unchanged pages are skipped by ETag and notes older than the since
// mark are dropped locally. System notes, internal notes and resolved
// threads are left out.
func (l *gitlab) Comments(ctx context.Context, number int, since Cursor) ([]Comment, Cursor, error) {
	const key = "since:discussions"
	next := Cursor{}
	if v, ok := since[key]; ok {
		next[key] = v
	}
	var floor time.Time
	if t, err := time.Parse(time.RFC3339Nano, since[key]); err == nil {
		floor = t.Add(-sinceSlack)
	}
	var out []Comment
	first := l.c.url(l.mrPath(number, "/discussions"), url.Values{"per_page": {strconv.Itoa(perPage)}})
	err := l.c.pages(ctx, "discussions", first, since, next, func(b []byte) (int, error) {
		var list []glDiscussion
		if err := json.Unmarshal(b, &list); err != nil {
			return 0, err
		}
		for _, d := range list {
			var diff *glPosition
			isDiff := false
			if len(d.Notes) > 0 {
				diff = d.Notes[0].Position
				isDiff = diff != nil || d.Notes[0].Type == "DiffNote"
			}
			for _, n := range d.Notes {
				advance(next, key, n.UpdatedAt.Time)
				if d.Resolved || n.System || n.Internal || n.UpdatedAt.Before(floor) {
					continue
				}
				c := Comment{
					ID: l.noteID(number, n.ID), ThreadID: d.ID, Author: n.Author.Username, Body: n.Body,
					URL: l.noteURL(number, n.ID), Created: n.CreatedAt.Time, Updated: n.UpdatedAt.Time,
					Kind: CommentConversation,
				}
				if isDiff {
					c.Kind = CommentReviewComment
					pos := n.Position
					if pos == nil {
						pos = diff
					}
					if pos != nil {
						c.File = pos.NewPath
						if c.File == "" {
							c.File = pos.OldPath
						}
						if pos.NewLine != nil {
							c.Line = *pos.NewLine
						} else if pos.OldLine != nil {
							c.Line = *pos.OldLine
						}
					}
				}
				out = append(out, c)
			}
		}
		return len(list), nil
	})
	if err != nil {
		return nil, since, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, next, nil
}

func (l *gitlab) fromNote(iid int, thread, kind string, n glNote) Comment {
	return Comment{
		ID: l.noteID(iid, n.ID), ThreadID: thread, Author: n.Author.Username, Body: n.Body,
		URL: l.noteURL(iid, n.ID), Created: n.CreatedAt.Time, Updated: n.UpdatedAt.Time, Kind: kind,
	}
}

// Reply answers in the comment's thread; a single comment becomes a thread.
func (l *gitlab) Reply(ctx context.Context, number int, to Comment, body string) (Comment, error) {
	if to.ThreadID != "" {
		var n glNote
		path := l.mrPath(number, "/discussions/"+url.PathEscape(to.ThreadID)+"/notes")
		err := l.write(ctx, http.MethodPost, path, map[string]string{"body": clip(body, gitlabMaxBody)}, &n)
		if err == nil {
			kind := to.Kind
			if kind == "" {
				kind = CommentConversation
			}
			return l.fromNote(number, to.ThreadID, kind, n), nil
		}
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusNotFound {
			return Comment{}, err
		}
		// The thread is gone: start a new one that links to the comment.
	}
	return l.Post(ctx, number, withRef(body, refBlock(to)))
}

// Post starts a new thread on the merge request.
func (l *gitlab) Post(ctx context.Context, number int, body string) (Comment, error) {
	var d glDiscussion
	if err := l.write(ctx, http.MethodPost, l.mrPath(number, "/discussions"), map[string]string{"body": clip(body, gitlabMaxBody)}, &d); err != nil {
		return Comment{}, err
	}
	if len(d.Notes) == 0 {
		return Comment{}, errors.New("vcs: GitLab returned a thread without notes")
	}
	return l.fromNote(number, d.ID, CommentConversation, d.Notes[0]), nil
}

// Hide resolves the note's thread, which folds it away on the merge
// request; GitLab has no other way to hide a note.
func (l *gitlab) Hide(ctx context.Context, c Comment) error {
	iid, _, ok := parseNoteID(c.ID)
	if !ok {
		return fmt.Errorf("vcs: %q is not a GitLab note ID", c.ID)
	}
	if c.ThreadID == "" {
		return ErrCantHide
	}
	path := l.mrPath(iid, "/discussions/"+url.PathEscape(c.ThreadID))
	err := l.write(ctx, http.MethodPut, path, map[string]bool{"resolved": true}, nil)
	var he *HTTPError
	if errors.As(err, &he) && (he.Status == http.StatusBadRequest || he.Status == http.StatusForbidden) {
		return ErrCantHide // not a resolvable thread
	}
	return gone(err)
}

func (l *gitlab) Edit(ctx context.Context, c Comment, body string) (Comment, error) {
	iid, id, ok := parseNoteID(c.ID)
	if !ok {
		return Comment{}, fmt.Errorf("vcs: %q is not a GitLab note ID", c.ID)
	}
	path := l.mrPath(iid, "/notes/"+strconv.FormatInt(id, 10))
	// A fallback reply's link to what it answers survives the edit.
	if refOf(body) == "" {
		var old glNote
		if err := l.get(ctx, path, nil, &old); err != nil {
			return Comment{}, gone(err)
		}
		body = withRef(body, refOf(old.Body))
	}
	var n glNote
	if err := l.write(ctx, http.MethodPut, path, map[string]string{"body": clip(body, gitlabMaxBody)}, &n); err != nil {
		return Comment{}, gone(err)
	}
	kind := c.Kind
	if kind == "" {
		kind = CommentConversation
	}
	return l.fromNote(iid, c.ThreadID, kind, n), nil
}
