package anime

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/url"
	"time"
)

const MaxHistoryExportBytes = 512 << 20

type HistoryExport struct {
	db           *sql.DB
	tx           *sql.Tx
	rows         *sql.Rows
	ctx          context.Context
	cancel       context.CancelFunc
	subscription int64
	limit        int
	header       map[string]any
}

// A separate query-only connection holds the snapshot without occupying the
// engine's single write connection. Media bytes and service-account secrets
// are excluded; the operator's original RSS/release addresses are preserved.
func (e *Engine) OpenHistoryExport(ctx context.Context, subscription int64, limit int) (*HistoryExport, error) {
	if subscription < 0 || limit < 1 || limit > 20000 {
		return nil, inputError("导出范围无效，数量上限为 1–20000")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	dsn := "file:" + url.PathEscape(e.Store.filename) + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=cache_size(-2048)&_pragma=mmap_size(0)&_query_only=1"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		cancel()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		db.Close()
		cancel()
		return nil, err
	}
	export := &HistoryExport{db: db, tx: tx, ctx: ctx, cancel: cancel, subscription: subscription, limit: limit}
	fail := func(err error) (*HistoryExport, error) { export.Close(); return nil, err }
	var invalidSubscription bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM subscriptions WHERE (?=0 OR id=?) AND (length(title)>200 OR length(feed_url)>2048 OR length(include_rule)>512 OR length(exclude_rule)>512 OR length(last_error)>4096))`, subscription, subscription).Scan(&invalidSubscription); err != nil {
		return fail(err)
	}
	if invalidSubscription {
		return fail(inputError("订阅历史中有超限字段，未开始导出"))
	}
	q := `SELECT ` + subColumns + ` FROM subscriptions WHERE (?=0 OR id=?) ORDER BY id LIMIT 100`
	rows, err := tx.QueryContext(ctx, q, subscription, subscription)
	if err != nil {
		return fail(err)
	}
	subs := []Subscription{}
	for rows.Next() {
		v, err := scanSub(rows)
		if err != nil {
			rows.Close()
			return fail(err)
		}
		subs = append(subs, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fail(err)
	}
	if subscription > 0 && len(subs) == 0 {
		return fail(ErrNotFound)
	}
	var count, dedup int64
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM episodes WHERE (?=0 OR subscription_id=?)`, subscription, subscription).Scan(&count); err != nil {
		return fail(err)
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM anime_dedup WHERE (?=0 OR subscription_id=?)`, subscription, subscription).Scan(&dedup); err != nil {
		return fail(err)
	}
	if count > 20000 || dedup > 200000 {
		return fail(inputError("历史库超过正常容量上限，未开始导出"))
	}
	var oversized bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM episodes WHERE (?=0 OR subscription_id=?) AND (length(title)>2048 OR length(url)>8192 OR length(guid)>4096 OR length(hash)>128 OR length(error)>4096 OR length(library_path)>4096))`, subscription, subscription).Scan(&oversized); err != nil {
		return fail(err)
	}
	if oversized {
		return fail(inputError("历史中有超限记录，请先排查后再导出"))
	}
	export.header = map[string]any{"kind": "header", "format": "nasctl-anime-history", "version": 1, "created_at": time.Now().Unix(), "subscriptions": subs, "episodes": count, "dedup": dedup, "limit": limit, "truncated": count > int64(limit)}
	export.rows, err = tx.QueryContext(ctx, `SELECT `+epColumns+`,e.guid FROM episodes e JOIN subscriptions s ON s.id=e.subscription_id WHERE (?=0 OR e.subscription_id=?) ORDER BY e.id LIMIT ?`, subscription, subscription, limit)
	if err != nil {
		return fail(err)
	}
	return export, nil
}
func (x *HistoryExport) Close() {
	if x.cancel != nil {
		x.cancel()
	}
	if x.rows != nil {
		x.rows.Close()
		x.rows = nil
	}
	if x.tx != nil {
		x.tx.Rollback()
		x.tx = nil
	}
	if x.db != nil {
		x.db.Close()
		x.db = nil
	}
}

func (x *HistoryExport) WriteTo(w io.Writer) (int64, error) {
	var written int64
	count := 0
	dedup := 0
	line := func(value any) error {
		if err := x.ctx.Err(); err != nil {
			return err
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if len(data) > 512<<10 || written+int64(len(data)+1) > MaxHistoryExportBytes-4096 {
			return inputError("导出达到 512 MiB 限额，请缩小范围")
		}
		n, err := w.Write(append(data, '\n'))
		written += int64(n)
		if err == nil && n != len(data)+1 {
			return io.ErrShortWrite
		}
		return err
	}
	finish := func(err error) (int64, error) {
		footer := map[string]any{"kind": "end", "complete": err == nil, "episodes": count, "dedup": dedup, "truncated": x.header["truncated"]}
		if err != nil {
			footer["error"] = "导出未完成，请缩小范围或重试"
		}
		data, _ := json.Marshal(footer)
		n, writeErr := w.Write(append(data, '\n'))
		written += int64(n)
		if err == nil {
			err = writeErr
		}
		return written, err
	}
	if err := line(x.header); err != nil {
		return finish(err)
	}
	for x.rows.Next() {
		var e Episode
		var guid string
		err := x.rows.Scan(&e.ID, &e.SubscriptionID, &e.Series, &e.Title, &e.URL, &e.Number, &e.Season, &e.State, &e.Hash, &e.Progress, &e.Error, &e.LibraryPath, &e.UpdatedAt, &e.Attempts, &e.EverSubmitted, &e.WatchPosition, &e.WatchDuration, &e.Watched, &e.WatchUpdatedAt, &guid)
		if err != nil {
			return finish(err)
		}
		if err := line(map[string]any{"kind": "episode", "episode": e, "guid": guid}); err != nil {
			return finish(err)
		}
		count++
	}
	err := x.rows.Err()
	x.rows.Close()
	x.rows = nil
	if err != nil {
		return finish(err)
	}
	rows, err := x.tx.QueryContext(x.ctx, `SELECT episode_id,subscription_id,season,number,guid_sha256,removed_at FROM anime_dedup WHERE (?=0 OR subscription_id=?) ORDER BY episode_id LIMIT 200000`, x.subscription, x.subscription)
	if err != nil {
		return finish(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, sub, at int64
		var season, number int
		var guid string
		if err := rows.Scan(&id, &sub, &season, &number, &guid, &at); err != nil {
			return finish(err)
		}
		if err := line(map[string]any{"kind": "dedup", "episode_id": id, "subscription_id": sub, "season": season, "number": number, "guid_sha256": guid, "removed_at": at}); err != nil {
			return finish(err)
		}
		dedup++
	}
	return finish(rows.Err())
}
