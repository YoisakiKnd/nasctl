// Package album is a bounded reader of an injected local handler or an explicit
// remote album service. It never opens the index, decodes media or starts work.
package album

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/config"
)

const MaxJSONBytes = 2 << 20
const MaxThumbnailBytes = 2 << 20

type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string         { return e.Message }
func invalid(message string) error     { return &Error{400, "bad_request", message} }
func unavailable(message string) error { return &Error{502, "album_unavailable", message} }

type Client struct {
	base, token, publicURL string
	mode                   string
	http                   *http.Client
}

func New(cfg config.Album) (*Client, error) {
	if cfg.APIBase == "" {
		return nil, nil
	}
	if err := config.ValidateAlbumBase(cfg.APIBase); err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{Proxy: nil, MaxIdleConns: 8, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 8 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if host == "localhost" {
			host = "127.0.0.1"
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
	}
	return &Client{base: strings.TrimRight(cfg.APIBase, "/"), token: cfg.Token, publicURL: cfg.PublicURL, mode: "remote", http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("album redirects are refused") }}}, nil
}
func (c *Client) Close() {
	if c != nil {
		c.http.CloseIdleConnections()
	}
}
func (c *Client) PublicURL() string { return c.publicURL }
func (c *Client) Mode() string      { return c.mode }
func (c *Client) request(ctx context.Context, method, endpoint string, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+endpoint, nil)
	if err != nil {
		return nil, unavailable("相册连接配置无效")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for _, name := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if value := headers.Get(name); value != "" && len(value) <= 512 {
			req.Header.Set(name, value)
		}
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, unavailable("相册服务连接失败，请检查服务地址、网络和运行状态")
	}
	return response, nil
}
func responseError(status int) error {
	switch status {
	case 400:
		return invalid("相册筛选参数无效，或集合尚未建立索引")
	case 401:
		return &Error{502, "album_auth_failed", "相册服务凭据无效，请检查 hub.album.token"}
	case 403:
		return &Error{403, "album_refused", "相册服务拒绝读取此文件，请检查集合挂载和原文件权限"}
	case 404:
		return &Error{404, "album_not_found", "相册记录或原文件不存在"}
	case 429, 503:
		return &Error{503, "album_busy", "相册服务正在处理其他请求，请稍后重试"}
	default:
		return unavailable(fmt.Sprintf("相册服务返回 HTTP %d，请稍后重试", status))
	}
}
func (c *Client) responseError(response *http.Response) error {
	if c.mode == "local" && response.Header.Get("X-Nasctl-Album-Problem") == "index_unavailable" {
		return &Error{503, "album_index_unavailable", "本地相册索引无法读取，请检查索引文件和权限；已有数据未重建或覆盖"}
	}
	return responseError(response.StatusCode)
}
func (c *Client) json(ctx context.Context, endpoint string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response, err := c.request(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return c.responseError(response)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxJSONBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return unavailable("相册响应读取失败")
	}
	if len(data) > MaxJSONBytes {
		return unavailable("相册响应超过 2 MiB 上限，请缩小查询范围")
	}
	if err = json.Unmarshal(data, out); err != nil {
		return unavailable("相册响应格式无效，请确认地址指向 albumctl API")
	}
	return nil
}

type Query struct {
	Collection, Q, Kind, From, To, Sort, Cursor string
	Limit                                       int
	IncludeMissing, GPS, Fallback               bool
}

