package anime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/config"
)

type Engine struct {
	Store                     *Store
	cfg                       config.Anime
	downloader                Downloader
	feedClient                *http.Client
	downloadRoot, libraryRoot *os.Root
	metadataDir               string
	mu                        sync.Mutex
	statusMu                  sync.Mutex
	metadataMu                sync.Mutex
	running                   bool
	lastRun                   int64
	lastSync                  int64
	lastError                 string
	// Admit is checked before each unit of background work (pause/resources).
	Admit  func(context.Context) error
	OnPoll func(context.Context)
	OnSync func(context.Context)
}
type Status struct {
	Configured bool           `json:"configured"`
	AutoPoll   bool           `json:"auto_poll"`
	Running    bool           `json:"running"`
	LastRun    int64          `json:"last_run"`
	LastSync   int64          `json:"last_sync"`
	LastError  string         `json:"last_error"`
	Counts     map[string]int `json:"counts"`
}
type RefreshResult struct {
	Discovered int      `json:"discovered"`
	Submitted  int      `json:"submitted"`
	Archived   int      `json:"archived"`
	Skipped    int      `json:"skipped"`
	Errors     []string `json:"errors"`
}

func Open(cfg config.Anime, stateDir string) (*Engine, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.MaxItemsPerFeed == 0 {
		cfg.MaxItemsPerFeed = 200
	}
	if cfg.PollIntervalSeconds < 60 {
		cfg.PollIntervalSeconds = 900
	}
	store, err := OpenStore(filepath.Join(stateDir, "anime.sqlite"))
	if err != nil {
		return nil, err
	}
	e := &Engine{Store: store, cfg: cfg, downloader: NewQBittorrent(cfg.QBittorrentURL, cfg.Username, cfg.Password), feedClient: publicHTTPClient()}
	e.metadataDir = filepath.Join(stateDir, "anime-metadata-backups")
	// Missing volumes are an error, never created as empty libraries on /.
	e.downloadRoot, err = os.OpenRoot(cfg.DownloadDir)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("anime: download directory unavailable: %w", err)
	}
	e.libraryRoot, err = os.OpenRoot(cfg.LibraryDir)
	if err != nil {
		e.downloadRoot.Close()
		store.Close()
		return nil, fmt.Errorf("anime: library directory unavailable: %w", err)
	}
	return e, nil
}
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.feedClient.CloseIdleConnections()
	e.downloadRoot.Close()
	e.libraryRoot.Close()
	return e.Store.Close()
}
func (e *Engine) Status(ctx context.Context) Status {
	e.statusMu.Lock()
	s := Status{Configured: true, AutoPoll: e.cfg.AutoPoll, Running: e.running, LastRun: e.lastRun, LastSync: e.lastSync, LastError: e.lastError}
	e.statusMu.Unlock()
	counts, err := e.Store.Counts(ctx)
	if err != nil {
		s.LastError = err.Error()
	}
	s.Counts = counts
	return s
}

// Run is a single, cancellable scheduler with no overlapping refreshes. It is
// optional; manual refresh works with auto_poll=false.
func (e *Engine) Run(ctx context.Context) {
	if !e.cfg.AutoPoll {
		return
	}
	feeds := time.NewTicker(time.Duration(e.cfg.PollIntervalSeconds) * time.Second)
	downloads := time.NewTicker(time.Minute)
	defer feeds.Stop()
	defer downloads.Stop()
	e.runTicks(ctx, feeds.C, downloads.C)
}
func (e *Engine) runTicks(ctx context.Context, feeds, downloads <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-feeds:
			if e.OnPoll != nil {
				e.OnPoll(ctx)
			} else {
				_, _ = e.Refresh(ctx, 0)
			}
		case <-downloads:
			if e.OnSync != nil {
				e.OnSync(ctx)
			} else {
				_, _ = e.Sync(ctx, 0)
			}
		}
	}
}
func (e *Engine) admit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.Admit != nil {
		return e.Admit(ctx)
	}
	return nil
}

func (e *Engine) Refresh(ctx context.Context, subscriptionID int64) (RefreshResult, error) {
	return e.run(ctx, subscriptionID, true)
}

