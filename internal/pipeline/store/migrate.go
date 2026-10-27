package store

import (
	"context"
	"fmt"
)

// migrations are applied in order; the number of the last one applied is kept
// in PRAGMA user_version. Never edit a released migration, append a new one.
var migrations = []string{
	// 1: the initial schema.
	`
CREATE TABLE runners (
    pipeline     TEXT PRIMARY KEY,
    pid          INTEGER NOT NULL,
    host         TEXT NOT NULL DEFAULT '',
    started_at   INTEGER NOT NULL,
    heartbeat_at INTEGER NOT NULL
);

CREATE TABLE counters (
    pipeline TEXT PRIMARY KEY,
    next_run INTEGER NOT NULL
);

CREATE TABLE runs (
    id                 INTEGER PRIMARY KEY,
    pipeline           TEXT NOT NULL,
    number             INTEGER NOT NULL,
    profile            TEXT NOT NULL,
    issue_id           TEXT NOT NULL,
    issue_key          TEXT NOT NULL DEFAULT '',
    stage              TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL,
    needs_input_reason TEXT NOT NULL DEFAULT '',
    host               TEXT NOT NULL DEFAULT '',
    worktree           TEXT NOT NULL DEFAULT '',
    branch             TEXT NOT NULL DEFAULT '',
    base               TEXT NOT NULL DEFAULT '',
    agent              TEXT NOT NULL DEFAULT '',
    agent_session      TEXT NOT NULL DEFAULT '',
    vcs_kind           TEXT NOT NULL DEFAULT '',
    vcs_repo           TEXT NOT NULL DEFAULT '',
    pr_number          INTEGER NOT NULL DEFAULT 0,
    pr_url             TEXT NOT NULL DEFAULT '',
    gh_issue_number    INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    finished_at        INTEGER,
    UNIQUE (pipeline, number)
);
-- One active run per issue of a profile.
CREATE UNIQUE INDEX runs_active_issue ON runs (profile, issue_id)
    WHERE status NOT IN ('done', 'closed', 'canceled', 'failed');
CREATE INDEX runs_by_pipeline ON runs (pipeline, number);

CREATE TABLE invocations (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER,
    exit_code   INTEGER,
    end_reason  TEXT NOT NULL DEFAULT '',
    last_output TEXT NOT NULL DEFAULT ''
);
CREATE INDEX invocations_by_run ON invocations (run_id, id);

CREATE TABLE messages (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    source      TEXT NOT NULL,
    external_id TEXT NOT NULL DEFAULT '',
    thread_id   TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX messages_by_run ON messages (run_id, id);

CREATE TABLE inbox (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    source      TEXT NOT NULL,
    external_id TEXT NOT NULL,
    author      TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL DEFAULT '',
    url         TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    consumed_by INTEGER,
    UNIQUE (source, external_id)
);
CREATE INDEX inbox_by_run ON inbox (run_id, id);

CREATE TABLE cursors (
    pipeline TEXT NOT NULL,
    source   TEXT NOT NULL,
    key      TEXT NOT NULL,
    value    TEXT NOT NULL,
    PRIMARY KEY (pipeline, source, key)
);

CREATE TABLE seen_issues (
    pipeline      TEXT NOT NULL,
    issue_id      TEXT NOT NULL,
    first_seen_at INTEGER NOT NULL,
    PRIMARY KEY (pipeline, issue_id)
);

CREATE TABLE seen_items (
    source      TEXT NOT NULL,
    external_id TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (source, external_id)
);

CREATE TABLE events (
    id     INTEGER PRIMARY KEY,
    run_id INTEGER NOT NULL,
    at     INTEGER NOT NULL,
    kind   TEXT NOT NULL,
    data   TEXT NOT NULL DEFAULT 'null'
);
CREATE INDEX events_by_run ON events (run_id, id);
`,
	// 2: small per-run key-value state (the engine keeps permission state here).
	`
CREATE TABLE run_state (
    run_id INTEGER NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    key    TEXT NOT NULL,
    value  TEXT NOT NULL,
    PRIMARY KEY (run_id, key)
);
`,
}

// migrate brings the schema to the latest version. It runs in an immediate
// transaction and rereads the version inside it, so processes opening the
// same file at once apply each migration exactly once.
func (s *Store) migrate(ctx context.Context) error {
	return s.Tx(ctx, func(tx *Store) error {
		var version int
		if err := tx.q.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
			return fmt.Errorf("reading the schema version: %w", err)
		}
		if version > len(migrations) {
			return fmt.Errorf("the state database has schema version %d, newer than this ymcp understands (%d); update ymcp", version, len(migrations))
		}
		for i := version; i < len(migrations); i++ {
			if _, err := tx.q.ExecContext(ctx, migrations[i]); err != nil {
				return fmt.Errorf("applying migration %d: %w", i+1, err)
			}
			// PRAGMA takes no bound parameters.
			if _, err := tx.q.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
				return fmt.Errorf("recording migration %d: %w", i+1, err)
			}
		}
		return nil
	})
}

// SchemaVersion is the schema version of the open database.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	if err := s.q.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("reading the schema version: %w", err)
	}
	return v, nil
}
