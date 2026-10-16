package hubweb

import (
	"context"
	"encoding/json"
	"errors"
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

func (s *Server) requireAnime(w http.ResponseWriter, r *http.Request, write bool) bool {
	if s.Anime == nil {
		writeError(w, 503, "not_configured", "native anime module is disabled; configure [hub.anime]")
		return false
	}
	if write && principalOf(r.Context()).Kind == domain.PrincipalAI {
		writeError(w, 403, "denied", "anime changes require an operator credential")
		return false
	}
	if write && s.Jobs.ReadOnlyConsole() {
		writeError(w, 403, "read_only", "console is read-only")
		return false
	}
	return true
}
func (s *Server) animeError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		writeError(w, 408, "interrupted", "追番操作已中断")
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, 504, "timeout", "追番请求超时，请稍后重试")
		return
	}
	status := http.StatusBadRequest
	if errors.Is(err, anime.ErrNotFound) {
		status = 404
	}
	if errors.Is(err, anime.ErrBusy) {
		status = 409
	}
	var input *anime.InputError
	if errors.As(err, &input) {
		writeError(w, status, "anime_error", input.Message)
		return
	}
	if errors.Is(err, anime.ErrBusy) {
		writeError(w, status, "anime_error", "已有追番任务正在运行，请稍后再试")
		return
	}
	if errors.Is(err, anime.ErrNotFound) {
		writeError(w, status, "anime_error", "订阅或剧集不存在")
		return
	}
	s.fail(w, status, "anime_error", err)
}
func (s *Server) handleAnimeStatus(w http.ResponseWriter, r *http.Request) {
	if s.Anime == nil {
		writeJSON(w, 200, anime.Status{Counts: map[string]int{}})
		return
	}
	out := s.Anime.Status(r.Context())
	if principalOf(r.Context()).ReadOnlyUser() {
		out.LastError = ""
	}
	writeJSON(w, 200, out)
}
func (s *Server) handleAnimeSubscriptions(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnime(w, r, false) {
		return
	}
	rows, err := s.Anime.Store.Subscriptions(r.Context())
	if err != nil {
		s.animeError(w, err)
		return
	}
	type subscriptionView struct {
		anime.Subscription
		PosterAvailable bool   `json:"poster_available"`
		PosterVersion   string `json:"poster_version,omitempty"`
	}
	views := make([]subscriptionView, 0, len(rows))
	for _, sub := range rows {
		// Provider URLs may contain private RSS passkeys. Browsing the shared
		// library does not grant a viewer the owner's download credentials.
		if principalOf(r.Context()).ReadOnlyUser() {
			sub.FeedURL = ""
			sub.LastError = ""
		}
		views = append(views, subscriptionView{Subscription: sub, PosterAvailable: s.Anime.PosterAvailable(sub), PosterVersion: s.Anime.PosterVersion(sub)})
	}
	writeJSON(w, 200, map[string]any{"subscriptions": views})
}
func (s *Server) handleSaveAnimeSubscription(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnime(w, r, true) {
		return
	}
	sub, err := decodeAnimeSubscription(w, r)
	if err != nil {
		writeError(w, 400, "bad_request", "订阅 JSON 无效或包含未知字段")
		return
	}
	if r.Method == http.MethodPut {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			writeError(w, 400, "bad_request", "invalid subscription id")
			return
		}
		sub.ID = id
	} else {
		sub.ID = 0
	}
	saved, err := s.Anime.SaveSubscription(r.Context(), sub)
	if err != nil {
		s.animeError(w, err)
		return
	}
	s.recordAnime(r, "anime.subscription", strconv.FormatInt(saved.ID, 10), "saved")
	writeJSON(w, 200, saved)
}
func (s *Server) handleAnimeEpisodes(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnime(w, r, false) {
		return
	}
	state := r.URL.Query().Get("state")
	switch state {
	case "", "discovered", "queued", "submitted", "downloading", "paused", "checking", "archived", "failed", "ignored":
	default:
		writeError(w, 400, "bad_request", "invalid episode state")
		return
	}
	id, err := animeSubscriptionID(r)
	if err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	if id > 0 {
		if _, err = s.Anime.Store.Subscription(r.Context(), id); err != nil {
			s.animeError(w, err)
			return
		}
	}
	watch := r.URL.Query().Get("watch")
	switch watch {
	case "", "watched", "unwatched", "in_progress":
	default:
		writeError(w, 400, "bad_request", "观看状态筛选无效")
		return
	}
	rows, err := s.Anime.Store.EpisodesForWatch(r.Context(), id, state, watch, intParam(r, "limit", 50), intParam(r, "offset", 0))
	if err != nil {
		s.animeError(w, err)
		return
	}
	if principalOf(r.Context()).ReadOnlyUser() {
		for i := range rows {
			rows[i] = viewerEpisode(rows[i])
		}
	}
	writeJSON(w, 200, map[string]any{"episodes": rows})
}

