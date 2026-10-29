// Package store keeps the state of ymcp pipelines in one SQLite file.
//
// The file lives next to the config (see DefaultPath), is opened in WAL mode
// and is shared by the runner, which writes, and the TUI and CLI, which read
// and add the odd answer, each usually a separate process. The schema is
// versioned by migrations in code and applied when the file is opened.
// SQLite is the pure Go modernc.org/sqlite driver, so nothing needs cgo.
//
// All times are stored as unix milliseconds and returned in UTC.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
)

// Errors returned by the store.
var (
	// ErrNotFound means the requested row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrActiveRun means the issue already has a run that is not finished.
	ErrActiveRun = errors.New("store: the issue already has an active run")
	// ErrRunnerBusy means a live runner already holds the pipeline.
	ErrRunnerBusy = errors.New("store: another runner holds the pipeline")
	// ErrRunFinished means the run has a final status and cannot be changed.
	ErrRunFinished = errors.New("store: the run has ended")
	// ErrRunActive means the run has not ended, so it cannot be deleted.
	ErrRunActive = errors.New("store: the run has not ended")
	// ErrRunNotDone means the run did not end as done, so it can't be
	// reopened.
	ErrRunNotDone = errors.New("store: the run did not end as done")
	// ErrRunnerLost means the caller no longer holds the runner lock, e.g.
	// because another runner took it over after a missed heartbeat.
	ErrRunnerLost = errors.New("store: the runner lock is not held")
)

// openRetry is how long Open keeps retrying while other processes hold
// SQLite locks, which several processes creating the file at once can do
// even with a busy timeout.
const openRetry = 5 * time.Second

// DefaultPath is where the state database lives: state.db in the ymcp
// directory of the user config directory. YMCP_STATE overrides it.
func DefaultPath() (string, error) {
	if p := os.Getenv("YMCP_STATE"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating the user config directory: %w", err)
	}
	return filepath.Join(dir, "ymcp", "state.db"), nil
}

// querier is what a *sql.DB and a *sql.Tx have in common.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store is the state database. It is safe for concurrent use. The value
// passed to the function of Tx is a Store bound to that transaction.
type Store struct {
	db *sql.DB // nil for a transaction-bound store
	q  querier
	tx *sql.Tx

	clock sync.RWMutex // guards now
	now   func() time.Time
}

// Open opens the database at path, creating the file and its directory when
// missing, and migrates it to the current schema. It retries for up to five
// seconds while other processes hold locks.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("opening state: empty path")
	}
	inMemory := path == ":memory:" || strings.HasPrefix(path, "file::memory:")
	if !inMemory {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("creating the state directory: %w", err)
		}
		// Create the file ourselves so it is private to the user.
		if f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600); err != nil {
			return nil, fmt.Errorf("creating the state file: %w", err)
		} else {
			f.Close()
		}
	}

	deadline := time.Now().Add(openRetry)
	for {
		s, err := openOnce(path)
		if err == nil {
			return s, nil
		}
		if !isBusy(err) || time.Now().After(deadline) {
			return nil, fmt.Errorf("opening state %s: %w", path, err)
		}
		time.Sleep(time.Duration(10+rand.IntN(40)) * time.Millisecond)
	}
}

func openOnce(path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	// One connection serializes this process's use of the file; other
	// processes are handled by SQLite's own locking and the busy timeout.
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := &Store{db: db, q: db, now: time.Now}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// dsn builds the driver connection string. The busy timeout comes before the
// journal mode: pragmas run in order and the switch to WAL needs the timeout.
func dsn(path string) string {
	if path == ":memory:" {
		path = "file::memory:"
	}
	if !strings.HasPrefix(path, "file:") {
		p := filepath.ToSlash(path)
		if runtime.GOOS == "windows" && len(p) > 1 && p[1] == ':' {
			p = "/" + p
		}
		path = "file:" + (&url.URL{Path: p}).EscapedPath()
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "_txlock=immediate" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"
}

// Close closes the database. On a transaction-bound store it does nothing.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Tx runs fn in one transaction: everything written through the store passed
// to fn is committed if fn returns nil and rolled back otherwise. The store
// passed to fn must be used instead of the outer one, which would wait for
// the transaction to end. Calling Tx on that store just runs fn again in the
// same transaction.
func (s *Store) Tx(ctx context.Context, fn func(tx *Store) error) (err error) {
	if s.tx != nil {
		return fn(s)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(&Store{q: tx, tx: tx, now: s.currentClock()}); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing a transaction: %w", err)
	}
	return nil
}

// isBusy reports whether err is SQLite telling us another connection holds
// a lock (SQLITE_BUSY or SQLITE_LOCKED, including their extended codes).
func isBusy(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		c := se.Code() & 0xff
		return c == 5 || c == 6
	}
	return false
}

// isUnique reports whether err is a UNIQUE constraint violation.
func isUnique(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == 2067 || se.Code() == 1555 // CONSTRAINT_UNIQUE, CONSTRAINT_PRIMARYKEY
	}
	return false
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

func nullMS(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: ms(*t), Valid: true}
}

func fromNullMS(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMS(v.Int64)
	return &t
}

// stamp is the current time at millisecond precision, the precision of
// everything stored.
func (s *Store) stamp() time.Time {
	return time.UnixMilli(s.currentClock()().UnixMilli()).UTC()
}

// SetClock replaces the clock behind every timestamp the store writes
// (CreatedAt, UpdatedAt, event times and so on); nil restores time.Now. It
// is meant for tests with a fake clock and only affects transactions that
// begin after the call.
func (s *Store) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	s.clock.Lock()
	s.now = now
	s.clock.Unlock()
}

func (s *Store) currentClock() func() time.Time {
	s.clock.RLock()
	defer s.clock.RUnlock()
	return s.now
}
