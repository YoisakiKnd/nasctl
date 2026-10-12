// Package hubstore is the console's own durable state: jobs, their event
// history, the audit log and the mode table.
//
// It is a separate SQLite file from albumctl's index on purpose. The index is
// owned by the scanner and its schema moves when the scanner moves; the console's
// job records must not be dragged along by that, and a console that cannot open
// its job database should still be able to start and say so. Two files also mean
// two independent failure domains: a corrupt job table cannot take the media
// index with it.
//
// Everything here is bounded. Every list is paged, every history is pruned, and
// no query returns an unbounded slice, because the console's own bookkeeping is
// exactly the kind of thing that grows without anyone noticing.
package hubstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	_ "modernc.org/sqlite"
)

// SchemaVersion is the on-disk schema this build understands.
const SchemaVersion = 1

// Errors returned by this package.
var (
	// ErrNotFound means the requested row does not exist.
	ErrNotFound = errors.New("hubstore: not found")
	// ErrConflict means a write lost a race with another write: a job was
	// already claimed, or a state transition was refused.
	ErrConflict = errors.New("hubstore: conflicting write")
)

// Limits bounds every read and the growth of every table.
type Limits struct {
	MaxOpenJobs int
	MaxHistory  int
	AuditDays   int
}

// DefaultLimits returns the shipped bounds.
func DefaultLimits() Limits {
	return Limits{MaxOpenJobs: 32, MaxHistory: 5000, AuditDays: 90}
}

// Store is the console's durable state.
type Store struct {
	db     *sql.DB
	path   string
	limits Limits
}

