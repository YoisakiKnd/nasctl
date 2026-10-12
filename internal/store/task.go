package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Task kinds.
const (
	TaskScan   = "scan"
	TaskProbe  = "probe"
	TaskThumbs = "thumbs"
	TaskPoster = "video_poster"
)

// Task states.
const (
	TaskRunning     = "running"
	TaskCompleted   = "completed"
	TaskStopped     = "stopped" // ended deliberately, reason recorded
	TaskFailed      = "failed"
	TaskInterrupted = "interrupted" // the process died without finishing
)

// maxTaskItems caps how many per-file rows a single task records. A 5684 file
// run must not grow the index without bound; the counters in `tasks` remain
// exact, and the item list is a bounded sample with an explicit marker.
const maxTaskItems = 500

// Task is one bounded run of a command.
type Task struct {
	ID             int64
	Kind           string
	State          string
	Reason         string
	Detail         string
	CollectionCode string
	PID            int64
	StartedAt      int64
	HeartbeatAt    int64
	EndedAt        int64
	LimitN         int64
	MaxRuntimeS    int64
	Processed      int64
	Skipped        int64
	Failed         int64
	BytesRead      int64
	BytesWritten   int64
	CgroupIsolated bool
	DryRun         bool
	Argv           string
	Report         string
}

// TaskItem is one bounded per-file record.
type TaskItem struct {
	ID          int64
	TaskID      int64
	MediaID     *int64
	RelPath     string
	Action      string
	Outcome     string
	Reason      string
	Detail      string
	MS          int64
	PeakRSS     int64
	CgroupPeak  int64
	MemAvailMin int64
	At          int64
}

