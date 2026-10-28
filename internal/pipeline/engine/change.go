package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
)

// Inbox and message sources of a change's comments.
const (
	sourceGitHub = "github"
	sourceGitLab = "gitlab"
)

// Keys of the run state that the change of a run uses.
const (
	stateVCSCursor  = "vcs_cursor"   // JSON vcs.Cursor of the change's comments
	stateVCSComment = "vcs_comment:" // + inbox external id: JSON vcs.Comment of a queued comment
)

// Kinds of Options.Notify.
const (
	NotifyNeedsInput  = "needs_input"  // the run waits for a human
	NotifyRunFailed   = "run_failed"   // a turn failed; the run waits for a human
	NotifyReviewReady = "review_ready" // the agent's work is ready for review
	NotifyRunDone     = "run_done"     // the run ended: merged, done or closed
)

// Remote is the code host of a pipeline whose strategy opens pull or merge
// requests, as Options.NewStrategy found it.
type Remote struct {
	Repo        vcs.Repo
	TokenSource string // where the token came from: gh, glab or keyring
	Strategy    vcs.Strategy
}

// newStrategy is the real Options.NewStrategy.
func newStrategy(ctx context.Context, kind vcs.Kind, originURL string) (Remote, error) {
	repo, err := vcs.ParseRemote(originURL)
	if err != nil {
		return Remote{}, err
	}
	rem := Remote{Repo: repo}
	auth, err := vcs.TokenFor(ctx, repo)
	if err != nil {
		return rem, err
	}
	rem.TokenSource = auth.Source
	if rem.Strategy, err = vcs.New(kind, repo, auth); err != nil {
		return rem, err
	}
	return rem, nil
}

// needsChange reports whether a strategy kind opens pull or merge requests.
func needsChange(kind string) bool { return kind != "" && kind != string(vcs.KindBranch) }

// sourceOf is the inbox source of a strategy kind's comments.
func sourceOf(kind string) string {
	if kind == string(vcs.KindGitLabMR) {
		return sourceGitLab
	}
	return sourceGitHub
}

// isVCSSource reports whether an inbox or message source is a code host.
func isVCSSource(s string) bool { return s == sourceGitHub || s == sourceGitLab }

// repoName is host/owner/name, or "" for a zero Repo.
func repoName(r vcs.Repo) string {
	if r.Host == "" {
		return ""
	}
	return r.Host + "/" + r.Path
}

// originURL reads the origin remote of the repository at dir.
func originURL(ctx context.Context, h workspace.Host, dir string) (string, error) {
	out, stderr, err := h.Run(ctx, dir, []string{"git", "remote", "get-url", "origin"}, nil)
	if err != nil {
		return "", fmt.Errorf("reading the origin remote of %s: %v: %s", dir, err, strings.TrimSpace(string(stderr)))
	}
	u := strings.TrimSpace(string(out))
	if u == "" {
		return "", fmt.Errorf("the repository at %s has no origin remote", dir)
	}
	return u, nil
}

// connectVCS builds the pipeline's strategy from the workspace's origin
// and signs in to the code host.
func connectVCS(ctx context.Context, p spec.Pipeline, host workspace.Host, opts Options) (Remote, string, error) {
	origin, err := originURL(ctx, host, p.Workspace.Dir)
	if err != nil {
		return Remote{}, "", fmt.Errorf("pipeline %s: strategy %s: %w", p.Name, p.Strategy.Kind, err)
	}
	rem, err := opts.NewStrategy(ctx, vcs.Kind(p.Strategy.Kind), origin)
	if err == nil && rem.Strategy == nil {
		err = errors.New("no strategy was built")
	}
	if err != nil {
		return rem, "", fmt.Errorf("pipeline %s: strategy %s: %w", p.Name, p.Strategy.Kind, err)
	}
	me, err := rem.Strategy.Me(ctx)
	if err != nil {
		return rem, "", fmt.Errorf("pipeline %s: signing in to %s: %w", p.Name, rem.Repo.Host, err)
	}
	return rem, me, nil
}

