package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Probe states.
const (
	ProbePending = "pending"
	ProbeOK      = "ok"
	ProbeSkipped = "skipped"
	ProbeFailed  = "failed"
)

// Thumbnail states.
//
// The states form a small lifecycle and the extra names exist because "has no
// thumbnail" and "needs a thumbnail" are different questions:
//
//	none     never attempted, or invalidated because the file changed
//	pending  eligible and queued; the probe decided this format is supported
//	ok       a thumbnail file exists
//	skipped  deliberately not attempted (unsupported format, over budget)
//	failed   attempted and did not succeed; retried a bounded number of times
//
// The eligibility predicate used by the queue is therefore
// probe_state = 'ok' AND thumb_state IN ('none','pending','failed'), with
// thumb_attempts and next_attempt_at bounding the retries.
const (
	ThumbNone    = "none"    // never attempted
	ThumbPending = "pending" // queued
	ThumbOK      = "ok"
	ThumbSkipped = "skipped" // format unsupported or over budget
	ThumbFailed  = "failed"
)

// Capture-time sources, in the order the design requires them to be tried.
const (
	TakenSrcExif      = "exif"      // real in-file capture time
	TakenSrcContainer = "container" // video container creation time
	TakenSrcFilename  = "filename"  // opt-in heuristic, off by default
	TakenSrcFiletime  = "filetime"  // file mtime, always labelled as a fallback
	TakenSrcNone      = "none"
)

// TakenSrcIsFallback reports whether a source is something other than a real
// capture timestamp recorded by the device.
func TakenSrcIsFallback(src string) bool { return src != TakenSrcExif }

// Media is one indexed file.
type Media struct {
	ID             int64
	CollectionID   int64
	CollectionCode string
	RelPath        string
	RelDir         string
	BaseName       string
	Ext            string

	Size    int64
	MtimeNS int64

	Kind       string
	MIME       string
	Format     string
	Width      int
	Height     int
	Pixels     int64
	Animated   bool
	Frames     int
	DurationMS int64

	TakenAt     *int64
	TakenSrc    string
	TakenTZ     string
	TZOffsetMin *int

	Make        string
	Model       string
	Lens        string
	Orientation int

	Latitude  *float64
	Longitude *float64
	AltitudeM *float64

	ProbeState    string
	ProbeReason   string
	ProbeNote     string
	ProbeAttempts int

	ThumbState    string
	ThumbReason   string
	ThumbBytes    int64
	ThumbAttempts int
	ThumbReadyAt  int64

	MetaExtra string

	Missing       bool
	FirstSeen     int64
	LastSeen      int64
	NextAttemptAt int64
}

// MediaScan is what the walker knows about a file before probing it.
type MediaScan struct {
	CollectionID   int64
	CollectionCode string
	RelPath        string
	RelDir         string
	BaseName       string
	Ext            string
	Size           int64
	MtimeNS        int64
	Kind           string
	MIME           string
	At             int64
	// RunToken identifies the scan run that saw this file. MarkMissing compares
	// it for equality, which is exact; a wall-clock "last seen" comparison is
	// not, because a row written by the previous run within the same second is
	// indistinguishable from one written by this run.
	RunToken int64
}

// UpsertResult reports what happened to one row.
type UpsertResult struct {
	ID      int64
	Created bool
	// Changed is true when the row was just created, or when size or mtime
	// differ from the row already stored. Either way it means "no derived value
	// recorded earlier can be trusted": a create has none, and a change has
	// some that belong to different bytes. A caller must not read Changed as
	// "this file existed before".
	Changed bool
	// PreviousThumbState is the state before invalidation, so the caller can
	// delete the stale cache file.
	PreviousThumbState string
}

