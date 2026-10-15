package settings

import (
	"context"
	"errors"

	"github.com/YoisakiKnd/nasctl/internal/config"
)

var ErrConflict = errors.New("settings: saved configuration changed; reload before saving")

type Anime struct {
	Enabled                bool   `json:"enabled"`
	AutoPoll               bool   `json:"auto_poll"`
	PollIntervalSeconds    int    `json:"poll_interval_seconds"`
	MaxItemsPerFeed        int    `json:"max_items_per_feed"`
	QBittorrentURL         string `json:"qbittorrent_url"`
	DownloadDir            string `json:"download_dir"`
	QBittorrentDownloadDir string `json:"qbittorrent_download_dir"`
	LibraryDir             string `json:"library_dir"`
}
type View struct {
	Storage     []config.HubStorage `json:"storage"`
	Files       config.FileLimits   `json:"files"`
	Jobs        config.JobLimits    `json:"jobs"`
	Anime       Anime               `json:"anime"`
	GuardMemory config.GuardMemory  `json:"guard_memory"`
}
type Snapshot struct {
	Revision        string          `json:"revision"`
	Saved           View            `json:"saved"`
	Active          View            `json:"active"`
	Secrets         map[string]bool `json:"secrets"`
	RestartRequired bool            `json:"restart_required"`
}
type Secret struct {
	Action string `json:"action"`
	Value  string `json:"value,omitempty"`
}
type Request struct {
	Revision string            `json:"revision"`
	Settings View              `json:"settings"`
	Secrets  map[string]Secret `json:"secrets,omitempty"`
}
type Control interface {
	Get(context.Context) (Snapshot, error)
	Validate(context.Context, Request) (Snapshot, error)
	Save(context.Context, Request) (Snapshot, error)
}