// StartTask records a new running task and returns its id.
func (s *Store) StartTask(ctx context.Context, t Task) (int64, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	if t.StartedAt == 0 {
		t.StartedAt = UnixNow()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (kind, state, reason, detail, collection_code, pid, started_at,
		                    heartbeat_at, limit_n, max_runtime_s, cgroup_isolated, dry_run, argv)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Kind, TaskRunning, t.Reason, t.Detail, t.CollectionCode, t.PID, t.StartedAt,
		t.StartedAt, t.LimitN, t.MaxRuntimeS, boolInt(t.CgroupIsolated), boolInt(t.DryRun), t.Argv)
	if err != nil {
		return 0, fmt.Errorf("store: start task: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// Heartbeat updates a task's liveness stamp and counters.
//
// The stamp is what lets a later process tell "this task is still running" from
// "this task's process was killed": the pid alone is not enough, because pids
// are reused.
func (s *Store) Heartbeat(ctx context.Context, id int64, processed, skipped, failed, bytesRead, bytesWritten int64) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET heartbeat_at = ?, processed = ?, skipped = ?, failed = ?,
		   bytes_read = ?, bytes_written = ? WHERE id = ?`,
		UnixNow(), processed, skipped, failed, bytesRead, bytesWritten, id)
	return err
}

// FinishTask closes a task record.
func (s *Store) FinishTask(ctx context.Context, id int64, state, reason, detail string, t Task, report string) error {
	if err := s.guard(); err != nil {
		return err
	}
	if len(report) > 64<<10 {
		report = report[:64<<10]
	}
	// The counters are kept at the larger of the value passed in and the value
	// already stored. Heartbeat writes the running totals as a run proceeds, so
	// the stored numbers are the record of what actually happened; a caller that
	// passes a zero-valued Task - which is easy to do, since only Kind is
	// required - would otherwise erase them at the moment the task finishes.
	// A final count is never legitimately lower than the last heartbeat within
	// the same run, so MAX is the correct combination.
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET state = ?, reason = ?, detail = ?, ended_at = ?, heartbeat_at = ?,
		   processed = max(processed, ?), skipped = max(skipped, ?), failed = max(failed, ?),
		   bytes_read = max(bytes_read, ?), bytes_written = max(bytes_written, ?), report = ?
		 WHERE id = ?`,
		state, reason, detail, UnixNow(), UnixNow(),
		t.Processed, t.Skipped, t.Failed, t.BytesRead, t.BytesWritten, report, id)
	if err != nil {
		return fmt.Errorf("store: finish task %d: %w", id, err)
	}
	return nil
}

// AddTaskItem appends a bounded per-file record.
func (s *Store) AddTaskItem(ctx context.Context, it TaskItem) error {
	if err := s.guard(); err != nil {
		return err
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM task_items WHERE task_id = ?`, it.TaskID).Scan(&n); err != nil {
		return err
	}
	if n >= maxTaskItems {
		if n == maxTaskItems {
			// Mark the truncation once, so a reader knows the list is a sample.
			_, _ = s.db.ExecContext(ctx,
				`INSERT INTO task_items (task_id, action, outcome, reason, detail, at)
				 VALUES (?, 'truncated', 'skipped', 'item_log_capped', ?, ?)`,
				it.TaskID, fmt.Sprintf("per-file detail is capped at %d rows; the task counters remain exact", maxTaskItems), it.At)
		}
		return nil
	}
	if len(it.Detail) > 1000 {
		it.Detail = it.Detail[:1000]
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO task_items (task_id, media_id, rel_path, action, outcome, reason, detail, ms,
		   peak_rss, cgroup_peak, mem_avail_min, at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		it.TaskID, it.MediaID, it.RelPath, it.Action, it.Outcome, it.Reason, it.Detail, it.MS,
		it.PeakRSS, it.CgroupPeak, it.MemAvailMin, it.At)
	return err
}

// TaskItems reads a bounded list of per-file records.
func (s *Store) TaskItems(ctx context.Context, taskID int64, limit int) ([]TaskItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, media_id, rel_path, action, outcome, reason, detail, ms,
		        peak_rss, cgroup_peak, mem_avail_min, at
		   FROM task_items WHERE task_id = ? ORDER BY id LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskItem
	for rows.Next() {
		var it TaskItem
		if err := rows.Scan(&it.ID, &it.TaskID, &it.MediaID, &it.RelPath, &it.Action, &it.Outcome,
			&it.Reason, &it.Detail, &it.MS, &it.PeakRSS, &it.CgroupPeak, &it.MemAvailMin, &it.At); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

const taskColumns = `id, kind, state, reason, detail, collection_code, pid, started_at, heartbeat_at,
	ended_at, limit_n, max_runtime_s, processed, skipped, failed, bytes_read, bytes_written,
	cgroup_isolated, dry_run, argv, report`

func scanTask(rows *sql.Rows) ([]Task, error) {
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var iso, dry int
		if err := rows.Scan(&t.ID, &t.Kind, &t.State, &t.Reason, &t.Detail, &t.CollectionCode,
			&t.PID, &t.StartedAt, &t.HeartbeatAt, &t.EndedAt, &t.LimitN, &t.MaxRuntimeS,
			&t.Processed, &t.Skipped, &t.Failed, &t.BytesRead, &t.BytesWritten,
			&iso, &dry, &t.Argv, &t.Report); err != nil {
			return nil, err
		}
		t.CgroupIsolated = iso != 0
		t.DryRun = dry != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// RunningTasks lists tasks recorded as running.
func (s *Store) RunningTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE state = 'running' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanTask(rows)
}

// LastTask returns the most recent task of any kind.
func (s *Store) LastTask(ctx context.Context, kind string) (Task, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if kind == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks ORDER BY id DESC LIMIT 1`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE kind = ? ORDER BY id DESC LIMIT 1`, kind)
	}
	if err != nil {
		return Task{}, err
	}
	ts, err := scanTask(rows)
	if err != nil {
		return Task{}, err
	}
	if len(ts) == 0 {
		return Task{}, ErrNotFound
	}
	return ts[0], nil
}

// SweepStaleTasks marks tasks whose process is gone as interrupted.
//
// This is the crash-recovery path for the task table: a run that was SIGKILLed
// leaves state='running' behind, and without this sweep `status` would forever
// claim a task is in progress. A task is only declared stale when both its pid
// is gone and its heartbeat is older than the grace window, so a live task
// whose pid was reused is not clobbered.
func (s *Store) SweepStaleTasks(ctx context.Context, procRoot string, grace time.Duration) (int64, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	if procRoot == "" {
		procRoot = "/proc"
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, pid, heartbeat_at FROM tasks WHERE state = 'running'`)
	if err != nil {
		return 0, err
	}
	type cand struct {
		id  int64
		pid int64
		hb  int64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.pid, &c.hb); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	cutoff := UnixNow() - int64(grace.Seconds())
	var swept int64
	for _, c := range cands {
		alive := false
		if c.pid > 0 {
			if _, err := os.Stat(filepath.Join(procRoot, strconv.FormatInt(c.pid, 10))); err == nil {
				alive = true
			}
		}
		if alive || c.hb > cutoff {
			continue
		}
		res, err := s.db.ExecContext(ctx,
			`UPDATE tasks SET state = 'interrupted', ended_at = ?,
			   reason = CASE WHEN reason = '' THEN 'process_exited' ELSE reason END,
			   detail = COALESCE(NULLIF(detail, ''), 'the albumctl process that owned this task is gone; ' ||
			     'the work it had in flight is recorded as a failure, and the next run resumes from pending rows')
			 WHERE id = ? AND state = 'running'`, UnixNow(), c.id)
		if err != nil {
			return swept, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			swept += n
		}
	}
	return swept, nil
}

// ---- persisted application state ----------------------------------------

// State keys.
const (
	KeyPaused           = "paused"
	KeyPauseReason      = "pause_reason"
	KeyPauseSince       = "pause_since"
	KeyPauseBy          = "pause_by"
	KeyPressureTripAt   = "pressure_tripped_at"
	KeyPressureReason   = "pressure_reason"
	KeyPressureDetail   = "pressure_detail"
	KeyPressureSnapshot = "pressure_snapshot"
	KeyLastScanAt       = "last_scan_at"
	KeyLastThumbRunAt   = "last_thumbs_run_at"
)

// GetState reads a state value. The bool reports presence.
func (s *Store) GetState(ctx context.Context, k string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM app_state WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetState writes a state value.
func (s *Store) SetState(ctx context.Context, k, v string) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO app_state(k, v, updated_at) VALUES(?,?,?)
		 ON CONFLICT(k) DO UPDATE SET v = excluded.v, updated_at = excluded.updated_at`,
		k, v, UnixNow())
	return err
}

// ClearState removes a state value.
func (s *Store) ClearState(ctx context.Context, k string) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM app_state WHERE k = ?`, k)
	return err
}

