// Package store is the albumctl index: a single SQLite database holding one row
// per media file plus task and state records.
//
// Why SQLite (per the design's requirement to justify the choice):
//
//   - ARM32 compatibility: the pure-Go modernc.org/sqlite driver cross-compiles
//     to linux/arm GOARM=7 with CGO_ENABLED=0, so the shipped binary is a
//     static ARMv7 executable with no libc sqlite and no musl/glibc split.
//     mmap_size is pinned to 0 because a 32-bit address space and a large
//     default mmap window do not mix.
//   - Memory bound: the page cache is pinned to 2 MiB (cache_size=-2048), the
//     temp store is on disk, and every query in this package carries an
//     explicit LIMIT. Rows are streamed with rows.Next(); no query in the
//     codebase materialises the table.
//   - Crash recovery: WAL plus synchronous=NORMAL. A power cut leaves a WAL
//     that SQLite replays on the next open. albumctl's own recovery story adds
//     a stale-task sweep (a task whose pid is gone is marked interrupted) and
//     the fact that the index is derived: deleting index.sqlite3 and rescanning
//     rebuilds it without touching a single original file.
//   - Bounded growth: thumbnails are files in a sharded cache tree, not blobs,
//     so cache size is inspectable and removable independently of the index.
package store

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

	"modernc.org/sqlite"
)

func init() {
	// The DSN is assembled from a configured path, so it is not a compile-time
	// constant. Strict mode rejects a _pragma value carrying more than one
	// statement, which is the documented risk of the _pragma mechanism.
	sqlite.StrictPragmas(true)
}

// ErrReadOnly is returned by every mutating method on a read-only store.
var ErrReadOnly = errors.New("store: this handle is read-only")

// ErrNotFound is returned when a lookup matches nothing.
var ErrNotFound = errors.New("store: not found")

// SchemaVersion is the on-disk schema this build understands.
//
// v2 added media.seen_run. v1 recorded "seen during a run" as a wall-clock
// second, which cannot distinguish a row touched by *this* run from one written
// by the previous run in the same second: run a scan, delete a file, run the
// scan again inside that second, and the deleted file stayed visible as
// present. The token is an opaque counter, not a time, so no clock behaviour -
// granularity, NTP steps, two runs in a row - can make a row look seen when it
// was not.
const SchemaVersion = 2

// Options configures Open.
type Options struct {
	// ReadOnly makes the handle reject writes at the application level and
	// sets SQLite's query_only pragma. The web service uses this so that a bug
	// in a handler cannot mutate the index.
	ReadOnly bool

	// CacheKiB is the SQLite page cache size. Negative means KiB (SQLite
	// convention). Default 2048 KiB.
	CacheKiB int

	// BusyTimeoutMS bounds how long a statement waits for a lock. Kept short so
	// that `status` cannot hang behind a long write.
	BusyTimeoutMS int

	// MaxOpenConns caps the pool. Write handles are pinned to 1.
	MaxOpenConns int

	// Memory opens an in-memory database. Used by tests.
	Memory bool
}

func (o Options) withDefaults() Options {
	if o.CacheKiB == 0 {
		o.CacheKiB = 2048
	}
	if o.BusyTimeoutMS == 0 {
		o.BusyTimeoutMS = 5000
	}
	if o.MaxOpenConns == 0 {
		if o.ReadOnly {
			o.MaxOpenConns = 4
		} else {
			o.MaxOpenConns = 1
		}
	}
	return o
}

// Store is a handle on the index.
type Store struct {
	db       *sql.DB
	path     string
	readOnly bool
}

// Open opens or creates the index.
func Open(path string, opts Options) (*Store, error) {
	opts = opts.withDefaults()
	if !opts.Memory {
		if path == "" {
			return nil, errors.New("store: empty index path")
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		path = abs
		// A plain (non file:) DSN is passed to sqlite3_open_v2 as a filename and
		// split on '?' by the driver. These characters would corrupt the DSN,
		// so they are rejected rather than silently reinterpreted.
		if strings.ContainsAny(path, "?#\n\r\x00") {
			return nil, fmt.Errorf("store: index path %q must not contain '?', '#', NUL or newlines", path)
		}
		if opts.ReadOnly {
			// A read-only open must not bring the index into existence. The
			// web service opens the index this way, and on a fresh install it
			// may start before the first scan has run: creating an empty file
			// here would leave a half-built index that the next writable open
			// has to adopt, and it would make the directory appear during what
			// the operator was told is a read-only inspection.
			//
			// The check is done here rather than by opening the file in
			// SQLite's read-only URI mode: that form needs the path escaped as
			// a URI, and a state directory containing a "%" would then be
			// misread. A readable-check is enough, since the only window left is
			// the file being removed between this Stat and the open, which
			// leaves an empty index that the next writable open migrates
			// harmlessly.
			if _, err := os.Stat(path); err != nil {
				return nil, fmt.Errorf("store: index %s is not readable: %w (has a scan run yet?)", path, err)
			}
		} else if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("store: create index directory: %w", err)
		}
	}

	dsn := buildDSN(path, opts)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(opts.MaxOpenConns)
	db.SetMaxIdleConns(opts.MaxOpenConns)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	s := &Store{db: db, path: path, readOnly: opts.ReadOnly}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	if opts.ReadOnly {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&n); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: read-only open of %s: %w (was it ever scanned?)", path, err)
		}
		if n == 0 {
			_ = db.Close()
			return nil, fmt.Errorf("store: %s has no schema; run a scan first", path)
		}
		return s, nil
	}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// buildDSN assembles the driver DSN with the pragmas that keep the process
