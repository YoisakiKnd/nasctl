// Package anime owns nasctl's automatic anime subscription and library pipeline.
// It does not depend on an existing suzu database or a Python runtime.
package anime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/statelease"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("anime: not found")
var ErrBusy = errors.New("anime: a refresh is already running")

type Subscription struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	FeedURL      string `json:"feed_url"`
	Include      string `json:"include"`
	Exclude      string `json:"exclude"`
	Season       int    `json:"season"`
	Enabled      bool   `json:"enabled"`
	AutoDownload bool   `json:"auto_download"`
	TMDBID       int64  `json:"tmdb_id"`
	StartEpisode int    `json:"start_episode"`
	EndEpisode   int    `json:"end_episode"`
	LastChecked  int64  `json:"last_checked"`
	LastError    string `json:"last_error"`
	ArchivedAt   int64  `json:"archived_at"`
}

type Episode struct {
	ID             int64   `json:"id"`
	SubscriptionID int64   `json:"subscription_id"`
	Series         string  `json:"series"`
	Title          string  `json:"title"`
	URL            string  `json:"url,omitempty"`
	Number         int     `json:"number"`
	Season         int     `json:"season"`
	State          string  `json:"state"`
	Hash           string  `json:"hash,omitempty"`
	Progress       float64 `json:"progress"`
	Error          string  `json:"error,omitempty"`
	LibraryPath    string  `json:"library_path,omitempty"`
	UpdatedAt      int64   `json:"updated_at"`
	Attempts       int     `json:"attempts"`
	EverSubmitted  bool    `json:"ever_submitted"`
	WatchPosition  float64 `json:"watch_position"`
	WatchDuration  float64 `json:"watch_duration"`
	Watched        bool    `json:"watched"`
	WatchUpdatedAt int64   `json:"watch_updated_at"`
}

type Store struct {
	db       *sql.DB
	filename string
	lease    *statelease.Lease
}

func OpenStore(filename string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(filename), 0750); err != nil {
		return nil, err
	}
	lease, err := statelease.Acquire(filepath.Dir(filename), false)
	if err != nil {
		return nil, err
	}
	keepLease := false
	defer func() {
		if !keepLease {
			_ = lease.Close()
		}
	}()
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(filename)+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, filename: filename, lease: lease}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS subscriptions (
		 id INTEGER PRIMARY KEY, title TEXT NOT NULL, feed_url TEXT NOT NULL,
		 include_rule TEXT NOT NULL DEFAULT '', exclude_rule TEXT NOT NULL DEFAULT '',
		 season INTEGER NOT NULL DEFAULT 1, enabled INTEGER NOT NULL DEFAULT 1,
		 auto_download INTEGER NOT NULL DEFAULT 0, tmdb_id INTEGER NOT NULL DEFAULT 0,
		 last_checked INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS episodes (
		 id INTEGER PRIMARY KEY, subscription_id INTEGER NOT NULL REFERENCES subscriptions(id),
		 guid TEXT NOT NULL, title TEXT NOT NULL, url TEXT NOT NULL, number INTEGER NOT NULL,
		 season INTEGER NOT NULL, state TEXT NOT NULL DEFAULT 'discovered', hash TEXT NOT NULL DEFAULT '',
		 progress REAL NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', library_path TEXT NOT NULL DEFAULT '',
		 updated_at INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
		 UNIQUE(subscription_id,guid), UNIQUE(subscription_id,season,number))`,
		`CREATE INDEX IF NOT EXISTS idx_anime_state ON episodes(state,updated_at)`,
	}
	for _, q := range stmts {
		if _, err = db.ExecContext(ctx, q); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err = s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	keepLease = true
	return s, nil
}

func (s *Store) Close() error {
	err := s.db.Close()
	if release := s.lease.Close(); err == nil {
		err = release
	}
	return err
}

func (s *Store) SaveSubscription(ctx context.Context, sub Subscription) (*Subscription, error) {
	if err := validateSubscription(sub); err != nil {
		return nil, err
	}
	sub = normalizeSubscription(sub)
	if sub.ID == 0 {
		id, err := s.nextID(ctx, "subscriptions")
		if err != nil {
			return nil, err
		}
		res, err := s.db.ExecContext(ctx, `INSERT INTO subscriptions(id,title,feed_url,include_rule,exclude_rule,season,enabled,auto_download,tmdb_id,start_episode,end_episode)
		 SELECT ?,?,?,?,?,?,?,?,?,?,? WHERE (SELECT count(*) FROM subscriptions)<100`, id, sub.Title, sub.FeedURL, sub.Include, sub.Exclude, sub.Season, sub.Enabled, sub.AutoDownload, sub.TMDBID, sub.StartEpisode, sub.EndEpisode)
		if err != nil {
			return nil, err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil, errors.New("anime: subscription limit is 100")
		}
		sub.ID, _ = res.LastInsertId()
	} else {
		// Existing release destinations depend on the series title and season.
		// Freeze those after discovery rather than silently renaming a library.
		old, err := s.Subscription(ctx, sub.ID)
		if err != nil {
			return nil, err
		}
		if old.ArchivedAt != 0 {
			return nil, inputError("订阅已归档，请先恢复后再编辑")
		}
		if old.Title != sub.Title || old.Season != sub.Season {
			var n int
			if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM episodes WHERE subscription_id=?`, sub.ID).Scan(&n); err != nil {
				return nil, err
			}
			if n > 0 {
				return nil, errors.New("anime: title and season cannot change after episodes were discovered; create a new subscription")
			}
		}
		_, err = s.db.ExecContext(ctx, `UPDATE subscriptions SET title=?,feed_url=?,include_rule=?,exclude_rule=?,season=?,enabled=?,auto_download=?,tmdb_id=?,start_episode=?,end_episode=? WHERE id=?`, sub.Title, sub.FeedURL, sub.Include, sub.Exclude, sub.Season, sub.Enabled, sub.AutoDownload, sub.TMDBID, sub.StartEpisode, sub.EndEpisode, sub.ID)
		if err != nil {
			return nil, err
		}
	}
	return s.Subscription(ctx, sub.ID)
}

