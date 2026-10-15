package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SealMedia retains the row ID as an anonymous tombstone, preventing old links
// from ever resolving to a newly allocated media row. Current metadata is wiped.
func (s *Store) SealMedia(ctx context.Context, id int64) error {
	if e := s.guard(); e != nil {
		return e
	}
	if _, e := s.db.ExecContext(ctx, "PRAGMA secure_delete=ON"); e != nil {
		return e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var collectionID int64
	var oldKind string
	var missing bool
	if e = tx.QueryRowContext(ctx, "SELECT collection_id,kind,missing FROM media WHERE id=?", id).Scan(&collectionID, &oldKind, &missing); errors.Is(e, sql.ErrNoRows) {
		return ErrNotFound
	} else if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `UPDATE media SET rel_path=?,rel_dir='',base_name='private',ext='',size=0,mtime_ns=0,kind='private',mime='application/octet-stream',format='',width=0,height=0,pixels=0,animated=0,frames=0,duration_ms=0,taken_at=NULL,taken_src='none',taken_tz='',tz_offset_min=NULL,make='',model='',lens='',orientation=0,latitude=NULL,longitude=NULL,altitude_m=NULL,probe_state='skipped',probe_reason='',probe_note='',probe_attempts=0,thumb_state='skipped',thumb_reason='',thumb_bytes=0,thumb_attempts=0,thumb_ready_at=0,meta_extra='',missing=1,first_seen=0,last_seen=0,seen_run=0,next_attempt_at=0,search_text='' WHERE id=?`, fmt.Sprintf(".nasctl-photo-vault/%d", id), id)
	if e != nil {
		return e
	}
	if oldKind != "private" && !missing {
		if _, e = tx.ExecContext(ctx, "UPDATE collections SET media_count=max(0,media_count-1) WHERE id=?", collectionID); e != nil {
			return e
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}

	var busy, log, checkpointed int
	e = s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed)
	if e == nil && busy != 0 {
		return fmt.Errorf("album privacy checkpoint is busy")
	}
	return e
}
