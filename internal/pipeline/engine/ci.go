package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
)

// sourceCI is the inbox source of a failed CI.
const sourceCI = "ci"

// stateCI is the run state key of the CI watch of the run's last push
// (ciWatch as JSON).
const stateCI = "ci"

// Bounds of the CI watch.
const (
	// ciNoneFor is how long a pushed commit may go without any CI before
	// the runner stops looking: the repository may have none.
	ciNoneFor  = 10 * time.Minute
	ciLogBytes = 6000  // the end of each failed job's log
	ciLogJobs  = 3     // the jobs whose logs the agent gets
	ciBodyMax  = 20000 // the whole message
)

// ciWatch is what the runner knows of the CI of a run's last push.
type ciWatch struct {
	SHA     string    `json:"sha"`
	Since   time.Time `json:"since"`
	Running bool      `json:"running,omitempty"` // said that it runs
	Done    bool      `json:"done,omitempty"`    // nothing more to watch
	Fixes   int       `json:"fixes,omitempty"`   // failures in a row given to the agent
}

// newCI is the real Options.NewCI.
func newCI(ctx context.Context, originURL string) (vcs.CI, error) {
	repo, err := vcs.ParseRemote(originURL)
	if err != nil {
		return nil, err
	}
	auth, err := vcs.TokenFor(ctx, repo)
	if err != nil {
		return nil, err
	}
	return vcs.NewCI(repo, auth)
}

// connectCI is the CI the runner watches: the strategy's own, or for the
// branch strategy one found from origin. Without one the runner logs why
// and runs on.
func connectCI(ctx context.Context, p spec.Pipeline, host workspace.Host, strategy vcs.Strategy, opts Options, logger *slog.Logger) vcs.CI {
	if !p.CI.Watched() {
		return nil
	}
	if ci, ok := strategy.(vcs.CI); ok {
		return ci
	}
	origin, err := originURL(ctx, host, p.Workspace.Dir)
	if err == nil {
		var ci vcs.CI
		if ci, err = opts.NewCI(ctx, origin); err == nil && ci != nil {
			return ci
		}
	}
	logger.Info("not watching CI", "reason", err)
	return nil
}

// ciPushed starts watching the CI of a commit the runner pushed. The count
// of fixes in a row carries over when the turn only answered the CI; a
// turn a human asked for starts it again.
func (r *Runner) ciPushed(ctx context.Context, run store.Run, sha string, fix bool) {
	if r.ci == nil || sha == "" {
		return
	}
	w, _ := r.ciWatch(ctx, run)
	if w.SHA == sha && !w.Done {
		return
	}
	fixes := 0
	if fix {
		fixes = w.Fixes
	}
	r.saveCIWatch(ctx, run, ciWatch{SHA: sha, Since: r.now(), Fixes: fixes})
}

// onlyCI reports whether every item of the turn is a failed CI.
func (t *turn) onlyCI() bool {
	for _, it := range t.items {
		if it.Source != sourceCI {
			return false
		}
	}
	return len(t.items) > 0
}

func (r *Runner) ciWatch(ctx context.Context, run store.Run) (ciWatch, bool) {
	var w ciWatch
	v, ok, err := r.st.RunState(ctx, run.ID, stateCI)
	if err != nil || !ok || v == "" {
		return w, false
	}
	return w, json.Unmarshal([]byte(v), &w) == nil
}

func (r *Runner) saveCIWatch(ctx context.Context, run store.Run, w ciWatch) {
	b, _ := json.Marshal(w)
	if err := r.st.SetRunState(context.WithoutCancel(ctx), run.ID, stateCI, string(b)); err != nil {
		r.log.Warn("saving the CI watch failed", "run", run.Number, "err", err)
	}
}

// pollCI reads the CI of the last push of each run in review. A failure
// gives the agent a turn with the failed jobs, up to ci.fixes in a row;
// then the run waits for a human.
func (r *Runner) pollCI(ctx context.Context) {
	if r.ci == nil {
		return
	}
	for _, wk := range r.liveWorkers() {
		if ctx.Err() != nil {
			return
		}
		run, err := r.st.Run(ctx, wk.id)
		if err != nil || run.Status != store.StatusReview {
			continue // a working run pushes again; one that waits waits for a human
		}
		w, ok := r.ciWatch(ctx, run)
		if !ok || w.Done || w.SHA == "" {
			continue
		}
		res, err := r.ci.Checks(ctx, w.SHA)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Warn("reading the CI failed", "run", run.Number, "sha", w.SHA, "err", err)
			}
			continue
		}
		switch res.State {
		case vcs.CINone:
			if r.now().Sub(w.Since) >= ciNoneFor {
				w.Done = true
				r.saveCIWatch(ctx, run, w)
			}
		case vcs.CIPending:
			if !w.Running {
				w.Running = true
				r.saveCIWatch(ctx, run, w)
				r.event(ctx, run.ID, "ci.running", map[string]string{"sha": w.SHA, "url": res.URL})
			}
		case vcs.CISuccess:
			w.Done, w.Fixes = true, 0
			r.saveCIWatch(ctx, run, w)
			r.log.Info("the CI passed", "run", run.Number, "sha", w.SHA)
			r.event(ctx, run.ID, "ci.passed", map[string]string{"sha": w.SHA, "url": res.URL})
		case vcs.CIFailure:
			w.Done = true
			r.ciFailed(ctx, wk, run, w, res)
		}
	}
}

