package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
)

// Keys of the run state of the change's title and description.
const (
	stateChangeDraft = "change_draft" // JSON changeDraft the agent passed to set_change
	stateChangeSet   = "change_set"   // the hash of the title and body ymcp last set on the change
)

// describeAgain is the prompt of the second try when the agent didn't
// call set_change.
const describeAgain = "Call the ymcp tool set_change now with the title and the description; do nothing else."

// changeDraft is a title and description for the run's change.
type changeDraft struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// SetChange keeps the title and description the agent passed to
// set_change; describe reads them.
func (c callbacks) SetChange(ctx context.Context, runID int64, title, description string) error {
	b, _ := json.Marshal(changeDraft{Title: title, Description: description})
	if err := c.r.st.SetRunState(ctx, runID, stateChangeDraft, string(b)); err != nil {
		return err
	}
	c.r.event(ctx, runID, "agent.set_change", map[string]string{"title": title, "description": trimText(description, 2000)})
	return nil
}

// draft is what the agent last passed to set_change.
func (t *turn) draft() (changeDraft, bool) {
	var d changeDraft
	v, ok, err := t.r.st.RunState(t.bg, t.run.ID, stateChangeDraft)
	if err != nil || !ok || v == "" || json.Unmarshal([]byte(v), &d) != nil {
		return changeDraft{}, false
	}
	return d, strings.TrimSpace(d.Description) != ""
}

// describe asks the agent, in its session, for the title and description
// of the run's change: a turn of its own with prompts.describe, whose
// answer comes structured through set_change rather than out of what the
// agent says. A second, shorter turn follows when it didn't call the
// tool; then the text of that turn, which ran no tools, is the
// description. ok is false when nothing came.
func (t *turn) describe() (d changeDraft, ok bool) {
	r := t.r
	if r.vcs == nil || !r.p.Strategy.Described() || t.opts.Dir == "" {
		return changeDraft{}, false
	}
	if err := r.st.SetRunState(t.bg, t.run.ID, stateChangeDraft, ""); err != nil {
		r.log.Warn("clearing the change draft failed", "run", t.run.Number, "err", err)
	}
	t.vars["change.noun"] = strings.ToLower(r.changeNoun())
	t.vars["change.action"] = "opens"
	if t.run.PRNumber > 0 {
		t.vars["change.action"] = "updates"
	}
	prompt, err := text.Render(r.prompts.Describe, t.vars)
	if err != nil {
		r.log.Warn("prompts.describe failed", "run", t.run.Number, "err", err)
		return changeDraft{}, false
	}
	var last string
	for _, p := range []string{prompt, describeAgain} {
		res, ran := t.describeTurn(p)
		if d, ok := t.draft(); ok {
			return d, true
		}
		if !ran {
			break
		}
		last = res.Text
	}
	if last = strings.TrimSpace(last); last != "" {
		r.log.Info("the agent didn't call set_change; its answer is the description", "run", t.run.Number)
		return changeDraft{Description: last}, true
	}
	r.log.Warn("the agent wrote no title or description", "run", t.run.Number)
	return changeDraft{}, false
}

// describeTurn runs one prompt in the agent's session, resumed; ran is
// false when it could not.
func (t *turn) describeTurn(prompt string) (agent.Result, bool) {
	r := t.r
	opts := t.opts
	opts.SessionID, opts.SystemPrompt = t.run.AgentSession, ""
	if opts.SessionID == "" {
		return agent.Result{}, false
	}
	sess, err := r.agent.Start(t.ctx, opts)
	if err != nil {
		if t.ctx.Err() == nil {
			r.log.Warn("resuming the agent to describe the change failed", "run", t.run.Number, "err", err)
		}
		return agent.Result{}, false
	}
	r.event(t.bg, t.run.ID, "describe.start", map[string]string{"prompt": trimText(prompt, promptLimit)})
	res, cause, err := t.drive(sess, prompt)
	sess.Close()
	t.drain(sess)
	r.forgetGroup(t.bg, t.run)
	reason := cmpOr(cause, res.StopReason)
	if err != nil {
		reason = err.Error()
	}
	r.event(t.bg, t.run.ID, "describe.end", map[string]string{"reason": reason})
	return res, err == nil && cause == ""
}

// publishChange opens the run's change after a push, or updates the one
// it has, with the title and description the agent wrote for it.
func (t *turn) publishChange(answer string) error {
	if t.r.vcs == nil {
		return nil
	}
	d, ok := t.describe()
	if t.run.PRNumber == 0 {
		return t.openChange(answer, d, ok)
	}
	if ok {
		t.updateChange(d)
	}
	return nil
}

// updateChange sets the title and description on the run's change, unless
// a person edited them since ymcp last did.
func (t *turn) updateChange(d changeDraft) {
	r, run := t.r, t.run
	ctx, cancel := context.WithTimeout(t.bg, gitTimeout)
	defer cancel()
	cur, err := r.vcs.Change(ctx, run.PRNumber)
	if err != nil {
		r.log.Warn("reading the change failed", "run", run.Number, "err", err)
		return
	}
	if cur.State != vcs.StateOpen {
		return
	}
	set, ok, err := r.st.RunState(t.bg, run.ID, stateChangeSet)
	if err != nil || !ok || set != changeHash(cur) {
		r.event(t.bg, run.ID, "vcs.update_skipped", map[string]string{"url": run.PRURL, "reason": "edited on the code host"})
		return
	}
	title := d.Title
	if title == "" {
		title = t.changeTitle()
	}
	ch, err := r.vcs.Update(ctx, run.PRNumber, title, r.decorateVCS(run.Number, d.Description))
	if err != nil {
		r.log.Warn("updating the change failed", "run", run.Number, "err", err)
		r.event(t.bg, run.ID, "vcs.update_failed", map[string]string{"url": run.PRURL, "error": err.Error()})
		return
	}
	t.rememberChange(ch)
	r.event(t.bg, run.ID, "vcs.updated", map[string]string{"url": run.PRURL, "title": ch.Title})
}

// rememberChange keeps the hash of the title and body ymcp set on the
// change, to tell a person's edit later.
func (t *turn) rememberChange(ch vcs.Change) {
	if err := t.r.st.SetRunState(t.bg, t.run.ID, stateChangeSet, changeHash(ch)); err != nil {
		t.r.log.Warn("saving the change's hash failed", "run", t.run.Number, "err", err)
	}
}

// changeHash identifies a change's title and body as the host keeps them,
// line endings and surrounding space aside.
func changeHash(ch vcs.Change) string {
	norm := func(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }
	sum := sha256.Sum256([]byte(norm(ch.Title) + "\x00" + norm(ch.Body)))
	return hex.EncodeToString(sum[:])
}

// cmpOr is the first of a and b that is not empty.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
