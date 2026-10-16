package anime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const mikanOrigin = "https://mikanani.me"
const sourceLimit = 12
const maxSourceBytes = 4 << 20

type SourceCandidate struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	OriginalName string `json:"original_name,omitempty"`
	Date         string `json:"date,omitempty"`
	Overview     string `json:"overview,omitempty"`
	URL          string `json:"url"`
	Poster       string `json:"poster,omitempty"`
}
type SourceSearch struct {
	Results   []SourceCandidate `json:"results"`
	Page      int               `json:"page"`
	HasMore   bool              `json:"has_more"`
	Truncated bool              `json:"truncated"`
}
type SourceFeed struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}
type MikanSource struct {
	ID        int64        `json:"id"`
	Name      string       `json:"name"`
	BangumiID int64        `json:"bangumi_id,omitempty"`
	URL       string       `json:"url"`
	Feeds     []SourceFeed `json:"feeds"`
	Truncated bool         `json:"truncated"`
}

// Queries only fixed public origins. No credentials, redirects, persistence,
// downloads or media-library writes are involved in discovery.
func (e *Engine) sourceRequest(ctx context.Context, method, address string, body []byte, limit int64) ([]byte, error) {
	ctx, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
	if err != nil {
		return nil, inputError("番剧搜索参数无效")
	}
	req.Header.Set("User-Agent", "nasctl/0.1 (https://github.com/YoisakiKnd/nasctl)")
	req.Header.Set("Accept", "application/json, text/html")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	client := *e.feedClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return inputError("番剧搜索服务重定向已拒绝") }
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, inputError("番剧搜索服务暂时无法连接，请稍后重试或手动填写 RSS")
	}
	defer res.Body.Close()
	if res.StatusCode == 429 {
		return nil, inputError("搜索过于频繁，请稍后重试")
	}
	if res.StatusCode != 200 {
		return nil, inputError(fmt.Sprintf("番剧搜索服务返回 HTTP %d，请稍后重试或手动填写 RSS", res.StatusCode))
	}
	var data []byte
	if strings.HasPrefix(address, mikanOrigin+"/Home/Search?") {
		data, err = readMikanSearchPrefix(res.Body)
	} else {
		data, err = io.ReadAll(io.LimitReader(res.Body, limit+1))
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, inputError("番剧搜索响应读取失败")
	}
	if int64(len(data)) > limit {
		return nil, inputError("番剧搜索响应超过大小上限，请缩小搜索范围或手动填写 RSS")
	}
	return data, nil
}