// checkStrategy adds the strategy lines of Check: the remote, the token and
// the account on the code host.
func checkStrategy(ctx context.Context, p spec.Pipeline, host workspace.Host, opts Options, add func(string, bool, string)) {
	if !needsChange(p.Strategy.Kind) {
		add("strategy", true, "branch: the branch is pushed, no pull or merge request is opened")
		return
	}
	if strings.TrimSpace(p.Workspace.Dir) == "" {
		return
	}
	origin, err := originURL(ctx, host, p.Workspace.Dir)
	if err != nil {
		add("strategy: remote", false, err.Error())
		return
	}
	rem, err := opts.NewStrategy(ctx, vcs.Kind(p.Strategy.Kind), origin)
	if rem.Repo.Host != "" {
		add("strategy: remote", true, fmt.Sprintf("%s (%s)", repoName(rem.Repo), p.Strategy.Kind))
	}
	if err == nil && rem.Strategy == nil {
		err = errors.New("no strategy was built")
	}
	if err != nil {
		name := "strategy: token"
		switch {
		case rem.Repo.Host == "":
			name = "strategy: remote"
		case rem.TokenSource != "":
			name = "strategy"
		}
		add(name, false, err.Error())
		return
	}
	source := rem.TokenSource
	if source == "" {
		source = "an unnamed source"
	}
	add("strategy: token", true, "from "+source)
	me, err := rem.Strategy.Me(ctx)
	if err != nil {
		add("strategy: account", false, err.Error())
		return
	}
	add("strategy: account", true, "signed in to "+rem.Repo.Host+" as "+me)
}

// hasChange reports whether the run has a pull or merge request to follow.
func (r *Runner) hasChange(run store.Run) bool { return r.vcs != nil && run.PRNumber > 0 }

// changeNoun names the change for people.
func (r *Runner) changeNoun() string {
	if r.vcsSource == sourceGitLab {
		return "Merge request"
	}
	return "Pull request"
}

// decorateVCS marks text the runner writes on the code host; the run number
// in the header doesn't link to an issue or pull request there.
func (r *Runner) decorateVCS(number int, body string) string {
	return text.DecorateCodeHost(body, "@"+r.vcsMe, r.mark(number))
}

// publishVCS writes body on the run's change as the message m: it edits m
// when there is one (writing anew when a human deleted it) and otherwise
// replies to `to`, or posts a top-level comment when to is nil. It records
// the message with the state and returns it and the comment written.
func (r *Runner) publishVCS(ctx context.Context, run store.Run, m *store.Message, to *vcs.Comment, kind, state, body string) (store.Message, vcs.Comment, error) {
	ctx = context.WithoutCancel(ctx)
	full := r.decorateVCS(run.Number, body)
	if m != nil && m.ExternalID != "" && m.State != stateHidden {
		c, err := r.vcs.Edit(ctx, vcs.Comment{ID: m.ExternalID, ThreadID: m.ThreadID}, full)
		if err == nil {
			m.State = state
			if err := r.st.UpdateMessage(ctx, *m); err != nil {
				r.log.Warn("saving a message failed", "run", run.Number, "err", err)
			}
			return *m, c, nil
		}
		if !errors.Is(err, vcs.ErrGone) {
			return *m, vcs.Comment{}, err
		}
		// Deleted by hand: write it again.
		r.event(ctx, run.ID, "vcs.reposted", map[string]string{"id": m.ExternalID})
	}
	var c vcs.Comment
	var err error
	if to != nil {
		c, err = r.vcs.Reply(ctx, run.PRNumber, *to, full)
	} else {
		c, err = r.vcs.Post(ctx, run.PRNumber, full)
	}
	if err != nil {
		if m != nil {
			return *m, vcs.Comment{}, err
		}
		return store.Message{}, vcs.Comment{}, err
	}
	if m != nil && m.ID != 0 {
		m.ExternalID, m.ThreadID, m.State = c.ID, c.ThreadID, state
		if err := r.st.UpdateMessage(ctx, *m); err != nil {
			r.log.Warn("saving a message failed", "run", run.Number, "err", err)
		}
		return *m, c, nil
	}
	msg, err := r.st.AddMessage(ctx, store.Message{RunID: run.ID, Source: r.vcsSource, ExternalID: c.ID, ThreadID: c.ThreadID, Kind: kind, State: state})
	if err != nil {
		r.log.Warn("saving a message failed", "run", run.Number, "err", err)
	}
	return msg, c, nil
}

