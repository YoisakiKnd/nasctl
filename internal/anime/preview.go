package anime

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

type InputError struct{ Message string }

func (e *InputError) Error() string   { return e.Message }
func inputError(message string) error { return &InputError{Message: message} }

func (e *Engine) fetchFeed(ctx context.Context, address string) ([]FeedItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, inputError("RSS 地址无效")
	}
	req.Header.Set("User-Agent", "nasctl/1.0 (+https://github.com/YoisakiKnd/nasctl)")
	response, err := e.feedClient.Do(req)
	if err != nil {
		return nil, inputError("RSS 请求失败，请检查地址、网络和公开访问权限")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, inputError(fmt.Sprintf("RSS 返回 HTTP %d", response.StatusCode))
	}
	items, err := ParseFeed(response.Body, e.cfg.MaxItemsPerFeed)
	if err != nil {
		return nil, inputError("RSS 格式无效或超过 2 MiB 读取上限")
	}
	return items, nil
}

// matchReason is shared by preview and discovery. A passing title must also be
// an identifiable single episode of the requested season and episode range.
func matchReason(sub Subscription, item FeedItem) string {
	sub = normalizeSubscription(sub)
	if item.Episode == 0 {
		return "合集或无法识别单集集数"
	}
	if item.Episode < sub.StartEpisode || (sub.EndEpisode != 0 && item.Episode > sub.EndEpisode) {
		return "不在集数范围内"
	}
	include := sub.Include
	if include == "" {
		include = regexp.QuoteMeta(sub.Title)
	}
	matched, _ := regexp.MatchString("(?i)"+include, item.Title)
	if !matched {
		return "未命中包含规则或番剧名称"
	}
	if sub.Exclude != "" {
		excluded, _ := regexp.MatchString("(?i)"+sub.Exclude, item.Title)
		if excluded {
			return "命中排除规则"
		}
	}
	if !matches(sub, item.Title) {
		return "季数不匹配"
	}
	return ""
}

type PreviewItem struct {
	Title    string `json:"title"`
	Number   int    `json:"number"`
	Matched  bool   `json:"matched"`
	Existing bool   `json:"existing"`
	Reason   string `json:"reason"`
}
type PreviewResult struct {
	Scanned   int           `json:"scanned"`
	Matched   int           `json:"matched"`
	New       int           `json:"new"`
	Existing  int           `json:"existing"`
	Truncated bool          `json:"truncated"`
	Items     []PreviewItem `json:"items"`
}

// Preview fetches a bounded feed and evaluates an unsaved draft. It never
// discovers episodes, changes subscription state or contacts the downloader.
func (e *Engine) Preview(ctx context.Context, sub Subscription) (PreviewResult, error) {
	result := PreviewResult{Items: []PreviewItem{}}
	if err := validateSubscription(sub); err != nil {
		return result, err
	}
	sub = normalizeSubscription(sub)
	if !e.mu.TryLock() {
		return result, ErrBusy
	}
	defer e.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := e.admit(ctx); err != nil {
		return result, err
	}
	existing := map[int]bool{}
	if sub.ID != 0 {
		if _, err := e.Store.Subscription(ctx, sub.ID); err != nil {
			return result, err
		}
		rows, err := e.Store.db.QueryContext(ctx, `SELECT number FROM episodes WHERE subscription_id=? AND season=? UNION SELECT number FROM anime_dedup WHERE subscription_id=? AND season=?`, sub.ID, sub.Season, sub.ID, sub.Season)
		if err != nil {
			return result, err
		}
		for rows.Next() {
			var number int
			if err = rows.Scan(&number); err != nil {
				rows.Close()
				return result, err
			}
			existing[number] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return result, err
		}
	}
	items, err := e.fetchFeed(ctx, sub.FeedURL)
	if err != nil {
		return result, err
	}
	seen := map[int]bool{}
	result.Scanned = len(items)
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		reason := matchReason(sub, item)
		row := PreviewItem{Title: item.Title, Number: item.Episode, Reason: reason, Matched: reason == ""}
		if row.Matched {
			result.Matched++
			row.Existing = existing[item.Episode]
			if row.Existing {
				row.Reason = "本季已有这一集，包含忽略和历史清理后的去重标记"
				result.Existing++
			} else if seen[item.Episode] {
				row.Reason = "RSS 内同一集的重复发布"
			} else {
				row.Reason = "可发现的新剧集"
				result.New++
			}
			seen[item.Episode] = true
		}
		if len(result.Items) < 50 {
			result.Items = append(result.Items, row)
		} else {
			result.Truncated = true
		}
	}
	return result, nil
}
