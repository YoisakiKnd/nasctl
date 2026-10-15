package hubweb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/anime"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

func (s *Server) animeConfirmed(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireAnime(w, r, true) {
		return false
	}
	if r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请核对本次修改范围并确认")
		return false
	}
	return true
}
func (s *Server) handleAnimeMetadataRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.animeConfirmed(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, 400, "bad_request", "订阅编号无效")
		return
	}
	var input struct {
		Limit   int   `json:"limit"`
		AfterID int64 `json:"after_id"`
		Replace bool  `json:"replace"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF || input.Limit < 1 || input.Limit > 50 || input.AfterID < 0 {
		writeError(w, 400, "bad_request", "请输入 1–50 集的处理限额与有效继续位置")
		return
	}
	opt := anime.MetadataOptions{Subscription: id, Limit: input.Limit, AfterID: input.AfterID, Replace: input.Replace}
	args := operations.NewArgs().Int("subscription", id).Int("limit", int64(input.Limit)).Int("after_id", input.AfterID).Bool("replace", input.Replace)
	s.startBackground(w, r, "anime.metadata.refresh", args, strconv.FormatInt(id, 10), true, func(ctx context.Context, job *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		out, err := s.Anime.RefreshMetadata(ctx, opt, func(done, total int64) { _ = s.Jobs.Store().Progress(ctx, job.ID, 0, 0, done, total, "episodes") })
		data, _ := json.Marshal(map[string]any{"kind": "anime.metadata", "result": out})
		finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.Jobs.Store().Transition(finalCtx, job.ID, domain.JobRunning, string(data))
		return jobs.Result{Completed: err == nil, Detail: string(data)}, err
	})
}
func (s *Server) handleAnimeMetadataBackups(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnimeOperator(w, r) {
		return
	}
	items, err := s.Anime.MetadataBackups(r.Context())
	if err != nil {
		s.animeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"backups": items, "max_files": 1000, "max_bytes": 64 << 20})
}
func (s *Server) handleAnimeMetadataBackupExport(w http.ResponseWriter, r *http.Request) {
	if !s.requireAnimeOperator(w, r) {
		return
	}
	info, data, err := s.Anime.ExportMetadataBackup(r.Context(), r.PathValue("id"))
	if err != nil {
		s.animeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition(path.Base(info.Path)+".previous"))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(data)
}
func (s *Server) handleAnimeMetadataRestore(w http.ResponseWriter, r *http.Request) {
	if !s.animeConfirmed(w, r) {
		return
	}
	id := r.PathValue("id")
	args := operations.NewArgs().Str("backup", id)
	s.startBackground(w, r, "anime.metadata.restore", args, id, true, func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		err := s.Anime.RestoreMetadata(ctx, id)
		return jobs.Result{Completed: err == nil, Detail: "所选元数据版本已恢复；恢复前的版本也已保留"}, err
	})
}
func (s *Server) handleAnimeMetadataBackupDelete(w http.ResponseWriter, r *http.Request) {
	if !s.animeConfirmed(w, r) {
		return
	}
	var input struct {
		SHA string `json:"sha256"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF {
		writeError(w, 400, "bad_request", "备份确认信息无效")
		return
	}
	id := r.PathValue("id")
	args := operations.NewArgs().Str("backup", id).Str("sha256", input.SHA)
	if op, ok := s.Jobs.Registry().Lookup("anime.metadata.backup.delete"); !ok || op.Check(args) != nil {
		writeError(w, 400, "bad_request", "备份参数无效")
		return
	}
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	defer s.work.Done()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: principalOf(r.Context()), OpID: "anime.metadata.backup.delete", Args: args, Target: id, OperatorConsent: true, Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		err := s.Anime.DeleteMetadataBackup(ctx, id, input.SHA)
		return jobs.Result{Completed: err == nil}, err
	}})
	if out.Err != nil {
		s.animeError(w, out.Err)
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}
