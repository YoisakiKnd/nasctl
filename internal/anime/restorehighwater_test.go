package anime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/statelease"
)

func TestRestoreNeverReusesPostBackupOrCleanedIdentities(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	current := filepath.Join(base, "current.sqlite")
	private := filepath.Join(base, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(private, "staged.sqlite")
	old, err := OpenStore(current)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.db.Exec(`INSERT INTO subscriptions(id,title,feed_url,season) VALUES(900,'Current','https://example.com/current',1); UPDATE anime_sequences SET next_id=501 WHERE name='subscriptions'; UPDATE anime_sequences SET next_id=5001 WHERE name='episodes'; INSERT INTO anime_dedup(episode_id,subscription_id,season,number,guid_sha256,removed_at) VALUES(8500,900,1,1,'minimal',1)`); err != nil {
		t.Fatal(err)
	}
	if err = old.Close(); err != nil {
		t.Fatal(err)
	}
	copy, err := OpenStore(staged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = copy.db.Exec(`PRAGMA user_version=4; DROP TABLE anime_sequences; DROP TABLE anime_dedup`); err != nil {
		t.Fatal(err)
	}
	if err = copy.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := statelease.Acquire(base, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	floor, err := PreserveRestoreHighwater(ctx, current, staged)
	if err != nil || floor.Subscriptions != 901 || floor.Episodes != 8501 {
		t.Fatal(floor, err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	copy, err = OpenStore(staged)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	sub, err := copy.SaveSubscription(ctx, Subscription{Title: "After Restore", FeedURL: "https://example.com/restore", Season: 1, Enabled: true})
	if err != nil || sub.ID != 901 {
		t.Fatal(sub, err)
	}
	if _, err = copy.discover(ctx, *sub, FeedItem{GUID: "post-restore", Title: "After Restore - 01", URL: "https://example.com/file.torrent", Episode: 1}); err != nil {
		t.Fatal(err)
	}
	rows, err := copy.EpisodesFor(ctx, sub.ID, "", 10, 0)
	if err != nil || len(rows) != 1 || rows[0].ID != 8501 {
		t.Fatal(rows, err)
	}
	old, err = OpenStore(current)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var next int64
	if err = old.db.QueryRow("SELECT next_id FROM anime_sequences WHERE name='subscriptions'").Scan(&next); err != nil || next != 501 {
		t.Fatal("current database modified", next, err)
	}
}

func TestRestoreHighwaterRefusesAliasAndUnsupportedCurrentSchema(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "current.sqlite")
	staged := filepath.Join(root, "staged.sqlite")
	db, err := OpenStore(current)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Link(current, staged); err != nil {
		t.Fatal(err)
	}
	if _, err = PreserveRestoreHighwater(context.Background(), current, staged); err == nil {
		t.Fatal("hardlinked staging accepted")
	}
	if err = os.Remove(staged); err != nil {
		t.Fatal(err)
	}
	copy, err := OpenStore(staged)
	if err != nil {
		t.Fatal(err)
	}
	if err = copy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenStore(current)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = PreserveRestoreHighwater(context.Background(), current, staged); err == nil {
		t.Fatal("future current schema accepted")
	}
}