// ciFailed hands a failed CI to the agent, or to a human once the agent
// has had its turns.
func (r *Runner) ciFailed(ctx context.Context, wk *worker, run store.Run, w ciWatch, res vcs.CIRun) {
	var names []string
	for _, j := range res.Failed() {
		names = append(names, j.Name)
	}
	r.log.Info("the CI failed", "run", run.Number, "sha", w.SHA, "jobs", names)
	r.event(ctx, run.ID, "ci.failed", map[string]any{"sha": w.SHA, "url": res.URL, "jobs": names})
	if w.Fixes >= r.p.CI.MaxFixes() {
		r.saveCIWatch(ctx, run, w)
		reason := fmt.Sprintf("The CI failed %d times in a row.", w.Fixes+1)
		body := reason + " " + ciLink(res) + "\n\nReply with a comment to have the agent try again, or fix it yourself."
		r.toNeedsInput(ctx, run, reason, body, nil, true)
		return
	}
	w.Fixes++
	r.saveCIWatch(ctx, run, w)
	body := r.ciReport(ctx, w.SHA, res)
	added, err := r.st.AddInbox(ctx, store.InboxItem{RunID: run.ID, Source: sourceCI, ExternalID: "ci-" + w.SHA, Author: "CI", Body: body, URL: res.URL})
	if err != nil {
		r.log.Warn("queueing the failed CI failed", "run", run.Number, "err", err)
		return
	}
	if added {
		r.event(ctx, run.ID, "inbox", map[string]string{"source": sourceCI, "author": "CI", "url": res.URL, "text": trimText(body, 500)})
		wk.wakeUp()
	}
}

// ciLink names the CI's page in markdown.
func ciLink(res vcs.CIRun) string {
	if res.URL == "" {
		return ""
	}
	return "[The pipeline](" + res.URL + ")"
}

// ciReport is what the agent gets of a failed CI: the failed jobs and the
// end of their logs.
func (r *Runner) ciReport(ctx context.Context, sha string, res vcs.CIRun) string {
	var b strings.Builder
	short := sha
	if len(short) > 10 {
		short = short[:10]
	}
	fmt.Fprintf(&b, "The CI failed on %s, the commit ymcp pushed last.", short)
	if res.URL != "" {
		b.WriteString(" " + res.URL)
	}
	b.WriteString("\n")
	for i, j := range res.Failed() {
		fmt.Fprintf(&b, "\n### %s", j.Name)
		if j.URL != "" {
			b.WriteString(" (" + j.URL + ")")
		}
		b.WriteString("\n")
		if i >= ciLogJobs || j.ID == 0 {
			continue
		}
		log, err := r.ci.Log(ctx, j, ciLogBytes*4)
		if err != nil {
			r.log.Warn("reading a job's log failed", "job", j.Name, "err", err)
			continue
		}
		if log = cleanLog(log, ciLogBytes); log != "" {
			b.WriteString("```\n" + log + "\n```\n")
		}
	}
	b.WriteString("\nFind the cause and fix it so the CI passes; the logs are on the host too (gh, glab). " +
		"If the failure is not caused by the branch (a flaky test, the infrastructure), say so and change nothing.")
	return trimText(b.String(), ciBodyMax)
}

// gitlabSection matches GitLab's collapsible section markers in a trace.
var gitlabSection = regexp.MustCompile(`section_(start|end):\d+:[^\r\n]*\r?`)

// cleanLog is the end of a job's log as text: colours, GitLab's section
// markers and lines a carriage return overwrote taken out, at most limit
// bytes.
func cleanLog(s string, limit int) string {
	s = gitlabSection.ReplaceAllString(s, "")
	s = strings.ReplaceAll(ansi.Strip(s), "\x1b", "") // a lone ESC Strip leaves after broken UTF-8
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if k := strings.LastIndexByte(l, '\r'); k >= 0 {
			lines[i] = l[k+1:]
		}
	}
	s = strings.TrimSpace(strings.Join(lines, "\n"))
	if len(s) > limit {
		s = s[len(s)-limit:]
		if k := strings.IndexByte(s, '\n'); k >= 0 && k < 512 {
			s = s[k+1:]
		}
	}
	return s
}
