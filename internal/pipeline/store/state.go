package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Cursor returns a polling cursor, or "" when none is stored.
func (s *Store) Cursor(ctx context.Context, pipeline, source, key string) (string, error) {
	var v string
	err := s.q.QueryRowContext(ctx, `SELECT value FROM cursors WHERE pipeline = ? AND source = ? AND key = ?`,
		pipeline, source, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading cursor %s/%s/%s: %w", pipeline, source, key, err)
	}
	return v, nil
}

// SetCursor stores a polling cursor.
func (s *Store) SetCursor(ctx context.Context, pipeline, source, key, value string) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO cursors (pipeline, source, key, value) VALUES (?, ?, ?, ?)
		ON CONFLICT (pipeline, source, key) DO UPDATE SET value = excluded.value`, pipeline, source, key, value)
	if err != nil {
		return fmt.Errorf("saving cursor %s/%s/%s: %w", pipeline, source, key, err)
	}
	return nil
}

// Seen returns the update stamp recorded for an item (a comment, say) by
// MarkSeen, and whether it was seen at all.
func (s *Store) Seen(ctx context.Context, source, externalID string) (updatedAt string, ok bool, err error) {
	err = s.q.QueryRowContext(ctx, `SELECT updated_at FROM seen_items WHERE source = ? AND external_id = ?`,
		source, externalID).Scan(&updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading seen item %s/%s: %w", source, externalID, err)
	}
	return updatedAt, true, nil
}

// MarkSeen records an item and its update stamp, replacing an earlier one.
func (s *Store) MarkSeen(ctx context.Context, source, externalID, updatedAt string) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO seen_items (source, external_id, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (source, external_id) DO UPDATE SET updated_at = excluded.updated_at`, source, externalID, updatedAt)
	if err != nil {
		return fmt.Errorf("marking %s/%s seen: %w", source, externalID, err)
	}
	return nil
}

// SeenIssue reports whether the pipeline has met the issue before.
func (s *Store) SeenIssue(ctx context.Context, pipeline, issueID string) (bool, error) {
	var one int
	err := s.q.QueryRowContext(ctx, `SELECT 1 FROM seen_issues WHERE pipeline = ? AND issue_id = ?`, pipeline, issueID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading seen issue %s/%s: %w", pipeline, issueID, err)
	}
	return true, nil
}

// MarkIssueSeen records that the pipeline has met the issue. Doing it again
// keeps the first time.
func (s *Store) MarkIssueSeen(ctx context.Context, pipeline, issueID string) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO seen_issues (pipeline, issue_id, first_seen_at) VALUES (?, ?, ?)
		ON CONFLICT (pipeline, issue_id) DO NOTHING`, pipeline, issueID, ms(s.stamp()))
	if err != nil {
		return fmt.Errorf("marking issue %s/%s seen: %w", pipeline, issueID, err)
	}
	return nil
}

// RunState returns a value kept for a run by SetRunState, and whether there
// is one.
func (s *Store) RunState(ctx context.Context, runID int64, key string) (string, bool, error) {
	var v string
	err := s.q.QueryRowContext(ctx, `SELECT value FROM run_state WHERE run_id = ? AND key = ?`, runID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading state %q of run %d: %w", key, runID, err)
	}
	return v, true, nil
}

// SetRunState keeps a value for a run under key, replacing an earlier one.
// An empty value deletes the key. The values go away with the run.
func (s *Store) SetRunState(ctx context.Context, runID int64, key, value string) error {
	var err error
	if value == "" {
		_, err = s.q.ExecContext(ctx, `DELETE FROM run_state WHERE run_id = ? AND key = ?`, runID, key)
	} else {
		_, err = s.q.ExecContext(ctx, `INSERT INTO run_state (run_id, key, value) VALUES (?, ?, ?)
			ON CONFLICT (run_id, key) DO UPDATE SET value = excluded.value`, runID, key, value)
	}
	if err != nil {
		return fmt.Errorf("saving state %q of run %d: %w", key, runID, err)
	}
	return nil
}

// Event is an entry of a run's timeline, the feed of the TUI and the logs.
type Event struct {
	ID    int64
	RunID int64
	At    time.Time
	Kind  string
	Data  json.RawMessage
}

// AddEvent appends an event to a run's timeline; data is stored as JSON.
func (s *Store) AddEvent(ctx context.Context, runID int64, kind string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encoding event %s: %w", kind, err)
	}
	if _, err := s.q.ExecContext(ctx, `INSERT INTO events (run_id, at, kind, data) VALUES (?, ?, ?, ?)`,
		runID, ms(s.stamp()), kind, string(b)); err != nil {
		return fmt.Errorf("adding event %s: %w", kind, err)
	}
	return nil
}

// Events pages through a run's events in order: those with an id above
// afterID, at most limit of them (all when limit is not positive). Pass the
// last id seen to get the next page.
func (s *Store) Events(ctx context.Context, runID int64, afterID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	rows, err := s.q.QueryContext(ctx, `SELECT id, run_id, at, kind, data FROM events
		WHERE run_id = ? AND id > ? ORDER BY id LIMIT ?`, runID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		var data string
		if err := rows.Scan(&e.ID, &e.RunID, &at, &e.Kind, &data); err != nil {
			return nil, fmt.Errorf("listing events: %w", err)
		}
		e.At = fromMS(at)
		e.Data = json.RawMessage(data)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	return out, nil
}
