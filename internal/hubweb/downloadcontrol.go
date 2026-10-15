package hubweb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

func (s *Server) handleAnimeDownloadControl(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnime(w, r, true) {
		return
	}
	if r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请确认本次下载器操作")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, 400, "bad_request", "剧集编号无效")
		return
	}
	var input struct {
		Action string `json:"action"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF || (input.Action != "pause" && input.Action != "resume" && input.Action != "recheck") {
		writeError(w, 400, "bad_request", "下载操作无效")
		return
	}
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	defer s.work.Done()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if s.WorkContext != nil {
		stop := context.AfterFunc(s.WorkContext, cancel)
		defer stop()
	}
	args := operations.NewArgs().Int("episode", id)
	var saved any
	out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: principalOf(r.Context()), OpID: "anime.download." + input.Action, Args: args, Target: strconv.FormatInt(id, 10), Sync: true, Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		value, err := s.Anime.ControlDownload(ctx, id, input.Action)
		saved = value
		return jobs.Result{Completed: err == nil, Detail: "下载器单集操作：" + input.Action}, err
	}})
	if out.Err != nil {
		if errors.Is(out.Err, jobs.ErrGated) || errors.Is(out.Err, jobs.ErrDenied) || errors.Is(out.Err, jobs.ErrNeedsApproval) {
			s.fileWriteError(w, out.Err)
			return
		}
		s.animeError(w, out.Err)
		return
	}
	writeJSON(w, 200, saved)
}
