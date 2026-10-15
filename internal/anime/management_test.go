package anime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPreviewRangeDuplicatesAndNoSideEffects(t *testing.T) {
	e, down, _ := newEngine(t, sampleRSS)
	ctx := context.Background()
	sub, _ := e.Store.Subscription(ctx, 1)
	preview, err := e.Preview(ctx, *sub)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Scanned != 4 || preview.Matched != 3 || preview.New != 2 {
		t.Fatalf("preview=%+v", preview)
	}
	counts, _ := e.Store.Counts(ctx)
	saved, _ := e.Store.Subscription(ctx, 1)
	if len(counts) != 0 || down.adds != 0 || saved.LastChecked != 0 {
		t.Fatalf("preview changed state: counts=%v adds=%d sub=%+v", counts, down.adds, saved)
	}
	sub.StartEpisode = 2
	sub.EndEpisode = 2
	preview, err = e.Preview(ctx, *sub)
	if err != nil || preview.Matched != 1 || preview.New != 1 {
		t.Fatalf("range=%+v err=%v", preview, err)
	}
	if _, err = e.SaveSubscription(ctx, *sub); err != nil {
		t.Fatal(err)
	}
	result, err := e.Refresh(ctx, 0)
	if err != nil || result.Discovered != 1 {
		t.Fatalf("discovery disagrees with preview: %+v %v", result, err)
	}
	preview, err = e.Preview(ctx, *sub)
	if err != nil || preview.Existing != 1 || preview.New != 0 {
		t.Fatalf("existing=%+v err=%v", preview, err)
	}
	sub.Include = "["
	if _, err = e.Preview(ctx, *sub); err == nil {
		t.Fatal("invalid rule accepted")
	}
	sub.Include = ""
	sub.StartEpisode = 5
	sub.EndEpisode = 2
	if _, err = e.SaveSubscription(ctx, *sub); err == nil {
		t.Fatal("reversed range accepted")
	}
}

func TestSyncBypassesRSSAndIgnoredEpisodeDoesNotAutoSubmit(t *testing.T) {
	e, down, _ := newEngine(t, sampleRSS)
	ctx := context.Background()
	if _, err := e.Refresh(ctx, 0); err != nil {
		t.Fatal(err)
	}
	rows, _ := e.Store.Episodes(ctx, "", 50, 0)
	if err := e.Ignore(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	sub, _ := e.Store.Subscription(ctx, 1)
	sub.AutoDownload = true
	if _, err := e.SaveSubscription(ctx, *sub); err != nil {
		t.Fatal(err)
	}
	e.feedClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("sync contacted RSS")
		return nil, errors.New("offline")
	})}
	result, err := e.Sync(ctx, 0)
	if err != nil || result.Submitted != 1 || down.adds != 1 {
		t.Fatalf("sync=%+v adds=%d err=%v", result, down.adds, err)
	}
	ignored, _ := e.Store.Episode(ctx, rows[0].ID)
	if ignored.State != "ignored" {
		t.Fatalf("ignored=%+v", ignored)
	}
	submitted, _ := e.Store.Episode(ctx, rows[1].ID)
	if err = e.Ignore(ctx, submitted.ID); err == nil {
		t.Fatal("submitted download can be ignored")
	}
	if err = e.Queue(ctx, submitted.ID); err != nil {
		t.Fatal(err)
	}
	if err = e.Ignore(ctx, submitted.ID); err == nil {
		t.Fatal("explicit retry erased submission history")
	}
	if err = e.Queue(ctx, ignored.ID); err != nil {
		t.Fatal(err)
	}
	result, err = e.Sync(ctx, 0)
	if err != nil || result.Submitted != 1 || down.adds != 2 {
		t.Fatalf("explicit restore=%+v adds=%d err=%v", result, down.adds, err)
	}
	status := e.Status(ctx)
	if status.LastSync == 0 || status.LastRun == 0 {
		t.Fatalf("missing independent timestamps: %+v", status)
	}
}

