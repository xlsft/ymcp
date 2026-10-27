package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// sourceYouTrack is the inbox and message source of YouTrack comments.
const sourceYouTrack = "youtrack"

// Message kinds and states of store.Message.
const (
	msgAnswer     = "answer"     // the placeholder edited into the turn's answer
	msgReply      = "reply"      // an answer to one feedback item
	msgReport     = "report"     // report(text) of the agent
	msgPermission = "permission" // a permission question
	msgNotice     = "notice"     // a note of ymcp: needs input, canceled

	stateThinking = "thinking"
	stateDone     = "done"
	stateFailed   = "failed"
)

// comment is a comment as the engine reads it for prompts.
type comment struct {
	ID, Author, Text string
	Created          time.Time
}

// mark returns a fresh mark of the run's next message.
func (r *Runner) mark(number int) text.Mark {
	return text.Mark{Pipeline: r.p.Name, Run: number, Msg: text.NewMsgID()}
}

// postComment publishes a decorated comment on the issue and returns its id.
func (r *Runner) postComment(ctx context.Context, issueID string, number int, body string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	payload := map[string]string{"text": text.Decorate(body, r.mention(), r.mark(number))}
	if err := r.yt.Post(ctx, "issues/"+url.PathEscape(issueID)+"/comments", youtrack.Fields("id"), payload, &out); err != nil {
		return "", fmt.Errorf("posting a comment on %s: %w", issueID, err)
	}
	return out.ID, nil
}

// editComment replaces the text of one of the agent's comments.
func (r *Runner) editComment(ctx context.Context, issueID, commentID string, number int, body string) error {
	payload := map[string]string{"text": text.Decorate(body, r.mention(), r.mark(number))}
	path := "issues/" + url.PathEscape(issueID) + "/comments/" + url.PathEscape(commentID)
	if err := r.yt.Post(ctx, path, youtrack.Fields("id"), payload, nil); err != nil {
		return fmt.Errorf("editing comment %s on %s: %w", commentID, issueID, err)
	}
	return nil
}

// hideComment marks a comment deleted, as the UI does: it disappears from
// the issue and stays in its history, where it can be restored.
func (r *Runner) hideComment(ctx context.Context, issueID, commentID string) error {
	path := "issues/" + url.PathEscape(issueID) + "/comments/" + url.PathEscape(commentID)
	if err := r.yt.Post(ctx, path, youtrack.Fields("id"), map[string]bool{"deleted": true}, nil); err != nil {
		return fmt.Errorf("hiding comment %s on %s: %w", commentID, issueID, err)
	}
	return nil
}

// isNotFound reports whether err is a YouTrack 404.
func isNotFound(err error) bool {
	var ye *youtrack.Error
	return errors.As(err, &ye) && ye.Status == http.StatusNotFound
}

// warnUnlessGone logs a failed write of a run's message as a warning, or
// quietly at debug level when YouTrack answered 404: the issue was deleted,
// which the run learns about on its own.
func (r *Runner) warnUnlessGone(msg string, run store.Run, err error) {
	if isNotFound(err) {
		r.log.Debug(msg, "run", run.Number, "issue", run.IssueKey, "err", err)
		return
	}
	r.log.Warn(msg, "run", run.Number, "err", err)
}

