package hubweb

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/YoisakiKnd/nasctl/internal/anime"
)

func (s *Server) handleAnimeMetadataSearch(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnimeOperator(w, r) {
		return
	}
	page := 1
	var err error
	if value := r.URL.Query().Get("page"); value != "" {
		page, err = strconv.Atoi(value)
		if err != nil {
			writeError(w, 400, "bad_request", "元数据页码无效")
			return
		}
	}
	result, err := s.Anime.SearchMetadata(r.Context(), r.URL.Query().Get("q"), page)
	if err != nil {
		s.animeError(w, err)
		return
	}
	s.recordAnime(r, "anime.metadata.search", "tmdb-tv", "page "+strconv.Itoa(page))
	writeJSON(w, 200, result)
}
func (s *Server) handleAnimeMetadataDetails(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnimeOperator(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "bad_request", "TMDB 剧集 ID 无效")
		return
	}
	result, err := s.Anime.MetadataDetails(r.Context(), id)
	if err != nil {
		s.animeError(w, err)
		return
	}
	s.recordAnime(r, "anime.metadata.details", strconv.FormatInt(id, 10), "read")
	writeJSON(w, 200, result)
}
func (s *Server) handleAnimeSubscriptionState(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnime(w, r, true) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "bad_request", "订阅编号无效")
		return
	}
	var input struct {
		Enabled  *bool `json:"enabled"`
		Archived *bool `json:"archived"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	d.DisallowUnknownFields()
	if err = d.Decode(&input); err != nil {
		writeError(w, 400, "bad_request", "订阅操作 JSON 无效")
		return
	}
	if err = d.Decode(new(any)); err != io.EOF {
		writeError(w, 400, "bad_request", "只能提交一个订阅操作")
		return
	}
	archive := strings.HasSuffix(r.URL.Path, "/archive")
	var saved *anime.Subscription
	action, outcome := "anime.subscription.enabled", "paused"
	if archive {
		if input.Archived == nil || input.Enabled != nil {
			writeError(w, 400, "bad_request", "请明确是否归档订阅")
			return
		}
		saved, err = s.Anime.SetSubscriptionArchived(r.Context(), id, *input.Archived)
		action = "anime.subscription.archive"
		outcome = "restored-paused"
		if *input.Archived {
			outcome = "archived"
		}
	} else {
		if input.Enabled == nil || input.Archived != nil {
			writeError(w, 400, "bad_request", "请明确是否启用订阅")
			return
		}
		saved, err = s.Anime.SetSubscriptionEnabled(r.Context(), id, *input.Enabled)
		if *input.Enabled {
			outcome = "enabled"
		}
	}
	if err != nil {
		s.animeError(w, err)
		return
	}
	s.recordAnime(r, action, strconv.FormatInt(id, 10), outcome)
	writeJSON(w, 200, saved)
}