func (q Query) Values() (url.Values, error) {
	if len(q.Collection) > 32 || len(q.Q) > 256 || len(q.Cursor) > 512 {
		return nil, invalid("集合、搜索文本或分页标记过长")
	}
	if q.Limit == 0 {
		q.Limit = 48
	}
	if q.Limit < 1 || q.Limit > 100 {
		return nil, invalid("相册每页数量范围为 1–100")
	}
	if q.Sort == "" {
		q.Sort = "time_desc"
	}
	switch q.Sort {
	case "time_desc", "time_asc", "name_asc":
	default:
		return nil, invalid("相册排序方式无效")
	}
	switch q.Kind {
	case "", "image", "video", "other", "unsupported":
	default:
		return nil, invalid("相册媒体类型无效")
	}
	for _, date := range []string{q.From, q.To} {
		if date != "" {
			if _, err := time.Parse("2006-01-02", date); err != nil {
				return nil, invalid("相册日期须为 YYYY-MM-DD")
			}
		}
	}
	if q.From != "" && q.To != "" && q.From > q.To {
		return nil, invalid("开始日期不能晚于结束日期")
	}
	v := url.Values{"limit": {strconv.Itoa(q.Limit)}, "sort": {q.Sort}}
	for name, value := range map[string]string{"collection": q.Collection, "q": q.Q, "kind": q.Kind, "from": q.From, "to": q.To, "cursor": q.Cursor} {
		if value != "" {
			v.Set(name, value)
		}
	}
	for name, value := range map[string]bool{"include_missing": q.IncludeMissing, "gps": q.GPS, "fallback": q.Fallback} {
		if value {
			v.Set(name, "true")
		}
	}
	return v, nil
}
func (c *Client) Collections(ctx context.Context) (Collections, error) {
	var out Collections
	err := c.json(ctx, "/api/v1/collections", &out)
	if len(out.Items) > 1000 {
		return Collections{}, unavailable("相册集合超过 1000 项上限")
	}
	if err == nil && out.Items == nil {
		return Collections{}, unavailable("相册集合响应格式无效")
	}
	return out, err
}
func (c *Client) Status(ctx context.Context) (Status, error) {
	var out Status
	err := c.json(ctx, "/api/v1/status", &out)
	if err == nil && out.Version == "" {
		err = unavailable("相册状态响应格式无效，请确认连接到 albumctl")
	}
	return out, err
}
func (c *Client) List(ctx context.Context, q Query) (Listing, error) {
	var out Listing
	v, err := q.Values()
	if err != nil {
		return out, err
	}
	if err = c.json(ctx, "/api/v1/media?"+v.Encode(), &out); err != nil {
		return out, err
	}
	limit, _ := strconv.Atoi(v.Get("limit"))
	if len(out.Items) > limit || (out.NextCursor != nil && len(*out.NextCursor) > 512) {
		return Listing{}, unavailable("相册响应超过分页上限")
	}
	for i := range out.Items {
		if err = out.Items[i].normalize(); err != nil {
			return Listing{}, err
		}
	}
	if out.Items == nil {
		return Listing{}, unavailable("相册媒体响应格式无效")
	}
	return out, nil
}
func (c *Client) Details(ctx context.Context, id string) (Details, error) {
	var out Details
	if !ValidID(id) {
		return out, invalid("相册编号无效")
	}
	if err := c.json(ctx, "/api/v1/media/"+id, &out); err != nil {
		return out, err
	}
	if strconv.FormatInt(out.Item.ID, 10) != id {
		return out, unavailable("相册返回了不同的媒体编号")
	}
	if err := out.Item.normalize(); err != nil {
		return out, err
	}
	if len(out.Exif) > 50 || len(out.Paths) > 10 {
		return out, unavailable("相册详情超过字段上限")
	}
	return out, nil
}
func ValidID(id string) bool {
	if len(id) == 0 || len(id) > 18 || id[0] == '0' {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	_, err := strconv.ParseInt(id, 10, 64)
	return err == nil
}

// Stream is fixed to one of albumctl's read routes. Request credentials and
// cookies are never forwarded; only the configured album credential is sent.
func (c *Client) Stream(ctx context.Context, id, part, method string, headers http.Header) (*http.Response, error) {
	if !ValidID(id) {
		return nil, invalid("相册编号无效")
	}
	switch part {
	case "thumb", "raw", "download":
	default:
		return nil, invalid("相册读取方式无效")
	}
	if method != "GET" && method != "HEAD" {
		return nil, invalid("相册只支持读取")
	}
	if value := headers.Get("Range"); value != "" {
		if len(value) > 128 || !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
			return nil, &Error{416, "invalid_range", "相册每次只支持一个字节范围"}
		}
	}
	response, err := c.request(ctx, method, "/api/v1/media/"+id+"/"+part, headers)
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case 200, 206, 304, 416:
		return response, nil
	default:
		response.Body.Close()
		return nil, c.responseError(response)
	}
}
