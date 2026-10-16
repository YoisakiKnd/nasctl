package hubweb

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/YoisakiKnd/nasctl/internal/anime"
)

func animePathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, 400, "bad_request", "剧集或订阅编号无效")
		return 0, false
	}
	return id, true
}
func (s *Server) handleAnimePlayback(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnime(w, r, false) {
		return
	}
	id, ok := animePathID(w, r)
	if !ok {
		return
	}
	result, err := s.Anime.Playback(r.Context(), id)
	if err != nil {
		s.animeError(w, err)
		return
	}
	if principalOf(r.Context()).ReadOnlyUser() {
		result.Episode = viewerEpisode(result.Episode)
	}
	writeJSON(w, 200, result)
}
func (s *Server) handleSaveAnimePlayback(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnime(w, r, true) {
		return
	}
	id, ok := animePathID(w, r)
	if !ok {
		return
	}
	var update anime.PlaybackUpdate
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(&update); err != nil {
		writeError(w, 400, "bad_request", "观看状态 JSON 无效")
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeError(w, 400, "bad_request", "观看状态必须是单个 JSON 对象")
		return
	}
	result, err := s.Anime.Store.SavePlayback(r.Context(), id, update)
	if err != nil {
		s.animeError(w, err)
		return
	}
	// Periodic playback positions are browsing history, not audited calls. Only
	// an explicit watched/unwatched decision creates an audit record.
	if update.Watched != nil {
		s.recordAnime(r, "anime.watched", strconv.FormatInt(id, 10), "saved")
	}
	writeJSON(w, 200, result)
}
func (s *Server) mediaSlot(w http.ResponseWriter, r *http.Request) bool {
	select {
	case s.streamSlots <- struct{}{}:
		return true
	case <-r.Context().Done():
		return false
	default:
		w.Header().Set("Retry-After", "2")
		writeError(w, 503, "busy", "同时传输的文件过多，请稍后重试")
		return false
	}
}
func (s *Server) handleAnimePoster(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnime(w, r, false) {
		return
	}
	id, ok := animePathID(w, r)
	if !ok {
		return
	}
	if !s.mediaSlot(w, r) {
		return
	}
	defer func() { <-s.streamSlots }()
	f, info, err := s.Anime.OpenPoster(r.Context(), id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			err = anime.ErrNotFound
		}
		s.animeError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(transfer(w), r, info.Name(), info.ModTime(), f)
}
func (s *Server) handleAnimeSubtitle(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnime(w, r, false) {
		return
	}
	id, ok := animePathID(w, r)
	if !ok {
		return
	}
	if !s.mediaSlot(w, r) {
		return
	}
	defer func() { <-s.streamSlots }()
	f, info, err := s.Anime.OpenSubtitle(r.Context(), id, r.PathValue("name"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			err = anime.ErrNotFound
		}
		s.animeError(w, err)
		return
	}
	defer f.Close()
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", contentDisposition(info.Name()))
		http.ServeContent(transfer(w), r, info.Name(), info.ModTime(), f)
		return
	}
	b, err := anime.SubtitleVTT(info.Name(), f)
	if err != nil {
		s.animeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write(b)
}
