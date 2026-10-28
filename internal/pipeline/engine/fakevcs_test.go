package engine

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
)

// fakeVCS is an in-memory code host with one pull request. Every comment,
// human or agent, is in comments; Comments returns the ones created or
// edited since the cursor's revision.
type fakeVCS struct {
	kind vcs.Kind
	me   string

	mu       sync.Mutex
	opens    []vcs.OpenOptions
	updates  []vcs.OpenOptions // the titles and bodies Update was called with
	change   vcs.Change
	comments []*fakeVCSComment
	rev      int
	nextID   int
	limited  time.Time       // Change and Comments are rate limited until then
	calls    []fakeVCSCall   // Change and Comments calls
	cursors  []vcs.Cursor    // the cursors Comments was called with
	gone     map[string]bool // deleted comment ids: Edit gives ErrGone
	newErr   error           // returned by newStrategy
	repo     vcs.Repo
	replies  map[string]string   // reply id -> id of the comment it answers
	edits    map[string]int      // comment id -> number of edits
	bodies   map[string][]string // comment id -> every body it had
	closes   []int               // the changes Close was called for
	hidden   []string            // the comments Hide was called for
	ci       fakeCI              // its CI
}

type fakeVCSComment struct {
	vcs.Comment
	rev     int
	deleted bool
}

type fakeVCSCall struct {
	what    string
	at      time.Time
	limited bool // answered with a *vcs.RateLimitError
}

func newFakeVCS(kind vcs.Kind) *fakeVCS {
	return &fakeVCS{
		kind: kind, me: "octo", nextID: 1000,
		gone: map[string]bool{}, replies: map[string]string{}, edits: map[string]int{}, bodies: map[string][]string{},
		repo: vcs.Repo{Host: "github.test", Owner: "acme", Name: "api", Path: "acme/api", Kind: vcs.KindGitHubPR},
	}
}

// newStrategy is the harness's Options.NewStrategy.
func (f *fakeVCS) newStrategy(_ context.Context, kind vcs.Kind, origin string) (Remote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newErr != nil {
		return Remote{Repo: f.repo}, f.newErr
	}
	f.kind = kind
	return Remote{Repo: f.repo, TokenSource: "gh", Strategy: f}, nil
}

func (f *fakeVCS) Kind() vcs.Kind                     { return f.kind }
func (f *fakeVCS) Me(context.Context) (string, error) { return f.me, nil }

func (f *fakeVCS) Open(_ context.Context, o vcs.OpenOptions) (vcs.Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, o)
	if f.change.Number == 0 || f.change.State != vcs.StateOpen {
		n := max(f.change.Number+1, 42) // the first change is #42
		f.change = vcs.Change{Number: n, URL: "https://github.test/acme/api/pull/" + strconv.Itoa(n), State: vcs.StateOpen, Draft: o.Draft, Title: o.Title, Body: o.Body}
		if f.kind == vcs.KindGitHubIssuePR {
			f.change.Issue, f.change.IssueURL = 7, "https://github.test/acme/api/issues/7"
		}
	}
	return f.change, nil
}

func (f *fakeVCS) Update(_ context.Context, number int, title, body string) (vcs.Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if number != f.change.Number {
		return vcs.Change{}, fmt.Errorf("no change #%d", number)
	}
	f.updates = append(f.updates, vcs.OpenOptions{Title: title, Body: body})
	f.change.Title, f.change.Body = title, body
	return f.change, nil
}

// editChange changes the change's title and body as a person on the host
// would.
func (f *fakeVCS) editChange(title, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.change.Title, f.change.Body = title, body
}

func (f *fakeVCS) updateLog() []vcs.OpenOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]vcs.OpenOptions(nil), f.updates...)
}

// limitedNow records a call and reports the rate limit in force.
func (f *fakeVCS) limitedNow(what string) error {
	now := time.Now()
	limited := now.Before(f.limited)
	f.calls = append(f.calls, fakeVCSCall{what, now, limited})
	if limited {
		return &vcs.RateLimitError{Until: f.limited, Message: "API rate limit exceeded"}
	}
	return nil
}

func (f *fakeVCS) Change(_ context.Context, number int) (vcs.Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.limitedNow("change"); err != nil {
		return vcs.Change{}, err
	}
	if number != f.change.Number {
		return vcs.Change{}, fmt.Errorf("no change #%d", number)
	}
	return f.change, nil
}

func (f *fakeVCS) Comments(_ context.Context, number int, since vcs.Cursor) ([]vcs.Comment, vcs.Cursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.limitedNow("comments"); err != nil {
		return nil, since, err
	}
	f.cursors = append(f.cursors, since)
	from, _ := strconv.Atoi(since["rev"])
	var out []vcs.Comment
	for _, c := range f.comments {
		if c.rev > from && !c.deleted {
			out = append(out, c.Comment)
		}
	}
	return out, vcs.Cursor{"rev": strconv.Itoa(f.rev)}, nil
}

