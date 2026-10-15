package anime

import (
	"context"
	"time"
)

type Settings struct {
	AutoPoll               bool   `json:"auto_poll"`
	PollIntervalSeconds    int    `json:"poll_interval_seconds"`
	SyncIntervalSeconds    int    `json:"sync_interval_seconds"`
	MaxItemsPerFeed        int    `json:"max_items_per_feed"`
	QBittorrentURL         string `json:"qbittorrent_url"`
	DownloadDir            string `json:"download_dir"`
	QBittorrentDownloadDir string `json:"qbittorrent_download_dir"`
	LibraryDir             string `json:"library_dir"`
	TMDBConfigured         bool   `json:"tmdb_configured"`
}

func (e *Engine) Settings() Settings {
	return Settings{AutoPoll: e.cfg.AutoPoll, PollIntervalSeconds: e.cfg.PollIntervalSeconds, SyncIntervalSeconds: 60, MaxItemsPerFeed: e.cfg.MaxItemsPerFeed, QBittorrentURL: e.cfg.QBittorrentURL, DownloadDir: e.cfg.DownloadDir, QBittorrentDownloadDir: e.downloaderDirectory(), LibraryDir: e.cfg.LibraryDir, TMDBConfigured: e.cfg.TMDBToken != ""}
}

// Pending lets the scheduler stay idle when there is nothing to submit or
// reconcile, rather than creating an empty job and contacting the downloader.
func (e *Engine) Pending(ctx context.Context) (bool, error) {
	var pending bool
	err := e.Store.db.QueryRowContext(ctx, `SELECT EXISTS(
	 SELECT 1 FROM episodes e JOIN subscriptions s ON s.id=e.subscription_id
	 WHERE s.enabled=1 AND s.archived_at=0 AND (e.state IN ('queued','submitted','downloading','paused','checking') OR
	 (e.state='discovered' AND s.auto_download=1 AND e.number>=s.start_episode AND (s.end_episode=0 OR e.number<=s.end_episode))))`).Scan(&pending)
	return pending, err
}

type ConnectionStatus struct {
	Connected  bool   `json:"connected"`
	Version    string `json:"version,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
	CheckedAt  int64  `json:"checked_at"`
	Error      string `json:"error,omitempty"`
}

func (e *Engine) CheckConnection(ctx context.Context) (ConnectionStatus, error) {
	result := ConnectionStatus{CheckedAt: time.Now().Unix()}
	if !e.mu.TryLock() {
		return result, ErrBusy
	}
	defer e.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	checker, ok := e.downloader.(interface {
		Check(context.Context) (ConnectionStatus, error)
	})
	if !ok {
		return result, inputError("下载器不支持连接检查")
	}
	result, err := checker.Check(ctx)
	result.CheckedAt = time.Now().Unix()
	if err != nil {
		result.Connected = false
		result.Error = err.Error()
	}
	return result, nil
}

// Ignore is reversible bookkeeping only. Once a submission was attempted its
// outcome may be ambiguous; those episodes cannot be hidden as "not downloaded".
func (e *Engine) Ignore(ctx context.Context, id int64) error {
	if !e.mu.TryLock() {
		return ErrBusy
	}
	defer e.mu.Unlock()
	ep, err := e.Store.Episode(ctx, id)
	if err != nil {
		return err
	}
	if ep.State == "ignored" {
		return nil
	}
	if ep.State != "discovered" && !(ep.State == "queued" && ep.Attempts == 0 && !ep.EverSubmitted && ep.Hash == "") {
		return inputError("只能忽略尚未提交的剧集；已提交任务请在下载器中管理")
	}
	ep.State = "ignored"
	ep.Error = ""
	return e.Store.updateEpisode(ctx, ep)
}

// SetSubscriptionEnabled changes only scheduling state, so a quick action never
// writes an old form draft over newer rules or metadata selections.
func (e *Engine) SetSubscriptionEnabled(ctx context.Context, id int64, enabled bool) (*Subscription, error) {
	if !e.mu.TryLock() {
		return nil, ErrBusy
	}
	defer e.mu.Unlock()
	sub, err := e.Store.Subscription(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub.ArchivedAt != 0 {
		return nil, inputError("订阅已归档，请先恢复")
	}
	if _, err = e.Store.db.ExecContext(ctx, `UPDATE subscriptions SET enabled=? WHERE id=?`, enabled, id); err != nil {
		return nil, err
	}
	return e.Store.Subscription(ctx, id)
}

// Archiving keeps identity, release deduplication and playback history intact.
// Restore stays paused; opting back into scheduling is a separate user action.
func (e *Engine) SetSubscriptionArchived(ctx context.Context, id int64, archived bool) (*Subscription, error) {
	if !e.mu.TryLock() {
		return nil, ErrBusy
	}
	defer e.mu.Unlock()
	sub, err := e.Store.Subscription(ctx, id)
	if err != nil {
		return nil, err
	}
	if archived == (sub.ArchivedAt != 0) {
		return sub, nil
	}
	stamp := int64(0)
	if archived {
		stamp = time.Now().Unix()
	}
	if _, err = e.Store.db.ExecContext(ctx, `UPDATE subscriptions SET enabled=0,archived_at=? WHERE id=?`, stamp, id); err != nil {
		return nil, err
	}
	return e.Store.Subscription(ctx, id)
}