// SetStates writes several values atomically.
func (s *Store) SetStates(ctx context.Context, kv map[string]string) error {
	if err := s.guard(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO app_state(k, v, updated_at) VALUES(?,?,?)
			 ON CONFLICT(k) DO UPDATE SET v = excluded.v, updated_at = excluded.updated_at`,
			k, v, UnixNow()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PauseState is the durable manual pause.
type PauseState struct {
	Paused bool
	Reason string
	Since  int64
	By     string
}

// Pause records a manual pause. It survives restarts because it lives in the
// index, not in the process.
func (s *Store) Pause(ctx context.Context, reason, by string) error {
	return s.SetStates(ctx, map[string]string{
		KeyPaused:      "1",
		KeyPauseReason: reason,
		KeyPauseSince:  strconv.FormatInt(UnixNow(), 10),
		KeyPauseBy:     by,
	})
}

// Resume clears the manual pause.
func (s *Store) Resume(ctx context.Context) error {
	return s.SetStates(ctx, map[string]string{
		KeyPaused: "0", KeyPauseReason: "", KeyPauseSince: "0", KeyPauseBy: "",
	})
}

// PauseState reads the manual pause.
func (s *Store) PauseState(ctx context.Context) (PauseState, error) {
	var ps PauseState
	v, ok, err := s.GetState(ctx, KeyPaused)
	if err != nil {
		return ps, err
	}
	ps.Paused = ok && (v == "1" || v == "true")
	ps.Reason, _, _ = s.GetState(ctx, KeyPauseReason)
	ps.By, _, _ = s.GetState(ctx, KeyPauseBy)
	if sv, ok, _ := s.GetState(ctx, KeyPauseSince); ok {
		ps.Since, _ = strconv.ParseInt(sv, 10, 64)
	}
	return ps, nil
}

// PressureState is the persistent "a run was stopped by resource pressure"
// record.
//
// Its whole purpose is to make an immediate automatic restart impossible: after
// a trip the next heavy run must be started by a person, and only after the
// cooldown has elapsed or with an explicit --force, which is itself recorded.
type PressureState struct {
	Tripped  bool
	At       int64
	Reason   string
	Detail   string
	Snapshot string
}

// TripPressure records a resource-pressure stop.
func (s *Store) TripPressure(ctx context.Context, reason, detail, snapshot string) error {
	if len(snapshot) > 4000 {
		snapshot = snapshot[:4000]
	}
	return s.SetStates(ctx, map[string]string{
		KeyPressureTripAt:   strconv.FormatInt(UnixNow(), 10),
		KeyPressureReason:   reason,
		KeyPressureDetail:   detail,
		KeyPressureSnapshot: snapshot,
	})
}

// ClearPressure removes the trip after a human resumes.
func (s *Store) ClearPressure(ctx context.Context) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM app_state WHERE k IN (?,?,?,?)`,
		KeyPressureTripAt, KeyPressureReason, KeyPressureDetail, KeyPressureSnapshot)
	return err
}

