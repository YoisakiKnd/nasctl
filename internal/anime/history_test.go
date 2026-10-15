package anime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func archiveHistoryFixture(t *testing.T, e *Engine) {
	t.Helper()
	if _, err := e.SetSubscriptionArchived(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryCleanupRetainsDedupAndNeverReusesRemovedIDs(t *testing.T) {
	e, episodes, library := metadataRig(t)
	ctx := context.Background()
	opt := HistoryCleanup{Subscription: 1, ConfirmTitle: "Test Show", Limit: 1}
	if _, err := e.CleanupHistory(ctx, opt); err == nil {
		t.Fatal("active subscription cleaned")
	}
	archiveHistoryFixture(t, e)
	bad := opt
	bad.ConfirmTitle = "wrong"
	if _, err := e.CleanupHistory(ctx, bad); err == nil {
		t.Fatal("wrong name accepted")
	}
	result, err := e.CleanupHistory(ctx, opt)
	if err != nil || result.Deleted != 1 || result.Remaining != 1 {
		t.Fatal(result, err)
	}
	var retained int
	if err := e.Store.db.QueryRow(`SELECT count(*) FROM anime_dedup`).Scan(&retained); err != nil || retained != 1 {
		t.Fatal(retained, err)
	}
	if _, err := e.SetSubscriptionArchived(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetSubscriptionEnabled(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	refresh, err := e.Refresh(ctx, 1)
	if err != nil || refresh.Discovered != 0 {
		t.Fatal("cleared release rediscovered", refresh, err)
	}
	sub, _ := e.Store.Subscription(ctx, 1)
	preview, err := e.Preview(ctx, *sub)
	if err != nil || preview.New != 0 {
		t.Fatal("preview ignored dedup history", preview, err)
	}
	archiveHistoryFixture(t, e)
	opt.RemoveSubscription = true
	result, err = e.CleanupHistory(ctx, opt)
	if err != nil || !result.RemovedSubscription || result.Remaining != 0 {
		t.Fatal(result, err)
	}
	created, err := e.SaveSubscription(ctx, Subscription{Title: "New Identity", FeedURL: "https://example.com/feed", Season: 1, Enabled: true})
	if err != nil || created.ID <= 1 {
		t.Fatal("subscription ID reused", created, err)
	}
	if _, err := e.Store.discover(ctx, *created, FeedItem{GUID: "new-guid", Title: "New Identity - 01", URL: "https://example.com/new.torrent", Episode: 1}); err != nil {
		t.Fatal(err)
	}
	rows, err := e.Store.EpisodesFor(ctx, created.ID, "", 50, 0)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	for _, old := range episodes {
		if rows[0].ID <= old.ID {
			t.Fatal("episode ID reused", rows[0].ID, old.ID)
		}
		data, err := os.ReadFile(filepath.Join(library, old.LibraryPath))
		if err != nil || string(data) != "original video bytes" {
			t.Fatal("history cleanup changed video", string(data), err)
		}
	}
}

func TestHistoryCleanupRequiresExplicitDetachForLinkedDownloads(t *testing.T) {
	e, _, _ := metadataRig(t)
	ctx := context.Background()
	if _, err := e.Store.db.Exec(`UPDATE episodes SET state='downloading',ever_submitted=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	archiveHistoryFixture(t, e)
	opt := HistoryCleanup{Subscription: 1, ConfirmTitle: "Test Show", Limit: 50, RemoveSubscription: true}
	if _, err := e.CleanupHistory(ctx, opt); err == nil {
		t.Fatal("linked task detached implicitly")
	}
	opt.RemoveSubscription = false
	out, err := e.CleanupHistory(ctx, opt)
	if err != nil || out.Deleted != 1 || out.Remaining != 1 {
		t.Fatal(out, err)
	}
	opt.RemoveSubscription = true
	opt.DetachDownloads = true
	out, err = e.CleanupHistory(ctx, opt)
	if err != nil || out.Deleted != 1 || !out.RemovedSubscription {
		t.Fatal(out, err)
	}
}

func TestHistoryExportUsesReadOnlyConsistentSnapshotAndReportsCompletion(t *testing.T) {
	e, _, _ := metadataRig(t)
	ctx := context.Background()
	e.cfg.Password = "private-download-credential"
	e.cfg.TMDBToken = "private-metadata-credential"
	export, err := e.OpenHistoryExport(ctx, 1, 20000)
	if err != nil {
		t.Fatal(err)
	}
	defer export.Close()
	if _, err := export.tx.Exec(`DELETE FROM episodes`); err == nil {
		t.Fatal("export transaction is writable")
	}
	archiveHistoryFixture(t, e)
	if _, err := e.CleanupHistory(ctx, HistoryCleanup{Subscription: 1, ConfirmTitle: "Test Show", Limit: 1}); err != nil {
		t.Fatal("snapshot blocked writer", err)
	}
	var body bytes.Buffer
	if _, err := export.WriteTo(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.String(), "private-download-credential") || strings.Contains(body.String(), "private-metadata-credential") {
		t.Fatal("credential exported")
	}
	lines := strings.Split(strings.TrimSpace(body.String()), "\n")
	var header, footer map[string]any
	json.Unmarshal([]byte(lines[0]), &header)
	json.Unmarshal([]byte(lines[len(lines)-1]), &footer)
	if header["episodes"] != float64(2) || footer["episodes"] != float64(2) || footer["complete"] != true || footer["truncated"] != false {
		t.Fatal(header, footer)
	}
	current, err := e.OpenHistoryExport(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	body.Reset()
	if _, err := current.WriteTo(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.String(), `"kind":"dedup"`) {
		t.Fatal("retained dedup markers omitted")
	}
}