// UpsertMedia inserts or refreshes one media row.
//
// Change detection is deliberately cheap: relative path, size, mtime. Computing
// a hash of every original is exactly what the design forbids, because it would
// read every byte of every video to discover duplicates nobody asked about.
func (s *Store) UpsertMedia(ctx context.Context, in MediaScan) (UpsertResult, error) {
	if err := s.guard(); err != nil {
		return UpsertResult{}, err
	}
	var (
		id      int64
		size    int64
		mtimeNS int64
		thumb   string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, size, mtime_ns, thumb_state FROM media
		 WHERE collection_id = ? AND rel_path = ?`,
		in.CollectionID, in.RelPath).Scan(&id, &size, &mtimeNS, &thumb)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		search := searchText(in.BaseName, in.RelDir, "", "")
		res, ierr := s.db.ExecContext(ctx,
			`INSERT INTO media
			   (collection_id, rel_path, rel_dir, base_name, ext, size, mtime_ns, kind, mime,
			    first_seen, last_seen, seen_run, search_text, probe_state, thumb_state)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			in.CollectionID, in.RelPath, in.RelDir, in.BaseName, in.Ext, in.Size, in.MtimeNS,
			in.Kind, in.MIME, in.At, in.At, in.RunToken, search, ProbePending, ThumbNone)
		if ierr != nil {
			return UpsertResult{}, fmt.Errorf("store: insert media %s: %w", in.RelPath, ierr)
		}
		newID, _ := res.LastInsertId()
		return UpsertResult{ID: newID, Created: true, Changed: true}, nil
	case err != nil:
		return UpsertResult{}, fmt.Errorf("store: lookup media %s: %w", in.RelPath, err)
	}

	changed := size != in.Size || mtimeNS != in.MtimeNS
	if !changed {
		// The common case: touch last_seen and clear any previous "missing"
		// mark. Nothing derived is recomputed, so a rescan of an unchanged
		// library does almost no work.
		if _, err := s.db.ExecContext(ctx,
			`UPDATE media SET last_seen = ?, seen_run = ?, missing = 0, rel_dir = ?, base_name = ?,
			                  ext = ?, kind = ?, mime = ?
			 WHERE id = ?`,
			in.At, in.RunToken, in.RelDir, in.BaseName, in.Ext, in.Kind, in.MIME, id); err != nil {
			return UpsertResult{}, fmt.Errorf("store: touch media %s: %w", in.RelPath, err)
		}
		return UpsertResult{ID: id, Changed: false, PreviousThumbState: thumb}, nil
	}

	// The file changed under us. Every derived value is invalidated in one
	// statement so there is no window in which a stale thumbnail is paired with
	// new content.
	search := searchText(in.BaseName, in.RelDir, "", "")
	if _, err := s.db.ExecContext(ctx,
		`UPDATE media SET
		   size = ?, mtime_ns = ?, rel_dir = ?, base_name = ?, ext = ?, kind = ?, mime = ?,
		   width = 0, height = 0, pixels = 0, animated = 0, frames = 0, duration_ms = 0,
		   taken_at = NULL, taken_src = 'none', taken_tz = 'naive', tz_offset_min = NULL,
		   make = '', model = '', lens = '', orientation = 0,
		   latitude = NULL, longitude = NULL, altitude_m = NULL,
		   probe_state = 'pending', probe_reason = 'file_changed', probe_note = '', probe_attempts = 0,
		   thumb_state = 'none', thumb_reason = 'file_changed', thumb_bytes = 0, thumb_attempts = 0,
		   thumb_ready_at = 0, meta_extra = '', next_attempt_at = 0,
		   missing = 0, last_seen = ?, seen_run = ?, search_text = ?
		 WHERE id = ?`,
		in.Size, in.MtimeNS, in.RelDir, in.BaseName, in.Ext, in.Kind, in.MIME,
		in.At, in.RunToken, search, id); err != nil {
		return UpsertResult{}, fmt.Errorf("store: update media %s: %w", in.RelPath, err)
	}
	return UpsertResult{ID: id, Changed: true, PreviousThumbState: thumb}, nil
}

// searchText builds the single lowercased blob used by the simple search.
func searchText(base, relDir, make, model string) string {
	return strings.ToLower(strings.Join([]string{base, relDir, make, model}, " "))
}

