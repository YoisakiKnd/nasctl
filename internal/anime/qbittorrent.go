package anime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Torrent struct {
	Hash     string  `json:"hash"`
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	SavePath string  `json:"save_path"`
}

func validTorrentHash(hash string) bool {
	if len(hash) != 40 && len(hash) != 64 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

// Control targets one verified torrent only. qBittorrent 5 renamed pause/resume
// to stop/start: https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-(qBittorrent-5.0)
func (q *QBittorrent) Control(ctx context.Context, hash, action string) error {
	if !validTorrentHash(hash) {
		return inputError("下载任务编号无效")
	}
	endpoint := ""
	switch action {
	case "pause", "resume":
		version, err := q.request(ctx, http.MethodGet, "app/version", nil, true)
		if err != nil {
			return err
		}
		v := strings.TrimSpace(string(version))
		if !strings.HasPrefix(v, "v") || len(v) > 80 {
			return inputError("无法识别下载器版本")
		}
		major, err := strconv.Atoi(strings.SplitN(v[1:], ".", 2)[0])
		if err != nil || major < 4 {
			return inputError("此下载器版本不支持任务控制")
		}
		endpoint = action
		if major >= 5 {
			if action == "pause" {
				endpoint = "stop"
			} else {
				endpoint = "start"
			}
		}
	case "recheck":
		endpoint = "recheck"
	default:
		return inputError("不支持此下载操作")
	}
	_, err := q.request(ctx, http.MethodPost, "torrents/"+endpoint, url.Values{"hashes": {hash}}, true)
	return err
}

type TorrentFile struct {
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
}
type Downloader interface {
	Find(context.Context, string) ([]Torrent, error)
	Add(context.Context, string, string, string) error
	Files(context.Context, string) ([]TorrentFile, error)
}

var ErrDownloadRejected = errors.New("anime: downloader rejected the release")

type QBittorrent struct {
	base, username, password string
	client                   *http.Client
}

func NewQBittorrent(base, user, password string) *QBittorrent {
	jar, _ := cookiejar.New(nil)
	return &QBittorrent{base: strings.TrimRight(base, "/"), username: user, password: password, client: &http.Client{Jar: jar, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("downloader redirects are refused") }}}
}
func (q *QBittorrent) request(ctx context.Context, method, endpoint string, values url.Values, retry bool) ([]byte, error) {
	var reader io.Reader
	uri := q.base + "/api/v2/" + endpoint
	if method == http.MethodGet {
		if len(values) > 0 {
			uri += "?" + values.Encode()
		}
	} else {
		reader = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, uri, reader)
	if err != nil {
		return nil, errors.New("anime: invalid downloader address")
	}
	req.Header.Set("Referer", q.base+"/")
	baseURL, _ := url.Parse(q.base)
	req.Header.Set("Origin", baseURL.Scheme+"://"+baseURL.Host)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := q.client.Do(req)
	if err != nil {
		return nil, errors.New("无法连接 qBittorrent，请检查 WebUI 地址与网络")
	}
	body, readErr := io.ReadAll(io.LimitReader(res.Body, MaxFeedBytes+1))
	res.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if len(body) > MaxFeedBytes {
		return nil, errors.New("anime: downloader response exceeds 2 MiB")
	}
	if res.StatusCode == http.StatusForbidden && retry && endpoint != "auth/login" {
		b, err := q.request(ctx, http.MethodPost, "auth/login", url.Values{"username": {q.username}, "password": {q.password}}, false)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(b)) != "Ok." {
			return nil, errors.New("qBittorrent 登录失败，请检查用户名和密码")
		}
		return q.request(ctx, method, endpoint, values, false)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qBittorrent 返回 HTTP %d，请检查 WebUI 配置与访问权限", res.StatusCode)
	}
	return body, nil
}
func (q *QBittorrent) Find(ctx context.Context, tag string) ([]Torrent, error) {
	b, err := q.request(ctx, http.MethodGet, "torrents/info", url.Values{"tag": {tag}, "limit": {"2"}}, true)
	if err != nil {
		return nil, err
	}
	var out []Torrent
	err = json.Unmarshal(b, &out)
	return out, err
}
func (q *QBittorrent) Add(ctx context.Context, uri, savePath, tag string) error {
	b, err := q.request(ctx, http.MethodPost, "torrents/add", url.Values{"urls": {uri}, "savepath": {savePath}, "tags": {"nasctl," + tag}, "autoTMM": {"false"}}, true)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != "Ok." {
		return ErrDownloadRejected
	}
	return nil
}
func (q *QBittorrent) Files(ctx context.Context, hash string) ([]TorrentFile, error) {
	b, err := q.request(ctx, http.MethodGet, "torrents/files", url.Values{"hash": {hash}}, true)
	if err != nil {
		return nil, err
	}
	var out []TorrentFile
	err = json.Unmarshal(b, &out)
	return out, err
}

func (q *QBittorrent) Check(ctx context.Context) (ConnectionStatus, error) {
	result := ConnectionStatus{}
	version, err := q.request(ctx, http.MethodGet, "app/version", nil, true)
	if err != nil {
		return result, err
	}
	apiVersion, err := q.request(ctx, http.MethodGet, "app/webapiVersion", nil, true)
	if err != nil {
		return result, err
	}
	result.Version = strings.TrimSpace(string(version))
	result.APIVersion = strings.TrimSpace(string(apiVersion))
	if len(result.Version) > 80 || len(result.APIVersion) > 80 || !strings.HasPrefix(result.Version, "v") || len(result.Version) < 2 || result.Version[1] < '0' || result.Version[1] > '9' || len(result.APIVersion) == 0 || result.APIVersion[0] < '0' || result.APIVersion[0] > '9' {
		return ConnectionStatus{}, errors.New("下载器返回了非预期的版本信息，请确认地址指向 qBittorrent WebUI")
	}
	result.Connected = true
	return result, nil
}
