package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xlsft/ymcp/internal/proc"
)

// defaultStale is how long a runner may go without a heartbeat when
// AcquireRunner is not told.
const defaultStale = 30 * time.Second

// processAlive tells whether a pid of this machine is alive; a test seam.
var processAlive = proc.Alive

// HolderGone reports whether the holder of a runner lock, pid on
// holderHost, is known to be dead: it ran on host, this machine, and no
// process has its pid any more. A holder on another machine is never known
// to be gone; only its heartbeat tells.
func HolderGone(pid int, holderHost, host string) bool {
	return host != "" && holderHost == host && !processAlive(pid)
}

// AcquireRunner takes the runner lock of a pipeline for the process pid on
// host, this machine. It is ErrRunnerBusy when another runner holds it and
// has sent a heartbeat within staleAfter (30 seconds when not positive); a
// lock whose holder has gone quiet for longer, or whose holder ran on this
// machine and is dead (see HolderGone), is taken over, and the same pid on
// the same host may take its own lock again.
func (s *Store) AcquireRunner(ctx context.Context, pipeline string, pid int, host string, staleAfter time.Duration) error {
	if staleAfter <= 0 {
		staleAfter = defaultStale
	}
	return s.Tx(ctx, func(tx *Store) error {
		now := tx.stamp()
		var holderPID int
		var holderHost string
		var beat int64
		err := tx.q.QueryRowContext(ctx, `SELECT pid, host, heartbeat_at FROM runners WHERE pipeline = ?`, pipeline).
			Scan(&holderPID, &holderHost, &beat)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("reading the runner of %s: %w", pipeline, err)
		default:
			ours := holderPID == pid && holderHost == host
			if !ours && now.Sub(fromMS(beat)) <= staleAfter && !HolderGone(holderPID, holderHost, host) {
				return fmt.Errorf("%w: %s is held by pid %d on %s", ErrRunnerBusy, pipeline, holderPID, holderHost)
			}
		}
		_, err = tx.q.ExecContext(ctx, `INSERT INTO runners (pipeline, pid, host, started_at, heartbeat_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (pipeline) DO UPDATE SET pid = excluded.pid, host = excluded.host,
			started_at = excluded.started_at, heartbeat_at = excluded.heartbeat_at`,
			pipeline, pid, host, ms(now), ms(now))
		if err != nil {
			return fmt.Errorf("taking the runner of %s: %w", pipeline, err)
		}
		return nil
	})
}

// Heartbeat tells the others the runner is alive. It is ErrRunnerLost when
// pid no longer holds the lock, which means the runner must stop.
func (s *Store) Heartbeat(ctx context.Context, pipeline string, pid int) error {
	res, err := s.q.ExecContext(ctx, `UPDATE runners SET heartbeat_at = ? WHERE pipeline = ? AND pid = ?`,
		ms(s.stamp()), pipeline, pid)
	if err != nil {
		return fmt.Errorf("heartbeat of %s: %w", pipeline, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRunnerLost
	}
	return nil
}

// ReleaseRunner gives the lock up. It does nothing when pid does not hold it.
func (s *Store) ReleaseRunner(ctx context.Context, pipeline string, pid int) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM runners WHERE pipeline = ? AND pid = ?`, pipeline, pid); err != nil {
		return fmt.Errorf("releasing the runner of %s: %w", pipeline, err)
	}
	return nil
}

// Runner reports who holds the pipeline's runner lock and when it last sent
// a heartbeat; ok is false when nobody does. A holder that has gone quiet
// is still reported: judging staleness is up to the caller.
func (s *Store) Runner(ctx context.Context, pipeline string) (pid int, host string, heartbeat time.Time, ok bool, err error) {
	var beat int64
	err = s.q.QueryRowContext(ctx, `SELECT pid, host, heartbeat_at FROM runners WHERE pipeline = ?`, pipeline).
		Scan(&pid, &host, &beat)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", time.Time{}, false, nil
	}
	if err != nil {
		return 0, "", time.Time{}, false, fmt.Errorf("reading the runner of %s: %w", pipeline, err)
	}
	return pid, host, fromMS(beat), true, nil
}
