package anime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/config"
)

func TestReleaseParsingAndRules(t *testing.T) {
	for title, want := range map[string]int{
		"[Group] Test Show - 07 [1080p][CHS].mkv": 7,
		"Test Show S02E03.mkv":                    3,
		"[字幕组] 番剧 第12话 [1080p]":                   12,
		"Test Show [04v2][1080p]":                 4,
		"Test Show 1080p HEVC":                    0,
		"Test Show - 01-12 Batch":                 0,
	} {
		if got := ParseEpisode(title); got != want {
			t.Errorf("%q: got %d want %d", title, got, want)
		}
	}
	sub := Subscription{Title: "Test Show", Include: "Test Show.*1080p", Exclude: "HEVC", Season: 1}
	if !matches(sub, "Test Show - 01 [1080p]") {
		t.Fatal("matching release refused")
	}
	for _, title := range []string{"Other Show - 01 [1080p]", "Test Show - 01 [1080p][HEVC]", "Test Show S02E01 [1080p]"} {
		if matches(sub, title) {
			t.Fatalf("unexpected match: %s", title)
		}
	}
	if validateRules("[", "") == nil {
		t.Fatal("invalid regex accepted")
	}
}

func TestRSSBoundsAndUnsafeLinks(t *testing.T) {
	rss := `<rss><channel><item><guid>one</guid><title>Test - 01</title><enclosure url="magnet:?xt=urn:btih:abc"/></item><item><title>Test - 02</title><enclosure url="file:///etc/passwd"/></item></channel></rss>`
	items, err := ParseFeed(strings.NewReader(rss), 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Episode != 1 {
		t.Fatalf("items=%+v", items)
	}
	oversized := `<rss><channel><item><title>` + strings.Repeat("x", MaxFeedBytes) + `</title></item></channel></rss>`
	if _, err = ParseFeed(strings.NewReader(oversized), 200); err == nil {
		t.Fatal("oversized feed accepted")
	}
	if _, err = ParseFeed(strings.NewReader(`<rss><broken>`), 200); err == nil {
		t.Fatal("invalid XML accepted")
	}
}

type fakeDownloader struct {
	mu       sync.Mutex
	torrents map[string]Torrent
	files    []TorrentFile
	adds     int
	reject   error
}

func (f *fakeDownloader) Find(ctx context.Context, tag string) ([]Torrent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.torrents[tag]; ok {
		return []Torrent{t}, nil
	}
	return nil, nil
}
func (f *fakeDownloader) Add(ctx context.Context, uri, save, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adds++
	if f.reject != nil {
		return f.reject
	}
	f.torrents[tag] = Torrent{Hash: "hash", SavePath: save, Progress: 0, State: "downloading"}
	return nil
}
func (f *fakeDownloader) Files(context.Context, string) ([]TorrentFile, error) { return f.files, nil }

func newEngine(t *testing.T, rss string) (*Engine, *fakeDownloader, config.Anime) {
	t.Helper()
	base := t.TempDir()
	cfg := config.Anime{Enabled: true, QBittorrentURL: "http://127.0.0.1:9999", DownloadDir: filepath.Join(base, "downloads"), LibraryDir: filepath.Join(base, "library"), MaxItemsPerFeed: 200}
	for _, p := range []string{cfg.DownloadDir, cfg.LibraryDir} {
		if err := os.Mkdir(p, 0750); err != nil {
			t.Fatal(err)
		}
	}
	e, err := Open(cfg, filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, rss) }))
	t.Cleanup(feed.Close)
	e.feedClient = feed.Client()
	fake := &fakeDownloader{torrents: map[string]Torrent{}}
	e.downloader = fake
	_, err = e.Store.SaveSubscription(context.Background(), Subscription{Title: "Test Show", FeedURL: feed.URL, Season: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return e, fake, cfg
}

const sampleRSS = `<rss><channel>
 <item><guid>ep2</guid><title>[Group] Test Show - 02 [1080p]</title><enclosure url="https://example.com/2.torrent"/></item>
 <item><guid>ep1-v2</guid><title>[Other] Test Show - 01v2 [1080p]</title><enclosure url="https://example.com/1v2.torrent"/></item>
 <item><guid>ep1</guid><title>[Group] Test Show - 01 [1080p]</title><enclosure url="https://example.com/1.torrent"/></item>
 <item><guid>other</guid><title>Other Show - 03 [1080p]</title><enclosure url="https://example.com/3.torrent"/></item>
 </channel></rss>`

func TestPipelineDiscoveryDownloadArchiveAndRestart(t *testing.T) {
	ctx := context.Background()
	e, down, cfg := newEngine(t, sampleRSS)
	result, err := e.Refresh(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Discovered != 2 || down.adds != 0 {
		t.Fatalf("result=%+v adds=%d", result, down.adds)
	}
	result, err = e.Refresh(ctx, 0)
	if err != nil || result.Discovered != 0 {
		t.Fatalf("dedup result=%+v err=%v", result, err)
	}
	episodes, err := e.Store.Episodes(ctx, "discovered", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ep Episode
	for _, item := range episodes {
		if item.Number == 1 {
			ep = item
		}
	}
	if err = e.Queue(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	result, err = e.Refresh(ctx, 0)
	if err != nil || result.Submitted != 1 {
		t.Fatalf("submit=%+v err=%v", result, err)
	}
	tag := fmt.Sprintf("nasctl-episode-%d", ep.ID)
	torrent := down.torrents[tag]
	torrent.Progress = .5
	down.torrents[tag] = torrent
	if _, err = e.Refresh(ctx, 0); err != nil {
		t.Fatal(err)
	}
	ep, err = e.Store.Episode(ctx, ep.ID)
	if err != nil || ep.State != "downloading" || ep.Progress != .5 {
		t.Fatalf("episode=%+v err=%v", ep, err)
	}
	if err = os.MkdirAll(torrent.SavePath, 0750); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(torrent.SavePath, "original.mkv")
	if err = os.WriteFile(source, []byte("original bytes"), 0640); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(torrent.SavePath, "original.chs.ass"), []byte("subtitle"), 0640); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(torrent.SavePath, "original2.srt"), []byte("another episode's subtitles"), 0640); err != nil {
		t.Fatal(err)
	}
	down.files = []TorrentFile{{Name: "original.mkv", Progress: 1}, {Name: "original.chs.ass", Progress: 1}, {Name: "original2.srt", Progress: 1}}
	torrent.Progress = 1
	torrent.State = "uploading"
	down.torrents[tag] = torrent
	result, err = e.Refresh(ctx, 0)
	if err != nil || result.Archived != 1 || len(result.Errors) > 0 {
		t.Fatalf("archive=%+v err=%v", result, err)
	}
	ep, err = e.Store.Episode(ctx, ep.ID)
	if err != nil || ep.State != "archived" {
		t.Fatalf("episode=%+v err=%v", ep, err)
	}
	video := filepath.Join(cfg.LibraryDir, ep.LibraryPath)
	data, err := os.ReadFile(video)
	if err != nil || string(data) != "original bytes" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	a, _ := os.Stat(source)
	b, _ := os.Stat(video)
	if !os.SameFile(a, b) {
		t.Fatal("archive did not hard-link original")
	}
	for _, suffix := range []string{".nfo", ".chs.ass"} {
		if _, err = os.Stat(strings.TrimSuffix(video, ".mkv") + suffix); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = os.Stat(strings.TrimSuffix(video, ".mkv") + "2.srt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("another episode's subtitle matched a partial filename prefix")
	}
	if _, err = e.Refresh(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if down.adds != 1 {
		t.Fatalf("duplicate submissions: %d", down.adds)
	}
	if err = e.Queue(ctx, ep.ID); err == nil {
		t.Fatal("archived episode can be queued again")
	}
	// A second SQLite handle observes durable state without relying on memory.
	reopened, err := OpenStore(filepath.Join(filepath.Dir(cfg.DownloadDir), "state", "anime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.Episode(ctx, ep.ID)
	if err != nil || persisted.State != "archived" {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
}

func TestSubmissionAmbiguityDoesNotAutomaticallyRetry(t *testing.T) {
	e, down, _ := newEngine(t, sampleRSS)
	ctx := context.Background()
	_, _ = e.Refresh(ctx, 0)
	eps, _ := e.Store.Episodes(ctx, "discovered", 50, 0)
	if err := e.Queue(ctx, eps[0].ID); err != nil {
		t.Fatal(err)
	}
	down.reject = errors.New("transport failed after submission")
	result, err := e.Refresh(ctx, 0)
	if err != nil || len(result.Errors) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	_, _ = e.Refresh(ctx, 0)
	_, _ = e.Refresh(ctx, 0)
	if down.adds != 1 {
		t.Fatalf("ambiguous submission retried %d times", down.adds)
	}
	if err = e.Queue(ctx, eps[0].ID); err != nil {
		t.Fatal(err)
	}
	down.reject = nil
	_, _ = e.Refresh(ctx, 0)
	if down.adds != 2 {
		t.Fatal("explicit retry not submitted")
	}
}

func TestArchiveRefusesEscapeAndDoesNotClobber(t *testing.T) {
	e, _, cfg := newEngine(t, sampleRSS)
	ctx := context.Background()
	if err := osWrite(filepath.Join(cfg.DownloadDir, "video.mkv"), "source"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(cfg.DownloadDir), "outside")
	if err := os.Mkdir(outside, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cfg.LibraryDir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := e.archiveFile(ctx, "video.mkv", "escape/video.mkv"); err == nil {
		t.Fatal("archive escaped library root")
	}
	if err := osWrite(filepath.Join(cfg.LibraryDir, "video.mkv"), "existing"); err != nil {
		t.Fatal(err)
	}
	if err := e.archiveFile(ctx, "video.mkv", "video.mkv"); err == nil {
		t.Fatal("existing file overwritten")
	}
	b, _ := os.ReadFile(filepath.Join(cfg.LibraryDir, "video.mkv"))
	if string(b) != "existing" {
		t.Fatal("existing destination changed")
	}
	for _, p := range []string{"../outside/secret", "/etc/passwd", "a/../../secret", "a\\b"} {
		if _, err := safeRelative(p); err == nil {
			t.Fatalf("unsafe path accepted: %s", p)
		}
	}
}
func osWrite(name, content string) error { return os.WriteFile(name, []byte(content), 0640) }

func TestPauseAndRefreshConcurrency(t *testing.T) {
	e, _, _ := newEngine(t, sampleRSS)
	ctx := context.Background()
	e.Admit = func(context.Context) error { return errors.New("paused") }
	if _, err := e.Refresh(ctx, 0); err == nil {
		t.Fatal("refresh ignored pause")
	}
	e.mu.Lock()
	if _, err := e.Refresh(ctx, 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("err=%v", err)
	}
	if err := e.Queue(ctx, 1); !errors.Is(err, ErrBusy) {
		t.Fatalf("queue err=%v", err)
	}
	e.mu.Unlock()
}

func TestQBittorrentCookieAuthAndRejectedSubmission(t *testing.T) {
	logins, adds := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") == "" {
			t.Error("missing Referer")
		}
		if r.URL.Path == "/api/v2/auth/login" {
			logins++
			r.ParseForm()
			if r.Form.Get("password") != "secret" {
				t.Error("wrong password")
			}
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "session", Path: "/"})
			io.WriteString(w, "Ok.")
			return
		}
		if _, err := r.Cookie("SID"); err != nil {
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			json.NewEncoder(w).Encode([]Torrent{{Hash: "abc", Progress: .5}})
		case "/api/v2/torrents/add":
			adds++
			io.WriteString(w, "Fails.")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	q := NewQBittorrent(server.URL, "admin", "secret")
	ctx := context.Background()
	rows, err := q.Find(ctx, "nasctl-episode-1")
	if err != nil || len(rows) != 1 || logins != 1 {
		t.Fatalf("rows=%+v logins=%d err=%v", rows, logins, err)
	}
	if err = q.Add(ctx, "magnet:?xt=urn:btih:abc", "/downloads", "nasctl-episode-1"); !errors.Is(err, ErrDownloadRejected) {
		t.Fatalf("err=%v", err)
	}
	if adds != 1 || logins != 1 {
		t.Fatalf("adds=%d logins=%d", adds, logins)
	}
}

func TestPublicFeedClientRejectsPrivateNetwork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("private endpoint was reached") }))
	defer server.Close()
	res, err := publicHTTPClient().Get(server.URL)
	if res != nil {
		res.Body.Close()
	}
	if err == nil {
		t.Fatal("private feed request accepted")
	}
}

// Rejected submissions still consume the per-refresh work budget, and repeated
// refreshes must not keep moving the retry deadline into the future.
func TestRejectedDownloadsAreBoundedAndBackOff(t *testing.T) {
	var rss strings.Builder
	rss.WriteString("<rss><channel>")
	for i := 25; i >= 1; i-- {
		fmt.Fprintf(&rss, `<item><guid>%d</guid><title>Test Show - %02d</title><enclosure url="https://example.com/%d.torrent"/></item>`, i, i, i)
	}
	rss.WriteString("</channel></rss>")
	e, down, _ := newEngine(t, rss.String())
	ctx := context.Background()
	sub, err := e.Store.Subscription(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	sub.AutoDownload = true
	if _, err = e.SaveSubscription(ctx, *sub); err != nil {
		t.Fatal(err)
	}
	down.reject = ErrDownloadRejected
	if _, err = e.Refresh(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if down.adds != 20 {
		t.Fatalf("first refresh attempted %d downloads, want 20", down.adds)
	}
	ep, err := e.Store.Episode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	deadlineStart := ep.UpdatedAt
	if _, err = e.Refresh(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if down.adds != 25 {
		t.Fatalf("second refresh attempted %d downloads total, want 25", down.adds)
	}
	if _, err = e.Refresh(ctx, 0); err != nil {
		t.Fatal(err)
	}
	ep, err = e.Store.Episode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ep.UpdatedAt != deadlineStart || down.adds != 25 {
		t.Fatalf("backoff moved or retried: episode=%+v adds=%d", ep, down.adds)
	}
	if _, err = e.Store.db.ExecContext(ctx, "UPDATE episodes SET updated_at=updated_at-901 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	down.reject = nil
	if _, err = e.Refresh(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if down.adds != 26 {
		t.Fatalf("retry after deadline adds=%d, want 26", down.adds)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTMDBMetadataUsesExplicitIDAndIsNotOverwritten(t *testing.T) {
	e, _, cfg := newEngine(t, sampleRSS)
	e.cfg.TMDBToken = "test-token"
	calls := 0
	e.feedClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://api.themoviedb.org/3/tv/42?language=zh-CN" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("unexpected TMDB request: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":42,"name":"Different localized title","original_name":"Original & title","overview":"Plot <content>","first_air_date":"2026-01-01"}`)), Header: make(http.Header), Request: r}, nil
	})}
	sub := Subscription{Title: "Test Show", TMDBID: 42}
	if err := e.scrape(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfg.LibraryDir, "Test Show", "tvshow.nfo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<title>Test Show</title>", "Original &amp; title", "Plot &lt;content&gt;", `type="tmdb" default="true">42`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q in %s", want, b)
		}
	}
	if err = e.scrape(context.Background(), sub); err != nil || calls != 1 {
		t.Fatalf("existing metadata fetched again: calls=%d err=%v", calls, err)
	}
}