// Sync reconciles existing download intents and archives completed files without
// fetching RSS or metadata, so an unavailable feed cannot stall the library.
func (e *Engine) Sync(ctx context.Context, subscriptionID int64) (RefreshResult, error) {
	return e.run(ctx, subscriptionID, false)
}
func (e *Engine) run(ctx context.Context, subscriptionID int64, feeds bool) (result RefreshResult, err error) {
	result.Errors = []string{}
	if !e.mu.TryLock() {
		return result, ErrBusy
	}
	defer e.mu.Unlock()
	if err = e.admit(ctx); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	e.statusMu.Lock()
	e.running = true
	e.statusMu.Unlock()
	defer func() {
		e.statusMu.Lock()
		e.running = false
		if feeds {
			e.lastRun = time.Now().Unix()
		} else {
			e.lastSync = time.Now().Unix()
		}
		e.lastError = ""
		if err != nil {
			e.lastError = err.Error()
		} else if len(result.Errors) > 0 {
			e.lastError = strings.Join(result.Errors, "; ")
		}
		e.statusMu.Unlock()
	}()
	subs, err := e.Store.Subscriptions(ctx)
	if err != nil {
		return result, err
	}
	found := subscriptionID == 0
	for _, sub := range subs {
		if subscriptionID != 0 && sub.ID != subscriptionID {
			continue
		}
		found = true
		if !feeds || !sub.Enabled || sub.ArchivedAt != 0 {
			continue
		}
		if err = e.admit(ctx); err != nil {
			return result, err
		}
		n, skipped, ferr := e.refreshFeed(ctx, sub)
		result.Discovered += n
		result.Skipped += skipped
		message := ""
		if ferr != nil {
			message = ferr.Error()
			result.Errors = append(result.Errors, fmt.Sprintf("subscription %d: %s", sub.ID, message))
		}
		if cerr := e.Store.recordCheck(ctx, sub.ID, message); cerr != nil {
			return result, cerr
		}
	}
	if !found {
		return result, ErrNotFound
	}
	// At most 200 active releases and 20 new downloads per refresh. Older IDs
	// are processed first so a busy feed cannot starve an existing release.
	rows, qerr := e.Store.db.QueryContext(ctx, `SELECT e.id FROM episodes e JOIN subscriptions s ON s.id=e.subscription_id
	 WHERE s.enabled=1 AND s.archived_at=0 AND (?=0 OR s.id=?) AND (e.state IN ('queued','submitted','downloading','paused','checking') OR (e.state='discovered' AND s.auto_download=1 AND e.number>=s.start_episode AND (s.end_episode=0 OR e.number<=s.end_episode))) ORDER BY CASE WHEN e.state IN ('submitted','downloading','paused','checking') THEN 0 ELSE 1 END, e.id LIMIT 200`, subscriptionID, subscriptionID)
	if qerr != nil {
		return result, qerr
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return result, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	attempted := 0
	for _, id := range ids {
		if err = e.admit(ctx); err != nil {
			return result, err
		}
		ep, err := e.Store.Episode(ctx, id)
		if err != nil {
			return result, err
		}
		if ep.State == "queued" && ep.Attempts > 0 && time.Now().Unix()-ep.UpdatedAt < 900 {
			continue
		}
		if ep.State == "discovered" || ep.State == "queued" {
			if attempted >= 20 {
				continue
			}
			attempted++
		}
		submitted, archived, perr := e.advance(ctx, &ep)
		if submitted {
			result.Submitted++
		}
		if archived {
			result.Archived++
		}
		if perr != nil {
			ep.Error = perr.Error()
			result.Errors = append(result.Errors, fmt.Sprintf("episode %d: %s", id, ep.Error))
		}
		if err = e.Store.updateEpisode(ctx, ep); err != nil {
			return result, err
		}
	}
	return result, nil
}
func (e *Engine) refreshFeed(ctx context.Context, sub Subscription) (int, int, error) {
	items, err := e.fetchFeed(ctx, sub.FeedURL)
	if err != nil {
		return 0, 0, err
	}
	n, skipped := 0, 0
	// Most RSS sources are newest-first; ingest oldest-first for backfill.
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if matchReason(sub, item) != "" {
			skipped++
			continue
		}
		added, err := e.Store.discover(ctx, sub, item)
		if err != nil {
			return n, skipped, err
		}
		if added {
			n++
		}
	}
	if sub.TMDBID > 0 && e.cfg.TMDBToken != "" {
		if err = e.scrape(ctx, sub); err != nil {
			return n, skipped, err
		}
	}
	return n, skipped, nil
}
func (e *Engine) advance(ctx context.Context, ep *Episode) (bool, bool, error) {
	tag := fmt.Sprintf("nasctl-episode-%d", ep.ID)
	torrents, err := e.downloader.Find(ctx, tag)
	if err != nil {
		return false, false, err
	}
	if len(torrents) > 1 {
		return false, false, errors.New("multiple torrents have this episode tag; inspect downloader")
	}
	if len(torrents) == 0 {
		if ep.State == "submitted" || ep.State == "downloading" || ep.State == "paused" || ep.State == "checking" {
			return false, false, errors.New("submitted torrent is not visible; it may still be importing or was removed; retry explicitly")
		}
		if ep.Attempts >= 3 {
			ep.State = "failed"
			return false, false, errors.New("download submission failed three times; retry explicitly")
		}
		ep.Attempts++
		// Persist intent BEFORE the external side effect. If the NAS crashes
		// after acceptance, the next refresh reconciles by the stable tag.
		ep.State = "submitted"
		ep.EverSubmitted = true
		ep.Error = ""
		if err = e.Store.updateEpisode(ctx, *ep); err != nil {
			return false, false, err
		}
		err = e.downloader.Add(ctx, ep.URL, path.Join(e.downloaderDirectory(), fmt.Sprintf("subscription-%d", ep.SubscriptionID)), tag)
		if errors.Is(err, ErrDownloadRejected) {
			ep.State = "queued"
			if ep.Attempts >= 3 {
				ep.State = "failed"
			}
		}
		// A transport error is ambiguous: do not automatically resubmit it.
		return err == nil, false, err
	}
	t := torrents[0]
	ep.EverSubmitted = true
	ep.Hash = t.Hash
	ep.Progress = t.Progress
	ep.State = "downloading"
	ep.Error = ""
	if torrentPaused(t.State) {
		ep.State = "paused"
		return false, false, nil
	}
	if t.State == "error" || t.State == "missingFiles" {
		return false, false, errors.New("qBittorrent reports " + t.State)
	}
	if strings.Contains(strings.ToLower(t.State), "checking") {
		ep.State = "checking"
		return false, false, nil
	}
	if t.Progress < 1 {
		return false, false, nil
	}
	files, err := e.downloader.Files(ctx, t.Hash)
	if err != nil {
		return false, false, err
	}
	if err = e.archive(ctx, ep, t, files); err != nil {
		return false, false, err
	}
	ep.State = "archived"
	ep.Progress = 1
	ep.Error = ""
	return false, true, nil
}