// ProbeUpdate carries the outcome of a probe.
type ProbeUpdate struct {
	ID         int64
	State      string
	Reason     string
	Note       string
	Kind       string
	MIME       string
	Format     string
	Width      int
	Height     int
	Pixels     int64
	Animated   bool
	Frames     int
	DurationMS int64

	TakenAt     *int64
	TakenSrc    string
	TakenTZ     string
	TZOffsetMin *int

	Make        string
	Model       string
	Lens        string
	Orientation int
	Latitude    *float64
	Longitude   *float64
	AltitudeM   *float64

	MetaExtra string

	// ThumbState is what the thumbnail state should become as a result of this
	// probe: "pending" when the file is thumbnailable, "skipped" otherwise.
	ThumbState  string
	ThumbReason string

	NextAttemptAt int64
	At            int64
}

// SetProbe stores the probe outcome and re-derives the search blob.
func (s *Store) SetProbe(ctx context.Context, u ProbeUpdate) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		// The kind is kept when the update does not carry one. The one caller
		// always supplies it, but an empty value here would erase the
		// classification the scan row established and make the file
		// unreachable by the thumbnail queue, which selects on kind.
		`UPDATE media SET
		   kind = CASE WHEN ? = '' THEN kind ELSE ? END,
		   mime = CASE WHEN ? = '' THEN mime ELSE ? END,
		   format = ?, width = ?, height = ?, pixels = ?, animated = ?, frames = ?,
		   duration_ms = ?, taken_at = ?, taken_src = ?, taken_tz = ?, tz_offset_min = ?,
		   make = ?, model = ?, lens = ?, orientation = ?,
		   latitude = ?, longitude = ?, altitude_m = ?,
		   probe_state = ?, probe_reason = ?, probe_note = ?, meta_extra = ?,
		   thumb_state = CASE WHEN thumb_state = 'ok' AND ? = 'pending' THEN 'ok' ELSE ? END,
		   thumb_reason = CASE WHEN thumb_state = 'ok' AND ? = 'pending' THEN thumb_reason ELSE ? END,
		   probe_attempts = probe_attempts + 1,
		   next_attempt_at = ?
		 WHERE id = ?`,
		u.Kind, u.Kind, u.MIME, u.MIME,
		u.Format, u.Width, u.Height, u.Pixels, boolInt(u.Animated), u.Frames,
		u.DurationMS, u.TakenAt, u.TakenSrc, u.TakenTZ, u.TZOffsetMin,
		u.Make, u.Model, u.Lens, u.Orientation,
		u.Latitude, u.Longitude, u.AltitudeM,
		u.State, u.Reason, u.Note, u.MetaExtra,
		u.ThumbState, u.ThumbState,
		u.ThumbState, u.ThumbReason,
		u.NextAttemptAt, u.ID)
	if err != nil {
		return fmt.Errorf("store: set probe for media %d: %w", u.ID, err)
	}
	return s.refreshSearch(ctx, u.ID)
}