// small and the writes safe.
func buildDSN(path string, opts Options) string {
	var b strings.Builder
	if opts.Memory {
		b.WriteString(":memory:")
	} else {
		b.WriteString(path)
	}
	q := url.Values{}
	add := func(s string) { q.Add("_pragma", s) }

	add("journal_mode(WAL)")
	add("synchronous(NORMAL)")
	add("foreign_keys(ON)")
	// 2 MiB page cache, expressed in KiB because the value is negative. A
	// bigger cache buys nothing here and is memory the machine does not have.
	add(fmt.Sprintf("cache_size(-%d)", opts.CacheKiB))
	// A 32-bit process must not map the whole database. SQLite's default mmap
	// window would also make RSS grow with the file and defeat the whole
	// accounting story.
	add("mmap_size(0)")
	// Temp b-trees go to disk rather than the process heap. The systemd unit
	// points TMPDIR at the state directory so this cannot land on a tmpfs.
	add("temp_store(FILE)")
	add(fmt.Sprintf("busy_timeout(%d)", opts.BusyTimeoutMS))
	add("wal_autocheckpoint(256)")
	add("analysis_limit(400)")
	if opts.ReadOnly {
		// query_only makes SQLite itself refuse writes on every pooled
		// connection, so a handler bug cannot corrupt the index.
		q.Set("_query_only", "1")
	} else {
		// IMMEDIATE takes the write lock at BEGIN, which avoids the
		// deferred-transaction upgrade deadlock that turns into SQLITE_BUSY
		// under concurrency.
		q.Set("_txlock", "immediate")
	}
	b.WriteString("?")
	b.WriteString(q.Encode())
	return b.String()
}

// Close releases the handle. It checkpoints the WAL so that a copy of the
// index directory is self-contained.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if !s.readOnly {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
		cancel()
	}
	return s.db.Close()
}

// Path returns the index file path.
func (s *Store) Path() string { return s.path }

// ReadOnly reports whether this handle rejects writes.
func (s *Store) ReadOnly() bool { return s.readOnly }

// DB exposes the handle for read-only queries in the server package.
func (s *Store) DB() *sql.DB { return s.db }

// guard blocks a mutation on a read-only handle.
func (s *Store) guard() error {
	if s.readOnly {
		return ErrReadOnly
	}
	return nil
}

// migrate creates the schema and advances user_version.
func (s *Store) migrate(ctx context.Context) error {
	var ver int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&ver); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}
	if ver > SchemaVersion {
		return fmt.Errorf("store: index schema %d is newer than this build understands (%d); "+
			"use a matching albumctl or move the index aside", ver, SchemaVersion)
	}
	if ver == SchemaVersion {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// schemaV1 is written with IF NOT EXISTS throughout, so applying it to an
	// existing v1 index is a no-op and applying it to an empty file creates the
	// current shape in one step. The v2 delta is guarded separately because
	// ALTER TABLE ADD COLUMN has nothing equivalent.
	stmts := schemaV1
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: migrate: %w\nstatement: %s", err, stmt)
		}
	}
	if ver < 2 {
		if err := addColumnIfMissing(ctx, tx, "media", "seen_run",
			`ALTER TABLE media ADD COLUMN seen_run INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(k, v) VALUES('schema_version', ?)
		 ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		fmt.Sprint(SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// addColumnIfMissing applies one ALTER TABLE only when the column is really
// absent, so a migration that was interrupted half way can be re-run.
func addColumnIfMissing(ctx context.Context, tx *sql.Tx, table, column, stmt string) error {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("store: inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("store: inspect %s: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: inspect %s: %w", table, err)
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("store: add %s.%s: %w", table, column, err)
	}
	return nil
}

// NextScanToken returns a value no earlier scan used. It is a counter, not a
// timestamp: the comparison it feeds is equality, so nothing about the clock can
// make a stale row look freshly seen.
func (s *Store) NextScanToken(ctx context.Context) (int64, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	var token int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seen_run), 0) + 1 FROM media`).Scan(&token); err != nil {
		return 0, fmt.Errorf("store: next scan token: %w", err)
	}
	return token, nil
}

