package anime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"
)

type HistoryCleanup struct {
	Subscription       int64  `json:"subscription"`
	ConfirmTitle       string `json:"confirm_title"`
	Limit              int    `json:"limit"`
	Before             int64  `json:"before"`
	DetachDownloads    bool   `json:"detach_downloads"`
	RemoveSubscription bool   `json:"remove_subscription"`
}
type HistoryResult struct {
	Deleted             int64 `json:"deleted"`
	Remaining           int64 `json:"remaining"`
	RemovedSubscription bool  `json:"removed_subscription"`
}
type HistoryStatus struct {
	Episodes int64 `json:"episodes"`
	Dedup    int64 `json:"dedup"`
	Linked   int64 `json:"linked"`
	Archived bool  `json:"archived"`
}

func (e *Engine) HistoryStatus(ctx context.Context, id int64) (HistoryStatus, error) {
	out := HistoryStatus{}
	sub, err := e.Store.Subscription(ctx, id)
	if err != nil {
		return out, err
	}
	out.Archived = sub.ArchivedAt != 0 && !sub.Enabled
	err = e.Store.db.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN ever_submitted=1 AND state!='archived' THEN 1 ELSE 0 END),0) FROM episodes WHERE subscription_id=?`, id).Scan(&out.Episodes, &out.Linked)
	if err != nil {
		return out, err
	}
	err = e.Store.db.QueryRowContext(ctx, `SELECT count(*) FROM anime_dedup WHERE subscription_id=?`, id).Scan(&out.Dedup)
	return out, err
}

func (e *Engine) CleanupHistory(ctx context.Context, opt HistoryCleanup) (HistoryResult, error) {
	out := HistoryResult{}
	if opt.Subscription < 1 || opt.Limit < 1 || opt.Limit > 20000 || opt.Before < 0 || (!opt.RemoveSubscription && opt.Limit > 1000) {
		return out, inputError("历史清理限额无效；分批最多 1000 条，移除整个订阅最多 20000 条")
	}
	if !e.mu.TryLock() {
		return out, ErrBusy
	}
	defer e.mu.Unlock()
	if err := e.admit(ctx); err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sub, err := e.Store.Subscription(ctx, opt.Subscription)
	if err != nil {
		return out, err
	}
	if sub.ArchivedAt == 0 || sub.Enabled {
		return out, inputError("请先归档并暂停订阅，再清理历史")
	}
	if opt.ConfirmTitle != sub.Title {
		return out, inputError("请输入完整订阅名称确认本次清理")
	}
	tx, err := e.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM episodes WHERE subscription_id=?`, sub.ID).Scan(&out.Remaining); err != nil {
		return out, err
	}
	// Freeze the current high-water marks even for records imported by a tool.
	for _, kind := range []string{"episodes", "subscriptions"} {
		if _, err = tx.ExecContext(ctx, `UPDATE anime_sequences SET next_id=MAX(next_id,(SELECT COALESCE(MAX(id),0)+1 FROM `+kind+`)) WHERE name=?`, kind); err != nil {
			return out, err
		}
	}
	if opt.RemoveSubscription {
		if opt.Before != 0 || out.Remaining > int64(opt.Limit) {
			return out, inputError("移除订阅须清理全部历史，请去掉截止时间并给出足够的条目上限")
		}
		var linked bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM episodes WHERE subscription_id=? AND ever_submitted=1 AND state!='archived')`, sub.ID).Scan(&linked); err != nil {
			return out, err
		}
		if linked && !opt.DetachDownloads {
			return out, inputError("仍有可能关联下载器的记录，请先核对并明确确认解除关联")
		}
		for _, q := range []string{`DELETE FROM episodes WHERE subscription_id=?`, `DELETE FROM anime_dedup WHERE subscription_id=?`, `DELETE FROM subscriptions WHERE id=?`} {
			if _, err = tx.ExecContext(ctx, q, sub.ID); err != nil {
				return out, err
			}
		}
		out.Deleted = out.Remaining
		out.Remaining = 0
		out.RemovedSubscription = true
		return out, tx.Commit()
	}
	q := `SELECT id,guid,season,number FROM episodes WHERE subscription_id=? AND (?=0 OR updated_at<?)`
	if !opt.DetachDownloads {
		q += ` AND (state='archived' OR ever_submitted=0)`
	}
	q += ` ORDER BY id LIMIT ?`
	rows, err := tx.QueryContext(ctx, q, sub.ID, opt.Before, opt.Before, opt.Limit)
	if err != nil {
		return out, err
	}
	type record struct {
		id             int64
		guid           string
		season, number int
	}
	records := []record{}
	for rows.Next() {
		var v record
		if err = rows.Scan(&v.id, &v.guid, &v.season, &v.number); err != nil {
			rows.Close()
			return out, err
		}
		records = append(records, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	var retained int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM anime_dedup`).Scan(&retained); err != nil {
		return out, err
	}
	if retained+int64(len(records)) > 200000 {
		return out, inputError("去重标记达到 200000 条上限，可导出并移除不再需要的完整订阅")
	}
	for _, v := range records {
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(v.guid)))
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO anime_dedup(episode_id,subscription_id,season,number,guid_sha256,removed_at) VALUES(?,?,?,?,?,?)`, v.id, sub.ID, v.season, v.number, hash, time.Now().Unix()); err != nil {
			return out, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM episodes WHERE id=? AND subscription_id=?`, v.id, sub.ID); err != nil {
			return out, err
		}
		out.Deleted++
	}
	out.Remaining -= out.Deleted
	return out, tx.Commit()
}