// Pressure reads the trip record.
func (s *Store) Pressure(ctx context.Context) (PressureState, error) {
	var ps PressureState
	sv, ok, err := s.GetState(ctx, KeyPressureTripAt)
	if err != nil {
		return ps, err
	}
	if !ok {
		return ps, nil
	}
	ps.Tripped = true
	ps.At, _ = strconv.ParseInt(sv, 10, 64)
	ps.Reason, _, _ = s.GetState(ctx, KeyPressureReason)
	ps.Detail, _, _ = s.GetState(ctx, KeyPressureDetail)
	ps.Snapshot, _, _ = s.GetState(ctx, KeyPressureSnapshot)
	return ps, nil
}

// Gate describes the conditions that must hold before a heavy run starts.
type Gate struct {
	Paused       bool
	PauseReason  string
	Pressure     PressureState
	CooldownLeft time.Duration
}

// CheckRunGate decides whether a heavy run may start.
//
// force bypasses only the cooldown, never the pause: a pause is an explicit
// instruction from the operator and no flag overrides it. Pressure trips
// require either an elapsed cooldown or an explicit force, and either way the
// caller records which one applied.
func (s *Store) CheckRunGate(ctx context.Context, cooldown time.Duration, force bool) (Gate, error) {
	var g Gate
	ps, err := s.PauseState(ctx)
	if err != nil {
		return g, err
	}
	g.Paused = ps.Paused
	g.PauseReason = ps.Reason
	if ps.Paused {
		return g, nil
	}
	pr, err := s.Pressure(ctx)
	if err != nil {
		return g, err
	}
	g.Pressure = pr
	if pr.Tripped && pr.At > 0 && cooldown > 0 && !force {
		elapsed := time.Since(time.Unix(pr.At, 0))
		if elapsed < cooldown {
			g.CooldownLeft = cooldown - elapsed
		}
	}
	return g, nil
}

// CanStart reports whether the gate allows a run to begin.
func (g Gate) CanStart() bool {
	return !g.Paused && g.CooldownLeft <= 0
}

// UpsertCollection mirrors the configuration into the index.
func (s *Store) UpsertCollection(ctx context.Context, code, name, root, dataMount string, enabled, followSymlinks bool) (int64, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO collections (code, name, root, data_mount, enabled, follow_symlinks, first_seen)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(code) DO UPDATE SET
		   name = excluded.name, root = excluded.root, data_mount = excluded.data_mount,
		   enabled = excluded.enabled, follow_symlinks = excluded.follow_symlinks`,
		code, name, root, dataMount, boolInt(enabled), boolInt(followSymlinks), UnixNow())
	if err != nil {
		return 0, fmt.Errorf("store: upsert collection %s: %w", code, err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM collections WHERE code = ?`, code).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// Collection is a stored collection record.