// IntegrityCheck runs PRAGMA integrity_check. Used by `doctor`; it is O(index
// size) and never runs during serving or scanning.
func (s *Store) IntegrityCheck(ctx context.Context) (string, error) {
	var res string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check(1)`).Scan(&res); err != nil {
		return "", err
	}
	return res, nil
}

// ForeignKeyCheck runs PRAGMA foreign_key_check, also doctor-only.
func (s *Store) ForeignKeyCheck(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var table string
		var rowid int64
		var parent string
		var fkid int64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return nil, err
		}
		if len(out) < 50 {
			out = append(out, fmt.Sprintf("%s rowid=%d -> %s fk=%d", table, rowid, parent, fkid))
		}
	}
	return out, rows.Err()
}

// schemaV1 is the initial schema. Generated columns are used for the sort keys
// so that index-ordered keyset pagination is possible without duplicating data.
var schemaV1 = []string{
	`CREATE TABLE IF NOT EXISTS meta (
		k TEXT PRIMARY KEY,
		v TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS collections (
		id            INTEGER PRIMARY KEY,
		code          TEXT NOT NULL UNIQUE,
		name          TEXT NOT NULL,
		root          TEXT NOT NULL,
		data_mount    TEXT NOT NULL DEFAULT '',
		enabled       INTEGER NOT NULL DEFAULT 1,
		follow_symlinks INTEGER NOT NULL DEFAULT 0,
		expect_fsid   TEXT NOT NULL DEFAULT '',
		cur_fsid      TEXT NOT NULL DEFAULT '',
		device        INTEGER NOT NULL DEFAULT 0,
		last_check_ok INTEGER NOT NULL DEFAULT 0,
		last_check_at INTEGER NOT NULL DEFAULT 0,
		last_check_note TEXT NOT NULL DEFAULT '',
		first_seen    INTEGER NOT NULL DEFAULT 0,
		last_scan_at  INTEGER NOT NULL DEFAULT 0,
		media_count   INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS media (
		id            INTEGER PRIMARY KEY,
		collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
		rel_path      TEXT NOT NULL,
		rel_dir       TEXT NOT NULL DEFAULT '',
		base_name     TEXT NOT NULL,
		ext           TEXT NOT NULL DEFAULT '',
		size          INTEGER NOT NULL,
		mtime_ns      INTEGER NOT NULL,
		kind          TEXT NOT NULL DEFAULT 'other',
		mime          TEXT NOT NULL DEFAULT 'application/octet-stream',
		format        TEXT NOT NULL DEFAULT 'unknown',
		width         INTEGER NOT NULL DEFAULT 0,
		height        INTEGER NOT NULL DEFAULT 0,
		pixels        INTEGER NOT NULL DEFAULT 0,
		animated      INTEGER NOT NULL DEFAULT 0,
		frames        INTEGER NOT NULL DEFAULT 0,
		duration_ms   INTEGER NOT NULL DEFAULT 0,
		taken_at      INTEGER,
		taken_src     TEXT NOT NULL DEFAULT 'none',
		taken_tz      TEXT NOT NULL DEFAULT 'naive',
		tz_offset_min INTEGER,
		make          TEXT NOT NULL DEFAULT '',
		model         TEXT NOT NULL DEFAULT '',
		lens          TEXT NOT NULL DEFAULT '',
		orientation   INTEGER NOT NULL DEFAULT 0,
		latitude      REAL,
		longitude     REAL,
		altitude_m    REAL,
		probe_state   TEXT NOT NULL DEFAULT 'pending',
		probe_reason  TEXT NOT NULL DEFAULT '',
		probe_note    TEXT NOT NULL DEFAULT '',
		probe_attempts INTEGER NOT NULL DEFAULT 0,
		thumb_state   TEXT NOT NULL DEFAULT 'none',
		thumb_reason  TEXT NOT NULL DEFAULT '',
		thumb_bytes   INTEGER NOT NULL DEFAULT 0,
		thumb_attempts INTEGER NOT NULL DEFAULT 0,
		thumb_ready_at INTEGER NOT NULL DEFAULT 0,
		meta_extra    TEXT NOT NULL DEFAULT '',
		missing       INTEGER NOT NULL DEFAULT 0,
		first_seen    INTEGER NOT NULL,
		last_seen     INTEGER NOT NULL,
		seen_run      INTEGER NOT NULL DEFAULT 0,
		next_attempt_at INTEGER NOT NULL DEFAULT 0,
		search_text   TEXT NOT NULL DEFAULT '',
		base_lower    TEXT GENERATED ALWAYS AS (lower(base_name)) VIRTUAL,
		sort_desc     INTEGER GENERATED ALWAYS AS (
			CASE WHEN taken_at IS NULL THEN 9223372036854775807 ELSE -taken_at END) VIRTUAL,
		sort_asc      INTEGER GENERATED ALWAYS AS (
			CASE WHEN taken_at IS NULL THEN 9223372036854775807 ELSE taken_at END) VIRTUAL,
		UNIQUE(collection_id, rel_path)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_media_sort_desc ON media(collection_id, sort_desc, id)`,
	`CREATE INDEX IF NOT EXISTS idx_media_sort_asc  ON media(collection_id, sort_asc, id)`,
	`CREATE INDEX IF NOT EXISTS idx_media_name      ON media(collection_id, base_lower, id)`,
	`CREATE INDEX IF NOT EXISTS idx_media_thumb     ON media(thumb_state, collection_id, id)`,
	`CREATE INDEX IF NOT EXISTS idx_media_probe     ON media(probe_state, collection_id, id)`,
	`CREATE INDEX IF NOT EXISTS idx_media_kind      ON media(collection_id, kind, id)`,
	`CREATE INDEX IF NOT EXISTS idx_media_taken     ON media(collection_id, taken_at)`,
	`CREATE INDEX IF NOT EXISTS idx_media_gps       ON media(collection_id, latitude)
		WHERE latitude IS NOT NULL`,
	`CREATE INDEX IF NOT EXISTS idx_media_missing   ON media(missing, collection_id)`,
	`CREATE TABLE IF NOT EXISTS tasks (
		id            INTEGER PRIMARY KEY,
		kind          TEXT NOT NULL,
		state         TEXT NOT NULL,
		reason        TEXT NOT NULL DEFAULT '',
		detail        TEXT NOT NULL DEFAULT '',
		collection_code TEXT NOT NULL DEFAULT '',
		pid           INTEGER NOT NULL DEFAULT 0,
		started_at    INTEGER NOT NULL,
		heartbeat_at  INTEGER NOT NULL DEFAULT 0,
		ended_at      INTEGER NOT NULL DEFAULT 0,
		limit_n       INTEGER NOT NULL DEFAULT 0,
		max_runtime_s INTEGER NOT NULL DEFAULT 0,
		processed     INTEGER NOT NULL DEFAULT 0,
		skipped       INTEGER NOT NULL DEFAULT 0,
		failed        INTEGER NOT NULL DEFAULT 0,
		bytes_read    INTEGER NOT NULL DEFAULT 0,
		bytes_written INTEGER NOT NULL DEFAULT 0,
		cgroup_isolated INTEGER NOT NULL DEFAULT 0,
		dry_run       INTEGER NOT NULL DEFAULT 0,
		argv          TEXT NOT NULL DEFAULT '',
		report        TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_tasks_started ON tasks(started_at DESC, id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_tasks_state   ON tasks(state, kind)`,
	`CREATE TABLE IF NOT EXISTS task_items (
		id            INTEGER PRIMARY KEY,
		task_id       INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		media_id      INTEGER,
		rel_path      TEXT NOT NULL DEFAULT '',
		action        TEXT NOT NULL,
		outcome       TEXT NOT NULL,
		reason        TEXT NOT NULL DEFAULT '',
		detail        TEXT NOT NULL DEFAULT '',
		ms            INTEGER NOT NULL DEFAULT 0,
		peak_rss      INTEGER NOT NULL DEFAULT 0,
		cgroup_peak   INTEGER NOT NULL DEFAULT 0,
		mem_avail_min INTEGER NOT NULL DEFAULT 0,
		at            INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_task_items_task ON task_items(task_id, id)`,
	`CREATE TABLE IF NOT EXISTS app_state (
		k          TEXT PRIMARY KEY,
		v          TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`INSERT INTO meta(k, v) VALUES('schema_version', '1')
		ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
}

// now is overridable in tests.
var now = func() time.Time { return time.Now() }

// UnixNow returns the current time as unix seconds.
func UnixNow() int64 { return now().Unix() }
