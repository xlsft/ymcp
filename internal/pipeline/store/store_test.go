package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenMigratesTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil || v != len(migrations) {
		t.Fatalf("schema version = %d, %v; want %d", v, err, len(migrations))
	}
	r, err := s.CreateRun(ctx, "p", "prof", "1-1", "A-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
	if v, _ := s2.SchemaVersion(ctx); v != len(migrations) {
		t.Errorf("schema version after reopen = %d", v)
	}
	got, err := s2.Run(ctx, r.ID)
	if err != nil || got.IssueKey != "A-1" {
		t.Errorf("run after reopen = %+v, %v", got, err)
	}
	var mode string
	if err := s2.q.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if m := fi.Mode().Perm(); m&0o077 != 0 && runtime.GOOS != "windows" {
		t.Errorf("state file mode = %v, want private", m)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	s := open(t)
	if _, err := s.q.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations)+1)); err != nil {
		t.Fatal(err)
	}
	path := ""
	s.q.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path)
	s.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("Open on a newer schema: err = %v", err)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("YMCP_STATE", "/x/y.db")
	if p, err := DefaultPath(); err != nil || p != "/x/y.db" {
		t.Errorf("DefaultPath = %q, %v", p, err)
	}
	t.Setenv("YMCP_STATE", "")
	p, err := DefaultPath()
	if err != nil || filepath.Base(p) != "state.db" || filepath.Base(filepath.Dir(p)) != "ymcp" {
		t.Errorf("DefaultPath = %q, %v", p, err)
	}
}