// mirror posts body on the run's change too, so whoever reviews it there
// sees the question or the permission request and can answer it there.
func (r *Runner) mirror(ctx context.Context, run store.Run, kind, body string) {
	if !r.hasChange(run) {
		return
	}
	if _, _, err := r.publishVCS(ctx, run, nil, nil, kind, stateDone, body); err != nil {
		r.log.Warn("posting on the change failed", "run", run.Number, "err", err)
	}
}

// finalizeVCSThinking edits the run's placeholders on the change that an
// unfinished turn left into body. It reports whether there were any.
func (r *Runner) finalizeVCSThinking(ctx context.Context, run store.Run, body string) bool {
	if !r.hasChange(run) {
		return false
	}
	msgs, err := r.st.Messages(ctx, run.ID)
	if err != nil {
		return false
	}
	found := false
	for i := range msgs {
		m := msgs[i]
		if m.State != stateThinking || !isVCSSource(m.Source) {
			continue
		}
		found = true
		if _, _, err := r.publishVCS(ctx, run, &m, nil, m.Kind, stateFailed, body); err != nil {
			r.log.Warn("editing a placeholder on the change failed", "run", run.Number, "err", err)
		}
	}
	return found
}

// notify calls Options.Notify.
func (r *Runner) notify(ctx context.Context, kind string, run store.Run, what, body, url string) {
	if r.notifyFn == nil {
		return
	}
	title := fmt.Sprintf("%s · %s #%d: %s", run.IssueKey, r.p.Name, run.Number, what)
	r.notifyFn(context.WithoutCancel(ctx), kind, run, title, trimText(strings.TrimSpace(body), 1000), url)
}

// issueURL is the web link of the run's issue.
func (r *Runner) issueURL(run store.Run) string {
	return ytwatch.IssueURL(r.yt.BaseURL(), run.IssueKey)
}

// pollVCS follows the changes of active runs: a merged change ends the run
// as done, a closed one as closed, and new comments of allowed authors go
// to the run's inbox. A rate-limited run is skipped until the host allows
// requests again.
func (r *Runner) pollVCS(ctx context.Context) {
	if r.vcs == nil {
		return
	}
	for _, w := range r.liveWorkers() {
		if ctx.Err() != nil {
			return
		}
		run, err := r.st.Run(ctx, w.id)
		if err != nil || run.Status.Final() || run.PRNumber == 0 {
			continue
		}
		if until, ok := r.vcsWait[run.ID]; ok {
			if r.now().Before(until) {
				continue
			}
			delete(r.vcsWait, run.ID)
			r.log.Info("polling the change again", "run", run.Number)
		}
		err = r.pollChange(ctx, w, run)
		var rl *vcs.RateLimitError
		switch {
		case errors.As(err, &rl):
			r.vcsWait[run.ID] = rl.Until
			r.log.Warn("the code host is rate limiting; not polling the change until then", "run", run.Number, "until", rl.Until)
			r.event(ctx, run.ID, "vcs.rate_limited", map[string]string{"until": rl.Until.UTC().Format(time.RFC3339), "error": err.Error()})
		case err != nil && ctx.Err() == nil:
			r.log.Warn("polling the change failed", "run", run.Number, "err", err)
		}
	}
}

// pollChange reads the state and the new comments of a run's change.
func (r *Runner) pollChange(ctx context.Context, w *worker, run store.Run) error {
	ch, err := r.vcs.Change(ctx, run.PRNumber)
	if err != nil {
		return err
	}
	switch {
	case ch.Merged || ch.State == vcs.StateMerged:
		r.changeEnded(ctx, w, run, true)
		return nil
	case ch.State == vcs.StateClosed:
		r.changeEnded(ctx, w, run, false)
		return nil
	}
	return r.pollVCSComments(ctx, run)
}