func TestPendingAndSubscriptionFilter(t *testing.T) {
	e, _, _ := newEngine(t, sampleRSS)
	ctx := context.Background()
	if pending, err := e.Pending(ctx); err != nil || pending {
		t.Fatalf("empty pending=%v err=%v", pending, err)
	}
	if _, err := e.Refresh(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if pending, err := e.Pending(ctx); err != nil || pending {
		t.Fatalf("manual discoveries pending=%v err=%v", pending, err)
	}
	other, err := e.SaveSubscription(ctx, Subscription{Title: "Other", FeedURL: "https://example.com/rss", Season: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.discover(ctx, *other, FeedItem{GUID: "other", Title: "Other - 01", URL: "https://example.com/other.torrent", Episode: 1}); err != nil {
		t.Fatal(err)
	}
	rows, err := e.Store.EpisodesFor(ctx, other.ID, "discovered", 50, 0)
	if err != nil || len(rows) != 1 || rows[0].Series != "Other" {
		t.Fatalf("filter=%+v err=%v", rows, err)
	}
	if err = e.Queue(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if pending, err := e.Pending(ctx); err != nil || !pending {
		t.Fatalf("queued pending=%v err=%v", pending, err)
	}
	if err = e.Ignore(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if pending, err := e.Pending(ctx); err != nil || pending {
		t.Fatalf("ignored pending=%v err=%v", pending, err)
	}
}

func TestRangeMigrationPreservesStateAndReopens(t *testing.T) {
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
	if _, err = store.discover(ctx, *sub, FeedItem{GUID: "keep", Title: "Existing - 01", URL: "https://example.com/1.torrent", Episode: 1}); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the previous schema while keeping real rows in place.
	for _, q := range []string{`ALTER TABLE subscriptions DROP COLUMN start_episode`, `ALTER TABLE subscriptions DROP COLUMN end_episode`, `ALTER TABLE episodes DROP COLUMN ever_submitted`, `ALTER TABLE episodes DROP COLUMN watch_position`, `ALTER TABLE episodes DROP COLUMN watch_duration`, `ALTER TABLE episodes DROP COLUMN watched`, `ALTER TABLE episodes DROP COLUMN watch_updated_at`, `ALTER TABLE subscriptions DROP COLUMN archived_at`, `PRAGMA user_version=0`} {
		if _, err = store.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()
	for i := 0; i < 2; i++ {
		store, err = OpenStore(filename)
		if err != nil {
			t.Fatal(err)
		}
		got, err := store.Subscription(ctx, sub.ID)
		if err != nil || got.Title != "Existing" || got.StartEpisode != 1 || got.EndEpisode != 0 {
			t.Fatalf("migration=%+v err=%v", got, err)
		}
		rows, err := store.Episodes(ctx, "", 50, 0)
		if err != nil || len(rows) != 1 || rows[0].Number != 1 {
			t.Fatalf("episodes=%+v err=%v", rows, err)
		}
		store.Close()
	}
}

func TestRangeEditDoesNotAutoQueueOldDiscoveredEpisodes(t *testing.T) {
	e, down, _ := newEngine(t, sampleRSS)
	ctx := context.Background()
	if _, err := e.Refresh(ctx, 0); err != nil {
		t.Fatal(err)
	}
	sub, _ := e.Store.Subscription(ctx, 1)
	sub.AutoDownload = true
	sub.StartEpisode = 2
	sub.EndEpisode = 2
	if _, err := e.SaveSubscription(ctx, *sub); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if down.adds != 1 {
		t.Fatalf("range ignored on existing discoveries: adds=%d", down.adds)
	}
}

func TestSchedulerSeparatesFeedAndDownloadTicks(t *testing.T) {
	e, _, _ := newEngine(t, sampleRSS)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feeds := make(chan time.Time)
	downloads := make(chan time.Time)
	called := make(chan string, 2)
	done := make(chan struct{})
	e.OnPoll = func(context.Context) { called <- "feed" }
	e.OnSync = func(context.Context) { called <- "sync" }
	go func() { defer close(done); e.runTicks(ctx, feeds, downloads) }()
	downloads <- time.Now()
	if got := <-called; got != "sync" {
		t.Fatal(got)
	}
	feeds <- time.Now()
	if got := <-called; got != "feed" {
		t.Fatal(got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
}

func TestConnectionProbeUsesCookieAuthAndDoesNotExposeSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/auth/login" {
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "probe", Path: "/"})
			io.WriteString(w, "Ok.")
			return
		}
		if _, err := r.Cookie("SID"); err != nil {
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/api/v2/app/version":
			io.WriteString(w, "v5.1.0")
		case "/api/v2/app/webapiVersion":
			io.WriteString(w, "2.11.3")
		default:
			t.Errorf("unexpected probe: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	q := NewQBittorrent(server.URL, "operator", "not-in-result")
	result, err := q.Check(context.Background())
	if err != nil || !result.Connected || result.Version != "v5.1.0" || result.APIVersion != "2.11.3" {
		t.Fatalf("probe=%+v err=%v", result, err)
	}
	e, _, _ := newEngine(t, sampleRSS)
	e.downloader = q
	e.cfg.Password = "secret-password"
	e.cfg.TMDBToken = "secret-token"
	settings := fmt.Sprintf("%+v", e.Settings())
	if strings.Contains(settings, "secret-") {
		t.Fatalf("settings leaked secrets: %s", settings)
	}
	e.downloader = NewQBittorrent("http://127.0.0.1:1", "", "")
	failed, err := e.CheckConnection(context.Background())
	if err != nil || failed.Connected || failed.Error == "" {
		t.Fatalf("failure masked: %+v %v", failed, err)
	}
}
