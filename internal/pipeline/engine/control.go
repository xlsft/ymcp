package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/text"
)

// sourceCLI is the inbox source of answers given in the TUI or the CLI.
const sourceCLI = "cli"

// Answer queues text as a human reply to the run, as if written in
// YouTrack by the local user. A running runner picks it up within a
// second: it answers a question, an open permission request (/approve,
// /reject) or is feedback on a run in review. A run that ended as done or
// closed takes only /clear and /compact.
func Answer(ctx context.Context, st *store.Store, pipeline string, run int, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("the answer is empty")
	}
	r, err := st.RunByNumber(ctx, pipeline, run)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("pipeline %s has no run #%d", pipeline, run)
		}
		return err
	}
	if r.Status.Final() && !(EndedCommand(text) && (r.Status == store.StatusDone || r.Status == store.StatusClosed)) {
		return fmt.Errorf("run #%d of %s has ended (%s)", run, pipeline, r.Status)
	}
	var b [8]byte
	rand.Read(b[:]) // never fails
	author := localUser()
	if _, err := st.AddInbox(ctx, store.InboxItem{RunID: r.ID, Source: sourceCLI, ExternalID: "cli-" + hex.EncodeToString(b[:]), Author: author, Body: text}); err != nil {
		return err
	}
	return st.AddEvent(ctx, r.ID, "inbox", map[string]string{"source": sourceCLI, "author": author, "text": trimText(text, 500)})
}

// Cancel marks the run canceled. A live runner notices within a second
// (otherwise the next runner, when it starts), stops the agent, closes the
// run's pull or merge request and removes its worktree unless the pipeline
// keeps worktrees; the branch stays.
func Cancel(ctx context.Context, st *store.Store, pipeline string, run int) error {
	r, err := st.RunByNumber(ctx, pipeline, run)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("pipeline %s has no run #%d", pipeline, run)
		}
		return err
	}
	who := localUser()
	cur, err := markCanceled(ctx, st, r.ID, "Canceled by "+who+".", "Canceled from the CLI by "+who+".")
	if errors.Is(err, store.ErrRunFinished) {
		return fmt.Errorf("run #%d of %s has already ended (%s)", run, pipeline, cur.Status)
	}
	return err
}

// Comment is a comment posted on a run's issue, as YouTrack stored it.
type Comment struct {
	ID      string // e.g. 7-153
	Author  string // login
	Text    string
	URL     string
	Created time.Time
	Updated time.Time // zero when never edited
}

// Commented hands the run a comment the TUI posted on its issue as the
// pipeline's user, so a live runner acts on it within a second instead of
// on its next poll of YouTrack. The comment is queued and marked seen as
// that poll would do it (see pollComments): whichever of the two comes
// first queues it, and the other finds it seen. A run that has ended
// takes only /clear and /compact; any other comment is left to a reopened
// run (see Runner.queueSince).
func Commented(ctx context.Context, st *store.Store, pipeline string, run int, c Comment) error {
	if c.ID == "" {
		return errors.New("the comment has no id")
	}
	r, err := st.RunByNumber(ctx, pipeline, run)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("pipeline %s has no run #%d", pipeline, run)
		}
		return err
	}
	if r.Status.Final() && !EndedCommand(c.Text) {
		return nil
	}
	at := c.Created
	if !c.Updated.IsZero() {
		at = c.Updated
	}
	mark := seenMark(strconv.FormatInt(at.UnixMilli(), 10), c.Text)
	return st.Tx(ctx, func(tx *store.Store) error {
		if _, seen, err := tx.Seen(ctx, sourceYouTrack, c.ID); err != nil || seen {
			return err
		}
		if _, marked := text.ParseMarker(c.Text); !marked && !at.Before(r.CreatedAt) {
			added, err := tx.AddInbox(ctx, store.InboxItem{RunID: r.ID, Source: sourceYouTrack, ExternalID: c.ID, Author: c.Author, Body: c.Text, URL: c.URL})
			if err != nil {
				return err
			}
			if added {
				if err := tx.AddEvent(ctx, r.ID, "inbox", map[string]string{"source": sourceYouTrack, "author": c.Author, "url": c.URL, "text": trimText(c.Text, 500)}); err != nil {
					return err
				}
			}
		}
		return tx.MarkSeen(ctx, sourceYouTrack, c.ID, mark)
	})
}

// Delete removes a run that has ended from the local store, with its log,
// messages and inbox. The issue, the branch and the pull request stay as
// they are. A canceled run is kept until a runner has closed its change
// and removed its worktree. An issue held after its run was canceled by
// moving it back to the backlog is free to be captured again.
func Delete(ctx context.Context, st *store.Store, pipeline string, run int) error {
	r, err := st.RunByNumber(ctx, pipeline, run)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("pipeline %s has no run #%d", pipeline, run)
		}
		return err
	}
	if !r.Status.Final() {
		return fmt.Errorf("run #%d of %s is still %s; cancel it first", run, pipeline, r.Status)
	}
	due, _, err := st.RunState(ctx, r.ID, stateCanceled)
	if err != nil {
		return err
	}
	if due != "" {
		return fmt.Errorf("run #%d of %s is not cleaned up yet (its pull request closed, its worktree removed); delete it once a runner has done that", run, pipeline)
	}
	if err := st.DeleteRun(ctx, r.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("pipeline %s has no run #%d", pipeline, run)
		}
		return err
	}
	return nil
}

// localUser names the person at this machine.
func localUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		name := u.Username
		if i := strings.LastIndexAny(name, `\/`); i >= 0 { // DOMAIN\user on Windows
			name = name[i+1:]
		}
		return name
	}
	for _, v := range []string{"USER", "USERNAME"} {
		if s := os.Getenv(v); s != "" {
			return s
		}
	}
	return "cli"
}