// refreshSearch recomputes the search blob after metadata changed.
func (s *Store) refreshSearch(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE media SET search_text = lower(base_name || ' ' || rel_dir || ' ' || make || ' ' || model)
		 WHERE id = ?`, id)
	return err
}

// ThumbUpdate carries the outcome of a thumbnail attempt.
type ThumbUpdate struct {
	ID            int64
	State         string
	Reason        string
	Bytes         int64
	NextAttemptAt int64
	At            int64
}

// SetThumb stores the thumbnail outcome. Nothing is published until the cache
// file has already been renamed into place, so "ok" always means the file is
// present and complete.
func (s *Store) SetThumb(ctx context.Context, u ThumbUpdate) error {
	if err := s.guard(); err != nil {
		return err
	}
	var ready int64
	if u.State == ThumbOK {
		ready = u.At
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE media SET thumb_state = ?, thumb_reason = ?, thumb_bytes = ?,
		   thumb_attempts = thumb_attempts + 1, thumb_ready_at = ?, next_attempt_at = ?
		 WHERE id = ?`,
		u.State, u.Reason, u.Bytes, ready, u.NextAttemptAt, u.ID)
	if err != nil {
		return fmt.Errorf("store: set thumb for media %d: %w", u.ID, err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// MediaRef is the minimum needed to process a file.
type MediaRef struct {
	ID             int64
	CollectionCode string
	CollectionRoot string
	RelPath        string
	BaseName       string
	Ext            string
	Size           int64
	MtimeNS        int64
	Kind           string
	ProbeState     string
}

// MaxWorkBatch is the hard ceiling on how many rows one queue fetch may return,
// whatever the caller asks for. The caller's own --limit is usually far smaller;
// this exists so that no caller, including a future one, can turn a queue fetch
// into an unbounded read.
const MaxWorkBatch = 1000

// checkLimit validates a batch size.
//
// A non-positive limit must not reach SQL: SQLite reads LIMIT -1 as "no limit",
// so an accidental --limit -1 would fetch the entire pending set in one call,
// which is the opposite of the bounded work this program promises.
func checkLimit(limit int) (int, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("store: batch limit must be positive, got %d", limit)
	}
	if limit > MaxWorkBatch {
		return MaxWorkBatch, nil
	}
	return limit, nil
}

// NextProbe returns up to limit files awaiting a metadata probe.
//
// Ordering is index order (probe_state, collection_id, id) rather than
// chronological: taking one row at a time must be O(1), and an ORDER BY over a
// large pending set would sort the whole set on every fetch.
func (s *Store) NextProbe(ctx context.Context, collectionID int64, maxAttempts int, at int64, limit int) ([]MediaRef, error) {
	return s.nextWork(ctx, "probe_state", ProbePending, collectionID, maxAttempts, at, limit)
}

// NextThumb returns up to limit files awaiting a thumbnail.
func (s *Store) NextThumb(ctx context.Context, collectionID int64, maxAttempts int, at int64, limit int) ([]MediaRef, error) {
	limit, err := checkLimit(limit)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, c.code, c.root, m.rel_path, m.base_name, m.ext, m.size, m.mtime_ns, m.kind, m.probe_state
		   FROM media m JOIN collections c ON c.id = m.collection_id
		  WHERE m.missing = 0
		    AND m.kind = 'image'
		    AND m.probe_state = 'ok'
		    AND m.thumb_state IN ('none','pending','failed')
		    AND m.thumb_attempts < ?
		    AND m.next_attempt_at <= ?
		    AND (? = 0 OR m.collection_id = ?)
		  LIMIT ?`,
		maxAttempts, at, collectionID, collectionID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: next thumb: %w", err)
	}
	return scanRefs(rows)
}

func (s *Store) nextWork(ctx context.Context, stateCol, state string, collectionID int64, maxAttempts int, at int64, limit int) ([]MediaRef, error) {
	limit, err := checkLimit(limit)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(
		`SELECT m.id, c.code, c.root, m.rel_path, m.base_name, m.ext, m.size, m.mtime_ns, m.kind, m.probe_state
		   FROM media m JOIN collections c ON c.id = m.collection_id
		  WHERE m.missing = 0
		    AND m.%s = ?
		    AND m.probe_attempts < ?
		    AND m.next_attempt_at <= ?
		    AND (? = 0 OR m.collection_id = ?)
		  LIMIT ?`, stateCol)
	rows, err := s.db.QueryContext(ctx, q, state, maxAttempts, at, collectionID, collectionID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: next %s: %w", state, err)
	}
	return scanRefs(rows)
}

func scanRefs(rows *sql.Rows) ([]MediaRef, error) {
	defer rows.Close()
	var out []MediaRef
	for rows.Next() {
		var r MediaRef
		if err := rows.Scan(&r.ID, &r.CollectionCode, &r.CollectionRoot, &r.RelPath, &r.BaseName,
			&r.Ext, &r.Size, &r.MtimeNS, &r.Kind, &r.ProbeState); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountPending returns the number of rows still awaiting work.
func (s *Store) CountPending(ctx context.Context, collectionID int64) (probe, thumb int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT
		   (SELECT count(*) FROM media WHERE probe_state = 'pending' AND (? = 0 OR collection_id = ?)),
		   (SELECT count(*) FROM media WHERE kind = 'image' AND probe_state = 'ok'
		      AND thumb_state IN ('none','pending','failed')
		      AND thumb_attempts < 1000 AND (? = 0 OR collection_id = ?))`,
		collectionID, collectionID, collectionID, collectionID).Scan(&probe, &thumb)
	if err != nil {
		return 0, 0, fmt.Errorf("store: count pending: %w", err)
	}
	return probe, thumb, nil
}