// Queue is explicit acceptance of a discovered release or an explicit retry of
// a failed/ambiguous submission. Archived files are never re-downloaded.
func (e *Engine) Queue(ctx context.Context, id int64) error {
	if !e.mu.TryLock() {
		return ErrBusy
	}
	defer e.mu.Unlock()
	ep, err := e.Store.Episode(ctx, id)
	if err != nil {
		return err
	}
	if ep.State == "archived" {
		return errors.New("anime: episode is already archived")
	}
	sub, err := e.Store.Subscription(ctx, ep.SubscriptionID)
	if err != nil {
		return err
	}
	if sub.ArchivedAt != 0 {
		return inputError("订阅已归档，请先恢复并启用再选择下载")
	}
	if !sub.Enabled {
		return inputError("订阅已暂停，请先启用再选择下载")
	}
	ep.State = "queued"
	ep.Error = ""
	ep.Attempts = 0
	return e.Store.updateEpisode(ctx, ep)
}

// SaveSubscription serializes edits with discovery and archive work so that a
// refresh never publishes episodes using a concurrently changed series name.
func (e *Engine) SaveSubscription(ctx context.Context, sub Subscription) (*Subscription, error) {
	if !e.mu.TryLock() {
		return nil, ErrBusy
	}
	defer e.mu.Unlock()
	return e.Store.SaveSubscription(ctx, sub)
}