// Open opens (creating if needed) the console database.
//
// The parent directory is created here, unlike the read-only media index: this
// file is the console's own state and the console is its only writer, so
// creating it is expected rather than suspicious.
func Open(path string, limits Limits) (*Store, error) {
	if strings.ContainsAny(path, "?#\x00\n\r") {
		return nil, fmt.Errorf("hubstore: path %q contains a character that cannot be used "+
			"in a SQLite DSN", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return nil, fmt.Errorf("hubstore: cannot create state directory: %w", err)
	}
	if limits.MaxOpenJobs <= 0 {
		limits = DefaultLimits()
	}

	dsn := "file:" + url.PathEscape(abs) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection. The console is not a database server, and a single
	// connection removes a whole class of lock contention on a small machine.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("hubstore: cannot open %s: %w", abs, err)
	}

	s := &Store{db: db, path: abs, limits: limits}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Limits returns the store's bounds.
func (s *Store) Limits() Limits { return s.limits }

// schema is the full DDL. It is applied inside one transaction and is written to
// be re-runnable, so an interrupted first start leaves a usable file.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS jobs (
		id           TEXT PRIMARY KEY,
		op           TEXT NOT NULL,
		state        TEXT NOT NULL,
		principal    TEXT NOT NULL,
		params       TEXT NOT NULL DEFAULT '',
		target       TEXT NOT NULL DEFAULT '',
		created_at   INTEGER NOT NULL,
		started_at   INTEGER NOT NULL DEFAULT 0,
		ended_at     INTEGER NOT NULL DEFAULT 0,
		heartbeat_at INTEGER NOT NULL DEFAULT 0,
		pid          INTEGER NOT NULL DEFAULT 0,
		pgid         INTEGER NOT NULL DEFAULT 0,
		progress     INTEGER NOT NULL DEFAULT 0,
		total        INTEGER NOT NULL DEFAULT 0,
		unit         TEXT NOT NULL DEFAULT '',
		stop_reason  TEXT NOT NULL DEFAULT '',
		err          TEXT NOT NULL DEFAULT '',
		verified     INTEGER NOT NULL DEFAULT 0,
		approval_id  TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_jobs_state  ON jobs(state, created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_jobs_recent ON jobs(created_at DESC, id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_jobs_open   ON jobs(state) WHERE state IN
		('queued','running','waiting','paused','interrupted','unknown')`,
	`CREATE TABLE IF NOT EXISTS job_events (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id  TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
		at      INTEGER NOT NULL,
		from_st TEXT NOT NULL DEFAULT '',
		to_st   TEXT NOT NULL DEFAULT '',
		note    TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_job_events ON job_events(job_id, id)`,
	`CREATE TABLE IF NOT EXISTS audit (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		at        INTEGER NOT NULL,
		principal TEXT NOT NULL,
		action    TEXT NOT NULL,
		op        TEXT NOT NULL DEFAULT '',
		target    TEXT NOT NULL DEFAULT '',
		params    TEXT NOT NULL DEFAULT '',
		outcome   TEXT NOT NULL,
		note      TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_at ON audit(at DESC, id DESC)`,
	`CREATE TABLE IF NOT EXISTS modes (
		mode       TEXT PRIMARY KEY,
		source     TEXT NOT NULL,
		reason     TEXT NOT NULL DEFAULT '',
		set_at     INTEGER NOT NULL,
		expires_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS approvals (
		id         TEXT PRIMARY KEY,
		op         TEXT NOT NULL,
		principal  TEXT NOT NULL,
		target     TEXT NOT NULL DEFAULT '',
		params     TEXT NOT NULL DEFAULT '',
		granted_by TEXT NOT NULL,
		granted_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		used_at    INTEGER NOT NULL DEFAULT 0,
		used_by    TEXT NOT NULL DEFAULT '',
		one_shot   INTEGER NOT NULL DEFAULT 1
	)`,
	`CREATE INDEX IF NOT EXISTS idx_approvals_lookup ON approvals(op, principal, expires_at)`,
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, stmt := range schema {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("hubstore: applying schema: %w", err)
		}
	}
	var cur int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&cur); err != nil {
		return err
	}
	if cur > SchemaVersion {
		return fmt.Errorf("hubstore: %s was written by a newer console (schema %d, this "+
			"build understands %d); use a matching binary or move the file aside",
			s.path, cur, SchemaVersion)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES('schema',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprint(SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

// Job is one asynchronous request.
type Job struct {
	ID         string          `json:"id"`
	Op         string          `json:"op"`
	State      domain.JobState `json:"state"`
	Principal  string          `json:"principal"`
	Params     string          `json:"params"`
	Target     string          `json:"target"`
	CreatedAt  time.Time       `json:"created_at"`
	StartedAt  time.Time       `json:"started_at,omitempty"`
	EndedAt    time.Time       `json:"ended_at,omitempty"`
	Heartbeat  time.Time       `json:"heartbeat_at,omitempty"`
	PID        int             `json:"pid,omitempty"`
	PGID       int             `json:"pgid,omitempty"`
	Progress   int64           `json:"progress"`
	Total      int64           `json:"total"`
	Unit       string          `json:"unit,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
	Err        string          `json:"error,omitempty"`
	Verified   bool            `json:"verified"`
	ApprovalID string          `json:"approval_id,omitempty"`
}

// JobEvent is one recorded state change.
type JobEvent struct {
	At   time.Time `json:"at"`
	From string    `json:"from,omitempty"`
	To   string    `json:"to,omitempty"`
	Note string    `json:"note,omitempty"`
}

// NewJob describes a job to create.
type NewJob struct {
	ID         string
	Op         string
	Principal  string
	Params     string
	Target     string
	ApprovalID string
	// Async jobs start queued; a job that is admitted immediately may start
	// running. The caller decides, because only the caller knows whether it
	// already holds the resources.
	StartQueued bool
}

// CreateJob records a new job. It refuses to create one when the open-job cap is
// reached, so the queue cannot grow without bound.
func (s *Store) CreateJob(ctx context.Context, nj NewJob) (*Job, error) {
	state := domain.JobRunning
	if nj.StartQueued {
		state = domain.JobQueued
	}
	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var open int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE state IN ('queued','running','waiting','paused','interrupted','unknown')`,
	).Scan(&open); err != nil {
		return nil, err
	}
	if open >= s.limits.MaxOpenJobs {
		return nil, fmt.Errorf("%w: %d jobs are already open and the cap is %d; finish or "+
			"cancel some before starting more", ErrConflict, open, s.limits.MaxOpenJobs)
	}

	j := &Job{
		ID: nj.ID, Op: nj.Op, State: state, Principal: nj.Principal,
		Params: nj.Params, Target: nj.Target, ApprovalID: nj.ApprovalID,
		CreatedAt: time.Unix(now, 0),
	}
	if state == domain.JobRunning {
		j.StartedAt = time.Unix(now, 0)
		j.Heartbeat = time.Unix(now, 0)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO jobs(id,op,state,principal,params,target,created_at,started_at,heartbeat_at,approval_id)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Op, string(j.State), j.Principal, j.Params, j.Target, now,
		unixOrZero(j.StartedAt), unixOrZero(j.Heartbeat), j.ApprovalID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job_events(job_id,at,from_st,to_st,note) VALUES(?,?,?,?,?)`,
		j.ID, now, "", string(j.State), "created"); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return j, nil
}

// Job fetches one job by ID.
func (s *Store) Job(ctx context.Context, id string) (*Job, error) {
	row := s.db.QueryRowContext(ctx, jobColumns+` WHERE id = ?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: job %q", ErrNotFound, id)
	}
	return j, err
}

const jobColumns = `SELECT id,op,state,principal,params,target,created_at,started_at,
	ended_at,heartbeat_at,pid,pgid,progress,total,unit,stop_reason,err,verified,approval_id FROM jobs`

type rowScanner interface{ Scan(...any) error }

func scanJob(r rowScanner) (*Job, error) {
	var (
		j                           Job
		state                       string
		created, started, ended, hb int64
		verified                    int
	)
	if err := r.Scan(&j.ID, &j.Op, &state, &j.Principal, &j.Params, &j.Target,
		&created, &started, &ended, &hb, &j.PID, &j.PGID, &j.Progress, &j.Total,
		&j.Unit, &j.StopReason, &j.Err, &verified, &j.ApprovalID); err != nil {
		return nil, err
	}
	j.State = domain.JobState(state)
	j.CreatedAt = time.Unix(created, 0)
	j.StartedAt = unixTime(started)
	j.EndedAt = unixTime(ended)
	j.Heartbeat = unixTime(hb)
	j.Verified = verified != 0
	return &j, nil
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func unixTime(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// JobFilter selects jobs for a listing.
type JobFilter struct {
	State domain.JobState
	Op    string
	// Open restricts the listing to jobs that still hold a slot.
	Open bool
	// Limit and Offset page the result. Limit is required.
	Limit  int
	Offset int
}

// Jobs lists jobs, newest first, paged.
func (s *Store) Jobs(ctx context.Context, f JobFilter) ([]Job, error) {
	if f.Limit <= 0 {
		return nil, fmt.Errorf("hubstore: a job listing needs a positive limit")
	}
	if f.Limit > 500 {
		f.Limit = 500
	}
	var (
		where []string
		args  []any
	)
	if f.State != "" {
		where = append(where, "state = ?")
		args = append(args, string(f.State))
	}
	if f.Op != "" {
		where = append(where, "op = ?")
		args = append(args, f.Op)
	}
	if f.Open {
		where = append(where, `state IN ('queued','running','waiting','paused','interrupted','unknown')`)
	}
	q := jobColumns
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, f.Limit, f.Offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Job, 0, f.Limit)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// CountOpen returns how many jobs currently hold a slot.
func (s *Store) CountOpen(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE state IN ('queued','running','waiting','paused','interrupted','unknown')`).Scan(&n)
	return n, err
}

// CountByState returns job counts keyed by state.
func (s *Store) CountByState(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// Transition moves a job to a new state, refusing an illegal move.
//
// The check is done against the current row inside a transaction, so two writers
// cannot both believe they moved the job. An illegal transition is an error
// rather than a silent write: a job that "completed" from "queued" without ever
// running is exactly the kind of record that makes a console untrustworthy.
func (s *Store) Transition(ctx context.Context, id string, to domain.JobState, note string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var cur string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, id).Scan(&cur); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: job %q", ErrNotFound, id)
		}
		return err
	}
	if err := domain.JobState(cur).Transition(to); err != nil {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	now := time.Now().Unix()
	sets := []string{"state = ?"}
	args := []any{string(to)}
	if to == domain.JobRunning {
		sets = append(sets, "started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END",
			"heartbeat_at = ?")
		args = append(args, now, now)
	}
	if to.Terminal() {
		sets = append(sets, "ended_at = ?")
		args = append(args, now)
	}
	args = append(args, id)
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job_events(job_id,at,from_st,to_st,note) VALUES(?,?,?,?,?)`,
		id, now, cur, string(to), note); err != nil {
		return err
	}
	return tx.Commit()
}

// Progress records an instance identity and progress for a running job.
//
// pid and pgid are recorded so that a restart can find the process group the
// console started. They are evidence, not authority: reconciliation checks the
// process actually exists and belongs to this application before believing it.
func (s *Store) Progress(ctx context.Context, id string, pid, pgid int, done, total int64, unit string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET heartbeat_at = ?, pid = ?, pgid = ?, progress = ?, total = ?,
		 unit = CASE WHEN ? <> '' THEN ? ELSE unit END
		 WHERE id = ? AND state IN ('running','waiting')`,
		time.Now().Unix(), pid, pgid, done, total, unit, unit, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Not an error: a job can finish between the caller's read and this
		// write, and a stale progress write must not resurrect it.
		return nil
	}
	return nil
}

// Finish records the end of a job with its outcome.
func (s *Store) Finish(ctx context.Context, id string, state domain.JobState, stopReason, errMsg string, verified bool) error {
	if !state.Terminal() {
		return fmt.Errorf("hubstore: %q is not a terminal state", state)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var cur string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, id).Scan(&cur); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: job %q", ErrNotFound, id)
		}
		return err
	}
	if err := domain.JobState(cur).Transition(state); err != nil {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	now := time.Now().Unix()
	v := 0
	if verified {
		v = 1
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = ?, ended_at = ?, stop_reason = ?, err = ?, verified = ? WHERE id = ?`,
		string(state), now, stopReason, errMsg, v, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job_events(job_id,at,from_st,to_st,note) VALUES(?,?,?,?,?)`,
		id, now, cur, string(state), stopReason); err != nil {
		return err
	}
	return tx.Commit()
}

// JobEvents returns a job's recorded history, oldest first, capped.
func (s *Store) JobEvents(ctx context.Context, id string, limit int) ([]JobEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, from_st, to_st, note FROM job_events WHERE job_id = ? ORDER BY id LIMIT ?`,
		id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobEvent
	for rows.Next() {
		var (
			at       int64
			from, to string
			note     string
		)
		if err := rows.Scan(&at, &from, &to, &note); err != nil {
			return nil, err
		}
		out = append(out, JobEvent{At: time.Unix(at, 0), From: from, To: to, Note: note})
	}
	return out, rows.Err()
}

// Reconcile marks jobs that were running when the console died.
//
// It is called once at startup, before anything can be admitted. A job recorded
// as running whose process is gone becomes interrupted - not failed, and
// certainly not completed. The distinction matters: "interrupted" tells the
// operator the outcome is unknown and the work must be re-run, whereas "failed"
// would suggest the attempt ran to a conclusion.
//
// The predicate is supplied by the caller so that this package does not need to
// know how to check a process, keeping the database layer free of /proc.
func (s *Store) Reconcile(ctx context.Context, alive func(pid, pgid int) bool, now time.Time) (interrupted int, orphaned []Job, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()

	// The full row is read here rather than fetched afterwards through the
	// pool. The pool holds a single connection by design, so a query issued
	// through it while this transaction is open would wait for a connection
	// that cannot be released until the transaction ends: a self-deadlock that
	// presents as a slow console rather than as an error.
	rows, err := tx.QueryContext(ctx, jobColumns+` WHERE state IN ('running','waiting')`)
	if err != nil {
		return 0, nil, err
	}
	var cands []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return 0, nil, err
		}
		cands = append(cands, *j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	ts := now.Unix()
	for _, j := range cands {
		if j.PID > 0 && alive(j.PID, j.PGID) {
			// The process survived the console restart. It is an orphan: the
			// console can see it but did not start it in this life, so it has
			// no way to supervise it. It is reported for an operator to decide,
			// and never silently adopted or killed - killing a process group
			// whose provenance is uncertain could hit something that is not
			// ours.
			orphaned = append(orphaned, j)
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state = ?, ended_at = ?, stop_reason = ?
			 WHERE id = ? AND state IN ('running','waiting')`,
			string(domain.JobInterrupted), ts, "console restarted while this job was open", j.ID); err != nil {
			return 0, nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO job_events(job_id,at,from_st,to_st,note) VALUES(?,?,?,?,?)`,
			j.ID, ts, string(j.State), string(domain.JobInterrupted),
			"console restarted while this job was open"); err != nil {
			return 0, nil, err
		}
		interrupted++
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	return interrupted, orphaned, nil
}

// PruneHistory deletes finished jobs beyond the history cap, oldest first. It is
// what keeps the jobs table from growing forever on a machine that never gets
// reinstalled. It never touches an open job.
func (s *Store) PruneHistory(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM jobs WHERE id IN (
			SELECT id FROM jobs
			WHERE state IN ('completed','failed','cancelled')
			ORDER BY created_at DESC
			LIMIT -1 OFFSET ?
		)`, s.limits.MaxHistory)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneAudit deletes audit rows older than the retention window.
func (s *Store) PruneAudit(ctx context.Context, now time.Time) (int64, error) {
	cutoff := now.AddDate(0, 0, -s.limits.AuditDays).Unix()
	res, err := s.db.ExecContext(ctx, `DELETE FROM audit WHERE at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// AuditEntry is one recorded action.
type AuditEntry struct {
	At        time.Time `json:"at"`
	Principal string    `json:"principal"`
	Action    string    `json:"action"`
	Op        string    `json:"op,omitempty"`
	Target    string    `json:"target,omitempty"`
	Params    string    `json:"params,omitempty"`
	Outcome   string    `json:"outcome"`
	Note      string    `json:"note,omitempty"`
}

// Record appends an audit row. Failure to audit is reported to the caller; it is
// never swallowed, because an action with no audit trail is an action nobody can
// account for.
func (s *Store) Record(ctx context.Context, e AuditEntry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit(at,principal,action,op,target,params,outcome,note)
		 VALUES(?,?,?,?,?,?,?,?)`,
		time.Now().Unix(), e.Principal, e.Action, e.Op, e.Target, e.Params, e.Outcome, e.Note)
	return err
}

// Audit lists recent entries, newest first, paged.
func (s *Store) Audit(ctx context.Context, limit, offset int) ([]AuditEntry, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("hubstore: an audit listing needs a positive limit")
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT at,principal,action,op,target,params,outcome,note FROM audit
		 ORDER BY at DESC, id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AuditEntry, 0, limit)
	for rows.Next() {
		var (
			at int64
			e  AuditEntry
		)
		if err := rows.Scan(&at, &e.Principal, &e.Action, &e.Op, &e.Target, &e.Params,
			&e.Outcome, &e.Note); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Modes
// ---------------------------------------------------------------------------

// SetMode records a mode. Setting a mode that is already present replaces it, so
// that "set watching for 2h" twice extends rather than errors.
func (s *Store) SetMode(ctx context.Context, st domain.ModeState) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO modes(mode,source,reason,set_at,expires_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(mode) DO UPDATE SET source=excluded.source, reason=excluded.reason,
		   set_at=excluded.set_at, expires_at=excluded.expires_at`,
		string(st.Mode), st.Source, st.Reason, st.SetAt.Unix(), unixOrZero(st.ExpiresAt))
	return err
}

// ClearMode removes a mode.
func (s *Store) ClearMode(ctx context.Context, m domain.Mode) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM modes WHERE mode = ?`, string(m))
	return err
}

// Modes returns every stored mode, including expired ones. The caller decides
// what is active: keeping expired rows means a restart cannot resurrect a mode
// that had already lapsed, and the console can report that it did so.
func (s *Store) Modes(ctx context.Context) (domain.ModeSet, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT mode,source,reason,set_at,expires_at FROM modes ORDER BY mode`)
	if err != nil {
		return domain.ModeSet{}, err
	}
	defer rows.Close()
	var out domain.ModeSet
	for rows.Next() {
		var (
			mode, source, reason string
			setAt, expiresAt     int64
		)
		if err := rows.Scan(&mode, &source, &reason, &setAt, &expiresAt); err != nil {
			return domain.ModeSet{}, err
		}
		out.States = append(out.States, domain.ModeState{
			Mode:      domain.Mode(mode),
			Source:    source,
			Reason:    reason,
			SetAt:     time.Unix(setAt, 0),
			ExpiresAt: unixTime(expiresAt),
		})
	}
	return out, rows.Err()
}

// PurgeExpiredModes deletes modes whose expiry has passed, so that a lapsed
// window does not linger in the table forever.
func (s *Store) PurgeExpiredModes(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM modes WHERE expires_at <> 0 AND expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// Approvals
// ---------------------------------------------------------------------------

// Approval is a recorded permission for one operation.
type Approval struct {
	ID        string    `json:"id"`
	Op        string    `json:"op"`
	Principal string    `json:"principal"`
	Target    string    `json:"target"`
	Params    string    `json:"params"`
	GrantedBy string    `json:"granted_by"`
	GrantedAt time.Time `json:"granted_at"`
	ExpiresAt time.Time `json:"expires_at"`
	UsedAt    time.Time `json:"used_at,omitempty"`
	UsedBy    string    `json:"used_by,omitempty"`
	OneShot   bool      `json:"one_shot"`
}

// GrantApproval records an approval.
//
// grantedBy is recorded separately from principal on purpose. An agent asking for
// permission and an agent granting it must never be the same row: the caller
// layer checks that the granter is a human or the local operator, and this table
// is where the evidence lives.
func (s *Store) GrantApproval(ctx context.Context, a Approval) error {
	oneShot := 0
	if a.OneShot {
		oneShot = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO approvals(id,op,principal,target,params,granted_by,granted_at,expires_at,one_shot)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Op, a.Principal, a.Target, a.Params, a.GrantedBy,
		a.GrantedAt.Unix(), a.ExpiresAt.Unix(), oneShot)
	return err
}

// ConsumeApproval finds a valid approval for a call and marks it used.
//
// The match is exact on operation, principal, target and parameters. A parameter
// change must invalidate an approval - otherwise "approve a 10-file scan" becomes
// "approve a scan", and the bound the operator agreed to is gone.
func (s *Store) ConsumeApproval(ctx context.Context, op, principal, target, params string, now time.Time) (*Approval, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		a              Approval
		grantedAt, exp int64
		usedAt         int64
		oneShot        int
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id,op,principal,target,params,granted_by,granted_at,expires_at,used_at,used_by,one_shot
		 FROM approvals
		 WHERE op=? AND principal=? AND target=? AND params=? AND expires_at > ? AND used_at = 0
		 ORDER BY granted_at LIMIT 1`,
		op, principal, target, params, now.Unix(),
	).Scan(&a.ID, &a.Op, &a.Principal, &a.Target, &a.Params, &a.GrantedBy,
		&grantedAt, &exp, &usedAt, &a.UsedBy, &oneShot)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no valid approval for %s", ErrNotFound, op)
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE approvals SET used_at = ?, used_by = ? WHERE id = ?`, now.Unix(), principal, a.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	a.GrantedAt = time.Unix(grantedAt, 0)
	a.ExpiresAt = time.Unix(exp, 0)
	a.UsedAt = time.Unix(now.Unix(), 0)
	a.UsedBy = principal
	a.OneShot = oneShot != 0
	return &a, nil
}

// Approvals lists current approvals, newest first.
func (s *Store) Approvals(ctx context.Context, limit int) ([]Approval, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,op,principal,target,params,granted_by,granted_at,expires_at,used_at,used_by,one_shot
		 FROM approvals ORDER BY granted_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var (
			a             Approval
			ga, exp, used int64
			oneShot       int
		)
		if err := rows.Scan(&a.ID, &a.Op, &a.Principal, &a.Target, &a.Params, &a.GrantedBy,
			&ga, &exp, &used, &a.UsedBy, &oneShot); err != nil {
			return nil, err
		}
		a.GrantedAt = time.Unix(ga, 0)
		a.ExpiresAt = time.Unix(exp, 0)
		a.UsedAt = unixTime(used)
		a.OneShot = oneShot != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// RevokeApproval deletes an approval.
func (s *Store) RevokeApproval(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM approvals WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: approval %q", ErrNotFound, id)
	}
	return nil
}

// IntegrityCheck runs a quick SQLite integrity check.
func (s *Store) IntegrityCheck(ctx context.Context) (string, error) {
	var out string
	err := s.db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&out)
	return out, err
}