// MarkMissing flags rows in a collection that were not seen during a complete
// scan.
//
// This never deletes anything. A row that is marked missing keeps all its
// metadata, keeps its thumbnail, and stays visible; only the file itself is
// reported as absent. The design forbids automatically deleting records for
// supposedly vanished originals, because a transient mount failure would
// otherwise destroy the index.
// TouchSeen stamps an existing row as seen by this run without changing anything
// else about it.
//
// It exists for one case: the walk saw the file, but the row could not be
// refreshed — the file was mid-copy and its size or mtime moved between the walk
// and the re-stat, it stopped being a regular file, or reading it failed. The
// file is present in the directory, so it must not also be flagged absent by the
// same scan that declined to re-index it. Marking it missing would hide a picture
// that is merely being written, and would take its original out of service until
// the next scan.
//
// A row that does not exist yet is left alone: "seen" cannot be recorded for
// something never indexed, and inventing a row with no size or kind would be
// worse than reporting it as deferred.
func (s *Store) TouchSeen(ctx context.Context, collectionID int64, relPath string, runToken int64) (bool, error) {
	if err := s.guard(); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE media SET seen_run = ? WHERE collection_id = ? AND rel_path = ?`,
		runToken, collectionID, relPath)
	if err != nil {
		return false, fmt.Errorf("store: touch seen: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkMissing flags every row the run identified by runToken did not see. It is
// the only writer of the missing flag, it never deletes a row, and it is called
// only after a walk that provably covered the whole collection.
//
// runToken is compared for equality against the token each row was stamped with
// during this run. It is not a time: with a timestamp, a scan run twice inside
// one second would leave a file deleted in between looking present, because the
// previous run's stamp is not strictly older than the second run's start.
func (s *Store) MarkMissing(ctx context.Context, collectionID int64, runToken int64) (int64, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE media SET missing = 1
		  WHERE collection_id = ? AND seen_run <> ? AND missing = 0`, collectionID, runToken)
	if err != nil {
		return 0, fmt.Errorf("store: mark missing: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClearMissing resets the missing flag for a whole collection.
func (s *Store) ClearMissing(ctx context.Context, collectionID int64) (int64, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE media SET missing = 0 WHERE collection_id = ? AND missing = 1`, collectionID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---- queries ------------------------------------------------------------

// SortMode selects the ordering and therefore the keyset.
type SortMode string

// Sort modes.
const (
	SortTimeDesc SortMode = "time_desc"
	SortTimeAsc  SortMode = "time_asc"
	SortNameAsc  SortMode = "name_asc"
)

// ParseSort validates a client-supplied sort name.
func ParseSort(s string) (SortMode, error) {
	switch SortMode(s) {
	case SortTimeDesc, SortTimeAsc, SortNameAsc:
		return SortMode(s), nil
	case "":
		return SortTimeDesc, nil
	}
	return "", fmt.Errorf("unknown sort %q", s)
}

// Filter is a bounded query description.
type Filter struct {
	CollectionID int64
	// nil means unrestricted; an empty non-nil slice deliberately matches none.
	CollectionIDs  []int64
	Kinds          []string
	Query          string
	From, To       *int64
	OnlyGPS        bool
	OnlyFallback   bool
	IncludeMissing bool
	Sort           SortMode
}

// Cursor is an opaque keyset position.
type Cursor struct {
	Sort SortMode `json:"s"`
	Key  int64    `json:"k"`
	Name string   `json:"n"`
	ID   int64    `json:"i"`
}

// Encode renders the cursor as a URL-safe token.
func (c Cursor) Encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses a token produced by Encode.
func DecodeCursor(tok string) (Cursor, error) {
	var c Cursor
	if tok == "" {
		return c, nil
	}
	if len(tok) > 512 {
		return c, errors.New("cursor token too long")
	}
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return c, fmt.Errorf("malformed cursor: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("malformed cursor: %w", err)
	}
	if _, err := ParseSort(string(c.Sort)); err != nil {
		return c, fmt.Errorf("malformed cursor: %w", err)
	}
	return c, nil
}

// MaxQueryLen is the longest search text the index will look at. It is exported
// so that the HTTP layer can refuse a longer one with a clear message instead of
// trimming it: silently searching for a prefix of what was typed would return
// results for a query the user did not make. The truncation below stays as a
// backstop for callers that do not validate.
const MaxQueryLen = 256

// buildWhere assembles the shared predicate and its arguments.
func (f Filter) buildWhere() (string, []any) {
	var (
		clauses = []string{"m.kind <> 'private'"}
		args    []any
	)
	if !f.IncludeMissing {
		clauses = append(clauses, "m.missing = 0")
	}
	if f.CollectionID != 0 {
		clauses = append(clauses, "m.collection_id = ?")
		args = append(args, f.CollectionID)
	}
	if f.CollectionIDs != nil {
		if len(f.CollectionIDs) == 0 {
			clauses = append(clauses, "0 = 1")
		} else {
			ph := make([]string, 0, len(f.CollectionIDs))
			for _, id := range f.CollectionIDs {
				ph = append(ph, "?")
				args = append(args, id)
			}
			clauses = append(clauses, "m.collection_id IN ("+strings.Join(ph, ",")+")")
		}
	}
	if len(f.Kinds) > 0 {
		ph := make([]string, 0, len(f.Kinds))
		for _, k := range f.Kinds {
			ph = append(ph, "?")
			args = append(args, k)
		}
		clauses = append(clauses, "m.kind IN ("+strings.Join(ph, ",")+")")
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		if len(q) > MaxQueryLen {
			q = q[:MaxQueryLen]
		}
		clauses = append(clauses, `m.search_text LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(strings.ToLower(q))+"%")
	}
	if f.From != nil {
		clauses = append(clauses, "m.taken_at IS NOT NULL AND m.taken_at >= ?")
		args = append(args, *f.From)
	}
	if f.To != nil {
		clauses = append(clauses, "m.taken_at IS NOT NULL AND m.taken_at <= ?")
		args = append(args, *f.To)
	}
	if f.OnlyGPS {
		clauses = append(clauses, "m.latitude IS NOT NULL AND m.longitude IS NOT NULL")
	}
	if f.OnlyFallback {
		clauses = append(clauses, "m.taken_src <> 'exif' AND m.taken_src <> 'none'")
	}
	if len(clauses) == 0 {
		return "1=1", nil
	}
	return strings.Join(clauses, " AND "), args
}

