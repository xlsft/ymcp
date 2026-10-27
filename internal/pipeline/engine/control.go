package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"

	"github.com/xlsft/ymcp/internal/pipeline/store"
)

// sourceCLI is the inbox source of answers given in the TUI or the CLI.
const sourceCLI = "cli"

// Answer queues text as a human reply to the run, as if written in
// YouTrack by the local user. A running runner picks it up on its next
// poll: it answers a question, an open permission request (/approve,
// /reject) or is feedback on a run in review.
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
	if r.Status.Final() {
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

// Cancel marks the run canceled. A live runner notices on its next poll
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
