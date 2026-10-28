package vcs

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// States of the CI of a commit or of one of its jobs.
const (
	CINone    = "none"    // nothing ran for the commit, yet or at all
	CIPending = "pending" // something still runs or waits to
	CISuccess = "success" // everything finished and nothing failed
	CIFailure = "failure" // everything finished and something failed
)

// CIJob is one check run, commit status or pipeline job of a commit.
type CIJob struct {
	ID    int64 // for Log; 0 when the host keeps no log of it
	Name  string
	State string // one of the CI states
	URL   string
}

// CIRun is what the CI did for a commit.
type CIRun struct {
	State string // one of the CI states
	URL   string // the page of the commit's checks or pipeline
	Jobs  []CIJob
}

// Failed are the jobs that failed.
func (r CIRun) Failed() []CIJob {
	var out []CIJob
	for _, j := range r.Jobs {
		if j.State == CIFailure {
			out = append(out, j)
		}
	}
	return out
}

// CI reads what a code host's CI did for a commit. The GitHub and GitLab
// strategies are one; NewCI makes one for the branch strategy.
type CI interface {
	// Checks is the CI of a commit.
	Checks(ctx context.Context, sha string) (CIRun, error)
	// Log is the end of a job's log, at most limit bytes.
	Log(ctx context.Context, job CIJob, limit int) (string, error)
}

// NewCI returns the CI of a GitHub or GitLab repository.
func NewCI(repo Repo, auth Auth) (CI, error) {
	switch repo.Kind {
	case KindGitHubPR, KindGitHubIssuePR:
		s, err := New(KindGitHubPR, repo, auth)
		if err != nil {
			return nil, err
		}
		return s.(CI), nil
	case KindGitLabMR:
		s, err := New(KindGitLabMR, repo, auth)
		if err != nil {
			return nil, err
		}
		return s.(CI), nil
	}
	return nil, fmt.Errorf("vcs: %s is neither a GitHub nor a GitLab host", repo.Host)
}

// overall is the state of a commit from its jobs': pending while any is,
// then failure when any failed, success when some ran, else none.
func overall(jobs []CIJob) string {
	state := CINone
	for _, j := range jobs {
		switch j.State {
		case CIPending:
			return CIPending
		case CIFailure:
			state = CIFailure
		case CISuccess:
			if state == CINone {
				state = CISuccess
			}
		}
	}
	return state
}

// tail is the end of s, at most limit bytes, from a line start when it
// can.
func tail(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	s = s[len(s)-limit:]
	for i := 0; i < len(s) && i < 512; i++ {
		if s[i] == '\n' {
			return s[i+1:]
		}
	}
	return s
}

// rawGet reads a text answer, such as a job's log.
func (c *client) rawGet(ctx context.Context, path string) (string, error) {
	resp, err := c.send(ctx, http.MethodGet, c.url(path, nil), nil, "")
	if err != nil {
		return "", err
	}
	return string(resp.Body), nil
}

type ghCheckRuns struct {
	CheckRuns []struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
		App        struct {
			Slug string `json:"slug"`
		} `json:"app"`
	} `json:"check_runs"`
}

type ghStatus struct {
	Statuses []struct {
		Context   string `json:"context"`
		State     string `json:"state"`
		TargetURL string `json:"target_url"`
	} `json:"statuses"`
}

// Checks reads a commit's check runs and commit statuses.
func (g *github) Checks(ctx context.Context, sha string) (CIRun, error) {
	q := url.Values{"per_page": {"100"}}
	var runs ghCheckRuns
	if err := g.get(ctx, g.path("/commits/"+url.PathEscape(sha)+"/check-runs"), q, &runs); err != nil {
		return CIRun{}, err
	}
	var st ghStatus
	if err := g.get(ctx, g.path("/commits/"+url.PathEscape(sha)+"/status"), q, &st); err != nil {
		return CIRun{}, err
	}
	var jobs []CIJob
	for _, r := range runs.CheckRuns {
		j := CIJob{Name: r.Name, URL: r.HTMLURL, State: CISuccess}
		if r.App.Slug == "github-actions" {
			j.ID = r.ID // an Actions job: its log is readable
		}
		switch {
		case r.Status != "completed":
			j.State = CIPending
		case r.Conclusion == "failure" || r.Conclusion == "timed_out" || r.Conclusion == "action_required":
			j.State = CIFailure
		}
		jobs = append(jobs, j)
	}
	for _, s := range st.Statuses {
		j := CIJob{Name: s.Context, URL: s.TargetURL, State: CISuccess}
		switch s.State {
		case "pending":
			j.State = CIPending
		case "failure", "error":
			j.State = CIFailure
		}
		jobs = append(jobs, j)
	}
	web := g.c.web + "/" + g.repo.Owner + "/" + g.repo.Name + "/commit/" + sha + "/checks"
	return CIRun{State: overall(jobs), URL: web, Jobs: jobs}, nil
}

// Log reads the end of a GitHub Actions job's log.
func (g *github) Log(ctx context.Context, job CIJob, limit int) (string, error) {
	if job.ID == 0 {
		return "", nil
	}
	s, err := g.c.rawGet(ctx, g.path("/actions/jobs/"+strconv.FormatInt(job.ID, 10)+"/logs"))
	return tail(s, limit), err
}

type glPipeline struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	WebURL string `json:"web_url"`
}

type glJob struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	WebURL       string `json:"web_url"`
	AllowFailure bool   `json:"allow_failure"`
}

// Checks reads the latest pipeline of a commit and its failed jobs.
func (l *gitlab) Checks(ctx context.Context, sha string) (CIRun, error) {
	var ps []glPipeline
	q := url.Values{"sha": {sha}, "order_by": {"id"}, "sort": {"desc"}, "per_page": {"1"}}
	if err := l.get(ctx, l.path("/pipelines"), q, &ps); err != nil {
		return CIRun{}, err
	}
	if len(ps) == 0 {
		return CIRun{State: CINone}, nil
	}
	p := ps[0]
	run := CIRun{URL: p.WebURL}
	switch p.Status {
	case "success", "manual":
		run.State = CISuccess // manual: what is left waits for a person
	case "failed":
		run.State = CIFailure
	case "canceled", "skipped":
		run.State = CINone
	default: // created, waiting_for_resource, preparing, pending, running, scheduled
		run.State = CIPending
	}
	if run.State != CIFailure {
		return run, nil
	}
	var jobs []glJob
	jq := url.Values{"scope[]": {"failed"}, "per_page": {"100"}}
	if err := l.get(ctx, l.path("/pipelines/"+strconv.FormatInt(p.ID, 10)+"/jobs"), jq, &jobs); err != nil {
		return CIRun{}, err
	}
	for _, j := range jobs {
		if j.AllowFailure {
			continue
		}
		run.Jobs = append(run.Jobs, CIJob{ID: j.ID, Name: j.Name, State: CIFailure, URL: j.WebURL})
	}
	return run, nil
}

// Log reads the end of a GitLab job's trace.
func (l *gitlab) Log(ctx context.Context, job CIJob, limit int) (string, error) {
	if job.ID == 0 {
		return "", nil
	}
	s, err := l.c.rawGet(ctx, l.path("/jobs/"+strconv.FormatInt(job.ID, 10)+"/trace"))
	return tail(s, limit), err
}