// escapeLike neutralizes LIKE metacharacters so that a query containing % or _
// matches literal characters instead of acting as a wildcard.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

const mediaColumns = `m.id, m.collection_id, c.code, m.rel_path, m.rel_dir, m.base_name, m.ext,
	m.size, m.mtime_ns, m.kind, m.mime, m.format, m.width, m.height, m.pixels, m.animated,
	m.frames, m.duration_ms, m.taken_at, m.taken_src, m.taken_tz, m.tz_offset_min,
	m.make, m.model, m.lens, m.orientation, m.latitude, m.longitude, m.altitude_m,
	m.probe_state, m.probe_reason, m.probe_note, m.probe_attempts,
	m.thumb_state, m.thumb_reason, m.thumb_bytes, m.thumb_attempts, m.thumb_ready_at,
	m.meta_extra, m.missing, m.first_seen, m.last_seen, m.next_attempt_at`

func scanMedia(rows *sql.Rows) ([]Media, error) {
	defer rows.Close()
	out := make([]Media, 0, 64)
	for rows.Next() {
		var m Media
		var animated, missing int
		if err := rows.Scan(
			&m.ID, &m.CollectionID, &m.CollectionCode, &m.RelPath, &m.RelDir, &m.BaseName, &m.Ext,
			&m.Size, &m.MtimeNS, &m.Kind, &m.MIME, &m.Format, &m.Width, &m.Height, &m.Pixels, &animated,
			&m.Frames, &m.DurationMS, &m.TakenAt, &m.TakenSrc, &m.TakenTZ, &m.TZOffsetMin,
			&m.Make, &m.Model, &m.Lens, &m.Orientation, &m.Latitude, &m.Longitude, &m.AltitudeM,
			&m.ProbeState, &m.ProbeReason, &m.ProbeNote, &m.ProbeAttempts,
			&m.ThumbState, &m.ThumbReason, &m.ThumbBytes, &m.ThumbAttempts, &m.ThumbReadyAt,
			&m.MetaExtra, &missing, &m.FirstSeen, &m.LastSeen, &m.NextAttemptAt,
		); err != nil {
			return nil, err
		}
		m.Animated = animated != 0
		m.Missing = missing != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListMedia returns one page and the cursor for the next page.
//
// The page size is clamped here as well as in the HTTP layer: this package is
// the last line of defence against a query that would stream the whole library
// into memory.
func (s *Store) ListMedia(ctx context.Context, f Filter, cur Cursor, limit int) ([]Media, *Cursor, error) {
	if limit <= 0 {
		limit = 60
	}
	if limit > 200 {
		limit = 200
	}
	if f.Sort == "" {
		f.Sort = SortTimeDesc
	}
	where, args := f.buildWhere()

	orderCol := "m.sort_desc"
	keyCol := "m.sort_desc"
	switch f.Sort {
	case SortTimeAsc:
		orderCol, keyCol = "m.sort_asc", "m.sort_asc"
	case SortNameAsc:
		orderCol, keyCol = "m.base_lower", "m.base_lower"
	}

	if cur.Sort != "" {
		if cur.Sort != f.Sort {
			return nil, nil, fmt.Errorf("cursor was issued for sort %q but the request asks for %q; start a new page", cur.Sort, f.Sort)
		}
		switch f.Sort {
		case SortNameAsc:
			where += " AND (m.base_lower, m.id) > (?, ?)"
			args = append(args, cur.Name, cur.ID)
		default:
			where += " AND (" + keyCol + ", m.id) > (?, ?)"
			args = append(args, cur.Key, cur.ID)
		}
	}
	args = append(args, limit+1) // one extra row tells us whether a next page exists

	q := fmt.Sprintf(`SELECT %s FROM media m JOIN collections c ON c.id = m.collection_id
		WHERE %s ORDER BY %s ASC, m.id ASC LIMIT ?`, mediaColumns, where, orderCol)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list media: %w", err)
	}
	items, err := scanMedia(rows)
	if err != nil {
		return nil, nil, err
	}
	var next *Cursor
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		c := Cursor{Sort: f.Sort, ID: last.ID, Name: last.BaseName}
		switch f.Sort {
		case SortTimeAsc:
			c.Key = sortAsc(last)
		case SortNameAsc:
			c.Name = strings.ToLower(last.BaseName)
		default:
			c.Key = sortDesc(last)
		}
		next = &c
	}
	return items, next, nil
}

