package anime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const tmdbAPI = "https://api.themoviedb.org/3/"
const maxMetadataBytes = 512 << 10

type MetadataCandidate struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	OriginalName string `json:"original_name"`
	FirstAirDate string `json:"first_air_date"`
	Overview     string `json:"overview"`
}
type MetadataSearch struct {
	Results   []MetadataCandidate `json:"results"`
	Page      int                 `json:"page"`
	HasMore   bool                `json:"has_more"`
	Truncated bool                `json:"truncated"`
}
type MetadataSeason struct {
	Number   int    `json:"season_number"`
	Name     string `json:"name"`
	Episodes int    `json:"episode_count"`
	AirDate  string `json:"air_date"`
}
type MetadataSeries struct {
	MetadataCandidate
	Seasons []MetadataSeason `json:"seasons"`
}

func metadataText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return string(runes)
}
func cleanCandidate(v MetadataCandidate) MetadataCandidate {
	v.Name = metadataText(v.Name, 200)
	v.OriginalName = metadataText(v.OriginalName, 200)
	v.Overview = metadataText(v.Overview, 800)
	v.FirstAirDate = metadataText(v.FirstAirDate, 10)
	return v
}

// Fixed origin, short timeout, bounded body and no credential-bearing redirects.
// Search never touches subscriptions, the downloader or the media directory.
func (e *Engine) tmdbJSON(ctx context.Context, relative string, destination any) error {
	if e.cfg.TMDBToken == "" {
		return inputError("尚未配置 TMDB 凭据，可手动填写 ID，或在配置文件中设置 tmdb_token 后重启")
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tmdbAPI+relative, nil)
	if err != nil {
		return inputError("元数据查询参数无效")
	}
	req.Header.Set("Authorization", "Bearer "+e.cfg.TMDBToken)
	req.Header.Set("Accept", "application/json")
	client := *e.feedClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return inputError("TMDB 重定向已拒绝") }
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return inputError("TMDB 请求失败，请检查网络和凭据")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case 200:
	case 401, 403:
		return inputError("TMDB 凭据无效或没有访问权限")
	case 404:
		return inputError("未找到这个 TMDB 剧集")
	case 429:
		return inputError("TMDB 查询过于频繁，请稍后重试")
	default:
		return inputError(fmt.Sprintf("TMDB 返回 HTTP %d，请稍后重试", response.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return inputError("TMDB 响应读取失败")
	}
	if len(data) > maxMetadataBytes {
		return inputError("TMDB 响应超过 512 KiB 上限")
	}
	if err = json.Unmarshal(data, destination); err != nil {
		return inputError("TMDB 响应格式无效")
	}
	return nil
}
func (e *Engine) SearchMetadata(ctx context.Context, query string, page int) (MetadataSearch, error) {
	out := MetadataSearch{Results: []MetadataCandidate{}, Page: page}
	query = strings.TrimSpace(query)
	if query == "" || !utf8.ValidString(query) || utf8.RuneCountInString(query) > 200 || strings.ContainsFunc(query, unicode.IsControl) || page < 1 || page > 20 {
		return out, inputError("请输入 1–200 字的番剧名称，页码范围为 1–20")
	}
	if !e.metadataMu.TryLock() {
		return out, inputError("已有元数据查询正在进行，请稍后再试")
	}
	defer e.metadataMu.Unlock()
	params := url.Values{"query": {query}, "language": {"zh-CN"}, "include_adult": {"false"}, "page": {fmt.Sprint(page)}}
	var data struct {
		Results    []MetadataCandidate `json:"results"`
		TotalPages int                 `json:"total_pages"`
	}
	if err := e.tmdbJSON(ctx, "search/tv?"+params.Encode(), &data); err != nil {
		return out, err
	}
	seen := map[int64]bool{}
	for _, v := range data.Results {
		if len(out.Results) >= 20 {
			out.Truncated = true
			break
		}
		if v.ID < 1 || v.ID > 2147483647 || seen[v.ID] {
			continue
		}
		v = cleanCandidate(v)
		if v.Name == "" {
			v.Name = v.OriginalName
		}
		if v.Name == "" {
			continue
		}
		seen[v.ID] = true
		out.Results = append(out.Results, v)
	}
	out.HasMore = page < 20 && data.TotalPages > page
	return out, nil
}
func (e *Engine) MetadataDetails(ctx context.Context, id int64) (MetadataSeries, error) {
	out := MetadataSeries{Seasons: []MetadataSeason{}}
	if id < 1 || id > 2147483647 {
		return out, inputError("TMDB 剧集 ID 无效")
	}
	if !e.metadataMu.TryLock() {
		return out, inputError("已有元数据查询正在进行，请稍后再试")
	}
	defer e.metadataMu.Unlock()
	var data MetadataSeries
	if err := e.tmdbJSON(ctx, fmt.Sprintf("tv/%d?language=zh-CN", id), &data); err != nil {
		return out, err
	}
	if data.ID != id {
		return out, inputError("TMDB 返回了不同剧集，未应用选择")
	}
	out.MetadataCandidate = cleanCandidate(data.MetadataCandidate)
	if out.Name == "" {
		out.Name = out.OriginalName
	}
	if out.Name == "" {
		return out, inputError("TMDB 未返回有效的剧集名称")
	}
	seen := map[int]bool{}
	for _, v := range data.Seasons {
		if v.Number < 1 || v.Number > 99 || v.Episodes < 0 || v.Episodes > 10000 || seen[v.Number] {
			continue
		}
		seen[v.Number] = true
		v.Name = metadataText(v.Name, 200)
		v.AirDate = metadataText(v.AirDate, 10)
		out.Seasons = append(out.Seasons, v)
	}
	sort.Slice(out.Seasons, func(i, j int) bool { return out.Seasons[i].Number < out.Seasons[j].Number })
	return out, nil
}
