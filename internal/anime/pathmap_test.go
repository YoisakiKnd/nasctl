package anime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestContainerDownloadPrefixMapsToLocalRootWithoutEscaping(t *testing.T) {
	e, down, cfg := newEngine(t, sampleRSS)
	e.cfg.QBittorrentDownloadDir = "/downloads/anime"
	for _, tc := range []struct{ remote, want string }{{"/downloads/anime", "."}, {"/downloads/anime/", "."}, {"/downloads/anime/subscription-1", "subscription-1"}, {"/downloads/anime/sub/folder/", "sub/folder"}} {
		got, err := e.localDownloadPath(tc.remote)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	for _, remote := range []string{"relative", "/downloads/anime-other/secret", "/downloads/anime/../../secret", "/downloads/other", "/downloads/anime/\x00x", "/downloads/anime\\outside"} {
		if _, err := e.localDownloadPath(remote); err == nil {
			t.Fatal("unsafe mapping", remote)
		}
	}
	ctx := context.Background()
	if _, err := e.Refresh(ctx, 0); err != nil {
		t.Fatal(err)
	}
	episodes, err := e.Store.Episodes(ctx, "discovered", 50, 0)
	if err != nil || len(episodes) == 0 {
		t.Fatal(episodes, err)
	}
	ep := episodes[0]
	if err := e.Queue(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(ctx, ep.SubscriptionID); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("nasctl-episode-%d", ep.ID)
	torrent := down.torrents[tag]
	if torrent.SavePath != "/downloads/anime/subscription-1" {
		t.Fatal("submission did not use remote prefix", torrent)
	}
	local := filepath.Join(cfg.DownloadDir, "subscription-1")
	if err := os.MkdirAll(local, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "episode.mkv"), []byte("mapped video bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "episode.zh.srt"), []byte("mapped subtitle"), 0600); err != nil {
		t.Fatal(err)
	}
	down.files = []TorrentFile{{Name: "episode.mkv", Progress: 1}, {Name: "episode.zh.srt", Progress: 1}}
	torrent.State = "uploading"
	torrent.Progress = 1
	down.torrents[tag] = torrent
	result, err := e.Sync(ctx, ep.SubscriptionID)
	if err != nil || result.Archived != 1 || len(result.Errors) != 0 {
		t.Fatal(result, err)
	}
	ep, err = e.Store.Episode(ctx, ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(cfg.LibraryDir, ep.LibraryPath))
	if err != nil || string(content) != "mapped video bytes" {
		t.Fatal(string(content), err)
	}
	if _, err := os.Stat(filepath.Join(local, "episode.mkv")); err != nil {
		t.Fatal("mapping removed original", err)
	}
}