func TestCreateRunNumbering(t *testing.T) {
	s := open(t)
	for i, want := range []int{1, 2, 3} {
		r, err := s.CreateRun(ctx, "backend", "yt", fmt.Sprintf("2-%d", i), fmt.Sprintf("BE-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if r.Number != want || r.Status != StatusQueued || r.Pipeline != "backend" || r.CreatedAt.IsZero() ||
			r.FinishedAt != nil || r.CreatedAt.Location() != time.UTC {
			t.Errorf("run %d = %+v", want, r)
		}
	}
	// Numbers are per pipeline.
	r, err := s.CreateRun(ctx, "frontend", "yt", "2-0", "BE-0")
	if err == nil {
		t.Fatalf("same active issue in another pipeline should conflict, got %+v", r)
	}
	r, err = s.CreateRun(ctx, "frontend", "yt", "9-9", "FE-9")
	if err != nil || r.Number != 1 {
		t.Fatalf("frontend first run = %+v, %v", r, err)
	}
}

func TestCreateRunActiveConflict(t *testing.T) {
	s := open(t)
	first, err := s.CreateRun(ctx, "p", "yt", "2-1", "A-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ctx, "p", "yt", "2-1", "A-1"); !errors.Is(err, ErrActiveRun) {
		t.Fatalf("second active run: err = %v, want ErrActiveRun", err)
	}
	// The failed attempt must not burn a number.
	other, err := s.CreateRun(ctx, "p", "yt", "2-2", "A-2")
	if err != nil || other.Number != 2 {
		t.Fatalf("next run = %+v, %v; want number 2", other, err)
	}
	// A different profile may run the same issue id.
	if _, err := s.CreateRun(ctx, "p", "other", "2-1", "A-1"); err != nil {
		t.Errorf("same id in another profile: %v", err)
	}
	// Every non-final status keeps the issue busy; every final one frees it.
	for _, st := range []RunStatus{StatusWorking, StatusNeedsInput, StatusReview} {
		first.Status = st
		if err := s.UpdateRun(ctx, first); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateRun(ctx, "p", "yt", "2-1", "A-1"); !errors.Is(err, ErrActiveRun) {
			t.Errorf("status %s: err = %v, want ErrActiveRun", st, err)
		}
	}
	for i, st := range []RunStatus{StatusDone, StatusClosed, StatusCanceled, StatusFailed} {
		first.Status = st
		if err := s.UpdateRun(ctx, first); err != nil {
			t.Fatal(err)
		}
		next, err := s.CreateRun(ctx, "p", "yt", "2-1", "A-1")
		if err != nil {
			t.Fatalf("status %s: %v", st, err)
		}
		if next.Number != 4+i {
			t.Errorf("status %s: number = %d, want %d", st, next.Number, 4+i)
		}
		next.Status = StatusCanceled
		if err := s.UpdateRun(ctx, next); err != nil {
			t.Fatal(err)
		}
		first = next
	}
}

func TestReviveIntoActiveConflict(t *testing.T) {
	s := open(t)
	old, _ := s.CreateRun(ctx, "p", "yt", "2-1", "A-1")
	old.Status = StatusDone
	if err := s.UpdateRun(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ctx, "p", "yt", "2-1", "A-1"); err != nil {
		t.Fatal(err)
	}
	old.Status = StatusWorking
	if err := s.UpdateRun(ctx, old); !errors.Is(err, ErrActiveRun) {
		t.Errorf("reviving a run next to an active one: err = %v, want ErrActiveRun", err)
	}
}

func TestCreateRunConcurrentNumbers(t *testing.T) {
	s := open(t)
	const n = 20
	var wg sync.WaitGroup
	numbers := make(chan int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.CreateRun(ctx, "p", "yt", fmt.Sprintf("2-%d", i), "")
			if err != nil {
				t.Error(err)
				return
			}
			numbers <- r.Number
		}()
	}
	wg.Wait()
	close(numbers)
	seen := map[int]bool{}
	for v := range numbers {
		if seen[v] || v < 1 || v > n {
			t.Errorf("bad or duplicate number %d", v)
		}
		seen[v] = true
	}
	if len(seen) != n {
		t.Errorf("got %d distinct numbers, want %d", len(seen), n)
	}
}

func TestRunLookupAndUpdate(t *testing.T) {
	s := open(t)
	r, _ := s.CreateRun(ctx, "p", "yt", "2-1", "A-1")
	if _, err := s.Run(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Run(999): %v", err)
	}
	if _, err := s.RunByNumber(ctx, "p", 5); !errors.Is(err, ErrNotFound) {
		t.Errorf("RunByNumber(5): %v", err)
	}
	if err := s.UpdateRun(ctx, Run{ID: 999}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateRun(999): %v", err)
	}

	r.Stage = "In Progress"
	r.Status = StatusWorking
	r.NeedsInputReason = "why"
	r.Host, r.Worktree, r.Branch, r.Base = "local", "/wt", "A-1-fix", "main"
	r.Agent, r.AgentSession = "claude", "sess-1"
	r.VCSKind, r.VCSRepo, r.PRNumber, r.PRURL, r.GHIssueNumber = "github", "acme/api", 42, "https://x/42", 7
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunByNumber(ctx, "p", r.Number)
	if err != nil {
		t.Fatal(err)
	}
	r.UpdatedAt = got.UpdatedAt
	if got != r {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, r)
	}
	if got.FinishedAt != nil {
		t.Errorf("working run has FinishedAt %v", got.FinishedAt)
	}

	got.Status = StatusDone
	if err := s.UpdateRun(ctx, got); err != nil {
		t.Fatal(err)
	}
	done, _ := s.Run(ctx, r.ID)
	if done.FinishedAt == nil || done.FinishedAt.Before(done.CreatedAt) {
		t.Errorf("done run FinishedAt = %v", done.FinishedAt)
	}
}

func TestRunsListing(t *testing.T) {
	s := open(t)
	a, _ := s.CreateRun(ctx, "a", "yt", "1", "")
	b, _ := s.CreateRun(ctx, "a", "yt", "2", "")
	s.CreateRun(ctx, "b", "yt", "3", "")
	b.Status = StatusDone
	if err := s.UpdateRun(ctx, b); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Runs(ctx, "a", false)
	if len(all) != 2 || all[0].ID != a.ID || all[1].ID != b.ID {
		t.Errorf("all of a = %+v", all)
	}
	act, _ := s.Runs(ctx, "a", true)
	if len(act) != 1 || act[0].ID != a.ID {
		t.Errorf("active of a = %+v", act)
	}
	every, _ := s.Runs(ctx, "", true)
	if len(every) != 2 {
		t.Errorf("active of every pipeline = %d, want 2", len(every))
	}
}

func TestMessages(t *testing.T) {
	s := open(t)
	r, _ := s.CreateRun(ctx, "p", "yt", "1", "")
	m, err := s.AddMessage(ctx, Message{RunID: r.ID, Source: "youtrack", ExternalID: "c1", Kind: "answer", State: "thinking"})
	if err != nil || m.ID == 0 || m.CreatedAt.IsZero() {
		t.Fatalf("AddMessage = %+v, %v", m, err)
	}
	m.State, m.ThreadID = "done", "t1"
	if err := s.UpdateMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMessage(ctx, Message{ID: 999}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateMessage(999): %v", err)
	}
	list, err := s.Messages(ctx, r.ID)
	if err != nil || len(list) != 1 || list[0].State != "done" || list[0].ThreadID != "t1" || list[0].Source != "youtrack" {
		t.Errorf("Messages = %+v, %v", list, err)
	}
	if other, _ := s.Messages(ctx, r.ID+1); len(other) != 0 {
		t.Errorf("other run's messages = %+v", other)
	}
}

