package hubweb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

func (s *Server) albumOperator(w http.ResponseWriter, r *http.Request) bool {
	if s.AlbumControl == nil || s.Jobs == nil {
		writeError(w, 503, "not_configured", "本地相册管理未启用")
		return false
	}
	if principalOf(r.Context()).Kind != domain.PrincipalUser || s.Jobs.ReadOnlyConsole() {
		writeError(w, 403, "denied", "此操作需要可写控制台的操作者权限")
		return false
	}
	if r.Method != "GET" && r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请核对目录与任务范围后确认")
		return false
	}
	return true
}
func (s *Server) handleAlbumSettings(w http.ResponseWriter, r *http.Request) {
	if !s.albumOperator(w, r) {
		return
	}
	writeJSON(w, 200, s.AlbumControl.Configuration())
}
func albumInput(w http.ResponseWriter, r *http.Request, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		writeError(w, 400, "bad_request", "相册操作 JSON 无效或包含未知字段")
		return false
	}
	return true
}
func (s *Server) handleAlbumDirectory(w http.ResponseWriter, r *http.Request) {
	if !s.albumOperator(w, r) {
		return
	}
	var input album.Directory
	remove := r.Method == "DELETE"
	if remove {
		input.Code = r.PathValue("code")
	} else if !albumInput(w, r, &input) {
		return
	}
	// Making a directory private, or changing or removing one that is, needs
	// the vault password: otherwise anyone who can manage the album could
	// switch the protection off.
	touchesPrivate := input.Private
	for _, d := range s.AlbumControl.Configuration().Directories {
		if d.Code == input.Code && d.Private {
			touchesPrivate = true
		}
	}
	if touchesPrivate && !s.privateSession(r) {
		writeError(w, 403, "vault_locked", "修改私密目录需要先解锁保险箱。\n私密目录只能由设备管理员在解锁保险箱后设置、修改或取消。")
		return
	}
	args := operations.NewArgs().Str("code", input.Code).Str("root", input.Root).Str("path", input.Path).Str("name", input.Name).Bool("enabled", input.Enabled).Bool("allow_shared_volume", input.AllowSharedVolume).Bool("private", input.Private).Bool("remove", remove)
	if op, ok := s.Jobs.Registry().Lookup("album.configure"); !ok || op.Check(args) != nil {
		writeError(w, 400, "bad_request", "相册目录参数无效")
		return
	}
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	defer s.work.Done()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: principalOf(r.Context()), OpID: "album.configure", Args: args, Target: input.Code, OperatorConsent: true, Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		var err error
		if remove {
			err = s.AlbumControl.RemoveDirectory(ctx, input.Code)
		} else {
			err = s.AlbumControl.Configure(ctx, input)
		}
		return jobs.Result{Completed: err == nil, Detail: "本地相册目录配置已更新；原文件未修改"}, err
	}})
	if out.Err != nil {
		message := "目录未保存，请检查存储目录、集合代码、名称及运行任务"
		if detail := out.Err.Error(); strings.HasPrefix(detail, "私密目录") {
			message = detail
		} else if detail != "" {
			message += "\n" + detail
		}
		writeError(w, 400, "album_configuration", message)
		return
	}
	writeJSON(w, 200, s.AlbumControl.Configuration())
}

func (s *Server) handleAlbumTask(w http.ResponseWriter, r *http.Request) {
	if !s.albumOperator(w, r) {
		return
	}
	var input album.Work
	if !albumInput(w, r, &input) {
		return
	}
	input.Operation = "album." + r.PathValue("operation")
	duration, err := album.ValidateWork(input)
	if err != nil {
		writeError(w, 400, "bad_request", "请选择目录、每批 1–10000 项和 1 秒至 12 小时的运行时限")
		return
	}
	args := operations.NewArgs()
	if input.Operation == "album.scan" || input.Operation == "album.thumbs" {
		args.Str("collection", input.Collection).Int("limit", int64(input.Limit)).Str("max_runtime", input.MaxRuntime)
	}
	if input.Operation == "album.pause" {
		args.Str("reason", input.Reason)
	}
	op, ok := s.Jobs.Registry().Lookup(input.Operation)
	if !ok || op.Check(args) != nil {
		writeError(w, 400, "bad_request", "相册任务参数无效")
		return
	}
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	base := s.WorkContext
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, duration+20*time.Second)
	accepted := make(chan string, 1)
	done := make(chan jobs.Outcome, 1)
	principal := principalOf(r.Context())
	go func() {
		defer s.work.Done()
		defer cancel()
		id := ""
		defer func() { s.unregisterActive(id) }()
		out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: principal, OpID: input.Operation, Args: args, Target: input.Collection, OnAccepted: func(job *hubstore.Job) { id = job.ID; s.registerActive(id, cancel); accepted <- id }, Handler: func(ctx context.Context, job *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
			lastMessage := ""
			events := 0
			result, err := s.AlbumControl.Run(ctx, input, func(p album.WorkProgress) {
				_ = s.Jobs.Store().Progress(ctx, job.ID, p.PID, p.PGID, p.Processed, p.Total, "entries")
				if p.Detail != "" && p.Detail != lastMessage && events < 100 {
					_ = s.Jobs.Store().Transition(ctx, job.ID, domain.JobRunning, p.Detail)
					lastMessage = p.Detail
					events++
				}
			})
			finishCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stop()
			if result.Detail != "" {
				_ = s.Jobs.Store().Transition(finishCtx, job.ID, domain.JobRunning, result.Detail)
			}
			return jobs.Result{Completed: result.Completed, StopReason: result.StopReason, Detail: result.Detail}, err
		}})
		done <- out
		pruneCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_, _ = s.Jobs.Store().PruneHistory(pruneCtx)
	}()
	select {
	case id := <-accepted:
		writeJSON(w, 202, map[string]string{"job_id": id})
	case out := <-done:
		if out.Job != nil {
			writeJSON(w, 202, map[string]string{"job_id": out.Job.ID})
		} else {
			s.fileWriteError(w, out.Err)
		}
	case <-r.Context().Done():
		return
	}
}
