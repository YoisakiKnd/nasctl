package anime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSubscriptionArchiveKeepsMediaHistoryDedupAndRestoresPaused(t *testing.T) {
	e, down, cfg := newEngine(t, sampleRSS)
	ctx := context.Background()
	if _, err := e.Refresh(ctx, 0); err != nil {
		t.Fatal(err)
	}
	rows, _ := e.Store.Episodes(ctx, "", 50, 0)
	ep := rows[0]
	if err := e.Queue(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	sub, _ := e.Store.Subscription(ctx, 1)
	sub.AutoDownload = true
	sub.Include = "Test.*"
	sub.TMDBID = 42
	if _, err := e.SaveSubscription(ctx, *sub); err != nil {
		t.Fatal(err)
	}
	ep, _ = e.Store.Episode(ctx, ep.ID)
	ep.State = "archived"
	ep.LibraryPath = "keep.mp4"
	ep.EverSubmitted = true
	if err := e.Store.updateEpisode(ctx, ep); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.db.ExecContext(ctx, `UPDATE episodes SET watch_position=12,watch_duration=60,watch_updated_at=100 WHERE id=?`, ep.ID); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(cfg.LibraryDir, "keep.mp4")
	if err := os.WriteFile(media, []byte("keep content"), 0640); err != nil {
		t.Fatal(err)
	}
	before, _ := e.Store.Episodes(ctx, "", 50, 0)
	archived, err := e.SetSubscriptionArchived(ctx, 1, true)
	if err != nil || archived.ArchivedAt == 0 || archived.Enabled || !archived.AutoDownload {
		t.Fatal(archived, err)
	}
	if pending, err := e.Pending(ctx); err != nil || pending {
		t.Fatal("archived pending", pending, err)
	}
	if _, err = e.Refresh(ctx, 0); err != nil || down.adds != 0 {
		t.Fatal("archived task advanced", err)
	}
	if _, err = e.SetSubscriptionEnabled(ctx, 1, true); err == nil {
		t.Fatal("archived enabled")
	}
	if _, err = e.SaveSubscription(ctx, *sub); err == nil {
		t.Fatal("old draft bypassed archive")
	}
	if err = e.Queue(ctx, rows[1].ID); err == nil {
		t.Fatal("archived queue")
	}
	after, _ := e.Store.Episodes(ctx, "", 50, 0)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("episode history changed", before, after)
	}
	b, err := os.ReadFile(media)
	if err != nil || string(b) != "keep content" {
		t.Fatal("media changed", err)
	}
	video, _, err := e.OpenVideo(ctx, ep.ID)
	if err != nil {
		t.Fatal("archived subscription media no longer playable", err)
	}
	content, err := io.ReadAll(video)
	video.Close()
	if err != nil || string(content) != "keep content" {
		t.Fatal("archived media stream", err)
	}
	reopened, err := OpenStore(filepath.Join(filepath.Dir(cfg.LibraryDir), "state", "anime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.Subscription(ctx, 1)
	if err != nil || persisted.ArchivedAt != archived.ArchivedAt {
		t.Fatal(persisted, err)
	}
	if _, err = e.SetSubscriptionArchived(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	restored, _ := e.Store.Subscription(ctx, 1)
	if restored.Enabled || restored.ArchivedAt != 0 || restored.Include != "Test.*" || restored.TMDBID != 42 {
		t.Fatal(restored)
	}
	if _, err = e.SetSubscriptionEnabled(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	result, err := e.Refresh(ctx, 0)
	if err != nil || result.Discovered != 0 {
		t.Fatal("dedup lost on restore", result, err)
	}
}

func TestArchiveSchemaThreeMigrationPreservesWatchState(t *testing.T) {
	ctx := context.Background()
	filename := filepath.Join(t.TempDir(), "anime.sqlite")
	store, err := OpenStore(filename)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := store.SaveSubscription(ctx, Subscription{Title: "Existing", FeedURL: "https://example.com/rss", Season: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.discover(ctx, *sub, FeedItem{GUID: "one", Title: "Existing - 01", URL: "https://example.com/one.torrent", Episode: 1}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE episodes SET state='archived',ever_submitted=1,library_path='keep.mp4',watch_position=17,watch_duration=90,watch_updated_at=123`, `ALTER TABLE subscriptions DROP COLUMN archived_at`, `PRAGMA user_version=3`} {
		if _, err = store.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := store.Episodes(ctx, "", 50, 0)
	store.Close()
	for i := 0; i < 2; i++ {
		store, err = OpenStore(filename)
		if err != nil {
			t.Fatal(err)
		}
		after, _ := store.Episodes(ctx, "", 50, 0)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("migration altered history")
		}
		saved, err := store.Subscription(ctx, sub.ID)
		if err != nil || saved.ArchivedAt != 0 || !saved.Enabled {
			t.Fatal(saved, err)
		}
		store.Close()
	}
}
