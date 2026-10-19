package hubweb

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/YoisakiKnd/nasctl/internal/desktopapps"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/sysinfo"
)

// Wallpapers stores the desktop wallpaper shared by every member.
type Wallpapers interface {
	SetWallpaper(io.Reader) error
	Wallpaper() (*os.File, os.FileInfo, string, error)
	RemoveWallpaper() error
}

// handleSystemStatus feeds the desktop widgets. It measures on request only:
// no sampler runs while nobody is looking at the desktop.
func (s *Server) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	if principalOf(r.Context()).Kind == domain.PrincipalAI {
		writeError(w, 403, "denied", "系统状态仅对 NAS 成员开放")
		return
	}
	if s.System == nil {
		s.System = sysinfo.New()
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, s.System.Read())
}

func (s *Server) handleWallpaper(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r.Context())
	if p.Kind == domain.PrincipalAI {
		writeError(w, 403, "denied", "壁纸仅对 NAS 成员开放")
		return
	}
	if s.Wallpaper == nil {
		writeError(w, 404, "not_found", "没有自定义壁纸")
		return
	}
	if r.Method == "GET" || r.Method == "HEAD" {
		f, info, kind, err := s.Wallpaper.Wallpaper()
		// The desktop asks whether there is one at all; "no" is an answer,
		// not a failed request in the browser's console.
		if r.URL.Query().Has("exists") {
			if err == nil {
				f.Close()
			}
			writeJSON(w, 200, map[string]bool{"custom": err == nil})
			return
		}
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, 404, "not_found", "没有自定义壁纸")
			return
		}
		if err != nil {
			writeError(w, 500, "wallpaper_error", "壁纸暂时无法读取")
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", kind)
		w.Header().Set("Cache-Control", "private, max-age=300")
		w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano()))
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		http.ServeContent(w, r, "wallpaper", info.ModTime(), f)
		return
	}
	if !p.DeviceAdmin() {
		writeError(w, 403, "denied", "仅管理员可以更换共享壁纸")
		return
	}
	if s.Jobs != nil && s.Jobs.ReadOnlyConsole() {
		writeError(w, 403, "read_only", "当前不能更换壁纸")
		return
	}
	if r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请确认更换壁纸")
		return
	}
	if r.Method == "DELETE" {
		if err := s.Wallpaper.RemoveWallpaper(); err != nil {
			writeError(w, 500, "wallpaper_error", "壁纸未能删除")
			return
		}
		writeJSON(w, 200, map[string]bool{"custom": false})
		return
	}
	if r.ContentLength > desktopapps.MaxWallpaperBytes {
		writeError(w, 413, "too_large", "壁纸不能超过 12 MiB")
		return
	}
	err := s.Wallpaper.SetWallpaper(http.MaxBytesReader(w, r.Body, desktopapps.MaxWallpaperBytes+1))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, 413, "too_large", "壁纸不能超过 12 MiB")
		return
	case errors.Is(err, desktopapps.ErrWallpaper):
		writeError(w, 400, "bad_wallpaper", "请上传 JPEG、PNG 或 WebP 图片，不超过 12 MiB")
		return
	case err != nil:
		s.fail(w, 500, "wallpaper_error", err)
		return
	}
	writeJSON(w, 200, map[string]bool{"custom": true})
}
