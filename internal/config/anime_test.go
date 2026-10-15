package config

import (
	"strings"
	"testing"
)

const nativeDoc = `[paths]
state_dir="/srv/nasctl/state"
cache_dir="/srv/nasctl/cache"
[hub.anime]
enabled=true
qbittorrent_url="http://127.0.0.1:8080"
download_dir="/mnt/data/downloads/anime"
library_dir="/mnt/data/library/anime"
`

func TestNativeAnimeDoesNotRequirePhotoCollections(t *testing.T) {
	cfg, err := Parse(nativeDoc)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Collections) != 0 || cfg.Hub.Anime.PollIntervalSeconds != 900 || cfg.Hub.Anime.MaxItemsPerFeed != 200 {
		t.Fatalf("cfg=%+v", cfg.Hub.Anime)
	}
}
func TestNativeAnimeRejectsUnsafeConfiguration(t *testing.T) {
	for _, doc := range []string{
		strings.Replace(nativeDoc, `library_dir="/mnt/data/library/anime"`, `library_dir="/mnt/data/downloads/anime/library"`, 1),
		strings.Replace(nativeDoc, `library_dir="/mnt/data/library/anime"`, `library_dir="/srv/nasctl/state/library"`, 1),
		strings.Replace(nativeDoc, `download_dir="/mnt/data/downloads/anime"`, `download_dir="/"`, 1),
		strings.Replace(nativeDoc, `qbittorrent_url="http://127.0.0.1:8080"`, `qbittorrent_url="file:///etc/passwd"`, 1),
		nativeDoc + "poll_interval_seconds=1\n",
		nativeDoc + "max_items_per_feed=100000\n",
		nativeDoc + "qbittorrent_download_dir=\"/\"\n",
		nativeDoc + "qbittorrent_download_dir=\"relative\"\n",
		nativeDoc + "qbittorrent_download_dir=\"/downloads/../etc\"\n",
	} {
		if _, err := Parse(doc); err == nil {
			t.Fatalf("unsafe configuration accepted:\n%s", doc)
		}
	}
}
func TestContainerAnimeDownloadPrefix(t *testing.T) {
	cfg, err := Parse(nativeDoc + "qbittorrent_download_dir=\"/downloads/anime\"\n")
	if err != nil || cfg.Hub.Anime.QBittorrentDownloadDir != "/downloads/anime" {
		t.Fatal(cfg.Hub.Anime, err)
	}
}
