package anime

import (
	"context"
	"errors"
)

// The previous native module had user_version=0. Upgrade in one transaction;
// opening an existing state directory preserves every subscription and episode.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 5 {
		return errors.New("anime: database belongs to a newer version")
	}
	if version == 0 {
		for _, q := range []string{
			`ALTER TABLE subscriptions ADD COLUMN start_episode INTEGER NOT NULL DEFAULT 1`,
			`ALTER TABLE subscriptions ADD COLUMN end_episode INTEGER NOT NULL DEFAULT 0`,
		} {
			if _, err = tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		version = 1
	}
	if version == 1 {
		for _, q := range []string{
			`ALTER TABLE episodes ADD COLUMN ever_submitted INTEGER NOT NULL DEFAULT 0`,
			`UPDATE episodes SET ever_submitted=1 WHERE state IN ('queued','submitted','downloading','archived','failed') OR attempts>0 OR hash!=''`,
			`PRAGMA user_version=2`,
		} {
			if _, err = tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		version = 2
	}
	if version == 2 {
		for _, q := range []string{
			`ALTER TABLE episodes ADD COLUMN watch_position REAL NOT NULL DEFAULT 0`,
			`ALTER TABLE episodes ADD COLUMN watch_duration REAL NOT NULL DEFAULT 0`,
			`ALTER TABLE episodes ADD COLUMN watched INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE episodes ADD COLUMN watch_updated_at INTEGER NOT NULL DEFAULT 0`,
			`PRAGMA user_version=3`,
		} {
			if _, err = tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		version = 3
	}
	if version == 3 {
		if _, err = tx.ExecContext(ctx, `ALTER TABLE subscriptions ADD COLUMN archived_at INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `PRAGMA user_version=4`); err != nil {
			return err
		}
		version = 4
	}
	if version == 4 {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS anime_sequences(name TEXT PRIMARY KEY,next_id INTEGER NOT NULL)`,
			`INSERT OR IGNORE INTO anime_sequences(name,next_id) SELECT 'subscriptions',COALESCE(MAX(id),0)+1 FROM subscriptions`,
			`INSERT OR IGNORE INTO anime_sequences(name,next_id) SELECT 'episodes',COALESCE(MAX(id),0)+1 FROM episodes`,
			`CREATE TABLE IF NOT EXISTS anime_dedup(episode_id INTEGER PRIMARY KEY,subscription_id INTEGER NOT NULL,season INTEGER NOT NULL,number INTEGER NOT NULL,guid_sha256 TEXT NOT NULL,removed_at INTEGER NOT NULL,UNIQUE(subscription_id,season,number))`,
			`CREATE INDEX IF NOT EXISTS idx_anime_dedup_guid ON anime_dedup(subscription_id,guid_sha256)`,
			`PRAGMA user_version=5`,
		} {
			if _, err = tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
