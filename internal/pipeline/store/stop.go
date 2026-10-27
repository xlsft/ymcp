package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// The stop request of a runner is kept as a cursor of the pipeline, so it
// needs no table of its own.
const (
	stopSource = "runner"
	stopKey    = "stop"
)

// RequestStop asks the runner of the pipeline to stop. It is a flag in the
// database, so it works where signals do not (Windows) and from another
// process; the runner polls it with StopRequested. Asking again moves the
// request to now.
func (s *Store) RequestStop(ctx context.Context, pipeline string) error {
	return s.SetCursor(ctx, pipeline, stopSource, stopKey, strconv.FormatInt(ms(s.stamp()), 10))
}

// StopRequested reports whether a stop was requested at or after since.
// A runner passes the time it started, so that a request made while no
// runner was there does not stop the next one.
func (s *Store) StopRequested(ctx context.Context, pipeline string, since time.Time) (bool, error) {
	v, err := s.Cursor(ctx, pipeline, stopSource, stopKey)
	if err != nil || v == "" {
		return false, err
	}
	at, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return false, fmt.Errorf("reading the stop request of %s: %w", pipeline, err)
	}
	return !fromMS(at).Before(since.Truncate(time.Millisecond)), nil
}

// ClearStop withdraws the stop request of the pipeline.
func (s *Store) ClearStop(ctx context.Context, pipeline string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM cursors WHERE pipeline = ? AND source = ? AND key = ?`,
		pipeline, stopSource, stopKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("clearing the stop request of %s: %w", pipeline, err)
	}
	return nil
}
