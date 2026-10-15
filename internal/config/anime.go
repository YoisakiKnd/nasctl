package config

// Anime configures the native RSS -> qBittorrent -> library pipeline.
// Local directories refer to the same files visible to qBittorrent; containers
// may use a different absolute prefix via QBittorrentDownloadDir.
type Anime struct {
	Enabled                bool   `toml:"enabled"`
	AutoPoll               bool   `toml:"auto_poll"`
	PollIntervalSeconds    int    `toml:"poll_interval_seconds"`
	MaxItemsPerFeed        int    `toml:"max_items_per_feed"`
	QBittorrentURL         string `toml:"qbittorrent_url"`
	Username               string `toml:"username"`
	Password               string `toml:"password"`
	DownloadDir            string `toml:"download_dir"`
	QBittorrentDownloadDir string `toml:"qbittorrent_download_dir"`
	LibraryDir             string `toml:"library_dir"`
	TMDBToken              string `toml:"tmdb_token"`
}