func viewerEpisode(ep anime.Episode) anime.Episode {
	ep.URL = ""
	ep.Hash = ""
	ep.Error = ""
	return ep
}
func (s *Server) handleAnimeRefresh(w http.ResponseWriter, r *http.Request) {
	s.runAnimeTask(w, r, false)
}
func (s *Server) handleAnimeSync(w http.ResponseWriter, r *http.Request) {
	s.runAnimeTask(w, r, true)
}
func (s *Server) runAnimeTask(w http.ResponseWriter, r *http.Request, syncOnly bool) {
	if !s.requireAnime(w, r, true) {
		return
	}
	id, err := animeSubscriptionID(r)
	if err != nil {
		writeError(w, 400, "bad_request", err.Error())
		return
	}
	if id > 0 {
		if _, err = s.Anime.Store.Subscription(r.Context(), id); err != nil {
			s.animeError(w, err)
			return
		}
	}
	op := "anime.refresh"
	action := s.Anime.Refresh
	if syncOnly {
		op = "anime.sync"
		action = s.Anime.Sync
	}
	args := operations.NewArgs().Int("subscription", id)
	ctx := s.WorkContext
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	accepted := make(chan *hubstore.Job, 1)
	done := make(chan jobs.Outcome, 1)
	p := principalOf(r.Context())
	if !s.beginWork() {
		cancel()
		writeError(w, 503, "shutting_down", "服务正在停止，请稍后重试")
		return
	}
	go func() {
		defer s.work.Done()
		defer cancel()
		jobID := ""
		defer func() { s.unregisterActive(jobID) }()
		done <- s.Jobs.Run(ctx, jobs.RunOptions{Principal: p, OpID: op, Args: args, Target: strconv.FormatInt(id, 10), OnAccepted: func(j *hubstore.Job) { jobID = j.ID; s.registerActive(j.ID, cancel); accepted <- j }, Handler: func(ctx context.Context, j *hubstore.Job, a *operations.Args) (jobs.Result, error) {
			result, err := action(ctx, id)
			if err == nil && len(result.Errors) > 0 {
				err = errors.New(result.Errors[0])
			}
			return jobs.Result{Completed: err == nil, Detail: fmt.Sprintf("discovered %d, submitted %d, archived %d, skipped %d", result.Discovered, result.Submitted, result.Archived, result.Skipped)}, err
		}})
	}()
	select {
	case j := <-accepted:
		writeJSON(w, http.StatusAccepted, map[string]any{"job_id": j.ID})
	case out := <-done:
		if out.Job != nil {
			writeJSON(w, http.StatusAccepted, map[string]any{"job_id": out.Job.ID})
		} else if out.Err != nil {
			s.animeError(w, out.Err)
		} else {
			writeJSON(w, 200, map[string]any{"job": out.Job})
		}
	case <-r.Context().Done():
		return
	}
}
func (s *Server) handleAnimeQueue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnime(w, r, true) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, 400, "bad_request", "invalid episode id")
		return
	}
	if err = s.Anime.Queue(r.Context(), id); err != nil {
		s.animeError(w, err)
		return
	}
	s.recordAnime(r, "anime.queue", strconv.FormatInt(id, 10), "queued")
	writeJSON(w, 200, map[string]any{"queued": true})
}
func (s *Server) recordAnime(r *http.Request, op, target, outcome string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	_ = s.Jobs.Store().Record(ctx, hubstore.AuditEntry{Principal: principalOf(r.Context()).String(), Action: "call", Op: op, Target: target, Outcome: outcome, At: time.Now()})
}
func (s *Server) handleAnimeVideo(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnime(w, r, false) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, 400, "bad_request", "invalid episode id")
		return
	}
	if !s.mediaSlot(w, r) {
		return
	}
	defer func() { <-s.streamSlots }()
	f, fi, err := s.Anime.OpenVideo(r.Context(), id)
	if err != nil {
		s.animeError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", contentTypeFor(fi.Name()))
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(transfer(w), r, fi.Name(), fi.ModTime(), f)
}

