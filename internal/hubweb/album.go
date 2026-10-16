package hubweb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/album"
)

func (s *Server) requireAlbum(w http.ResponseWriter) bool {
	if s.Album == nil {
		writeError(w, 503, "not_configured", "相册已停用，请在设置中启用本地相册")
		return false
	}
	return true
}
func (s *Server) albumError(w http.ResponseWriter, err error) {
	var upstream *album.Error
	if errors.As(err, &upstream) {
		writeError(w, upstream.Status, upstream.Code, upstream.Message)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, 504, "album_timeout", "相册请求超时，请稍后重试")
		return
	}
	if errors.Is(err, context.Canceled) {
		writeError(w, 408, "interrupted", "相册请求已中断")
		return
	}
	writeError(w, 502, "album_unavailable", "相册服务暂不可用，请检查运行状态")
}
func (s *Server) handleAlbumStatus(w http.ResponseWriter, r *http.Request) {
	if s.Album == nil {
		writeJSON(w, 200, map[string]any{"configured": false, "connected": false, "mode": "disabled"})
		return
	}
	status, err := s.Album.Status(r.Context())
	if err != nil {
		message := "相册服务暂不可用"
		var problem *album.Error
		if errors.As(err, &problem) {
			message = problem.Message
		}
		writeJSON(w, 200, map[string]any{"configured": true, "connected": false, "mode": s.Album.Mode(), "error": message})
		return
	}
	writeJSON(w, 200, map[string]any{"configured": true, "connected": true, "mode": s.Album.Mode(), "index_ready": status.IndexReady, "version": status.Version, "paused": status.Paused, "pause_reason": status.PauseReason, "warnings": status.Warnings})
}

