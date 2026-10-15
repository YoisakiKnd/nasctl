package anime

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// RestoreHighwater holds the next allocatable identities, including identities
// whose historical rows were cleaned up while external downloader tags survived.
type RestoreHighwater struct {
	Subscriptions int64 `json:"subscriptions_next_id"`
	Episodes      int64 `json:"episodes_next_id"`
}

// PreserveRestoreHighwater prepares a private staged database before an offline
// restore. The caller MUST hold the exclusive service-state lock and stop all
// writers until publication. It never writes the current database. This helper
// alone does not acquire that lock and must not be called by a live HTTP restore.
func PreserveRestoreHighwater(ctx context.Context, current, staged string) (RestoreHighwater, error) {
	var zero RestoreHighwater
	if !filepath.IsAbs(current) || !filepath.IsAbs(staged) || current == staged {
		return zero, errors.New("anime: invalid restore staging paths")
	}
	old, err := os.Lstat(current)
	if err != nil {
		return zero, err
	}
	stage, err := os.Lstat(staged)
	if err != nil {
		return zero, err
	}
	private, err := os.Stat(filepath.Dir(staged))
	if err != nil {
		return zero, err
	}
	if !old.Mode().IsRegular() || !stage.Mode().IsRegular() || os.SameFile(old, stage) || !private.IsDir() || private.Mode().Perm()&0077 != 0 {
		return zero, errors.New("anime: restore requires a separate private staged database")
	}
	u := url.URL{Scheme: "file", Path: current}
	u.RawQuery = url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(1000)", "query_only(1)", "mmap_size(0)", "cache_size(-1024)"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return zero, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	var schema int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schema); err != nil {
		return zero, err
	}
	if schema < 1 || schema > 5 {
		return zero, errors.New("anime: unsupported current state schema for restore")
	}
	floor := RestoreHighwater{Subscriptions: 1, Episodes: 1}
	for _, entry := range []struct {
		table string
		value *int64
	}{{"subscriptions", &floor.Subscriptions}, {"episodes", &floor.Episodes}} {
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(id),0)+1 FROM "+entry.table).Scan(entry.value); err != nil {
			return zero, err
		}
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='anime_sequences')").Scan(&exists); err != nil {
		return zero, err
	}
	if exists {
		for _, entry := range []struct {
			name  string
			value *int64
		}{{"subscriptions", &floor.Subscriptions}, {"episodes", &floor.Episodes}} {
			var next int64
			if err = tx.QueryRowContext(ctx, "SELECT next_id FROM anime_sequences WHERE name=?", entry.name).Scan(&next); err != nil {
				return zero, err
			}
			*entry.value = max(*entry.value, next)
		}
	}
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='anime_dedup')").Scan(&exists); err != nil {
		return zero, err
	}
	if exists {
		var nextEp, nextSub int64
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(episode_id),0)+1,COALESCE(MAX(subscription_id),0)+1 FROM anime_dedup").Scan(&nextEp, &nextSub); err != nil {
			return zero, err
		}
		floor.Episodes = max(floor.Episodes, nextEp)
		floor.Subscriptions = max(floor.Subscriptions, nextSub)
	}
	if floor.Episodes < 1 || floor.Subscriptions < 1 || floor.Episodes > 9007199254740991 || floor.Subscriptions > 9007199254740991 {
		return zero, errors.New("anime: identity high-water mark is outside the supported range")
	}
	// Migrations affect the staged copy only. The current, potentially newer
	// allocator remains untouched until a complete restore is published.
	copy, err := OpenStore(staged)
	if err != nil {
		return zero, err
	}
	defer copy.Close()
	write, err := copy.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer write.Rollback()
	for _, entry := range []struct {
		name  string
		value *int64
	}{{"subscriptions", &floor.Subscriptions}, {"episodes", &floor.Episodes}} {
		if _, err = write.ExecContext(ctx, "UPDATE anime_sequences SET next_id=MAX(next_id,?,(SELECT COALESCE(MAX(id),0)+1 FROM "+entry.name+")) WHERE name=?", *entry.value, entry.name); err != nil {
			return zero, err
		}
		if err = write.QueryRowContext(ctx, "SELECT next_id FROM anime_sequences WHERE name=?", entry.name).Scan(entry.value); err != nil {
			return zero, err
		}
		if *entry.value < 1 || *entry.value > 9007199254740991 {
			return zero, errors.New("anime: staged identity high-water mark is outside the supported range")
		}
	}
	if err = write.Commit(); err != nil {
		return zero, err
	}
	return floor, nil
}
