package hubweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

func (s *Server) registerActive(id string, cancel context.CancelFunc) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if s.workStopping {
		cancel()
		return
	}
	if s.activeWork == nil {
		s.activeWork = make(map[string]context.CancelFunc)
	}
	s.activeWork[id] = cancel
}
func (s *Server) unregisterActive(id string) {
	s.workMu.Lock()
	delete(s.activeWork, id)
	s.workMu.Unlock()
}
func (s *Server) canCancel(id string) bool {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	return s.activeWork[id] != nil
}

func (s *Server) handleCancelWork(w http.ResponseWriter, r *http.Request) {
	if s.Jobs == nil {
		writeError(w, 503, "not_configured", "任务管理尚未配置")
		return
	}
	if principalOf(r.Context()).Kind != domain.PrincipalUser {
		writeError(w, 403, "denied", "停止任务需要操作者凭据")
		return
	}
	if r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请确认停止此任务")
		return
	}
	id := r.PathValue("id")
	if len(id) > 64 || id == "" {
		writeError(w, 400, "bad_request", "任务编号无效")
		return
	}
	if !principalOf(r.Context()).DeviceAdmin() {
		job, err := s.Jobs.Store().Job(r.Context(), id)
		if err != nil || !canStopJob(principalOf(r.Context()), *job) {
			writeError(w, 404, "not_found", "no such job")
			return
		}
	}
	s.workMu.Lock()
	cancel := s.activeWork[id]
	delete(s.activeWork, id)
	s.workMu.Unlock()
	if cancel == nil {
		writeError(w, 409, "not_running", "此任务已结束、已请求停止或不在当前服务实例中运行")
		return
	}
	cancel()
	_ = s.Jobs.Store().Record(r.Context(), hubstore.AuditEntry{At: time.Now(), Principal: principalOf(r.Context()).String(), Action: "call", Op: "jobs.cancel", Target: id, Outcome: "requested"})
	writeJSON(w, 202, map[string]any{"requested": true, "job_id": id})
}
func (s *Server) handleFileBackground(w http.ResponseWriter, r *http.Request) {
	if !s.fileOperator(w, r, true) {
		return
	}
	if r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请核对并确认本次文件操作")
		return
	}
	var input struct {
		Root        string `json:"root"`
		ToRoot      string `json:"to_root"`
		From        string `json:"from"`
		To          string `json:"to"`
		Limit       int    `json:"limit"`
		MaxBytes    int64  `json:"max_bytes"`
		ID          string `json:"id"`
		ConfirmName string `json:"confirm_name"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		writeError(w, 400, "bad_request", "文件任务 JSON 无效")
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeError(w, 400, "bad_request", "文件任务必须是单个 JSON 对象")
		return
	}
	op := "files." + strings.TrimPrefix(r.URL.Path, "/api/v1/files/")
	args := operations.NewArgs().Str("root", input.Root)
	var action func(context.Context, func(int64, int64, string)) (any, error)
	if op == "files.copy" || op == "files.move" {
		if input.Limit > s.Files.Limits().CopyMaxEntries || input.MaxBytes > s.Files.Limits().CopyMaxBytes {
			s.fileWriteError(w, files.ErrTreeLimit)
			return
		}
		options, err := files.ValidateCopy(files.CopyOptions{SourceRoot: input.Root, DestinationRoot: input.ToRoot, From: input.From, To: input.To, Limit: input.Limit, MaxBytes: input.MaxBytes, Move: op == "files.move"})
		if err != nil {
			s.fileWriteError(w, err)
			return
		}
		var source, destination *files.RootInfo
		for _, root := range s.Files.Roots() {
			value := root
			if value.ID == input.Root {
				source = &value
			}
			if value.ID == input.ToRoot {
				destination = &value
			}
		}
		if source == nil || destination == nil {
			s.fileWriteError(w, files.ErrNoSuchRoot)
			return
		}
		if !source.Available || !destination.Available {
			s.fileWriteError(w, files.ErrRootUnavailable)
			return
		}
		if !destination.Writable || (options.Move && !source.Writable) {
			s.fileWriteError(w, files.ErrReadOnly)
			return
		}
		gate, use := s.privateGate(r), useCopy
		if options.Move {
			use = useMove
		}
		refused := gate.check(input.Root, options.From, use)
		if refused == nil {
			refused = gate.check(input.ToRoot, options.To, useInside)
		}
		if privateError(w, refused) {
			return
		}
		if _, err = s.Files.Stat(input.Root, options.From); err != nil {
			s.fileWriteError(w, err)
			return
		}
		if _, err = s.Files.Stat(input.ToRoot, options.To); err == nil {
			s.fileWriteError(w, files.ErrExists)
			return
		} else if !errors.Is(err, files.ErrNotExist) {
			s.fileWriteError(w, err)
			return
		}
		args.Str("to_root", input.ToRoot).Str("from", options.From).Str("to", options.To).Int("limit", int64(input.Limit)).Int("max_bytes", input.MaxBytes)
		action = func(ctx context.Context, report func(int64, int64, string)) (any, error) {
			result, err := s.Files.Copy(ctx, options, func(entries, bytes int64, _ string) {
				if bytes > 0 {
					report(bytes, 0, "bytes")
				} else {
					report(entries, 0, "entries")
				}
			})
			return result, err
		}
	} else if op == "files.purge" {
		if input.Limit < 1 || input.Limit > s.Files.Limits().CopyMaxEntries || input.ConfirmName == "" {
			s.fileWriteError(w, files.ErrConfirmation)
			return
		}
		if privateError(w, s.privateGate(r).trashID(input.Root, input.ID)) {
			return
		}
		args.Str("id", input.ID).Str("confirm_name", input.ConfirmName).Int("limit", int64(input.Limit))
		action = func(ctx context.Context, report func(int64, int64, string)) (any, error) {
			return s.Files.Purge(ctx, input.Root, input.ID, input.ConfirmName, input.Limit, func(done, total int64) { report(done, total, "entries") })
		}
	} else {
		writeError(w, 404, "not_found", "文件任务不存在")
		return
	}
	declared, ok := s.Jobs.Registry().Lookup(op)
	if !ok {
		writeError(w, 500, "internal_error", "文件任务未注册")
		return
	}
	if err := declared.Check(args); err != nil {
		writeError(w, 400, "bad_request", "文件任务参数无效")
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
	ctx, cancel := context.WithTimeout(base, declared.Timeout)
	accepted := make(chan string, 1)
	done := make(chan jobs.Outcome, 1)
	principal := principalOf(r.Context())
	go func() {
		defer s.work.Done()
		defer cancel()
		jobID := ""
		defer func() { s.unregisterActive(jobID) }()
		out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: principal, OpID: op, Args: args, Target: input.Root, OperatorConsent: true, OnAccepted: func(job *hubstore.Job) { jobID = job.ID; s.registerActive(job.ID, cancel); accepted <- job.ID }, Handler: func(ctx context.Context, job *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
			last := time.Time{}
			var amount, total int64
			unit := "entries"
			report := func(n, t int64, u string) {
				amount, total, unit = n, t, u
				if time.Since(last) < 500*time.Millisecond {
					return
				}
				last = time.Now()
				_ = s.Jobs.Store().Progress(ctx, job.ID, 0, 0, n, t, u)
			}
			result, err := action(ctx, report)
			finalCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer stop()
			if err == nil {
				total = amount
			}
			_ = s.Jobs.Store().Progress(finalCtx, job.ID, 0, 0, amount, total, unit)
			detail := ""
			switch value := result.(type) {
			case files.CopyResult:
				detail = fmt.Sprintf("目标 %s/%s；处理 %d 项、%d 字节，跳过 %d 个内部目录项", value.DestinationRoot, value.Path, value.Entries, value.Bytes, value.Skipped)
				if value.SourceTrashed != "" {
					detail += "；源内容回收记录 " + value.SourceTrashed
				}
				if value.Published && err != nil {
					detail = "目标内容已保存，源内容请核对；" + detail
				}
			case files.PurgeResult:
				detail = fmt.Sprintf("已永久删除 %d / %d 项", value.Removed, value.Total)
			}
			_ = s.Jobs.Store().Transition(finalCtx, job.ID, domain.JobRunning, detail)
			return jobs.Result{Completed: err == nil, Verified: err == nil, Detail: detail}, err
		}})
		s.unregisterActive(jobID)
		done <- out
		pruneCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_, _ = s.Jobs.Store().PruneHistory(pruneCtx)
		_, _ = s.Jobs.Store().PruneAudit(pruneCtx, time.Now())
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
