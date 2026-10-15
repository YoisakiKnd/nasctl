package anime

import (
	"path"
	"path/filepath"
	"strings"
)

func (e *Engine) downloaderDirectory() string {
	if e.cfg.QBittorrentDownloadDir != "" {
		return e.cfg.QBittorrentDownloadDir
	}
	return filepath.ToSlash(e.cfg.DownloadDir)
}

// Only the configured remote prefix maps into the already-open local root.
// Downloader responses never select a new local directory or filesystem root.
func (e *Engine) localDownloadPath(remote string) (string, error) {
	if !path.IsAbs(remote) || strings.ContainsAny(remote, "\\\x00") {
		return "", inputError("下载器返回了无效的下载路径")
	}
	base := path.Clean(e.downloaderDirectory())
	remote = path.Clean(remote)
	if remote == base {
		return ".", nil
	}
	if !strings.HasPrefix(remote, base+"/") {
		return "", inputError("下载器保存路径不在已配置的路径映射范围内，请检查 qbittorrent_download_dir")
	}
	rel := strings.TrimPrefix(remote, base+"/")
	return safeRelative(rel)
}
