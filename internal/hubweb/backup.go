package hubweb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/backups"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

func (s *Server) backupOperator(w http.ResponseWriter, r *http.Request) bool {
	if s.Backups == nil || s.Jobs == nil {
		writeError(w, 503, "not_configured", "本地备份尚未初始化")
		return false
	}
	if principalOf(r.Context()).Kind != domain.PrincipalUser {
		writeError(w, 403, "denied", "备份需要操作者身份")
		return false
	}
	if r.Method != "GET" {
		if s.Jobs.ReadOnlyConsole() {
			writeError(w, 403, "read_only", "控制台处于只读模式")
			return false
		}
		if r.Header.Get("X-Nasctl-Confirm") != "1" {
			writeError(w, 400, "confirmation_required", "请核对备份范围、保留策略或恢复目标后确认")
			return false
		}
	}
	return true
}
func backupInput(w http.ResponseWriter, r *http.Request, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		writeError(w, 400, "bad_request", "备份请求必须是单个 JSON 对象，且不能包含未知字段")
		return false
	}
	return true
}
func (s *Server) backupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, backups.ErrBusy):
		writeError(w, 409, "busy", "已有备份、校验或恢复正在运行，请稍后重试")
	case errors.Is(err, backups.ErrInvalid):
		writeError(w, 400, "backup_scope", "计划、版本或目标无效，或计划已更改，请刷新核对后再提交")
	case errors.Is(err, backups.ErrCorrupt):
		writeError(w, 409, "backup_corrupt", "备份清单或内容未通过校验，未发布恢复目标")
	case errors.Is(err, backups.ErrFull):
		writeError(w, 409, "backup_full", "备份数量、文件树或清单达到上限，请缩小范围或清理已确认的旧版本")
	default:
		s.fileWriteError(w, err)
	}
}
func (s *Server) handleBackupSettings(w http.ResponseWriter, r *http.Request) {
	if !s.backupOperator(w, r) {
		return
	}
	writeJSON(w, 200, s.Backups.Settings())
}
func (s *Server) handleBackupHistory(w http.ResponseWriter, r *http.Request) {
	if !s.backupOperator(w, r) {
		return
	}
	ctx, stop := context.WithTimeout(r.Context(), 10*time.Second)
	defer stop()
	h, err := s.Backups.History(ctx, r.URL.Query().Get("root"))
	if err != nil {
		s.backupError(w, err)
		return
	}
	writeJSON(w, 200, h)
}
func (s *Server) handleBackupPlan(w http.ResponseWriter, r *http.Request) {
	if !s.backupOperator(w, r) {
		return
	}
	var p backups.Plan
	remove := r.Method == "DELETE"
	if remove {
		p.ID = r.PathValue("id")
	} else if !backupInput(w, r, &p) {
		return
	}
	args := operations.NewArgs().Str("plan", p.ID).Str("scope", p.Digest()).Bool("remove", remove)
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	defer s.work.Done()
	ctx, stop := context.WithTimeout(r.Context(), 10*time.Second)
	defer stop()
	out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: principalOf(r.Context()), OpID: "backup.configure", Args: args, Target: p.ID, OperatorConsent: true, Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		var err error
		if remove {
			err = s.Backups.RemovePlan(ctx, p.ID)
		} else {
			err = s.Backups.Save(ctx, p)
		}
		return jobs.Result{Completed: err == nil, Verified: err == nil}, err
	}})
	if out.Err != nil {
		s.backupError(w, out.Err)
		return
	}
	writeJSON(w, 200, s.Backups.Settings())
}
func (s *Server) handleBackupWork(w http.ResponseWriter, r *http.Request) {
	if !s.backupOperator(w, r) {
		return
	}
	var input struct {
		Plan           string `json:"plan"`
		PlanSHA256     string `json:"plan_sha256"`
		Root           string `json:"root"`
		ID             string `json:"id"`
		ToRoot         string `json:"to_root"`
		To             string `json:"to"`
		ManifestSHA256 string `json:"manifest_sha256"`
		ConfirmID      string `json:"confirm_id"`
	}
	if !backupInput(w, r, &input) {
		return
	}
	op := "backup." + strings.TrimPrefix(r.URL.Path, "/api/v1/backup/")
	args := operations.NewArgs()
	target := input.Root + "/" + input.ID
	var action func(context.Context, func(backups.Progress)) (backups.Result, error)
	switch op {
	case "backup.run":
		args.Str("plan", input.Plan).Str("scope", input.PlanSHA256)
		target = input.Plan
		action = func(ctx context.Context, p func(backups.Progress)) (backups.Result, error) {
			return s.Backups.Run(ctx, input.Plan, input.PlanSHA256, p)
		}
	case "backup.verify":
		args.Str("root", input.Root).Str("id", input.ID)
		action = func(ctx context.Context, p func(backups.Progress)) (backups.Result, error) {
			return s.Backups.Verify(ctx, input.Root, input.ID, p)
		}
	case "backup.restore":
		args.Str("root", input.Root).Str("id", input.ID).Str("to_root", input.ToRoot).Str("to", input.To).Str("manifest_sha256", input.ManifestSHA256)
		action = func(ctx context.Context, p func(backups.Progress)) (backups.Result, error) {
			return s.Backups.Restore(ctx, backups.Restore{Root: input.Root, ID: input.ID, ToRoot: input.ToRoot, To: input.To, ManifestSHA256: input.ManifestSHA256}, p)
		}
	case "backup.delete", "backup.cleanup":
		if input.ID == "" || input.ConfirmID != input.ID {
			writeError(w, 400, "confirmation_required", "请准确输入待永久清理的完整版本编号")
			return
		}
		args.Str("root", input.Root).Str("id", input.ID).Str("confirm_id", input.ConfirmID)
		if op == "backup.delete" {
			args.Str("manifest_sha256", input.ManifestSHA256)
			action = func(ctx context.Context, p func(backups.Progress)) (backups.Result, error) {
				return s.Backups.Delete(ctx, input.Root, input.ID, input.ManifestSHA256, p)
			}
		} else {
			action = func(ctx context.Context, p func(backups.Progress)) (backups.Result, error) {
				return s.Backups.Cleanup(ctx, input.Root, input.ID, p)
			}
		}
	default:
		writeError(w, 404, "not_found", "备份任务不存在")
		return
	}
	s.startBackground(w, r, op, args, target, true, func(ctx context.Context, job *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		last := time.Time{}
		phase := ""
		var latest backups.Progress
		result, err := action(ctx, func(p backups.Progress) {
			latest = p
			changed := p.Phase != phase
			if !changed && time.Since(last) < 500*time.Millisecond {
				return
			}
			last = time.Now()
			_ = s.Jobs.Store().Progress(ctx, job.ID, 0, 0, p.Bytes, 0, "bytes")
			if changed {
				phase = p.Phase
				detail := map[string]string{"copy": "正在复制文件", "verify": "正在核对清单和文件内容", "restore": "正在准备恢复副本", "delete": "正在清理备份版本"}[phase]
				if p.Detail != "" {
					detail = p.Detail
				}
				if detail != "" {
					_ = s.Jobs.Store().Transition(ctx, job.ID, domain.JobRunning, detail)
				}
			}
		})
		finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		total := int64(0)
		if err == nil {
			total = latest.Bytes
		}
		_ = s.Jobs.Store().Progress(finish, job.ID, 0, 0, latest.Bytes, total, "bytes")
		note, _ := json.Marshal(map[string]any{"kind": "backup.result", "result": result})
		if saveErr := s.Jobs.Store().Transition(finish, job.ID, domain.JobRunning, string(note)); saveErr != nil {
			return jobs.Result{}, saveErr
		}
		return jobs.Result{Completed: err == nil, Verified: err == nil, Detail: result.Warning}, err
	})
}
