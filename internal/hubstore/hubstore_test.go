package hubstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/domain"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestSchemaIsCreatedAndVersioned(t *testing.T) {
	s := open(t)
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != SchemaVersion {
		t.Errorf("user_version = %d, want %d", ver, SchemaVersion)
	}
	if out, err := s.IntegrityCheck(ctx(t)); err != nil {
		t.Fatalf("integrity check: %v", err)
	} else if out != "ok" {
		t.Errorf("integrity check = %q, want ok", out)
	}
}

func TestReopeningAnExistingDatabaseWorks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")

	s1, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.CreateJob(context.Background(), NewJob{
		ID: "j1", Op: "system.status", Principal: "local:op",
	}); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer s2.Close()
	j, err := s2.Job(context.Background(), "j1")
	if err != nil {
		t.Fatalf("job did not survive the reopen: %v", err)
	}
	if j.Op != "system.status" {
		t.Errorf("op = %q, want system.status", j.Op)
	}
}

func TestANewerSchemaIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	s, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := Open(path, DefaultLimits()); err == nil {
		t.Fatal("a database written by a newer build was opened without complaint")
	}
}

func TestCreateJobRecordsTheRequest(t *testing.T) {
	s := open(t)
	j, err := s.CreateJob(ctx(t), NewJob{
		ID: "job-1", Op: "files.list", Principal: "ai:hermes",
		Params: "root=data", Target: "data/photos",
	})
	if err != nil {
		t.Fatal(err)
	}
	if j.State != domain.JobRunning {
		t.Errorf("state = %q, want running for a synchronous job", j.State)
	}
	got, err := s.Job(ctx(t), "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Principal != "ai:hermes" || got.Target != "data/photos" || got.Params != "root=data" {
		t.Errorf("job round-trip lost fields: %+v", got)
	}
	if got.StartedAt.IsZero() {
		t.Error("a job created as running has no start time")
	}

	// A queued job must not claim to have started.
	q, err := s.CreateJob(ctx(t), NewJob{
		ID: "job-2", Op: "album.scan", Principal: "user:op", StartQueued: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if q.State != domain.JobQueued {
		t.Errorf("state = %q, want queued", q.State)
	}
	if !q.StartedAt.IsZero() {
		t.Error("a queued job has a start time, so 'accepted' and 'started' look the same")
	}
}

func TestTheOpenJobCapIsEnforced(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"), Limits{
		MaxOpenJobs: 3, MaxHistory: 100, AuditDays: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := 0; i < 3; i++ {
		if _, err := s.CreateJob(context.Background(), NewJob{
			ID: fmt.Sprintf("j%d", i), Op: "album.scan", Principal: "user:op", StartQueued: true,
		}); err != nil {
			t.Fatalf("job %d: %v", i, err)
		}
	}
	_, err = s.CreateJob(context.Background(), NewJob{
		ID: "j4", Op: "album.scan", Principal: "user:op", StartQueued: true,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("creating a job past the cap: error = %v, want ErrConflict", err)
	}

	// Cancelling one frees a slot, so the cap bounds concurrency without
	// deadlocking the console. Cancelling is the legal exit from queued;
	// completing from queued is refused on purpose (a job that never ran must
	// not be recorded as work that was done).
	if err := s.Finish(context.Background(), "j0", domain.JobCancelled, "operator cancelled", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateJob(context.Background(), NewJob{
		ID: "j5", Op: "album.scan", Principal: "user:op", StartQueued: true,
	}); err != nil {
		t.Fatalf("the slot was not released: %v", err)
	}
}

func TestIllegalTransitionsAreRefused(t *testing.T) {
	s := open(t)
	c := ctx(t)
	if _, err := s.CreateJob(c, NewJob{ID: "j", Op: "album.scan", Principal: "user:op", StartQueued: true}); err != nil {
		t.Fatal(err)
	}
	// queued -> completed is the transition that would let the console claim
	// work it never started.
	if err := s.Transition(c, "j", domain.JobCompleted, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("queued -> completed: error = %v, want ErrConflict", err)
	}
	if err := s.Transition(c, "j", domain.JobRunning, "admitted"); err != nil {
		t.Fatalf("queued -> running: %v", err)
	}
	if err := s.Transition(c, "j", domain.JobCompleted, "done"); err != nil {
		t.Fatalf("running -> completed: %v", err)
	}
	// A terminal job is immutable.
	if err := s.Transition(c, "j", domain.JobRunning, "again"); !errors.Is(err, ErrConflict) {
		t.Errorf("completed -> running: error = %v, want ErrConflict", err)
	}
}

func TestAnUnknownJobIsReported(t *testing.T) {
	s := open(t)
	if _, err := s.Job(ctx(t), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	if err := s.Transition(ctx(t), "nope", domain.JobRunning, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestJobEventsRecordTheHistory(t *testing.T) {
	s := open(t)
	c := ctx(t)
	if _, err := s.CreateJob(c, NewJob{ID: "j", Op: "album.thumbs", Principal: "user:op", StartQueued: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(c, "j", domain.JobRunning, "admitted"); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(c, "j", domain.JobFailed, "resource_trip", "out of memory", false); err != nil {
		t.Fatal(err)
	}
	ev, err := s.JobEvents(c, "j", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 3 {
		t.Fatalf("events = %d, want 3 (created, admitted, finished): %+v", len(ev), ev)
	}
	if ev[0].To != string(domain.JobQueued) {
		t.Errorf("first event = %+v, want the creation", ev[0])
	}
	if ev[2].To != string(domain.JobFailed) || ev[2].Note != "resource_trip" {
		t.Errorf("last event = %+v, want the failure with its reason", ev[2])
	}
}

func TestFinishRefusesANonTerminalState(t *testing.T) {
	s := open(t)
	c := ctx(t)
	if _, err := s.CreateJob(c, NewJob{ID: "j", Op: "x", Principal: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(c, "j", domain.JobRunning, "", "", false); err == nil {
		t.Error("Finish accepted a non-terminal state, so a job could be 'finished' without ending")
	}
}

func TestJobsArePagedAndFiltered(t *testing.T) {
	s := open(t)
	c := ctx(t)
	for i := 0; i < 10; i++ {
		st := NewJob{ID: fmt.Sprintf("j%02d", i), Op: "album.scan", Principal: "user:op"}
		if i%2 == 0 {
			st.Op = "files.list"
			st.StartQueued = true
		}
		if _, err := s.CreateJob(c, st); err != nil {
			t.Fatal(err)
		}
	}
	page1, err := s.Jobs(c, JobFilter{Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 4 {
		t.Errorf("page 1 = %d jobs, want 4", len(page1))
	}
	page2, err := s.Jobs(c, JobFilter{Limit: 4, Offset: 4})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, j := range append(page1, page2...) {
		if seen[j.ID] {
			t.Errorf("job %s appears on two pages", j.ID)
		}
		seen[j.ID] = true
	}

	byOp, err := s.Jobs(c, JobFilter{Op: "files.list", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(byOp) != 5 {
		t.Errorf("filtered listing = %d, want 5", len(byOp))
	}
	for _, j := range byOp {
		if j.Op != "files.list" {
			t.Errorf("filter leaked op %q", j.Op)
		}
	}

	openJobs, err := s.Jobs(c, JobFilter{Open: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(openJobs) != 10 {
		t.Errorf("open jobs = %d, want 10", len(openJobs))
	}

	if _, err := s.Jobs(c, JobFilter{}); err == nil {
		t.Error("an unbounded job listing was allowed; every list must have a limit")
	}
}

func TestCounts(t *testing.T) {
	s := open(t)
	c := ctx(t)
	if _, err := s.CreateJob(c, NewJob{ID: "a", Op: "x", Principal: "u"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateJob(c, NewJob{ID: "b", Op: "x", Principal: "u", StartQueued: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(c, "a", domain.JobCompleted, "", "", true); err != nil {
		t.Fatal(err)
	}
	n, err := s.CountOpen(c)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("open = %d, want 1", n)
	}
	byState, err := s.CountByState(c)
	if err != nil {
		t.Fatal(err)
	}
	if byState[string(domain.JobCompleted)] != 1 || byState[string(domain.JobQueued)] != 1 {
		t.Errorf("counts = %v", byState)
	}
}

// TestReconcileMarksDeadJobsInterrupted is the restart-safety test. After a
// console restart, a job that was running and whose process is gone must become
// interrupted - not completed, and not silently left running.
func TestReconcileMarksDeadJobsInterrupted(t *testing.T) {
	s := open(t)
	c := ctx(t)
	if _, err := s.CreateJob(c, NewJob{ID: "dead", Op: "album.thumbs", Principal: "user:op"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress(c, "dead", 4242, 4242, 5, 10, "files"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateJob(c, NewJob{ID: "alive", Op: "album.thumbs", Principal: "user:op"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress(c, "alive", 99, 99, 1, 10, "files"); err != nil {
		t.Fatal(err)
	}

	interrupted, orphaned, err := s.Reconcile(c, func(pid, pgid int) bool {
		return pid == 99 // only the "alive" process exists
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if interrupted != 1 {
		t.Errorf("interrupted = %d, want 1", interrupted)
	}
	if len(orphaned) != 1 || orphaned[0].ID != "alive" {
		t.Fatalf("orphaned = %+v, want the one live process", orphaned)
	}

	dead, err := s.Job(c, "dead")
	if err != nil {
		t.Fatal(err)
	}
	if dead.State != domain.JobInterrupted {
		t.Errorf("state = %q, want interrupted", dead.State)
	}
	if dead.StopReason == "" {
		t.Error("an interrupted job carries no reason")
	}

	// The orphan is left alone: its provenance is uncertain, so the console
	// reports it rather than adopting or killing it.
	alive, err := s.Job(c, "alive")
	if err != nil {
		t.Fatal(err)
	}
	if alive.State != domain.JobRunning {
		t.Errorf("the live job's state = %q, want running (left for a human to decide)", alive.State)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	s := open(t)
	c := ctx(t)
	if _, err := s.CreateJob(c, NewJob{ID: "j", Op: "x", Principal: "u"}); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.Reconcile(c, func(int, int) bool { return false }, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Fatalf("first reconcile interrupted %d, want 1", first)
	}
	second, _, err := s.Reconcile(c, func(int, int) bool { return false }, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Errorf("second reconcile interrupted %d more, want 0: reconcile must be "+
			"idempotent so a repeated startup does not rewrite history", second)
	}
}

func TestPruneHistoryKeepsTheNewestAndNeverTouchesOpenJobs(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"), Limits{
		MaxOpenJobs: 100, MaxHistory: 3, AuditDays: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := ctx(t)

	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("done%02d", i)
		if _, err := s.CreateJob(c, NewJob{ID: id, Op: "x", Principal: "u"}); err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(c, id, domain.JobCompleted, "", "", true); err != nil {
			t.Fatal(err)
		}
	}
	// An open job must survive pruning even though it is old.
	if _, err := s.CreateJob(c, NewJob{ID: "open", Op: "x", Principal: "u", StartQueued: true}); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneHistory(c)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("pruned %d, want 5 (8 finished, cap 3)", n)
	}
	if _, err := s.Job(c, "open"); err != nil {
		t.Errorf("pruning removed an open job: %v", err)
	}
	remaining, err := s.Jobs(c, JobFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 4 {
		t.Errorf("remaining = %d, want 3 kept plus 1 open", len(remaining))
	}
}

func TestAuditRecordsAndIsPaged(t *testing.T) {
	s := open(t)
	c := ctx(t)
	for i := 0; i < 5; i++ {
		if err := s.Record(c, AuditEntry{
			Principal: "ai:hermes", Action: "call", Op: "files.list",
			Target: "data", Params: "root=data", Outcome: "ok",
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.Audit(c, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("audit page = %d, want 3", len(rows))
	}
	if rows[0].Principal != "ai:hermes" || rows[0].Op != "files.list" {
		t.Errorf("audit row lost fields: %+v", rows[0])
	}
	if rows[0].At.IsZero() {
		t.Error("audit row has no timestamp")
	}
	if _, err := s.Audit(c, 0, 0); err == nil {
		t.Error("an unbounded audit listing was allowed")
	}
}

func TestPruneAuditHonoursRetention(t *testing.T) {
	s := open(t)
	c := ctx(t)
	if err := s.Record(c, AuditEntry{Principal: "u", Action: "a", Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	// Nothing is old yet.
	if n, err := s.PruneAudit(c, time.Now()); err != nil || n != 0 {
		t.Errorf("pruned %d (err %v), want 0", n, err)
	}
	// Far in the future, everything is beyond retention.
	n, err := s.PruneAudit(c, time.Now().AddDate(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
}

func TestModesPersistAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")

	s1, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if err := s1.SetMode(context.Background(), domain.ModeState{
		Mode: domain.ModeManualPause, Source: "user:op", Reason: "machine is unstable",
		SetAt: time.Now().Truncate(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s1.SetMode(context.Background(), domain.ModeState{
		Mode: domain.ModeWatching, Source: "user:op", ExpiresAt: exp,
		SetAt: time.Now().Truncate(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := Open(path, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.Modes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	eff := got.Effective(time.Now())
	if eff.Mode != domain.ModeManualPause {
		t.Errorf("after restart the effective mode = %q, want manual_pause: a pause must "+
			"survive a restart", eff.Mode)
	}
	if eff.Reason != "machine is unstable" {
		t.Errorf("reason = %q, want it preserved", eff.Reason)
	}
}

func TestSetModeReplacesRatherThanDuplicates(t *testing.T) {
	s := open(t)
	c := ctx(t)
	for i := 0; i < 3; i++ {
		if err := s.SetMode(c, domain.ModeState{
			Mode: domain.ModeWatching, Source: "user:op",
			ExpiresAt: time.Now().Add(time.Duration(i+1) * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Modes(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.States) != 1 {
		t.Errorf("modes = %d, want 1: setting a mode twice must extend it, not duplicate it", len(got.States))
	}
}

func TestPurgeExpiredModes(t *testing.T) {
	s := open(t)
	c := ctx(t)
	if err := s.SetMode(c, domain.ModeState{
		Mode: domain.ModeWatching, Source: "u", ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode(c, domain.ModeState{Mode: domain.ModeManualPause, Source: "u"}); err != nil {
		t.Fatal(err)
	}
	n, err := s.PurgeExpiredModes(c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("purged %d, want 1 (the expired window only)", n)
	}
	got, err := s.Modes(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.States) != 1 || got.States[0].Mode != domain.ModeManualPause {
		t.Errorf("a durable mode was purged: %+v", got.States)
	}
}

func TestClearMode(t *testing.T) {
	s := open(t)
	c := ctx(t)
	if err := s.SetMode(c, domain.ModeState{Mode: domain.ModeManualPause, Source: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearMode(c, domain.ModeManualPause); err != nil {
		t.Fatal(err)
	}
	got, err := s.Modes(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.States) != 0 {
		t.Errorf("modes after clearing = %+v, want none", got.States)
	}
}

// TestApprovalMustMatchExactly is the test that keeps an approval from becoming
// a blanket permission. Approving a bounded scan of one collection for ten files
// must not authorise a scan of another collection, or of a thousand files.
func TestApprovalMustMatchExactly(t *testing.T) {
	s := open(t)
	c := ctx(t)
	now := time.Now()
	base := Approval{
		ID: "ap1", Op: "album.scan", Principal: "ai:hermes",
		Target: "holiday", Params: "limit=10",
		GrantedBy: "user:op", GrantedAt: now, ExpiresAt: now.Add(time.Hour), OneShot: true,
	}
	if err := s.GrantApproval(c, base); err != nil {
		t.Fatal(err)
	}

	mismatches := []struct {
		name                          string
		op, principal, target, params string
	}{
		{"a different operation", "album.thumbs", "ai:hermes", "holiday", "limit=10"},
		{"a different principal", "album.scan", "ai:other", "holiday", "limit=10"},
		{"a different collection", "album.scan", "ai:hermes", "photos", "limit=10"},
		{"a changed parameter", "album.scan", "ai:hermes", "holiday", "limit=1000"},
		{"a dropped parameter", "album.scan", "ai:hermes", "holiday", ""},
	}
	for _, m := range mismatches {
		t.Run(m.name, func(t *testing.T) {
			if _, err := s.ConsumeApproval(c, m.op, m.principal, m.target, m.params, now); !errors.Is(err, ErrNotFound) {
				t.Errorf("a mismatched approval was accepted: %v", err)
			}
		})
	}

	// The exact match works, and is consumed.
	got, err := s.ConsumeApproval(c, "album.scan", "ai:hermes", "holiday", "limit=10", now)
	if err != nil {
		t.Fatalf("the exact approval was not accepted: %v", err)
	}
	if got.GrantedBy != "user:op" {
		t.Errorf("granted_by = %q, want the granting human to be recorded", got.GrantedBy)
	}
	// One-shot: a second use must fail, or one approval would authorise a loop.
	if _, err := s.ConsumeApproval(c, "album.scan", "ai:hermes", "holiday", "limit=10", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("a one-shot approval was reused: %v", err)
	}
}

func TestAnExpiredApprovalIsRefused(t *testing.T) {
	s := open(t)
	c := ctx(t)
	now := time.Now()
	if err := s.GrantApproval(c, Approval{
		ID: "ap", Op: "album.scan", Principal: "ai:hermes", Target: "h", Params: "",
		GrantedBy: "user:op", GrantedAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour), OneShot: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeApproval(c, "album.scan", "ai:hermes", "h", "", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("an expired approval was accepted: %v", err)
	}
}

func TestApprovalsAreListedAndRevocable(t *testing.T) {
	s := open(t)
	c := ctx(t)
	now := time.Now()
	if err := s.GrantApproval(c, Approval{
		ID: "ap", Op: "album.scan", Principal: "ai:hermes", Target: "h", Params: "limit=10",
		GrantedBy: "user:op", GrantedAt: now, ExpiresAt: now.Add(time.Hour), OneShot: true,
	}); err != nil {
		t.Fatal(err)
	}
	list, err := s.Approvals(c, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "ap" || !list[0].OneShot {
		t.Fatalf("approvals = %+v", list)
	}
	if err := s.RevokeApproval(c, "ap"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeApproval(c, "ap"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking twice: error = %v, want ErrNotFound", err)
	}
}

// TestConcurrentCreatesRespectTheCap: the cap must hold under concurrent writers,
// not only in a sequential test. The store uses a single connection and an
// immediate transaction, so the count and the insert cannot interleave.
func TestConcurrentCreatesRespectTheCap(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"), Limits{
		MaxOpenJobs: 5, MaxHistory: 100, AuditDays: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		okCount int
	)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.CreateJob(context.Background(), NewJob{
				ID: fmt.Sprintf("c%02d", i), Op: "x", Principal: "u", StartQueued: true,
			})
			if err == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if okCount > 5 {
		t.Errorf("%d jobs were created under a cap of 5", okCount)
	}
	n, err := s.CountOpen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n > 5 {
		t.Errorf("open jobs = %d, above the cap of 5", n)
	}
}

func TestProgressDoesNotResurrectAFinishedJob(t *testing.T) {
	s := open(t)
	c := ctx(t)
	if _, err := s.CreateJob(c, NewJob{ID: "j", Op: "x", Principal: "u"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(c, "j", domain.JobCompleted, "", "", true); err != nil {
		t.Fatal(err)
	}
	// A late progress write from a worker that has not noticed the job ended.
	if err := s.Progress(c, "j", 1, 1, 99, 100, "files"); err != nil {
		t.Fatalf("a stale progress write returned an error: %v", err)
	}
	j, err := s.Job(c, "j")
	if err != nil {
		t.Fatal(err)
	}
	if j.State != domain.JobCompleted {
		t.Errorf("state = %q after a stale progress write, want completed", j.State)
	}
	if j.Progress != 0 {
		t.Errorf("progress = %d on a completed job, want it unchanged", j.Progress)
	}
}

func TestStateAndCountsAgreeOnWhatIsOpen(t *testing.T) {
	s := open(t)
	c := ctx(t)
	states := []domain.JobState{domain.JobQueued, domain.JobRunning, domain.JobWaiting, domain.JobPaused}
	for i, st := range states {
		id := fmt.Sprintf("j%d", i)
		nj := NewJob{ID: id, Op: "x", Principal: "u", StartQueued: true}
		if _, err := s.CreateJob(c, nj); err != nil {
			t.Fatal(err)
		}
		if st != domain.JobQueued {
			if err := s.Transition(c, id, st, ""); err != nil {
				t.Fatalf("%s -> %s: %v", domain.JobQueued, st, err)
			}
		}
	}
	n, err := s.CountOpen(c)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(states) {
		t.Errorf("open = %d, want %d", n, len(states))
	}
}