// changeEnded ends a run whose change was merged (done) or closed without
// a merge (closed), and applies the stage's command.
func (r *Runner) changeEnded(ctx context.Context, w *worker, run store.Run, merged bool) {
	status, stage, st, what := store.StatusClosed, stageClosed, r.p.Stages.Closed, "closed"
	if merged {
		status, stage, st, what = store.StatusDone, stageDone, r.p.Stages.Done, "merged"
	}
	r.log.Info("the change was "+what, "run", run.Number, "url", run.PRURL)
	r.event(ctx, run.ID, "vcs."+what, map[string]string{"url": run.PRURL})
	saved, ok := r.finishRun(ctx, w, status, stage)
	if ok && st.On() && st.Enter != "" {
		r.applyStage(ctx, saved, stage, st.Enter)
	}
}

// pollVCSComments queues the new comments of allowed authors on the run's
// change. The cursor, the seen marks and the inbox items are saved in one
// transaction.
func (r *Runner) pollVCSComments(ctx context.Context, run store.Run) error {
	var cur vcs.Cursor
	if v, ok, err := r.st.RunState(ctx, run.ID, stateVCSCursor); err != nil {
		return err
	} else if ok {
		if err := json.Unmarshal([]byte(v), &cur); err != nil {
			r.log.Warn("the change's cursor is unreadable; reading all comments again", "run", run.Number, "err", err)
			cur = nil
		}
	}
	items, next, err := r.vcs.Comments(ctx, run.PRNumber, cur)
	if err != nil {
		return err
	}
	own := map[string]bool{}
	msgs, err := r.st.Messages(ctx, run.ID)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if isVCSSource(m.Source) {
			own[m.ExternalID] = true
		}
	}
	allowed := map[string]bool{r.vcsMe: true}
	list, setting := r.p.Feedback.GitHub, "feedback.github"
	if r.vcsSource == sourceGitLab {
		list, setting = r.p.Feedback.GitLab, "feedback.gitlab"
	}
	for _, l := range list {
		allowed[l] = true
	}
	repo := run.VCSRepo
	if repo == "" {
		repo = r.vcsRepo
	}
	queued := 0
	err = r.st.Tx(context.WithoutCancel(ctx), func(tx *store.Store) error {
		for _, c := range items {
			at := c.Updated
			if at.IsZero() {
				at = c.Created
			}
			stamp := at.UTC().Format(time.RFC3339Nano)
			key := repo + "/" + c.ID
			prev, seen, err := tx.Seen(ctx, r.vcsSource, key)
			if err != nil {
				return err
			}
			mark := seenMark(stamp, c.Body)
			if seen && (prev == stamp || prev == mark) {
				continue
			}
			if seen && sameText(prev, c.Body) {
				// Unhidden, or touched without an edit: not a new message.
				if err := tx.MarkSeen(ctx, r.vcsSource, key, mark); err != nil {
					return err
				}
				continue
			}
			skip := ""
			switch _, marked := text.ParseMarker(c.Body); {
			case marked || own[c.ID]:
				skip = "agent message"
			case !allowed[c.Author]:
				skip = "author not in " + setting
			case !at.IsZero() && at.Before(run.CreatedAt):
				skip = "written before the run"
			}
			if skip == "" {
				ext := key
				if seen {
					ext = key + "@" + stamp // an edit is a new message
				}
				added, err := tx.AddInbox(ctx, store.InboxItem{RunID: run.ID, Source: r.vcsSource, ExternalID: ext, Author: c.Author, Body: c.Body, URL: c.URL})
				if err != nil {
					return err
				}
				if added {
					queued++
					meta, err := json.Marshal(c)
					if err != nil {
						return err
					}
					if err := tx.SetRunState(ctx, run.ID, stateVCSComment+ext, string(meta)); err != nil {
						return err
					}
					if err := tx.AddEvent(ctx, run.ID, "inbox", map[string]string{"source": r.vcsSource, "author": c.Author, "url": c.URL, "text": trimText(c.Body, 500)}); err != nil {
						return err
					}
				}
			} else if skip != "agent message" {
				if err := tx.AddEvent(ctx, run.ID, "feedback.skipped", map[string]string{"source": r.vcsSource, "author": c.Author, "url": c.URL, "reason": skip}); err != nil {
					return err
				}
			}
			if err := tx.MarkSeen(ctx, r.vcsSource, key, mark); err != nil {
				return err
			}
		}
		b, err := json.Marshal(next)
		if err != nil {
			return err
		}
		return tx.SetRunState(ctx, run.ID, stateVCSCursor, string(b))
	})
	if err != nil {
		return fmt.Errorf("saving the change's comments: %w", err)
	}
	if queued > 0 {
		r.log.Info("feedback queued", "run", run.Number, "issue", run.IssueKey, "source", r.vcsSource, "items", queued)
	}
	return nil
}

