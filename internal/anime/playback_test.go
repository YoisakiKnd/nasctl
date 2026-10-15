package anime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func playbackEpisode(t *testing.T, e *Engine, number int) Episode {
	t.Helper()
	ctx := context.Background()
	sub, err := e.Store.Subscription(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Store.discover(ctx, *sub, FeedItem{GUID: fmt.Sprint(number), Title: fmt.Sprintf("Test Show - %02d", number), URL: "https://example.com/one.torrent", Episode: number})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := e.Store.Episodes(ctx, "", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ep Episode
	for _, row := range rows {
		if row.Number == number {
			ep = row
		}
	}
	ep.State = "archived"
	ep.Progress = 1
	ep.LibraryPath = fmt.Sprintf("Test Show/Season 1/Test Show S01E%02d.mp4", number)
	if err = e.libraryRoot.MkdirAll("Test Show/Season 1", 0750); err != nil {
		t.Fatal(err)
	}
	if err = e.Store.updateEpisode(ctx, ep); err != nil {
		t.Fatal(err)
	}
	return ep
}
func pointer[T any](value T) *T { return &value }

func TestPlaybackProgressDurableIndependentAndFilterable(t *testing.T) {
	e, down, cfg := newEngine(t, sampleRSS)
	ctx := context.Background()
	ep := playbackEpisode(t, e, 1)
	other := playbackEpisode(t, e, 2)
	state, err := e.Store.SavePlayback(ctx, ep.ID, PlaybackUpdate{Position: pointer(30.0), Duration: pointer(120.0)})
	if err != nil || state.Position != 30 || state.Watched {
		t.Fatalf("saved: %+v %v", state, err)
	}
	// A background reconciler holding an old episode snapshot cannot clear playback.
	if err = e.Store.updateEpisode(ctx, ep); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Store.Episode(ctx, ep.ID)
	if got.WatchPosition != 30 || got.State != "archived" || got.Progress != 1 {
		t.Fatalf("background overwrite: %+v", got)
	}
	rows, err := e.Store.EpisodesForWatch(ctx, 1, "archived", "in_progress", 50, 0)
	if err != nil || len(rows) != 1 || rows[0].ID != ep.ID {
		t.Fatalf("continue filter: %+v %v", rows, err)
	}
	state, err = e.Store.SavePlayback(ctx, ep.ID, PlaybackUpdate{Position: pointer(115.0), Duration: pointer(120.0)})
	if err != nil || !state.Watched {
		t.Fatalf("completion: %+v %v", state, err)
	}
	state, err = e.Store.SavePlayback(ctx, ep.ID, PlaybackUpdate{Position: pointer(20.0), Duration: pointer(120.0)})
	if err != nil || !state.Watched {
		t.Fatal("seeking backwards silently cleared watched status")
	}
	state, err = e.Store.SavePlayback(ctx, ep.ID, PlaybackUpdate{Watched: pointer(false), Position: pointer(0.0), Duration: pointer(0.0)})
	if err != nil || state.Watched || state.Position != 0 {
		t.Fatalf("reset: %+v %v", state, err)
	}
	if _, err = e.Store.SavePlayback(ctx, other.ID, PlaybackUpdate{Watched: pointer(true)}); err != nil {
		t.Fatal(err)
	}
	rows, err = e.Store.EpisodesForWatch(ctx, 1, "", "watched", 50, 0)
	if err != nil || len(rows) != 1 || rows[0].ID != other.ID {
		t.Fatal("watched filter incorrect")
	}
	rows, err = e.Store.EpisodesForWatch(ctx, 1, "", "unwatched", 50, 0)
	if err != nil || len(rows) != 1 || rows[0].ID != ep.ID {
		t.Fatal("unwatched filter incorrect")
	}
	reopened, err := OpenStore(filepath.Join(filepath.Dir(cfg.DownloadDir), "state", "anime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err = reopened.Episode(ctx, other.ID)
	if err != nil || !got.Watched || got.WatchUpdatedAt == 0 {
		t.Fatal("watch state did not persist")
	}
	if down.adds != 0 {
		t.Fatal("playback contacted downloader")
	}
}

func TestPlaybackRejectsInvalidAndNonArchivedState(t *testing.T) {
	e, _, _ := newEngine(t, sampleRSS)
	ep := playbackEpisode(t, e, 1)
	ctx := context.Background()
	for _, update := range []PlaybackUpdate{{}, {Position: pointer(1.0)}, {Duration: pointer(2.0)}, {Position: pointer(-1.0), Duration: pointer(2.0)}, {Position: pointer(3.0), Duration: pointer(2.0)}, {Position: pointer(1.0), Duration: pointer(86401.0)}, {Position: pointer(math.NaN()), Duration: pointer(3.0)}, {Position: pointer(1.0), Duration: pointer(math.Inf(1))}} {
		if _, err := e.Store.SavePlayback(ctx, ep.ID, update); err == nil {
			t.Fatalf("accepted: %+v", update)
		}
	}
	ep.State = "discovered"
	if err := e.Store.updateEpisode(ctx, ep); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.SavePlayback(ctx, ep.ID, PlaybackUpdate{Watched: pointer(true)}); err == nil {
		t.Fatal("not archived accepted")
	}
	if _, err := e.Store.SavePlayback(ctx, 999, PlaybackUpdate{Watched: pointer(true)}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestSubtitleConversionBoundsAndEncoding(t *testing.T) {
	srt := "\ufeff1\r\n00:00:01,250 --> 00:00:02,500\r\n你好 <i>world</i>\r\n\r\n2\r\n00:01:03,001 --> 00:01:05,002\r\nSecond\r\n"
	converted, err := SubtitleVTT("show.srt", strings.NewReader(srt))
	if err != nil || !strings.HasPrefix(string(converted), "WEBVTT\n\n") || !strings.Contains(string(converted), "00:00:01.250 --> 00:00:02.500") || strings.Contains(string(converted), "\r") {
		t.Fatalf("conversion: %q %v", converted, err)
	}
	if got, err := SubtitleVTT("show.vtt", strings.NewReader(string(converted))); err != nil || string(got) != string(converted) {
		t.Fatal("VTT changed")
	}
	for _, tc := range []struct{ name, text string }{{"show.ass", "[Script Info]"}, {"show.srt", "no timeline"}, {"show.vtt", "bad header"}, {"show.srt", string([]byte{0xff, 0xfe})}, {"show.srt", strings.Repeat("x", maxSubtitleBytes+1)}, {"show.srt", strings.Repeat("\n", 20001)}} {
		if _, err := SubtitleVTT(tc.name, strings.NewReader(tc.text)); err == nil {
			t.Fatalf("invalid subtitle accepted: %s", tc.name)
		}
	}
}

func TestLibraryArtworkAndSubtitleContainment(t *testing.T) {
	e, _, cfg := newEngine(t, sampleRSS)
	ctx := context.Background()
	ep := playbackEpisode(t, e, 1)
	stem := strings.TrimSuffix(filepath.Join(cfg.LibraryDir, ep.LibraryPath), ".mp4")
	for suffix, content := range map[string]string{".srt": "1\n00:00:01,000 --> 00:00:02,000\nOne\n", ".chs.vtt": "WEBVTT\n\n", ".chs.ass": "[Script Info]", ".txt": "not subtitle"} {
		if err := os.WriteFile(stem+suffix, []byte(content), 0640); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret.srt")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, stem+".escape.srt"); err != nil {
		t.Fatal(err)
	}
	result, err := e.Playback(ctx, ep.ID)
	if err != nil || len(result.Subtitles) != 3 {
		t.Fatalf("siblings: %+v %v", result, err)
	}
	for _, sub := range result.Subtitles {
		if sub.Name == "Test Show S01E01.srt" && sub.Label != "默认字幕" {
			t.Fatal(sub.Label)
		}
		if sub.Format == "ass" && sub.Playable {
			t.Fatal("ASS advertised as playable")
		}
	}
	for _, name := range []string{"../secret.srt", "Test Show S01E02.srt", "Test Show S01E01.escape.srt", "Test Show S01E01.txt"} {
		f, _, err := e.OpenSubtitle(ctx, ep.ID, name)
		if f != nil {
			f.Close()
		}
		if err == nil {
			t.Fatalf("unsafe subtitle opened: %s", name)
		}
	}
	f, _, err := e.OpenSubtitle(ctx, ep.ID, "Test Show S01E01.srt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if !strings.Contains(string(b), "One") {
		t.Fatal("wrong sibling")
	}
	sub, _ := e.Store.Subscription(ctx, 1)
	if e.PosterAvailable(*sub) {
		t.Fatal("missing poster advertised")
	}
	poster := filepath.Join(cfg.LibraryDir, "Test Show", "poster.jpg")
	if err := os.WriteFile(poster, []byte("<html>not jpeg"), 0640); err != nil {
		t.Fatal(err)
	}
	if f, _, err := e.OpenPoster(ctx, 1); err == nil {
		f.Close()
		t.Fatal("HTML served as JPEG")
	}
	if err := os.WriteFile(poster, []byte{0xff, 0xd8, 0xff, 0xd9}, 0640); err != nil {
		t.Fatal(err)
	}
	f, _, err = e.OpenPoster(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(f)
	f.Close()
	if len(b) != 4 || !e.PosterAvailable(*sub) {
		t.Fatal("poster not available")
	}
	if err := os.Remove(poster); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, poster); err != nil {
		t.Fatal(err)
	}
	if e.PosterAvailable(*sub) {
		t.Fatal("symlink advertised")
	}
	if f, _, err := e.OpenPoster(ctx, 1); err == nil {
		f.Close()
		t.Fatal("poster symlink followed")
	}
}

func TestWatchSchemaTwoMigrationPreservesArchive(t *testing.T) {
	ctx := context.Background()
	filename := filepath.Join(t.TempDir(), "anime.sqlite")
	store, err := OpenStore(filename)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := store.SaveSubscription(ctx, Subscription{Title: "Existing", FeedURL: "https://example.com/rss", Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.discover(ctx, *sub, FeedItem{GUID: "one", Title: "Existing - 01", URL: "https://example.com/one.torrent", Episode: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`UPDATE episodes SET state='archived',library_path='Existing/Season 1/one.mp4',ever_submitted=1`, `ALTER TABLE episodes DROP COLUMN watch_position`, `ALTER TABLE episodes DROP COLUMN watch_duration`, `ALTER TABLE episodes DROP COLUMN watched`, `ALTER TABLE episodes DROP COLUMN watch_updated_at`, `ALTER TABLE subscriptions DROP COLUMN archived_at`, `PRAGMA user_version=2`} {
		if _, err = store.db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()
	store, err = OpenStore(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.Episodes(ctx, "", 50, 0)
	if err != nil || len(rows) != 1 || rows[0].State != "archived" || rows[0].LibraryPath == "" || !rows[0].EverSubmitted || rows[0].WatchPosition != 0 || rows[0].Watched {
		t.Fatalf("migration: %+v %v", rows, err)
	}
}
