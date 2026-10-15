package anime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Exercise the real qB HTTP adapter, ambiguous submission reconciliation,
// mapped archive, metadata/subtitles and durable playback as one workflow.
// Only the public RSS client is replaced with a loopback test client; production
// private-network feed restrictions remain covered by TestPublicClient tests.
func TestProtocolPipelineAmbiguousAcceptanceArchivePlaybackRestart(t *testing.T) {
	ctx := context.Background()
	e, _, cfg := newEngine(t, `<rss><channel><item><guid>protocol-one</guid><title>Test Show - 01 [1080p]</title><enclosure url="https://example.com/protocol.torrent"/></item></channel></rss>`)
	e.cfg.QBittorrentDownloadDir = "/downloads/anime"
	var mu sync.Mutex
	adds := 0
	accepted, complete := false, false
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	qb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			if !accepted {
				io.WriteString(w, "[]")
				return
			}
			if r.URL.Query().Get("tag") != "nasctl-episode-1" {
				t.Error("unstable episode tag")
			}
			progress, state := .5, "downloading"
			if complete {
				progress, state = 1, "uploading"
			}
			json.NewEncoder(w).Encode([]Torrent{{Hash: hash, SavePath: "/downloads/anime/subscription-1", State: state, Progress: progress}})
		case "/api/v2/torrents/add":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("savepath") != "/downloads/anime/subscription-1" || r.Form.Get("tags") != "nasctl,nasctl-episode-1" {
				t.Error("wrong mapping or tag", r.Form)
			}
			adds++
			accepted = true
			// The downloader accepted the task, but the caller cannot know it.
			w.WriteHeader(http.StatusBadGateway)
		case "/api/v2/torrents/files":
			if r.URL.Query().Get("hash") != hash {
				t.Error("wrong torrent hash")
			}
			json.NewEncoder(w).Encode([]TorrentFile{{Name: "original.mp4", Progress: 1}, {Name: "original.zh.srt", Progress: 1}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer qb.Close()
	e.downloader = NewQBittorrent(qb.URL, "", "")
	r, err := e.Refresh(ctx, 0)
	if err != nil || r.Discovered != 1 {
		t.Fatal(r, err)
	}
	if err := e.Queue(ctx, 1); err != nil {
		t.Fatal(err)
	}
	r, err = e.Sync(ctx, 1)
	if err != nil || len(r.Errors) != 1 {
		t.Fatal("ambiguous acceptance was not reported", r, err)
	}
	ep, err := e.Store.Episode(ctx, 1)
	if err != nil || ep.State != "submitted" || !ep.EverSubmitted {
		t.Fatal("submission intent not durable", ep, err)
	}
	if _, err := e.Sync(ctx, 1); err != nil {
		t.Fatal(err)
	}
	ep, err = e.Store.Episode(ctx, 1)
	if err != nil || ep.State != "downloading" || ep.Progress != .5 {
		t.Fatal("stable tag did not reconcile accepted download", ep, err)
	}
	source := filepath.Join(cfg.DownloadDir, "subscription-1")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	video := []byte("generated protocol video bytes")
	for name, data := range map[string][]byte{"original.mp4": video, "original.zh.srt": []byte("1\n00:00:00,000 --> 00:00:02,000\nGenerated subtitle\n")} {
		if err := os.WriteFile(filepath.Join(source, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	complete = true
	mu.Unlock()
	r, err = e.Sync(ctx, 1)
	if err != nil || r.Archived != 1 || len(r.Errors) != 0 {
		t.Fatal(r, err)
	}
	playback, err := e.Playback(ctx, 1)
	if err != nil || len(playback.Subtitles) != 1 || !playback.Subtitles[0].Playable {
		t.Fatal(playback, err)
	}
	archived := filepath.Join(cfg.LibraryDir, playback.Episode.LibraryPath)
	for _, name := range []string{archived, filepath.Join(source, "original.mp4")} {
		b, err := os.ReadFile(name)
		if err != nil || string(b) != string(video) {
			t.Fatal("media bytes changed", name, err)
		}
	}
	nfo, err := os.ReadFile(strings.TrimSuffix(archived, ".mp4") + ".nfo")
	if err != nil || !strings.Contains(string(nfo), "<episode>1</episode>") {
		t.Fatal("episode metadata missing", string(nfo), err)
	}
	position, duration := 6.0, 20.0
	if _, err := e.Store.SavePlayback(ctx, 1, PlaybackUpdate{Position: &position, Duration: &duration}); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(filepath.Dir(cfg.DownloadDir), "state")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(cfg, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	ep, err = restarted.Store.Episode(ctx, 1)
	if err != nil || ep.WatchPosition != position || ep.WatchDuration != duration || ep.State != "archived" {
		t.Fatal("playback/archive did not survive restart", ep, err)
	}
	restarted.downloader = NewQBittorrent(qb.URL, "", "")
	if _, err := restarted.Sync(ctx, 1); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if adds != 1 {
		t.Fatal(fmt.Sprintf("ambiguous response caused %d submissions", adds))
	}
}