// albumFor picks the client for a request. Routes under /album/private/ read
// only the private directories and need an unlocked vault session.
func (s *Server) albumFor(w http.ResponseWriter, r *http.Request) (*album.Client, bool) {
	if !s.requireAlbum(w) {
		return nil, false
	}
	if !strings.HasPrefix(r.URL.Path, "/api/v1/album/private/") {
		return s.Album, true
	}
	if !s.privateSession(r) {
		privateError(w, errPrivateLocked)
		return nil, false
	}
	return s.Album.Private(), true
}
func (s *Server) handleAlbumCollections(w http.ResponseWriter, r *http.Request) {
	client, ok := s.albumFor(w, r)
	if !ok {
		return
	}
	result, err := client.Collections(r.Context())
	if err != nil {
		s.albumError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func albumQuery(r *http.Request) (album.Query, error) {
	q := r.URL.Query()
	limit := 48
	if value := q.Get("limit"); value != "" {
		var err error
		limit, err = strconv.Atoi(value)
		if err != nil || limit <= 0 {
			return album.Query{}, fmt.Errorf("invalid album page size")
		}
	}
	return album.Query{Collection: q.Get("collection"), Q: q.Get("q"), Kind: q.Get("kind"), From: q.Get("from"), To: q.Get("to"), Sort: q.Get("sort"), Cursor: q.Get("cursor"), Limit: limit, IncludeMissing: boolParam(r, "include_missing"), GPS: boolParam(r, "gps"), Fallback: boolParam(r, "fallback")}, nil
}
func (s *Server) handleAlbumList(w http.ResponseWriter, r *http.Request) {
	client, ok := s.albumFor(w, r)
	if !ok {
		return
	}
	query, err := albumQuery(r)
	if err != nil {
		writeError(w, 400, "bad_request", "相册每页数量无效")
		return
	}
	result, err := client.List(r.Context(), query)
	if err != nil {
		s.albumError(w, err)
		return
	}
	if s.PhotoVault != nil {
		items := result.Items[:0]
		for _, item := range result.Items {
			if !s.PhotoVault.Protected(item.ID) {
				items = append(items, item)
			}
		}
		result.Items = items
	}
	writeJSON(w, 200, result)
}
func (s *Server) handleAlbumDetails(w http.ResponseWriter, r *http.Request) {
	client, ok := s.albumFor(w, r)
	if !ok {
		return
	}
	if !s.publicAlbumID(w, r) {
		return
	}
	result, err := client.Details(r.Context(), r.PathValue("id"))
	if err != nil {
		s.albumError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func safeAlbumType(value, part string) string {
	media, _, _ := mime.ParseMediaType(value)
	if part == "thumb" && media == "image/svg+xml" {
		return media
	}
	switch media {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/avif", "image/heic", "image/heif", "image/bmp", "image/tiff", "video/mp4", "video/webm", "video/quicktime", "video/x-matroska", "audio/mpeg", "audio/ogg", "audio/wav":
		return media
	}
	return "application/octet-stream"
}
func albumDisposition(value, part, id string) string {
	_, params, err := mime.ParseMediaType(value)
	name := "media-" + id
	if err == nil && params["filename"] != "" && len(params["filename"]) <= 1024 {
		name = path.Base(strings.ReplaceAll(params["filename"], "\\", "/"))
	}
	if part == "download" {
		return contentDisposition(name)
	}
	return mime.FormatMediaType("inline", map[string]string{"filename": name})
}
func (s *Server) handleAlbumStream(w http.ResponseWriter, r *http.Request) {
	client, ok := s.albumFor(w, r)
	if !ok {
		return
	}
	if !s.publicAlbumID(w, r) {
		return
	}
	id, part := r.PathValue("id"), r.PathValue("part")
	if part != "thumb" {
		if !s.mediaSlot(w, r) {
			return
		}
		defer func() { <-s.streamSlots }()
	}
	timeout := 30 * time.Minute
	if part == "thumb" {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	response, err := client.Stream(ctx, id, part, r.Method, r.Header)
	if err != nil {
		s.albumError(w, err)
		return
	}
	defer response.Body.Close()
	w.Header().Set("Cache-Control", "private, no-store")
	for _, name := range []string{"ETag", "Last-Modified", "Accept-Ranges", "Content-Range"} {
		if value := response.Header.Get(name); value != "" && len(value) <= 512 {
			w.Header().Set(name, value)
		}
	}
	if response.StatusCode == 304 {
		w.WriteHeader(304)
		return
	}
	if response.StatusCode == 416 {
		writeError(w, 416, "invalid_range", "请求的字节范围超出原文件")
		return
	}
	mediaType := safeAlbumType(response.Header.Get("Content-Type"), part)
	if part == "thumb" {
		if mediaType != "image/jpeg" && mediaType != "image/png" && mediaType != "image/webp" && mediaType != "image/svg+xml" {
			writeError(w, 502, "album_thumbnail_invalid", "相册缩略图格式无效")
			return
		}
		if response.ContentLength > album.MaxThumbnailBytes {
			writeError(w, 502, "album_thumbnail_large", "相册缩略图超过 2 MiB 上限")
			return
		}
		w.Header().Set("Content-Type", mediaType)
		// An SVG placeholder is safe as an image, but direct navigation must
		// not turn upstream content into a document with the console's origin.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		if r.Method == "HEAD" {
			if response.ContentLength >= 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(response.ContentLength, 10))
			}
			w.WriteHeader(response.StatusCode)
			return
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, album.MaxThumbnailBytes+1))
		if err != nil {
			s.albumError(w, err)
			return
		}
		if len(data) > album.MaxThumbnailBytes {
			writeError(w, 502, "album_thumbnail_large", "相册缩略图超过 2 MiB 上限")
			return
		}
		if response.ContentLength >= 0 && int64(len(data)) != response.ContentLength {
			writeError(w, 502, "album_thumbnail_incomplete", "相册缩略图传输不完整")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(data)
		return
	}
	if response.ContentLength < 0 {
		writeError(w, 502, "album_length_missing", "相册原文件缺少明确大小，未开始传输")
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", albumDisposition(response.Header.Get("Content-Disposition"), part, id))
	w.Header().Set("Content-Length", strconv.FormatInt(response.ContentLength, 10))
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	w.WriteHeader(response.StatusCode)
	if r.Method == "HEAD" {
		return
	}
	// Reads and writes each have an idle budget. The total context also bounds
	// slow but active transfers; the buffer is independent of original size.
	idle := time.AfterFunc(30*time.Second, cancel)
	defer idle.Stop()
	buffer := make([]byte, 64<<10)
	remaining := response.ContentLength
	emptyReads := 0
	for remaining > 0 {
		if ctx.Err() != nil {
			return
		}
		idle.Reset(30 * time.Second)
		want := int64(len(buffer))
		if remaining < want {
			want = remaining
		}
		n, readErr := response.Body.Read(buffer[:want])
		idle.Stop()
		if n > 0 {
			emptyReads = 0
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			written, writeErr := w.Write(buffer[:n])
			remaining -= int64(written)
			if writeErr != nil || written != n {
				return
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && s.Log != nil {
				s.Log.Warn("album transfer interrupted", "id", id, "part", part)
			}
			return
		}
		if n == 0 {
			emptyReads++
			if emptyReads >= 100 {
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}
}

func (s *Server) publicAlbumID(w http.ResponseWriter, r *http.Request) bool {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if s.PhotoVault != nil && s.PhotoVault.Protected(id) {
		writeError(w, 404, "not_found", "没有此照片")
		return false
	}
	return true
}