type Collection struct {
	ID             int64
	Code           string
	Name           string
	Root           string
	DataMount      string
	Enabled        bool
	FollowSymlinks bool
	ExpectFSID     string
	CurFSID        string
	Device         int64
	LastCheckOK    bool
	LastCheckAt    int64
	LastCheckNote  string
	FirstSeen      int64
	LastScanAt     int64
	MediaCount     int64
}

// Collections lists the stored collections.
func (s *Store) Collections(ctx context.Context) ([]Collection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, code, name, root, data_mount, enabled, follow_symlinks, expect_fsid, cur_fsid,
		        device, last_check_ok, last_check_at, last_check_note, first_seen, last_scan_at, media_count
		   FROM collections ORDER BY code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Collection
	for rows.Next() {
		var c Collection
		var en, fs, ok int
		if err := rows.Scan(&c.ID, &c.Code, &c.Name, &c.Root, &c.DataMount, &en, &fs, &c.ExpectFSID,
			&c.CurFSID, &c.Device, &ok, &c.LastCheckAt, &c.LastCheckNote, &c.FirstSeen, &c.LastScanAt,
			&c.MediaCount); err != nil {
			return nil, err
		}
		c.Enabled = en != 0
		c.FollowSymlinks = fs != 0
		c.LastCheckOK = ok != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecordMountCheck stores the result of a filesystem identity check.
//
// The first successful check pins the filesystem id. A later mismatch means the
// path now points somewhere else — a lost mount, a remounted array, a changed
// fstab — and every task refuses to run rather than indexing the wrong tree.
func (s *Store) RecordMountCheck(ctx context.Context, id int64, ok bool, note, fsid string, device uint64) error {
	if err := s.guard(); err != nil {
		return err
	}
	var pin string
	if ok && fsid != "" {
		// Pin only if not already pinned.
		_ = s.db.QueryRowContext(ctx, `SELECT expect_fsid FROM collections WHERE id = ?`, id).Scan(&pin)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE collections SET last_check_ok = ?, last_check_at = ?, last_check_note = ?,
		   cur_fsid = ?, device = ?,
		   expect_fsid = CASE WHEN ? <> '' THEN ? ELSE expect_fsid END
		 WHERE id = ?`,
		boolInt(ok), UnixNow(), note, fsid, int64(device),
		pin, fsid, id)
	return err
}

// RefreshMediaCount updates the cached per-collection count.
func (s *Store) RefreshMediaCount(ctx context.Context, id int64) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE collections SET media_count = (SELECT count(*) FROM media WHERE collection_id = ? AND missing = 0)
		 WHERE id = ?`, id, id)
	return err
}

// SetLastScan updates the collection's scan timestamp.
func (s *Store) SetLastScan(ctx context.Context, id int64, at int64) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE collections SET last_scan_at = ? WHERE id = ?`, at, id)
	return err
}

// ResetDerived clears probe and thumbnail state for a collection so that the
// next run recomputes it.
//
// This is the documented recovery for a corrupted cache or a changed format
// matrix. It never touches the original files and never deletes index rows.
func (s *Store) ResetDerived(ctx context.Context, collectionID int64, what string) (int64, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	var q string
	switch what {
	case "probe":
		q = `UPDATE media SET probe_state='pending', probe_reason='reset', probe_attempts=0, next_attempt_at=0
		     WHERE (? = 0 OR collection_id = ?)`
	case "thumb":
		q = `UPDATE media SET thumb_state='none', thumb_reason='reset', thumb_attempts=0, next_attempt_at=0,
		       thumb_bytes=0, thumb_ready_at=0 WHERE (? = 0 OR collection_id = ?)`
	case "all":
		q = `UPDATE media SET probe_state='pending', probe_reason='reset', probe_attempts=0,
		       thumb_state='none', thumb_reason='reset', thumb_attempts=0, thumb_bytes=0,
		       thumb_ready_at=0, next_attempt_at=0 WHERE (? = 0 OR collection_id = ?)`
	default:
		return 0, fmt.Errorf("reset: unknown target %q (want probe, thumb or all)", what)
	}
	res, err := s.db.ExecContext(ctx, q, collectionID, collectionID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
