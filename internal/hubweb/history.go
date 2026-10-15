package hubweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/anime"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

func (s *Server) handleAnimeHistoryStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnimeOperator(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, 400, "bad_request", "订阅编号无效")
		return
	}
	out, err := s.Anime.HistoryStatus(r.Context(), id)
	if err != nil {
		s.animeError(w, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleAnimeHistoryCleanup(w http.ResponseWriter, r *http.Request) {
	if !s.animeConfirmed(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, 400, "bad_request", "订阅编号无效")
		return
	}
	var input struct {
		Title  string `json:"confirm_title"`
		Limit  int    `json:"limit"`
		Before int64  `json:"before"`
		Detach bool   `json:"detach_downloads"`
		Remove bool   `json:"remove_subscription"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF || input.Limit < 1 || input.Limit > 20000 || (!input.Remove && input.Limit > 1000) || input.Before < 0 {
		writeError(w, 400, "bad_request", "历史清理参数无效")
		return
	}
	opt := anime.HistoryCleanup{Subscription: id, ConfirmTitle: input.Title, Limit: input.Limit, Before: input.Before, DetachDownloads: input.Detach, RemoveSubscription: input.Remove}
	args := operations.NewArgs().Int("subscription", id).Str("confirm_title", input.Title).Int("limit", int64(input.Limit)).Int("before", input.Before).Bool("detach_downloads", input.Detach).Bool("remove_subscription", input.Remove)
	s.startBackground(w, r, "anime.history.cleanup", args, strconv.FormatInt(id, 10), true, func(ctx context.Context, job *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		out, err := s.Anime.CleanupHistory(ctx, opt)
		data, _ := json.Marshal(map[string]any{"kind": "anime.history.cleanup", "result": out})
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.Jobs.Store().Transition(finishCtx, job.ID, domain.JobRunning, string(data))
		return jobs.Result{Completed: err == nil, Detail: string(data)}, err
	})
}

type historyHTTPWriter struct{ w http.ResponseWriter }

func (w historyHTTPWriter) Write(p []byte) (int, error) {
	_ = http.NewResponseController(w.w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	return w.w.Write(p)
}
func (s *Server) handleAnimeHistoryExport(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnimeOperator(w, r) {
		return
	}
	sub, err := animeSubscriptionID(r)
	if err != nil {
		writeError(w, 400, "bad_request", "订阅编号无效")
		return
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit < 1 || limit > 20000 {
		writeError(w, 400, "bad_request", "导出须明确指定 1–20000 条记录上限")
		return
	}
	if !s.mediaSlot(w, r) {
		return
	}
	defer func() { <-s.streamSlots }()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	export, err := s.Anime.OpenHistoryExport(ctx, sub, limit)
	if err != nil {
		s.animeError(w, err)
		return
	}
	defer export.Close()
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", contentDisposition(fmt.Sprintf("anime-history-%d.ndjson", time.Now().Unix())))
	w.Header().Set("Cache-Control", "private, no-store")
	stop := context.AfterFunc(ctx, func() { _ = http.NewResponseController(w).SetWriteDeadline(time.Now()) })
	defer stop()
	_, err = export.WriteTo(historyHTTPWriter{w})
	outcome := "exported"
	if err != nil {
		outcome = "incomplete"
	}
	s.recordAnime(r, "anime.history.export", strconv.FormatInt(sub, 10), outcome)
}
