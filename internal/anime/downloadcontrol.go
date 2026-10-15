package anime

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func torrentPaused(state string) bool {
	state = strings.ToLower(state)
	return strings.HasPrefix(state, "paused") || strings.HasPrefix(state, "stopped")
}

func (e *Engine) ControlDownload(ctx context.Context, id int64, action string) (*Episode, error) {
	if action != "pause" && action != "resume" && action != "recheck" {
		return nil, inputError("下载操作无效")
	}
	if !e.mu.TryLock() {
		return nil, ErrBusy
	}
	defer e.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if action != "pause" {
		if err := e.admit(ctx); err != nil {
			return nil, err
		}
	}
	ep, err := e.Store.Episode(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ep.EverSubmitted || ep.State == "archived" || ep.State == "ignored" {
		return nil, inputError("此剧集没有可控制的下载任务")
	}
	sub, err := e.Store.Subscription(ctx, ep.SubscriptionID)
	if err != nil {
		return nil, err
	}
	if action != "pause" && (!sub.Enabled || sub.ArchivedAt != 0) {
		return nil, inputError("订阅已暂停或归档，请先启用再恢复下载或校验")
	}
	control, ok := e.downloader.(interface {
		Control(context.Context, string, string) error
	})
	if !ok {
		return nil, inputError("当前下载器不支持任务控制")
	}
	torrents, err := e.downloader.Find(ctx, fmt.Sprintf("nasctl-episode-%d", id))
	if err != nil {
		return nil, err
	}
	if len(torrents) != 1 || !validTorrentHash(torrents[0].Hash) {
		return nil, inputError("未找到唯一匹配的下载任务，请先同步并检查下载器")
	}
	t := torrents[0]
	if ep.Hash != "" && !strings.EqualFold(ep.Hash, t.Hash) {
		return nil, inputError("下载任务标识已变化，未执行操作，请核对下载器")
	}
	if err := control.Control(ctx, t.Hash, action); err != nil {
		return nil, inputError("下载器操作未确认，请同步状态后重试；没有重新提交下载")
	}
	ep.Hash = t.Hash
	ep.Progress = t.Progress
	ep.Error = ""
	if action == "pause" {
		ep.State = "paused"
	} else if action == "recheck" {
		ep.State = "checking"
	} else {
		ep.State = "downloading"
	}
	if err := e.Store.updateEpisode(ctx, ep); err != nil {
		return nil, err
	}
	saved, err := e.Store.Episode(ctx, id)
	return &saved, err
}