// vcsComment returns the comment an inbox item of the change was made
// from, and forgets it.
func (r *Runner) vcsComment(ctx context.Context, it store.InboxItem) (vcs.Comment, bool) {
	ctx = context.WithoutCancel(ctx)
	key := stateVCSComment + it.ExternalID
	v, ok, err := r.st.RunState(ctx, it.RunID, key)
	if err != nil || !ok {
		return vcs.Comment{}, false
	}
	var c vcs.Comment
	if err := json.Unmarshal([]byte(v), &c); err != nil {
		return vcs.Comment{}, false
	}
	if err := r.st.SetRunState(ctx, it.RunID, key, ""); err != nil {
		r.log.Warn("forgetting a comment failed", "err", err)
	}
	return c, true
}

// vcsReply is the turn's placeholder answering one comment of the change.
type vcsReply struct {
	n   int            // the item's number in the batch
	to  vcs.Comment    // the comment answered
	msg *store.Message // nil until it is written
	url string
}

// openVCSPlaceholders loads the comments of the change the turn's items
// came from and answers each with a placeholder (in its thread where the
// host has threads).
func (t *turn) openVCSPlaceholders() {
	r, run := t.r, t.run
	if len(t.items) == 0 {
		return
	}
	for i, it := range t.items {
		if !isVCSSource(it.Source) {
			continue
		}
		if !r.hasChange(run) {
			continue
		}
		c, ok := r.vcsComment(t.bg, it)
		if !ok {
			c = vcs.Comment{Author: it.Author, Body: it.Body, URL: it.URL, Kind: vcs.CommentConversation}
		}
		t.vcsReplies = append(t.vcsReplies, &vcsReply{n: i + 1, to: c})
	}
	for _, rp := range t.vcsReplies {
		to := rp.to
		m, c, err := r.publishVCS(t.bg, run, nil, &to, msgReply, stateThinking, text.Working)
		if err != nil {
			r.log.Warn("posting a placeholder on the change failed", "run", run.Number, "err", err)
			continue
		}
		rp.msg, rp.url = &m, c.URL
	}
}

// writeVCS edits the placeholder rp into body (writing it when posting the
// placeholder failed).
func (t *turn) writeVCS(rp *vcsReply, state, body string) {
	to := rp.to
	m, c, err := t.r.publishVCS(t.bg, t.run, rp.msg, &to, msgReply, state, body)
	if err != nil {
		t.r.log.Warn("publishing on the change failed", "run", t.run.Number, "err", err)
		return
	}
	rp.msg = &m
	if c.URL != "" {
		rp.url = c.URL
	}
}

// finishVCS edits the turn's placeholders on the change into body: the
// first one gets it, the others point to it.
func (t *turn) finishVCS(state, body string) {
	first := ""
	for i, rp := range t.vcsReplies {
		b := body
		if i > 0 && first != "" {
			b = "See " + first
		}
		t.writeVCS(rp, state, b)
		if i == 0 {
			first = rp.url
		}
	}
}