func TestInboxDedupAndConsume(t *testing.T) {
	s := open(t)
	r, _ := s.CreateRun(ctx, "p", "yt", "1", "")
	r2, _ := s.CreateRun(ctx, "p", "yt", "2", "")
	add := func(runID int64, src, ext, body string) bool {
		t.Helper()
		ok, err := s.AddInbox(ctx, InboxItem{RunID: runID, Source: src, ExternalID: ext, Author: "me", Body: body, URL: "u"})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !add(r.ID, "youtrack", "c1", "one") {
		t.Error("first add should be added")
	}
	if add(r.ID, "youtrack", "c1", "one again") {
		t.Error("duplicate (source, external_id) should not be added")
	}
	if add(r2.ID, "youtrack", "c1", "other run") {
		t.Error("duplicate across runs should not be added")
	}
	if !add(r.ID, "github", "c1", "same id other source") {
		t.Error("same external id from another source should be added")
	}
	add(r.ID, "youtrack", "c2", "two")

	pending, err := s.PendingInbox(ctx, r.ID)
	if err != nil || len(pending) != 3 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	if pending[0].Body != "one" || pending[0].Author != "me" || pending[0].URL != "u" || pending[0].CreatedAt.IsZero() || pending[0].ConsumedBy != nil {
		t.Errorf("first pending = %+v", pending[0])
	}
	inv, err := s.StartInvocation(ctx, r.ID, "feedback")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeInbox(ctx, []int64{pending[0].ID, pending[1].ID}, inv.ID); err != nil {
		t.Fatal(err)
	}
	left, _ := s.PendingInbox(ctx, r.ID)
	if len(left) != 1 || left[0].ID != pending[2].ID {
		t.Errorf("left after consume = %+v", left)
	}
	// Consuming again keeps the first consumer; an empty list is fine.
	inv2, _ := s.StartInvocation(ctx, r.ID, "answer")
	if err := s.ConsumeInbox(ctx, []int64{pending[0].ID}, inv2.ID); err != nil {
		t.Fatal(err)
	}
	var by int64
	s.q.QueryRowContext(ctx, `SELECT consumed_by FROM inbox WHERE id = ?`, pending[0].ID).Scan(&by)
	if by != inv.ID {
		t.Errorf("consumed_by = %d, want %d", by, inv.ID)
	}
	if err := s.ConsumeInbox(ctx, nil, inv.ID); err != nil {
		t.Errorf("empty consume: %v", err)
	}
	// A consumed message stays deduplicated.
	if add(r.ID, "youtrack", "c1", "after consume") {
		t.Error("consumed message was added again")
	}
}

func TestInvocations(t *testing.T) {
	s := open(t)
	r, _ := s.CreateRun(ctx, "p", "yt", "1", "")
	inv, err := s.StartInvocation(ctx, r.ID, "start")
	if err != nil || inv.ID == 0 || inv.EndedAt != nil {
		t.Fatalf("StartInvocation = %+v, %v", inv, err)
	}
	code := 3
	inv.ExitCode, inv.EndReason, inv.LastOutput = &code, "silence", "tail"
	if err := s.EndInvocation(ctx, inv); err != nil {
		t.Fatal(err)
	}
	list, err := s.Invocations(ctx, r.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("Invocations = %+v, %v", list, err)
	}
	got := list[0]
	if got.EndedAt == nil || got.ExitCode == nil || *got.ExitCode != 3 || got.EndReason != "silence" || got.LastOutput != "tail" || got.Kind != "start" {
		t.Errorf("ended invocation = %+v", got)
	}
	if err := s.EndInvocation(ctx, Invocation{ID: 999}); !errors.Is(err, ErrNotFound) {
		t.Errorf("EndInvocation(999): %v", err)
	}
	// An invocation that ends without an exit code keeps it null.
	inv2, _ := s.StartInvocation(ctx, r.ID, "feedback")
	if err := s.EndInvocation(ctx, inv2); err != nil {
		t.Fatal(err)
	}
	list, _ = s.Invocations(ctx, r.ID)
	if list[1].ExitCode != nil {
		t.Errorf("exit code = %v, want nil", *list[1].ExitCode)
	}
}

func TestCursorsAndSeen(t *testing.T) {
	s := open(t)
	if v, err := s.Cursor(ctx, "p", "youtrack", "activities"); err != nil || v != "" {
		t.Errorf("missing cursor = %q, %v", v, err)
	}
	if err := s.SetCursor(ctx, "p", "youtrack", "activities", "c1"); err != nil {
		t.Fatal(err)
	}
	s.SetCursor(ctx, "p", "youtrack", "activities", "c2")
	s.SetCursor(ctx, "q", "youtrack", "activities", "other")
	if v, _ := s.Cursor(ctx, "p", "youtrack", "activities"); v != "c2" {
		t.Errorf("cursor = %q, want c2", v)
	}
	if v, _ := s.Cursor(ctx, "q", "youtrack", "activities"); v != "other" {
		t.Errorf("other pipeline's cursor = %q", v)
	}

	if _, ok, err := s.Seen(ctx, "youtrack", "7-1"); err != nil || ok {
		t.Errorf("Seen on empty = %v, %v", ok, err)
	}
	s.MarkSeen(ctx, "youtrack", "7-1", "100")
	s.MarkSeen(ctx, "youtrack", "7-1", "200")
	if u, ok, _ := s.Seen(ctx, "youtrack", "7-1"); !ok || u != "200" {
		t.Errorf("Seen = %q, %v", u, ok)
	}
	if seen, _ := s.SeenIssue(ctx, "p", "2-1"); seen {
		t.Error("issue seen before it was marked")
	}
	s.MarkIssueSeen(ctx, "p", "2-1")
	s.MarkIssueSeen(ctx, "p", "2-1")
	if seen, _ := s.SeenIssue(ctx, "p", "2-1"); !seen {
		t.Error("issue not seen after it was marked")
	}
	if seen, _ := s.SeenIssue(ctx, "q", "2-1"); seen {
		t.Error("seen issues leaked across pipelines")
	}
}

func TestTxCommitAndRollback(t *testing.T) {
	s := open(t)
	err := s.Tx(ctx, func(tx *Store) error {
		if err := tx.SetCursor(ctx, "p", "youtrack", "activities", "c1"); err != nil {
			return err
		}
		if err := tx.MarkSeen(ctx, "youtrack", "7-1", "100"); err != nil {
			return err
		}
		// Reads inside see the transaction's own writes.
		if v, _ := tx.Cursor(ctx, "p", "youtrack", "activities"); v != "c1" {
			t.Errorf("cursor inside tx = %q", v)
		}
		// A nested Tx joins the same transaction.
		return tx.Tx(ctx, func(inner *Store) error { return inner.MarkIssueSeen(ctx, "p", "2-1") })
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Cursor(ctx, "p", "youtrack", "activities"); v != "c1" {
		t.Errorf("committed cursor = %q", v)
	}
	if _, ok, _ := s.Seen(ctx, "youtrack", "7-1"); !ok {
		t.Error("committed seen mark missing")
	}

	boom := errors.New("boom")
	err = s.Tx(ctx, func(tx *Store) error {
		tx.SetCursor(ctx, "p", "youtrack", "activities", "c2")
		tx.MarkSeen(ctx, "youtrack", "7-2", "100")
		tx.MarkIssueSeen(ctx, "p", "2-2")
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx err = %v, want boom", err)
	}
	if v, _ := s.Cursor(ctx, "p", "youtrack", "activities"); v != "c1" {
		t.Errorf("cursor after rollback = %q, want c1", v)
	}
	if _, ok, _ := s.Seen(ctx, "youtrack", "7-2"); ok {
		t.Error("seen mark survived a rollback")
	}
	if seen, _ := s.SeenIssue(ctx, "p", "2-2"); seen {
		t.Error("issue mark survived a rollback")
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was swallowed")
			}
		}()
		s.Tx(ctx, func(tx *Store) error {
			tx.SetCursor(ctx, "p", "youtrack", "activities", "c3")
			panic("oops")
		})
	}()
	if v, _ := s.Cursor(ctx, "p", "youtrack", "activities"); v != "c1" {
		t.Errorf("cursor after a panic = %q, want c1 (and the connection must be free)", v)
	}
}

func TestEventsPaging(t *testing.T) {
	s := open(t)
	r, _ := s.CreateRun(ctx, "p", "yt", "1", "")
	other, _ := s.CreateRun(ctx, "p", "yt", "2", "")
	for i := 1; i <= 7; i++ {
		if err := s.AddEvent(ctx, r.ID, "tick", map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	s.AddEvent(ctx, other.ID, "noise", nil)
	if err := s.AddEvent(ctx, r.ID, "bad", make(chan int)); err == nil {
		t.Error("unencodable data should fail")
	}

	var got []int
	var after int64
	pages := 0
	for {
		page, err := s.Events(ctx, r.ID, after, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		pages++
		for _, e := range page {
			var d struct{ I int }
			if err := json.Unmarshal(e.Data, &d); err != nil || e.Kind != "tick" || e.RunID != r.ID || e.At.IsZero() {
				t.Fatalf("event = %+v (%v)", e, err)
			}
			got = append(got, d.I)
			after = e.ID
		}
	}
	if pages != 3 || fmt.Sprint(got) != "[1 2 3 4 5 6 7]" {
		t.Errorf("pages = %d, got = %v", pages, got)
	}
	if all, _ := s.Events(ctx, r.ID, 0, 0); len(all) != 7 {
		t.Errorf("no limit: %d events, want 7", len(all))
	}
	ev, _ := s.Events(ctx, other.ID, 0, 10)
	if len(ev) != 1 || string(ev[0].Data) != "null" {
		t.Errorf("nil data event = %+v", ev)
	}
}

func TestRunnerLock(t *testing.T) {
	s := open(t)
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	const stale = time.Minute

	if _, _, _, ok, err := s.Runner(ctx, "p"); ok || err != nil {
		t.Fatalf("Runner on empty = %v, %v", ok, err)
	}
	if err := s.AcquireRunner(ctx, "p", 100, "hostA", stale); err != nil {
		t.Fatal(err)
	}
	pid, host, beat, ok, err := s.Runner(ctx, "p")
	if err != nil || !ok || pid != 100 || host != "hostA" || !beat.Equal(clock) {
		t.Errorf("Runner = %d %s %v %v %v", pid, host, beat, ok, err)
	}

	// A live runner blocks everyone else, but not another pipeline.
	clock = clock.Add(30 * time.Second)
	if err := s.AcquireRunner(ctx, "p", 200, "hostA", stale); !errors.Is(err, ErrRunnerBusy) {
		t.Errorf("second runner: err = %v, want ErrRunnerBusy", err)
	}
	if err := s.AcquireRunner(ctx, "p", 100, "hostB", stale); !errors.Is(err, ErrRunnerBusy) {
		t.Errorf("same pid on another host: err = %v, want ErrRunnerBusy", err)
	}
	if err := s.AcquireRunner(ctx, "q", 200, "hostA", stale); err != nil {
		t.Errorf("another pipeline: %v", err)
	}
	// The holder may take its own lock again.
	if err := s.AcquireRunner(ctx, "p", 100, "hostA", stale); err != nil {
		t.Errorf("re-acquire by the holder: %v", err)
	}

	// Heartbeats keep the lock alive past the stale time.
	clock = clock.Add(50 * time.Second)
	if err := s.Heartbeat(ctx, "p", 100); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(50 * time.Second)
	if err := s.AcquireRunner(ctx, "p", 200, "hostA", stale); !errors.Is(err, ErrRunnerBusy) {
		t.Errorf("after a heartbeat: err = %v, want ErrRunnerBusy", err)
	}
	if err := s.Heartbeat(ctx, "p", 999); !errors.Is(err, ErrRunnerLost) {
		t.Errorf("Heartbeat by a stranger: %v, want ErrRunnerLost", err)
	}

	// A holder that went quiet is taken over; its heartbeat then fails.
	clock = clock.Add(2 * time.Minute)
	if err := s.AcquireRunner(ctx, "p", 200, "hostB", stale); err != nil {
		t.Fatalf("stale takeover: %v", err)
	}
	if pid, host, _, _, _ := s.Runner(ctx, "p"); pid != 200 || host != "hostB" {
		t.Errorf("holder after takeover = %d on %s", pid, host)
	}
	if err := s.Heartbeat(ctx, "p", 100); !errors.Is(err, ErrRunnerLost) {
		t.Errorf("Heartbeat by the old holder: %v, want ErrRunnerLost", err)
	}

	// Release: only the holder can; then the lock is free.
	if err := s.ReleaseRunner(ctx, "p", 100); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, _ := s.Runner(ctx, "p"); !ok {
		t.Error("a stranger released the lock")
	}
	if err := s.ReleaseRunner(ctx, "p", 200); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, _ := s.Runner(ctx, "p"); ok {
		t.Error("lock still held after release")
	}
	if err := s.AcquireRunner(ctx, "p", 300, "hostC", stale); err != nil {
		t.Errorf("acquire after release: %v", err)
	}
}

// Two-process tests: the test binary runs itself as a helper subprocess.

const helperEnv = "YMCP_STORE_TEST_HELPER"

// TestHelperProcess is not a test: it is the body of the subprocesses started
// by the tests below.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	path, id := os.Getenv(helperEnv+"_PATH"), os.Getenv(helperEnv+"_ID")
	n, _ := strconv.Atoi(os.Getenv(helperEnv + "_N"))
	s, err := Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper open:", err)
		os.Exit(2)
	}
	defer s.Close()
	switch mode {
	case "events":
		r, err := s.Run(ctx, 1)
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper run:", err)
			os.Exit(2)
		}
		for i := 1; i <= n; i++ {
			if err := s.AddEvent(ctx, r.ID, "tick", i); err != nil {
				fmt.Fprintln(os.Stderr, "helper event:", err)
				os.Exit(2)
			}
			time.Sleep(time.Millisecond)
		}
	case "runs":
		for i := 0; i < n; i++ {
			if _, err := s.CreateRun(ctx, "p", "yt", fmt.Sprintf("%s-%d", id, i), ""); err != nil {
				fmt.Fprintln(os.Stderr, "helper create:", err)
				os.Exit(2)
			}
		}
	}
	os.Exit(0)
}

func startHelper(t *testing.T, mode, path, id string, n int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, helperEnv+"_PATH="+path, helperEnv+"_ID="+id,
		helperEnv+"_N="+strconv.Itoa(n))
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return cmd
}

func waitHelper(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Wait(); err != nil {
		t.Errorf("helper failed: %v\n%s", err, cmd.Stderr.(*strings.Builder))
	}
}

func TestTwoProcessesShareWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, _ := s.CreateRun(ctx, "p", "yt", "1", "")

	const n = 300
	writer := startHelper(t, "events", path, "w", n)
	done := make(chan error, 1)
	go func() { done <- writer.Wait() }()

	// Read while the child writes: the count only grows and the parent's
	// own writes go through as well.
	last, own := 0, 0
	deadline := time.Now().Add(30 * time.Second)
	finished := false
	for !finished && time.Now().Before(deadline) {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("writer: %v\n%s", err, writer.Stderr.(*strings.Builder))
			}
			finished = true
		default:
		}
		evs, err := s.Events(ctx, r.ID, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		ticks := 0
		for _, e := range evs {
			if e.Kind == "tick" {
				ticks++
			}
		}
		if ticks < last {
			t.Fatalf("event count went backwards: %d after %d", ticks, last)
		}
		last = ticks
		if own < 50 {
			own++
			if err := s.AddEvent(ctx, r.ID, "parent", own); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !finished {
		t.Fatal("writer did not finish")
	}
	evs, _ := s.Events(ctx, r.ID, 0, 0)
	ticks, parents := 0, 0
	for _, e := range evs {
		switch e.Kind {
		case "tick":
			ticks++
		case "parent":
			parents++
		}
	}
	if ticks != n || parents != own {
		t.Errorf("ticks = %d (want %d), parent events = %d (want %d)", ticks, n, parents, own)
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Errorf("no WAL file: %v", err)
	}
}

func TestProcessesCreatingTheDatabaseTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	const procs, each = 4, 15
	var cmds []*exec.Cmd
	for i := range procs {
		cmds = append(cmds, startHelper(t, "runs", path, fmt.Sprintf("proc%d", i), each))
	}
	for _, c := range cmds {
		waitHelper(t, c)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runs, err := s.Runs(ctx, "p", false)
	if err != nil || len(runs) != procs*each {
		t.Fatalf("runs = %d, %v; want %d", len(runs), err, procs*each)
	}
	for i, r := range runs {
		if r.Number != i+1 {
			t.Fatalf("run %d has number %d: numbers are not 1..N", i, r.Number)
		}
	}
}