// add stores a comment; the caller holds mu.
func (f *fakeVCS) add(c vcs.Comment) vcs.Comment {
	f.nextID++
	f.rev++
	if c.ID == "" {
		c.ID = "issuecomment-" + strconv.Itoa(f.nextID)
	}
	if c.Kind == "" {
		c.Kind = vcs.CommentConversation
	}
	if c.URL == "" {
		c.URL = f.change.URL + "#" + c.ID
	}
	c.Created, c.Updated = time.Now(), time.Now()
	f.comments = append(f.comments, &fakeVCSComment{Comment: c, rev: f.rev})
	f.bodies[c.ID] = append(f.bodies[c.ID], c.Body)
	return c
}

func (f *fakeVCS) Reply(_ context.Context, number int, to vcs.Comment, body string) (vcs.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := vcs.Comment{Author: f.me, Body: body}
	if to.Kind == vcs.CommentReviewComment {
		f.nextID++
		c.ID, c.Kind, c.ThreadID = "discussion_r"+strconv.Itoa(f.nextID), vcs.CommentReviewComment, to.ThreadID
		c.File, c.Line = to.File, to.Line
	}
	c = f.add(c)
	f.replies[c.ID] = to.ID
	return c, nil
}

func (f *fakeVCS) Close(_ context.Context, number int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, number)
	if number == f.change.Number && f.change.State == vcs.StateOpen {
		f.change.State = vcs.StateClosed
	}
	return nil
}

func (f *fakeVCS) Hide(_ context.Context, c vcs.Comment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone[c.ID] {
		return fmt.Errorf("%w: 404", vcs.ErrGone)
	}
	f.hidden = append(f.hidden, c.ID)
	return nil
}

// hiddenIDs lists the comments Hide was called for.
func (f *fakeVCS) hiddenIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.hidden)
}

func (f *fakeVCS) Post(_ context.Context, number int, body string) (vcs.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.add(vcs.Comment{Author: f.me, Body: body}), nil
}

func (f *fakeVCS) Edit(_ context.Context, c vcs.Comment, body string) (vcs.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone[c.ID] {
		return vcs.Comment{}, fmt.Errorf("%w: 404", vcs.ErrGone)
	}
	for _, have := range f.comments {
		if have.ID == c.ID {
			f.rev++
			have.Body, have.Updated, have.rev = body, time.Now(), f.rev
			f.edits[c.ID]++
			f.bodies[c.ID] = append(f.bodies[c.ID], body)
			return have.Comment, nil
		}
	}
	return vcs.Comment{}, fmt.Errorf("%w: no comment %s", vcs.ErrGone, c.ID)
}

// comment writes a human comment on the change.
func (f *fakeVCS) comment(c vcs.Comment) vcs.Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.add(c)
}

// commentAll writes several human comments at once, so one poll sees them
// all.
func (f *fakeVCS) commentAll(cs ...vcs.Comment) []vcs.Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]vcs.Comment, len(cs))
	for i, c := range cs {
		out[i] = f.add(c)
	}
	return out
}

// deleteComment removes a comment as a human would.
func (f *fakeVCS) deleteComment(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gone[id] = true
	for _, c := range f.comments {
		if c.ID == id {
			c.deleted = true
		}
	}
}

func (f *fakeVCS) setState(state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.change.State, f.change.Merged = state, state == vcs.StateMerged
}

func (f *fakeVCS) limitUntil(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limited = t
}

func (f *fakeVCS) closeLog() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.closes)
}

func (f *fakeVCS) openLog() []vcs.OpenOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.opens)
}

func (f *fakeVCS) callLog() []fakeVCSCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeVCS) cursorLog() []vcs.Cursor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cursors)
}

// agentReplies returns the comments of pipeline backend's agents that are
// not deleted, without header, with the id of the comment each answers (""
// for top-level posts).
func (f *fakeVCS) agentReplies() []fakeAgentReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeAgentReply
	for _, c := range f.comments {
		if c.deleted {
			continue
		}
		if m, ok := text.ParseMarker(c.Body); ok && c.Author == f.me && m.Pipeline == "backend" {
			out = append(out, fakeAgentReply{ID: c.ID, To: f.replies[c.ID], Thread: c.ThreadID, Body: text.StripHeader(c.Body), Raw: c.Body, Edits: f.edits[c.ID], History: slices.Clone(f.bodies[c.ID])})
		}
	}
	return out
}

type fakeAgentReply struct {
	ID, To, Thread, Body, Raw string
	Edits                     int
	History                   []string
}

// notes records Options.Notify.
type notes struct {
	mu   sync.Mutex
	list []note
}

type note struct {
	Kind, Title, Body, URL string
	Run                    store.Run
}

func (n *notes) add(_ context.Context, kind string, run store.Run, title, body, url string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.list = append(n.list, note{kind, title, body, url, run})
}

func (n *notes) of(kind string) []note {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []note
	for _, x := range n.list {
		if x.Kind == kind {
			out = append(out, x)
		}
	}
	return out
}
