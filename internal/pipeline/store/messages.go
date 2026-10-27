package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Message is something the agent published on a platform; State is
// thinking, done or failed. It is the fallback way to recognise the agent's
// own messages when the marker was lost.
type Message struct {
	ID         int64
	RunID      int64
	Source     string
	ExternalID string
	ThreadID   string
	Kind       string
	State      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// AddMessage records a message and returns it with its id and times set.
func (s *Store) AddMessage(ctx context.Context, m Message) (Message, error) {
	now := s.stamp()
	m.CreatedAt, m.UpdatedAt = now, now
	res, err := s.q.ExecContext(ctx, `INSERT INTO messages (run_id, source, external_id, thread_id, kind, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, m.RunID, m.Source, m.ExternalID, m.ThreadID, m.Kind, m.State, ms(now), ms(now))
	if err != nil {
		return Message{}, fmt.Errorf("adding a message: %w", err)
	}
	if m.ID, err = res.LastInsertId(); err != nil {
		return Message{}, fmt.Errorf("adding a message: %w", err)
	}
	return m, nil
}

// UpdateMessage saves the source, external and thread ids, kind and state of
// m, and stamps its update time. It is ErrNotFound for an unknown message.
func (s *Store) UpdateMessage(ctx context.Context, m Message) error {
	res, err := s.q.ExecContext(ctx, `UPDATE messages SET source = ?, external_id = ?, thread_id = ?, kind = ?, state = ?,
		updated_at = ? WHERE id = ?`, m.Source, m.ExternalID, m.ThreadID, m.Kind, m.State, ms(s.stamp()), m.ID)
	if err != nil {
		return fmt.Errorf("updating message %d: %w", m.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Messages lists the messages of a run, oldest first.
func (s *Store) Messages(ctx context.Context, runID int64) ([]Message, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id, run_id, source, external_id, thread_id, kind, state, created_at, updated_at
		FROM messages WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var created, updated int64
		if err := rows.Scan(&m.ID, &m.RunID, &m.Source, &m.ExternalID, &m.ThreadID, &m.Kind, &m.State, &created, &updated); err != nil {
			return nil, fmt.Errorf("listing messages: %w", err)
		}
		m.CreatedAt, m.UpdatedAt = fromMS(created), fromMS(updated)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	return out, nil
}

// InboxItem is a message from a human waiting for the agent. ConsumedBy is
// the invocation that handled it, nil while it is pending.
type InboxItem struct {
	ID         int64
	RunID      int64
	Source     string
	ExternalID string
	Author     string
	Body       string
	URL        string
	CreatedAt  time.Time
	ConsumedBy *int64
}

// AddInbox queues a message for a run. A message with the same source and
// external id is queued only once, however often it is seen: added is false
// for a duplicate. A zero CreatedAt means now.
func (s *Store) AddInbox(ctx context.Context, it InboxItem) (added bool, err error) {
	created := it.CreatedAt
	if created.IsZero() {
		created = s.stamp()
	}
	res, err := s.q.ExecContext(ctx, `INSERT INTO inbox (run_id, source, external_id, author, body, url, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (source, external_id) DO NOTHING`,
		it.RunID, it.Source, it.ExternalID, it.Author, it.Body, it.URL, ms(created))
	if err != nil {
		return false, fmt.Errorf("adding to the inbox: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("adding to the inbox: %w", err)
	}
	return n > 0, nil
}

// PendingInbox lists the run's unconsumed messages, oldest first.
func (s *Store) PendingInbox(ctx context.Context, runID int64) ([]InboxItem, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id, run_id, source, external_id, author, body, url, created_at, consumed_by
		FROM inbox WHERE run_id = ? AND consumed_by IS NULL ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("listing the inbox: %w", err)
	}
	defer rows.Close()
	var out []InboxItem
	for rows.Next() {
		var it InboxItem
		var created int64
		var by sql.NullInt64
		if err := rows.Scan(&it.ID, &it.RunID, &it.Source, &it.ExternalID, &it.Author, &it.Body, &it.URL, &created, &by); err != nil {
			return nil, fmt.Errorf("listing the inbox: %w", err)
		}
		it.CreatedAt = fromMS(created)
		if by.Valid {
			v := by.Int64
			it.ConsumedBy = &v
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing the inbox: %w", err)
	}
	return out, nil
}

// ConsumeInbox marks the messages as handled by an invocation. Messages that
// are already consumed keep their first consumer.
func (s *Store) ConsumeInbox(ctx context.Context, ids []int64, invocationID int64) error {
	if len(ids) == 0 {
		return nil
	}
	args := []any{invocationID}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.q.ExecContext(ctx, `UPDATE inbox SET consumed_by = ? WHERE consumed_by IS NULL AND id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return fmt.Errorf("consuming the inbox: %w", err)
	}
	return nil
}

// Invocation is one agent turn of a run; Kind is start, feedback or answer.
type Invocation struct {
	ID         int64
	RunID      int64
	Kind       string
	StartedAt  time.Time
	EndedAt    *time.Time
	ExitCode   *int
	EndReason  string
	LastOutput string
}

// StartInvocation records the start of an agent turn.
func (s *Store) StartInvocation(ctx context.Context, runID int64, kind string) (Invocation, error) {
	now := s.stamp()
	res, err := s.q.ExecContext(ctx, `INSERT INTO invocations (run_id, kind, started_at) VALUES (?, ?, ?)`, runID, kind, ms(now))
	if err != nil {
		return Invocation{}, fmt.Errorf("starting an invocation: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Invocation{}, fmt.Errorf("starting an invocation: %w", err)
	}
	return Invocation{ID: id, RunID: runID, Kind: kind, StartedAt: now}, nil
}

// EndInvocation saves the end of an invocation: EndedAt (now when nil),
// ExitCode, EndReason and LastOutput.
func (s *Store) EndInvocation(ctx context.Context, inv Invocation) error {
	ended := inv.EndedAt
	if ended == nil {
		now := s.stamp()
		ended = &now
	}
	var code sql.NullInt64
	if inv.ExitCode != nil {
		code = sql.NullInt64{Int64: int64(*inv.ExitCode), Valid: true}
	}
	res, err := s.q.ExecContext(ctx, `UPDATE invocations SET ended_at = ?, exit_code = ?, end_reason = ?, last_output = ?
		WHERE id = ?`, ms(*ended), code, inv.EndReason, inv.LastOutput, inv.ID)
	if err != nil {
		return fmt.Errorf("ending invocation %d: %w", inv.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Invocations lists the invocations of a run, oldest first.
func (s *Store) Invocations(ctx context.Context, runID int64) ([]Invocation, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id, run_id, kind, started_at, ended_at, exit_code, end_reason, last_output
		FROM invocations WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("listing invocations: %w", err)
	}
	defer rows.Close()
	var out []Invocation
	for rows.Next() {
		var inv Invocation
		var started int64
		var ended, code sql.NullInt64
		if err := rows.Scan(&inv.ID, &inv.RunID, &inv.Kind, &started, &ended, &code, &inv.EndReason, &inv.LastOutput); err != nil {
			return nil, fmt.Errorf("listing invocations: %w", err)
		}
		inv.StartedAt = fromMS(started)
		inv.EndedAt = fromNullMS(ended)
		if code.Valid {
			c := int(code.Int64)
			inv.ExitCode = &c
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing invocations: %w", err)
	}
	return out, nil
}