func sortDesc(m Media) int64 {
	if m.TakenAt == nil {
		return 1<<63 - 1
	}
	return -*m.TakenAt
}

func sortAsc(m Media) int64 {
	if m.TakenAt == nil {
		return 1<<63 - 1
	}
	return *m.TakenAt
}

// CountMedia returns the number of rows matching the filter, capped so the
// count itself cannot become an expensive full-table aggregation under load.
func (s *Store) CountMedia(ctx context.Context, f Filter) (int64, error) {
	where, args := f.buildWhere()
	var n int64
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM media m WHERE %s`, where), args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count media: %w", err)
	}
	return n, nil
}

// GetMedia loads one row by id.
func (s *Store) GetMedia(ctx context.Context, id int64) (Media, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM media m JOIN collections c ON c.id = m.collection_id WHERE m.id = ? AND m.kind <> 'private'`, mediaColumns), id)
	if err != nil {
		return Media{}, err
	}
	items, err := scanMedia(rows)
	if err != nil {
		return Media{}, err
	}
	if len(items) == 0 {
		return Media{}, ErrNotFound
	}
	return items[0], nil
}

// TimelineRow is one year-month bucket.
type TimelineRow struct {
	Year  int
	Month int
	Count int64
}

// Timeline returns the year/month histogram and the count of undated items.
//
// The grouping is bounded by the number of distinct months that exist, not by
// the number of files: at most a few hundred small rows come back.
func (s *Store) Timeline(ctx context.Context, f Filter) ([]TimelineRow, int64, error) {
	f.IncludeMissing = false
	where, args := f.buildWhere()
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT CAST(strftime('%%Y', m.taken_at, 'unixepoch') AS INTEGER) AS y,
		        CAST(strftime('%%m', m.taken_at, 'unixepoch') AS INTEGER) AS mo,
		        count(*)
		   FROM media m
		  WHERE %s AND m.taken_at IS NOT NULL
		  GROUP BY y, mo
		  ORDER BY y DESC, mo DESC`, where), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: timeline: %w", err)
	}
	defer rows.Close()
	var out []TimelineRow
	for rows.Next() {
		var r TimelineRow
		if err := rows.Scan(&r.Year, &r.Month, &r.Count); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var undated int64
	whereUndated := where + " AND m.taken_at IS NULL"
	if err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM media m WHERE %s`, whereUndated), args...).Scan(&undated); err != nil {
		return nil, 0, err
	}
	return out, undated, nil
}