// openChange opens the run's pull or merge request after its first push.
// Its title and description are the ones the agent wrote for it (d, when
// ok); otherwise strategy.title, and a body that links the YouTrack issue
// and carries the turn's answer.
func (t *turn) openChange(answer string, d changeDraft, ok bool) error {
	r := t.r
	if r.vcs == nil || t.run.PRNumber > 0 {
		return nil
	}
	key, summary, link := t.vars["issue.key"], t.vars["issue.summary"], t.vars["issue.url"]
	if link == "" || link == "none" {
		link = r.issueURL(t.run)
	}
	title := t.changeTitle()
	body := "YouTrack issue: [" + key + "](" + link + ")"
	if a := strings.TrimSpace(answer); a != "" {
		body += "\n\n" + trimText(a, 30000)
	}
	if ok {
		if d.Title != "" {
			title = d.Title
		}
		body = trimText(d.Description, 30000)
	}
	o := vcs.OpenOptions{
		Branch: t.w.wt.Branch, Base: t.w.wt.Base, Title: title,
		Body: r.decorateVCS(t.run.Number, body), Draft: r.p.Strategy.Draft,
		Squash: r.p.Strategy.Squash, DeleteBranch: r.p.Strategy.DeleteBranch,
	}
	if r.vcs.Kind() == vcs.KindGitHubIssuePR {
		o.IssueTitle = strings.TrimSpace(key + " " + summary) // the mirror of the YouTrack issue
		ib := "Mirrors the YouTrack issue [" + key + "](" + link + ")."
		if d := strings.TrimSpace(t.vars["issue.description"]); d != "" && d != "none" {
			ib += "\n\n" + trimText(d, 30000)
		}
		o.IssueBody = r.decorateVCS(t.run.Number, ib)
	}
	ctx, cancel := context.WithTimeout(t.bg, gitTimeout)
	defer cancel()
	ch, err := r.vcs.Open(ctx, o)
	if err != nil {
		data := map[string]any{"error": err.Error()}
		if ch.Issue > 0 {
			data["issue"] = ch.IssueURL
		}
		r.event(t.bg, t.run.ID, "vcs.open_failed", data)
		return fmt.Errorf("opening the %s failed: %w", strings.ToLower(r.changeNoun()), err)
	}
	saved, err := r.st.ModifyRun(t.bg, t.run.ID, func(run *store.Run) error {
		run.VCSKind, run.VCSRepo = string(r.vcs.Kind()), r.vcsRepo
		run.PRNumber, run.PRURL, run.GHIssueNumber = ch.Number, ch.URL, ch.Issue
		return nil
	})
	if err != nil {
		return fmt.Errorf("saving the %s: %w", strings.ToLower(r.changeNoun()), err)
	}
	t.run = saved
	t.opened = ch.URL
	t.rememberChange(ch)
	t.vars["pr.url"] = ch.URL
	r.log.Info("opened the change", "run", saved.Number, "issue", saved.IssueKey, "url", ch.URL)
	data := map[string]any{"url": ch.URL, "number": ch.Number, "draft": ch.Draft}
	if ch.Issue > 0 {
		data["issue"] = ch.IssueURL
	}
	r.event(t.bg, saved.ID, "vcs.opened", data)
	return nil
}

// changeTitle renders strategy.title, with {{commit.subject}} the subject
// of the branch's first commit. A title that comes out empty, or a
// template that fails, gives the default.
func (t *turn) changeTitle() string {
	r := t.r
	v := maps.Clone(t.vars)
	if strings.Contains(r.p.Strategy.Title, "commit.subject") {
		ctx, cancel := context.WithTimeout(t.bg, gitTimeout)
		subject, err := workspace.FirstSubject(ctx, r.host, t.w.wt)
		cancel()
		if err != nil {
			r.log.Warn("reading the branch's first commit failed", "run", t.run.Number, "err", err)
		}
		v["commit.subject"] = subject
	}
	title, err := text.Render(r.p.Strategy.Title, v)
	if err != nil {
		r.log.Warn("strategy.title failed", "run", t.run.Number, "err", err)
	}
	if title = oneLineTitle(title); title == "" {
		title, _ = text.Render(spec.DefaultTitle, v)
	}
	return oneLineTitle(title)
}

// oneLineTitle is s on one line, without spaces at its ends.
func oneLineTitle(s string) string { return strings.Join(strings.Fields(s), " ") }

// changeLine names the change the turn opened, for a YouTrack comment.
func (t *turn) changeLine() string {
	if t.opened == "" {
		return ""
	}
	return "\n\n" + t.r.changeNoun() + ": " + t.opened
}