// publish writes body to YouTrack as a message of the run: it edits the
// message m when there is one (posting anew when a human deleted it) and
// posts a new comment otherwise. It records the message with the state and
// returns it.
func (r *Runner) publish(ctx context.Context, run store.Run, m *store.Message, kind, state, body string) (store.Message, error) {
	ctx = context.WithoutCancel(ctx)
	if m != nil && m.ExternalID != "" && m.State != stateHidden {
		err := r.editComment(ctx, run.IssueID, m.ExternalID, run.Number, body)
		if err == nil {
			m.State = state
			if err := r.st.UpdateMessage(ctx, *m); err != nil {
				r.log.Warn("saving a message failed", "run", run.Number, "err", err)
			}
			return *m, nil
		}
		if !isNotFound(err) {
			return *m, err
		}
		// The placeholder was deleted by hand: post the message again.
	}
	id, err := r.postComment(ctx, run.IssueID, run.Number, body)
	if err != nil {
		return store.Message{}, err
	}
	if m != nil && m.ID != 0 {
		m.ExternalID, m.State = id, state
		if err := r.st.UpdateMessage(ctx, *m); err != nil {
			r.log.Warn("saving a message failed", "run", run.Number, "err", err)
		}
		return *m, nil
	}
	msg, err := r.st.AddMessage(ctx, store.Message{RunID: run.ID, Source: sourceYouTrack, ExternalID: id, ThreadID: run.IssueID, Kind: kind, State: state})
	if err != nil {
		r.log.Warn("saving a message failed", "run", run.Number, "err", err)
	}
	return msg, nil
}

// comments lists the issue's comments, oldest first, without deleted ones.
func (r *Runner) comments(ctx context.Context, issueID string) ([]comment, error) {
	var raw []struct {
		ID      string `json:"id"`
		Text    string `json:"text"`
		Deleted bool   `json:"deleted"`
		Created int64  `json:"created"`
		Author  *struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	q := youtrack.Fields("id,text,deleted,created,author(login)")
	q.Set("$top", "500")
	if err := r.yt.Get(ctx, "issues/"+url.PathEscape(issueID)+"/comments", q, &raw); err != nil {
		return nil, fmt.Errorf("reading the comments of %s: %w", issueID, err)
	}
	out := make([]comment, 0, len(raw))
	for _, c := range raw {
		if c.Deleted {
			continue
		}
		cm := comment{ID: c.ID, Text: c.Text, Created: time.UnixMilli(c.Created)}
		if c.Author != nil {
			cm.Author = c.Author.Login
		}
		out = append(out, cm)
	}
	return out, nil
}

// humanComments renders the issue's comments that no agent wrote, for the
// {{comments}} variable.
func humanComments(cs []comment, own map[string]bool) string {
	var parts []string
	for _, c := range cs {
		if own[c.ID] {
			continue
		}
		if _, ok := text.ParseMarker(c.Text); ok {
			continue
		}
		t := strings.TrimSpace(c.Text)
		if t == "" {
			continue
		}
		if c.Author != "" {
			t = "@" + c.Author + ": " + t
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, "\n\n")
}

// previousAnswers returns the texts of this run's earlier messages, for the
// recap of a new session.
func (r *Runner) previousAnswers(cs []comment, number int) []string {
	var out []string
	for _, c := range cs {
		if m, ok := text.ParseMarker(c.Text); ok && m.Pipeline == r.p.Name && m.Run == number {
			if t := strings.TrimSpace(text.StripHeader(c.Text)); t != "" && t != text.Working {
				out = append(out, t)
			}
		}
	}
	return out
}

// issueVars are the template variables of an issue.
func issueVars(is ytwatch.Issue) text.Vars {
	v := text.Vars{
		"issue.id":          is.Key,
		"issue.key":         is.Key,
		"issue.dbid":        is.ID,
		"issue.summary":     is.Summary,
		"issue.description": is.Description,
		"issue.url":         is.URL,
	}
	keys := make([]string, 0, len(is.Fields))
	for k := range is.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v["issue.fields."+k] = is.Fields[k]
	}
	return v
}

// projectOf is the project short name of a readable issue id: YPC of YPC-12.
func projectOf(key string) string {
	i := strings.LastIndex(key, "-")
	if i <= 0 {
		return ""
	}
	if _, err := strconv.Atoi(key[i+1:]); err != nil {
		return ""
	}
	return key[:i]
}

// describeErr is the text of a YouTrack failure for a human: the command
// error's description when there is one.
func describeErr(err error) string {
	var ce *ytwatch.CommandError
	if errors.As(err, &ce) {
		return ce.Description
	}
	return err.Error()
}