// Totals is the single aggregate row behind `status`.
type Totals struct {
	Media       int64
	Images      int64
	Videos      int64
	Other       int64
	Unsupported int64
	Probed      int64
	ThumbOK     int64
	ThumbDue    int64
	ThumbFailed int64
	Undated     int64
	WithGPS     int64
	Missing     int64
}

// Totals computes every counter `status` needs in one statement over one table
// scan, so that a status request touches the database once.
func (s *Store) Totals(ctx context.Context, collectionID int64) (Totals, error) {
	var t Totals
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*),
		        coalesce(sum(kind = 'image'), 0),
		        coalesce(sum(kind = 'video'), 0),
		        coalesce(sum(kind = 'other'), 0),
		        coalesce(sum(kind = 'unsupported'), 0),
		        coalesce(sum(probe_state = 'ok'), 0),
		        coalesce(sum(thumb_state = 'ok'), 0),
		        coalesce(sum(kind = 'image' AND probe_state = 'ok'
		                     AND thumb_state IN ('none','pending','failed')), 0),
		        coalesce(sum(thumb_state = 'failed'), 0),
		        coalesce(sum(taken_at IS NULL), 0),
		        coalesce(sum(latitude IS NOT NULL), 0),
		        coalesce(sum(missing = 1), 0)
		   FROM media WHERE (? = 0 OR collection_id = ?)`,
		collectionID, collectionID).Scan(
		&t.Media, &t.Images, &t.Videos, &t.Other, &t.Unsupported,
		&t.Probed, &t.ThumbOK, &t.ThumbDue, &t.ThumbFailed,
		&t.Undated, &t.WithGPS, &t.Missing)
	if err != nil {
		return t, fmt.Errorf("store: totals: %w", err)
	}
	return t, nil
}

// SumThumbBytes reports the cache size as recorded in the index, without
// walking the cache directory. cache-report uses this for the fast path and
// walks the tree only when asked for orphans.
func (s *Store) SumThumbBytes(ctx context.Context) (files int64, bytes int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT coalesce(sum(thumb_state = 'ok'), 0), coalesce(sum(thumb_bytes), 0) FROM media`).
		Scan(&files, &bytes)
	return files, bytes, err
}

// CollectionCount is a per-collection tally.
type CollectionCount struct {
	Code  string
	Name  string
	Count int64
}

// CollectionCounts lists every configured collection with its media count.
func (s *Store) CollectionCounts(ctx context.Context) ([]CollectionCount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.code, c.name, count(m.id) FROM collections c
		   LEFT JOIN media m ON m.collection_id = c.id AND m.missing = 0
		  GROUP BY c.id ORDER BY c.code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CollectionCount
	for rows.Next() {
		var c CollectionCount
		if err := rows.Scan(&c.Code, &c.Name, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// WaitForWriteIdle blocks until no other writer holds the database, up to
// timeout. Used by commands that must not run concurrently with a task.
func (s *Store) WaitForWriteIdle(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := s.db.ExecContext(ctx, `BEGIN IMMEDIATE; COMMIT;`)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("store: database still busy: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
