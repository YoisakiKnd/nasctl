package store

import (
	"context"
	"fmt"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/cachepath"
)

// SchemaState describes whether the on-disk index matches this binary.
type SchemaState struct {
	OK       bool
	Found    int
	Expected int
	Note     string
}

// SchemaState reads the schema version actually present in the file.
//
// It is deliberately a separate query from Open: doctor must be able to say
// "the index is from a different build" about a file it could open, and must
// also describe a file that refuses to open.
func (s *Store) SchemaState(ctx context.Context) (SchemaState, error) {
	out := SchemaState{Expected: SchemaVersion}
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&out.Found); err != nil {
		return out, fmt.Errorf("store: read user_version: %w", err)
	}
	var meta string
	_ = s.db.QueryRowContext(ctx, `SELECT v FROM meta WHERE k = 'schema_version'`).Scan(&meta)
	if out.Found == SchemaVersion {
		out.OK = true
		if meta != "" && meta != fmt.Sprint(SchemaVersion) {
			out.Note = fmt.Sprintf("meta records %s but PRAGMA user_version is %d", meta, out.Found)
		}
		return out, nil
	}
	out.Note = fmt.Sprintf("PRAGMA user_version is %d, meta records %q", out.Found, meta)
	return out, nil
}

// PinCollectionFSID stores or clears the pinned filesystem identity.
//
// The pin exists so that a RAID0 array which failed to assemble cannot quietly
// be mistaken for an empty photo library: the path would still exist as a
// directory on the system disk, but its filesystem identity would differ.
func (s *Store) PinCollectionFSID(ctx context.Context, id int64, fsid string) error {
	if err := s.guard(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE collections SET expect_fsid = ? WHERE id = ?`, fsid, id)
	return err
}

// AllThumbPaths returns the cache path every indexed thumbnail should occupy.
//
// It returns paths for rows recorded as ok only, because a row that is pending
// or failed legitimately has no file. The caller compares this set against the
// directory; the store itself never deletes anything.
func (s *Store) AllThumbPaths(ctx context.Context, cacheRoot string, edge, depth int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.code, m.rel_path
		  FROM media m
		  JOIN collections c ON c.id = m.collection_id
		 WHERE m.thumb_state = 'ok' AND m.missing = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]string, 0, 4096)
	for rows.Next() {
		var code, rel string
		if err := rows.Scan(&code, &rel); err != nil {
			return nil, err
		}
		p, err := cachepath.Path(cacheRoot, cachepath.Key(code, rel), edge, depth)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetThumbOffline is not implemented on purpose. There is no method that
// removes index rows for files that are absent from disk: the design forbids
// "auto-delete disappeared originals", and the absence of a method is a
// stronger guarantee than a comment.

// TaskReportPrefix is a small helper for tests that need a stable age metric.
func AgeSince(ts int64) time.Duration { return time.Since(time.Unix(ts, 0)) }