// The useful series cards precede a potentially enormous torrent table.
// Stop reading at that list; do not buffer or parse the release history.
func readMikanSearchPrefix(r io.Reader) ([]byte, error) {
	var out bytes.Buffer
	chunk := make([]byte, 16<<10)
	for out.Len() <= 512<<10 {
		n, err := r.Read(chunk)
		out.Write(chunk[:n])
		data := out.Bytes()
		if start := bytes.Index(data, []byte("an-ul")); start >= 0 {
			if end := bytes.Index(data[start:], []byte("</ul>")); end >= 0 {
				return data[:start+end+len("</ul>")], nil
			}
		}
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, inputError("Mikan 搜索列表超过大小上限")
}
func (e *Engine) SearchSources(ctx context.Context, provider, query string, page int) (SourceSearch, error) {
	out := SourceSearch{Results: []SourceCandidate{}, Page: page}
	query = strings.TrimSpace(query)
	if query == "" || !utf8.ValidString(query) || utf8.RuneCountInString(query) > 100 || strings.ContainsFunc(query, unicode.IsControl) || page < 1 || page > 10 || (provider != "bangumi" && provider != "mikan") || (provider == "mikan" && page != 1) {
		return out, inputError("请选择 Bangumi 或 Mikan，并输入 1–100 字的番剧名称")
	}
	if !e.metadataMu.TryLock() {
		return out, ErrBusy
	}
	defer e.metadataMu.Unlock()
	if provider == "mikan" {
		data, err := e.sourceRequest(ctx, "GET", mikanOrigin+"/Home/Search?"+url.Values{"searchstr": {query}}.Encode(), nil, maxSourceBytes)
		if err != nil {
			return out, err
		}
		return parseMikanSearch(data)
	}
	body, _ := json.Marshal(map[string]any{"keyword": query, "sort": "match", "filter": map[string]any{"type": []int{2}}})
	data, err := e.sourceRequest(ctx, "POST", fmt.Sprintf("https://api.bgm.tv/v0/search/subjects?limit=%d&offset=%d", sourceLimit, (page-1)*sourceLimit), body, maxMetadataBytes)
	if err != nil {
		return out, err
	}
	var response struct {
		Total int `json:"total"`
		Data  []struct {
			ID      int64  `json:"id"`
			Type    int    `json:"type"`
			Name    string `json:"name"`
			NameCN  string `json:"name_cn"`
			Date    string `json:"date"`
			Summary string `json:"summary"`
			Images  struct {
				Common string `json:"common"`
			} `json:"images"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &response) != nil {
		return out, inputError("Bangumi 搜索响应格式无效")
	}
	seen := map[int64]bool{}
	for _, v := range response.Data {
		if !sourceID(v.ID) || v.Type != 2 || seen[v.ID] {
			continue
		}
		if len(out.Results) >= sourceLimit {
			out.Truncated = true
			break
		}
		name := metadataText(v.NameCN, 200)
		if name == "" {
			name = metadataText(v.Name, 200)
		}
		if name == "" {
			continue
		}
		seen[v.ID] = true
		out.Results = append(out.Results, SourceCandidate{ID: v.ID, Name: name, OriginalName: metadataText(v.Name, 200), Date: metadataText(v.Date, 10), Overview: metadataText(v.Summary, 300), URL: fmt.Sprintf("https://bgm.tv/subject/%d", v.ID), Poster: sourcePoster(v.Images.Common, "lain.bgm.tv", "/pic/")})
	}
	out.HasMore = page < 10 && response.Total > page*sourceLimit
	return out, nil
}
func sourceID(id int64) bool { return id > 0 && id <= 2147483647 }
func sourcePoster(raw, host, prefix string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" {
		return ""
	}
	if !u.IsAbs() {
		if host != "mikanani.me" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
			return ""
		}
		u.Scheme = "https"
		u.Host = host
	}
	validPath := strings.HasPrefix(u.Path, prefix)
	if host == "lain.bgm.tv" {
		validPath = strings.HasPrefix(u.Path, "/pic/") || strings.HasPrefix(u.Path, "/r/100/pic/") || strings.HasPrefix(u.Path, "/r/200/pic/") || strings.HasPrefix(u.Path, "/r/400/pic/") || strings.HasPrefix(u.Path, "/r/800/pic/")
	}
	if u.Scheme != "https" || u.Host != host || !validPath {
		return ""
	}
	return u.String()
}
func tokenAttr(t html.Token, key string) string {
	for _, a := range t.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func tokenClass(t html.Token, name string) bool {
	for _, c := range strings.Fields(tokenAttr(t, "class")) {
		if c == name {
			return true
		}
	}
	return false
}
func mikanID(raw string) int64 {
	const prefix = "/Home/Bangumi/"
	if !strings.HasPrefix(raw, prefix) {
		return 0
	}
	suffix := strings.TrimPrefix(raw, prefix)
	id, err := strconv.ParseInt(suffix, 10, 64)
	if err != nil || !sourceID(id) || suffix != strconv.FormatInt(id, 10) {
		return 0
	}
	return id
}
func sourceTokens(data []byte, visit func(html.Token)) error {
	z := html.NewTokenizer(bytes.NewReader(data))
	z.SetMaxBuf(64 << 10)
	for n := 0; n < 300000; n++ {
		if z.Next() == html.ErrorToken {
			if z.Err() == io.EOF {
				return nil
			}
			return inputError("Mikan 页面格式无效或超出解析上限")
		}
		visit(z.Token())
	}
	return inputError("Mikan 页面超过解析条目上限")
}
func parseMikanSearch(data []byte) (SourceSearch, error) {
	out := SourceSearch{Results: []SourceCandidate{}, Page: 1}
	var current *SourceCandidate
	seen := map[int64]bool{}
	err := sourceTokens(data, func(t html.Token) {
		if t.Type == html.StartTagToken || t.Type == html.SelfClosingTagToken {
			if t.Data == "a" {
				current = nil
				if id := mikanID(tokenAttr(t, "href")); id > 0 {
					current = &SourceCandidate{ID: id, URL: fmt.Sprintf("%s/Home/Bangumi/%d", mikanOrigin, id)}
				}
			}
			if current != nil {
				if tokenClass(t, "an-text") {
					current.Name = metadataText(tokenAttr(t, "title"), 200)
				}
				if image := sourcePoster(tokenAttr(t, "data-src"), "mikanani.me", "/images/Bangumi/"); image != "" {
					current.Poster = image
				}
			}
		}
		if t.Type == html.EndTagToken && t.Data == "a" && current != nil {
			if current.Name != "" && !seen[current.ID] {
				seen[current.ID] = true
				if len(out.Results) < sourceLimit {
					out.Results = append(out.Results, *current)
				} else {
					out.Truncated = true
				}
			}
			current = nil
		}
	})
	if err != nil {
		return out, err
	}
	if !bytes.Contains(data, []byte("an-ul")) {
		return out, inputError("Mikan 搜索页面已变化或需要验证，请手动填写 RSS")
	}
	return out, nil
}
func (e *Engine) MikanSource(ctx context.Context, id int64) (MikanSource, error) {
	if !sourceID(id) {
		return MikanSource{}, inputError("Mikan 番剧编号无效")
	}
	if !e.metadataMu.TryLock() {
		return MikanSource{}, ErrBusy
	}
	defer e.metadataMu.Unlock()
	data, err := e.sourceRequest(ctx, "GET", fmt.Sprintf("%s/Home/Bangumi/%d", mikanOrigin, id), nil, maxSourceBytes)
	if err != nil {
		return MikanSource{}, err
	}
	return parseMikanSource(data, id)
}
func parseMikanSource(data []byte, id int64) (MikanSource, error) {
	out := MikanSource{ID: id, URL: fmt.Sprintf("%s/Home/Bangumi/%d", mikanOrigin, id), Feeds: []SourceFeed{}}
	title := false
	group := int64(0)
	groupName := ""
	captureName := false
	foundFeed := false
	seen := map[int64]bool{}
	err := sourceTokens(data, func(t html.Token) {
		if t.Type == html.TextToken {
			if title {
				out.Name = metadataText(out.Name+" "+t.Data, 200)
			}
			if captureName && len(groupName) < 2048 {
				groupName += " " + t.Data
			}
			return
		}
		if t.Type == html.EndTagToken && t.Data == "p" {
			title = false
		}
		if t.Type != html.StartTagToken && t.Type != html.SelfClosingTagToken {
			return
		}
		if t.Data == "p" && tokenClass(t, "bangumi-title") {
			title = true
		}
		if t.Data == "div" && tokenClass(t, "subgroup-text") {
			group, _ = strconv.ParseInt(tokenAttr(t, "id"), 10, 64)
			groupName = ""
			captureName = true
		}
		if t.Data != "a" {
			return
		}
		raw := tokenAttr(t, "href")
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		if u.Scheme == "https" && (u.Host == "bgm.tv" || u.Host == "bangumi.tv") && u.RawQuery == "" && strings.HasPrefix(u.Path, "/subject/") {
			v, _ := strconv.ParseInt(strings.TrimPrefix(u.Path, "/subject/"), 10, 64)
			if sourceID(v) {
				out.BangumiID = v
			}
		}
		if u.IsAbs() || u.Host != "" || u.User != nil || u.Path != "/RSS/Bangumi" || u.Fragment != "" {
			return
		}
		v, _ := strconv.ParseInt(u.Query().Get("bangumiId"), 10, 64)
		if v != id {
			return
		}
		subgroup := u.Query().Get("subgroupid")
		if subgroup == "" {
			foundFeed = true
			return
		}
		sid, _ := strconv.ParseInt(subgroup, 10, 64)
		if !sourceID(sid) || sid != group || seen[sid] {
			return
		}
		captureName = false
		name := metadataText(strings.Join(strings.Fields(groupName), " "), 200)
		if name == "" {
			return
		}
		seen[sid] = true
		if len(out.Feeds) >= 64 {
			out.Truncated = true
			return
		}
		out.Feeds = append(out.Feeds, SourceFeed{ID: sid, Name: name, URL: mikanOrigin + "/RSS/Bangumi?" + url.Values{"bangumiId": {strconv.FormatInt(id, 10)}, "subgroupid": {strconv.FormatInt(sid, 10)}}.Encode()})
	})
	if err != nil {
		return out, err
	}
	if !foundFeed || out.Name == "" {
		return out, inputError("Mikan 未返回此番剧的有效 RSS，请重新选择或手动填写")
	}
	out.Feeds = append(out.Feeds, SourceFeed{Name: "全部字幕组（可能包含重复发布）", URL: fmt.Sprintf("%s/RSS/Bangumi?bangumiId=%d", mikanOrigin, id)})
	return out, nil
}
