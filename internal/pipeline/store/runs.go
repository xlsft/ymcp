package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RunStatus is where a run is in its life.
type RunStatus string

// Run statuses. Done, Closed, Canceled and Failed are final: they free the
// issue for a new run.
const (
	StatusQueued     RunStatus = "queued"
	StatusWorking    RunStatus = "working"
	StatusNeedsInput RunStatus = "needs_input"
	StatusReview     RunStatus = "review"
	StatusDone       RunStatus = "done"
	StatusClosed     RunStatus = "closed"
	StatusCanceled   RunStatus = "canceled"
	StatusFailed     RunStatus = "failed"
)

// Final reports whether the status ends the run.
func (s RunStatus) Final() bool {
	switch s {
	case StatusDone, StatusClosed, StatusCanceled, StatusFailed:
		return true
	}
	return false
}

// Run is one agent's work on one issue.
type Run struct {
	ID               int64
	Pipeline         string
	Number           int // per pipeline, starting at 1
	Profile          string
	IssueID          string
	IssueKey         string
	Stage            string
	Status           RunStatus
	NeedsInputReason string
	Host             string
	Worktree         string
	Branch           string
	Base             string
	Agent            string
	AgentSession     string
	VCSKind          string
	VCSRepo          string
	PRNumber         int
	PRURL            string
	GHIssueNumber    int // the GitHub issue of the issue+PR strategy
	CreatedAt        time.Time
	UpdatedAt        time.Time
	FinishedAt       *time.Time
}

const runColumns = `id, pipeline, number, profile, issue_id, issue_key, stage, status, needs_input_reason,
	host, worktree, branch, base, agent, agent_session, vcs_kind, vcs_repo, pr_number, pr_url,
	gh_issue_number, created_at, updated_at, finished_at`

type scanner interface{ Scan(dest ...any) error }

func scanRun(sc scanner) (Run, error) {
	var (
		r                Run
		status           string
		created, updated int64
		finished         sql.NullInt64
	)
	err := sc.Scan(&r.ID, &r.Pipeline, &r.Number, &r.Profile, &r.IssueID, &r.IssueKey, &r.Stage, &status,
		&r.NeedsInputReason, &r.Host, &r.Worktree, &r.Branch, &r.Base, &r.Agent, &r.AgentSession,
		&r.VCSKind, &r.VCSRepo, &r.PRNumber, &r.PRURL, &r.GHIssueNumber, &created, &updated, &finished)
	if err != nil {
		return Run{}, err
	}
	r.Status = RunStatus(status)
	r.CreatedAt = fromMS(created)
	r.UpdatedAt = fromMS(updated)
	r.FinishedAt = fromNullMS(finished)
	return r, nil
}

// CreateRun starts a run in the queued status. Its number is the next one of
// the pipeline, allocated atomically; it is ErrActiveRun when the issue of
// the profile already has a run that is not final (and then no number is
// used up).
func (s *Store) CreateRun(ctx context.Context, pipeline, profile, issueID, issueKey string) (Run, error) {
	var run Run
	err := s.Tx(ctx, func(tx *Store) error {
		var active int
		if err := tx.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE profile = ? AND issue_id = ?
			AND status NOT IN ('done', 'closed', 'canceled', 'failed')`, profile, issueID).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return ErrActiveRun
		}
		var number int
		if err := tx.q.QueryRowContext(ctx, `INSERT INTO counters (pipeline, next_run) VALUES (?, 2)
			ON CONFLICT (pipeline) DO UPDATE SET next_run = next_run + 1
			RETURNING next_run - 1`, pipeline).Scan(&number); err != nil {
			return err
		}
		now := ms(tx.stamp())
		res, err := tx.q.ExecContext(ctx, `INSERT INTO runs (pipeline, number, profile, issue_id, issue_key, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, pipeline, number, profile, issueID, issueKey, string(StatusQueued), now, now)
		if err != nil {
			if isUnique(err) {
				return ErrActiveRun
			}
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		run, err = tx.Run(ctx, id)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrActiveRun) {
			return Run{}, fmt.Errorf("%w: %s in profile %s", ErrActiveRun, issueID, profile)
		}
		return Run{}, fmt.Errorf("creating a run: %w", err)
	}
	return run, nil
}

// Run returns the run with the given id, or ErrNotFound.
func (s *Store) Run(ctx context.Context, id int64) (Run, error) {
	r, err := scanRun(s.q.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("reading run %d: %w", id, err)
	}
	return r, nil
}

// RunByNumber returns a pipeline's run by its number, or ErrNotFound.
func (s *Store) RunByNumber(ctx context.Context, pipeline string, number int) (Run, error) {
	r, err := scanRun(s.q.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE pipeline = ? AND number = ?`, pipeline, number))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("reading run %s #%d: %w", pipeline, number, err)
	}
	return r, nil
}

// Runs lists the runs of a pipeline by number, all of them or only those
// that are not final. An empty pipeline lists every pipeline's runs.
func (s *Store) Runs(ctx context.Context, pipeline string, active bool) ([]Run, error) {
	q := `SELECT ` + runColumns + ` FROM runs WHERE (? = '' OR pipeline = ?)`
	if active {
		q += ` AND status NOT IN ('done', 'closed', 'canceled', 'failed')`
	}
	q += ` ORDER BY pipeline, number`
	rows, err := s.q.QueryContext(ctx, q, pipeline, pipeline)
	if err != nil {
		return nil, fmt.Errorf("listing runs: %w", err)
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("listing runs: %w", err)
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing runs: %w", err)
	}
	return runs, nil
}

// UpdateRun saves every field of r except the id, pipeline, number, profile,
// issue and creation time, and stamps UpdatedAt. A run that gets a final
// status without a FinishedAt is finished now. It is ErrNotFound for an
// unknown run and ErrActiveRun when the update would revive a run of an
// issue that has another active one.
func (s *Store) UpdateRun(ctx context.Context, r Run) error {
	now := s.stamp()
	finished := r.FinishedAt
	if r.Status.Final() && finished == nil {
		finished = &now
	}
	res, err := s.q.ExecContext(ctx, `UPDATE runs SET issue_key = ?, stage = ?, status = ?, needs_input_reason = ?,
		host = ?, worktree = ?, branch = ?, base = ?, agent = ?, agent_session = ?, vcs_kind = ?, vcs_repo = ?,
		pr_number = ?, pr_url = ?, gh_issue_number = ?, updated_at = ?, finished_at = ? WHERE id = ?`,
		r.IssueKey, r.Stage, string(r.Status), r.NeedsInputReason, r.Host, r.Worktree, r.Branch, r.Base,
		r.Agent, r.AgentSession, r.VCSKind, r.VCSRepo, r.PRNumber, r.PRURL, r.GHIssueNumber, ms(now),
		nullMS(finished), r.ID)
	if err != nil {
		if isUnique(err) {
			return fmt.Errorf("%w: run %d", ErrActiveRun, r.ID)
		}
		return fmt.Errorf("updating run %d: %w", r.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
