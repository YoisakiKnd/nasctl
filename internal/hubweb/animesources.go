package hubweb

import (
	"net/http"
	"strconv"
)

func (s *Server) handleAnimeSources(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnimeOperator(w, r) {
		return
	}
	page := 1
	var err error
	if p := r.URL.Query().Get("page"); p != "" {
		page, err = strconv.Atoi(p)
		if err != nil {
			writeError(w, 400, "bad_request", "搜索页码无效")
			return
		}
	}
	result, err := s.Anime.SearchSources(r.Context(), r.URL.Query().Get("provider"), r.URL.Query().Get("q"), page)
	if err != nil {
		s.animeError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (s *Server) handleMikanSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnimeOperator(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "bad_request", "Mikan 番剧编号无效")
		return
	}
	result, err := s.Anime.MikanSource(r.Context(), id)
	if err != nil {
		s.animeError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