const subColumns = `id,title,feed_url,include_rule,exclude_rule,season,enabled,auto_download,tmdb_id,start_episode,end_episode,last_checked,last_error,archived_at`

type scanner interface{ Scan(...any) error }

func scanSub(row scanner) (Subscription, error) {
	var v Subscription
	err := row.Scan(&v.ID, &v.Title, &v.FeedURL, &v.Include, &v.Exclude, &v.Season, &v.Enabled, &v.AutoDownload, &v.TMDBID, &v.StartEpisode, &v.EndEpisode, &v.LastChecked, &v.LastError, &v.ArchivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}

func (s *Store) Subscription(ctx context.Context, id int64) (*Subscription, error) {
	v, err := scanSub(s.db.QueryRowContext(ctx, `SELECT `+subColumns+` FROM subscriptions WHERE id=?`, id))
	return &v, err
}
func (s *Store) Subscriptions(ctx context.Context) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+subColumns+` FROM subscriptions ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		v, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) recordCheck(ctx context.Context, id int64, message string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE subscriptions SET last_checked=?,last_error=? WHERE id=?`, time.Now().Unix(), message, id)
	return err
}

func (s *Store) discover(ctx context.Context, sub Subscription, item FeedItem) (bool, error) {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(item.GUID)))
	var known bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM episodes WHERE subscription_id=? AND (guid=? OR (season=? AND number=?))) OR EXISTS(SELECT 1 FROM anime_dedup WHERE subscription_id=? AND (guid_sha256=? OR (season=? AND number=?)))`, sub.ID, item.GUID, sub.Season, item.Episode, sub.ID, hash, sub.Season, item.Episode).Scan(&known)
	if err != nil || known {
		return false, err
	}
	id, err := s.nextID(ctx, "episodes")
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO episodes(id,subscription_id,guid,title,url,number,season,updated_at)
	 SELECT ?,?,?,?,?,?,?,? WHERE (SELECT count(*) FROM episodes)<20000 ON CONFLICT DO NOTHING`, id, sub.ID, item.GUID, item.Title, item.URL, item.Episode, sub.Season, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM episodes`).Scan(&count); err != nil {
			return false, err
		}
		if count >= 20000 {
			return false, errors.New("anime: episode limit reached (20000); archive history before adding more")
		}
	}
	return n > 0, nil
}

func (s *Store) nextID(ctx context.Context, kind string) (int64, error) {
	if kind != "episodes" && kind != "subscriptions" {
		return 0, inputError("记录类型无效")
	}
	var id int64
	// The table name is one of the two constants above. Retained counters never
	// fall after cleanup, including when the highest row was removed.
	err := s.db.QueryRowContext(ctx, `UPDATE anime_sequences SET next_id=MAX(next_id,(SELECT COALESCE(MAX(id),0)+1 FROM `+kind+`))+1 WHERE name=? AND next_id<9007199254740991 AND (SELECT COALESCE(MAX(id),0) FROM `+kind+`)<9007199254740991 RETURNING next_id-1`, kind).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, inputError("记录编号已达到安全上限")
	}
	return id, err
}

const epColumns = `e.id,e.subscription_id,s.title,e.title,e.url,e.number,e.season,e.state,e.hash,e.progress,e.error,e.library_path,e.updated_at,e.attempts,e.ever_submitted,e.watch_position,e.watch_duration,e.watched,e.watch_updated_at`

func scanEpisode(row scanner) (Episode, error) {
	var e Episode
	err := row.Scan(&e.ID, &e.SubscriptionID, &e.Series, &e.Title, &e.URL, &e.Number, &e.Season, &e.State, &e.Hash, &e.Progress, &e.Error, &e.LibraryPath, &e.UpdatedAt, &e.Attempts, &e.EverSubmitted, &e.WatchPosition, &e.WatchDuration, &e.Watched, &e.WatchUpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return e, err
}
func (s *Store) Episodes(ctx context.Context, state string, limit, offset int) ([]Episode, error) {
	return s.EpisodesFor(ctx, 0, state, limit, offset)
}
func (s *Store) EpisodesFor(ctx context.Context, subscriptionID int64, state string, limit, offset int) ([]Episode, error) {
	return s.EpisodesForWatch(ctx, subscriptionID, state, "", limit, offset)
}
func (s *Store) EpisodesForWatch(ctx context.Context, subscriptionID int64, state, watch string, limit, offset int) ([]Episode, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	if offset < 0 || offset > 20000 {
		offset = 0
	}
	q := `SELECT ` + epColumns + ` FROM episodes e JOIN subscriptions s ON s.id=e.subscription_id`
	args := []any{}
	if state != "" {
		q += ` WHERE e.state=?`
		args = append(args, state)
	}
	if subscriptionID > 0 {
		if state == "" {
			q += ` WHERE `
		} else {
			q += ` AND `
		}
		q += `e.subscription_id=?`
		args = append(args, subscriptionID)
	}
	if watch != "" {
		if state == "" && subscriptionID == 0 {
			q += ` WHERE `
		} else {
			q += ` AND `
		}
		switch watch {
		case "watched":
			q += `e.state='archived' AND e.watched=1`
		case "unwatched":
			q += `e.state='archived' AND e.watched=0`
		case "in_progress":
			q += `e.state='archived' AND e.watched=0 AND e.watch_position>0`
		default:
			return nil, inputError("观看状态筛选无效")
		}
	}
	if watch == "in_progress" {
		q += ` ORDER BY e.watch_updated_at DESC,e.id DESC LIMIT ? OFFSET ?`
	} else {
		q += ` ORDER BY e.id DESC LIMIT ? OFFSET ?`
	}
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Episode{}
	for rows.Next() {
		e, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) Episode(ctx context.Context, id int64) (Episode, error) {
	return scanEpisode(s.db.QueryRowContext(ctx, `SELECT `+epColumns+` FROM episodes e JOIN subscriptions s ON s.id=e.subscription_id WHERE e.id=?`, id))
}
func (s *Store) updateEpisode(ctx context.Context, e Episode) error {
	_, err := s.db.ExecContext(ctx, `UPDATE episodes SET state=?,hash=?,progress=?,error=?,library_path=?,updated_at=?,attempts=?,ever_submitted=? WHERE id=?`, e.State, e.Hash, e.Progress, e.Error, e.LibraryPath, time.Now().Unix(), e.Attempts, e.EverSubmitted, e.ID)
	return err
}
func (s *Store) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state,count(*) FROM episodes GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}

func normalizeSubscription(s Subscription) Subscription {
	if s.Season == 0 {
		s.Season = 1
	}
	if s.StartEpisode == 0 {
		s.StartEpisode = 1
	}
	return s
}
func validateSubscription(s Subscription) error {
	if s.StartEpisode < 0 || s.StartEpisode > 999 || s.EndEpisode < 0 || s.EndEpisode > 999 || (s.EndEpisode != 0 && s.EndEpisode < normalizeSubscription(s).StartEpisode) {
		return inputError("anime: 集数范围无效，起始集数为 1..999，结束集数为 0（不限）或不小于起始集数")
	}
	if strings.TrimSpace(s.Title) == "" || len(s.Title) > 200 || strings.ContainsAny(s.Title, "/\\\x00") || s.Title == "." || s.Title == ".." {
		return inputError("番剧名称为空、过长或包含路径分隔符，请使用普通番剧名称")
	}
	if s.ID < 0 || s.Season < 0 || s.Season > 99 || s.TMDBID < 0 {
		return inputError("anime: invalid id or season")
	}
	if len(s.FeedURL) > 2048 {
		return inputError("anime: feed URL is too long")
	}
	if err := validateHTTPURL(s.FeedURL); err != nil {
		return fmt.Errorf("anime: invalid feed URL: %w", err)
	}
	return validateRules(s.Include, s.Exclude)
}