// WaitWork is called after HTTP shutdown and cancellation of WorkContext.
func (s *Server) beginWork() bool {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if s.workStopping || (s.WorkContext != nil && s.WorkContext.Err() != nil) {
		return false
	}
	s.work.Add(1)
	return true
}
func (s *Server) WaitWork() { s.workMu.Lock(); s.workStopping = true; s.workMu.Unlock(); s.work.Wait() }

func animeSubscriptionID(r *http.Request) (int64, error) {
	value := r.URL.Query().Get("subscription")
	if value == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 0 {
		return 0, errors.New("订阅编号必须是非负整数")
	}
	return id, nil
}
func decodeAnimeSubscription(w http.ResponseWriter, r *http.Request) (anime.Subscription, error) {
	var sub anime.Subscription
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&sub); err != nil {
		return sub, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return sub, errors.New("expected exactly one JSON object")
	}
	return sub, nil
}
func (s *Server) requireAnimeOperator(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireAnime(w, r, false) {
		return false
	}
	if principalOf(r.Context()).Kind == domain.PrincipalAI {
		writeError(w, 403, "denied", "此操作需要操作者凭据")
		return false
	}
	return true
}
func (s *Server) handleAnimeSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnime(w, r, false) {
		return
	}
	writeJSON(w, 200, s.Anime.Settings())
}
func (s *Server) handleAnimeConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnimeOperator(w, r) {
		return
	}
	result, err := s.Anime.CheckConnection(r.Context())
	if err != nil {
		s.animeError(w, err)
		return
	}
	outcome := "connected"
	if !result.Connected {
		outcome = "unavailable"
	}
	s.recordAnime(r, "anime.connection", "configured-downloader", outcome)
	writeJSON(w, 200, result)
}
func (s *Server) handleAnimePreview(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnimeOperator(w, r) {
		return
	}
	sub, err := decodeAnimeSubscription(w, r)
	if err != nil {
		writeError(w, 400, "bad_request", "订阅 JSON 无效或包含未知字段")
		return
	}
	result, err := s.Anime.Preview(r.Context(), sub)
	if err != nil {
		s.animeError(w, err)
		return
	}
	// No URL or RSS token is written to the audit log.
	s.recordAnime(r, "anime.preview", "draft", "read")
	writeJSON(w, 200, result)
}
func (s *Server) handleAnimeIgnore(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnime(w, r, true) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, 400, "bad_request", "剧集编号无效")
		return
	}
	if err = s.Anime.Ignore(r.Context(), id); err != nil {
		s.animeError(w, err)
		return
	}
	s.recordAnime(r, "anime.ignore", strconv.FormatInt(id, 10), "ignored")
	writeJSON(w, 200, map[string]any{"ignored": true})
}
