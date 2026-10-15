package hubweb

import (
	"context"
	"encoding/json"
	"errors"
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

func (s *Server) fileOperator(w http.ResponseWriter, r *http.Request, write bool) bool {
	if s.Files == nil || s.Jobs == nil {
		writeError(w, 503, "not_configured", "文件管理尚未配置")
		return false
	}
	if principalOf(r.Context()).Kind == domain.PrincipalAI {
		writeError(w, 403, "denied", "文件写入与回收站需要操作者凭据")
		return false
	}
	if write && s.Jobs.ReadOnlyConsole() {
		writeError(w, 403, "read_only", "控制台处于只读模式")
		return false
	}
	return true
}
func (s *Server) fileWriteError(w http.ResponseWriter, err error) {
	status, code := mapFileErr(err)
	message := "文件操作未完成，请查看任务记录"
	switch {
	case errors.Is(err, files.ErrReadOnly):
		status, code, message = 403, "read_only", "此存储目录未启用写入"
	case errors.Is(err, files.ErrExists):
		status, code, message = 409, "exists", "目标名称已存在，请使用其他名称；已有文件未被覆盖"
	case errors.Is(err, files.ErrTooLarge):
		status, code, message = 413, "too_large", "文件超过上传大小上限"
	case errors.Is(err, files.ErrSizeMismatch):
		status, code, message = 400, "size_mismatch", "实际上传大小与声明不一致，未发布文件"
	case errors.Is(err, files.ErrTreeLimit):
		status, code, message = 400, "tree_limit", "操作达到条目、深度或总字节上限，请拆分任务"
	case errors.Is(err, files.ErrSourceChanged):
		status, code, message = 409, "source_changed", "源内容已变化，请核对后重新发起操作"
	case errors.Is(err, files.ErrConfirmation):
		status, code, message = 400, "confirmation_required", "请准确输入待永久删除的回收项名称"
	case errors.Is(err, files.ErrBusy), errors.Is(err, jobs.ErrBusy):
		status, code, message = 409, "busy", "已有文件写入或传输正在进行，请稍后重试"
	case errors.Is(err, files.ErrNoSpace):
		status, code, message = 507, "no_space", "存储空间或配额不足"
	case errors.Is(err, files.ErrTrashFull):
		status, code, message = 409, "trash_full", "回收站记录达到上限，请先恢复或清理回收内容"
	case errors.Is(err, files.ErrUnsupported):
		status, code, message = 409, "unsupported", "此文件系统不支持所需原子操作，或内部工作目录异常；未覆盖已有数据"
	case errors.Is(err, files.ErrBadPath):
		message = "相对路径或回收信息无效"
	case errors.Is(err, files.ErrOutsideRoot):
		message = "不能操作存储目录外或内部保留路径"
	case errors.Is(err, files.ErrNotExist):
		message = "文件、目标文件夹或回收记录不存在"
	case errors.Is(err, files.ErrRootUnavailable):
		message = "存储目录不可用，请检查挂载"
	case errors.Is(err, jobs.ErrGated):
		status, code, message = 409, "gated", "当前运行模式、资源状态或任务数量暂不允许此操作"
	case errors.Is(err, jobs.ErrDenied), errors.Is(err, jobs.ErrNeedsApproval):
		status, code, message = 403, "denied", "当前权限或请求参数不允许此操作"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, code, message = 408, "interrupted", "文件操作已中断或超时"
	}
	writeError(w, status, code, message)
}
func (s *Server) runFileWrite(w http.ResponseWriter, r *http.Request, op string, args *operations.Args, action func(context.Context) (any, error)) {
	if r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请核对目标并确认本次文件操作")
		return
	}
	declared, ok := s.Jobs.Registry().Lookup(op)
	if !ok {
		writeError(w, 500, "internal_error", "文件操作未注册")
		return
	}
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止，请稍后重试")
		return
	}
	defer s.work.Done()
	ctx, cancel := context.WithTimeout(r.Context(), declared.Timeout)
	defer cancel()
	if s.WorkContext != nil {
		stop := context.AfterFunc(s.WorkContext, cancel)
		defer stop()
	}
	bodyStopped := make(chan struct{})
	stopBody := context.AfterFunc(ctx, func() {
		defer close(bodyStopped)
		if op == "files.upload" {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now())
			_ = r.Body.Close()
		}
	})
	defer func() {
		if !stopBody() {
			<-bodyStopped
		}
	}()
	var result any
	out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: principalOf(r.Context()), OpID: op, Args: args, Target: args.String("root"), OperatorConsent: true, Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		var err error
		result, err = action(ctx)
		if ctx.Err() != nil && err != nil {
			err = ctx.Err()
		}
		return jobs.Result{Completed: err == nil, Verified: err == nil}, err
	}})
	if out.Job != nil {
		w.Header().Set("X-Nasctl-Job-ID", out.Job.ID)
	}
	pruneCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	_, _ = s.Jobs.Store().PruneHistory(pruneCtx)
	_, _ = s.Jobs.Store().PruneAudit(pruneCtx, time.Now())
	if out.Err != nil {
		s.fileWriteError(w, out.Err)
		return
	}
	writeJSON(w, 200, map[string]any{"job_id": out.Job.ID, "result": result})
}
func (s *Server) handleFileMutation(w http.ResponseWriter, r *http.Request) {
	if !s.fileOperator(w, r, true) {
		return
	}
	var input struct {
		Root string `json:"root"`
		Path string `json:"path"`
		From string `json:"from"`
		To   string `json:"to"`
		ID   string `json:"id"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		writeError(w, 400, "bad_request", "文件操作 JSON 无效")
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeError(w, 400, "bad_request", "文件操作必须是单个 JSON 对象")
		return
	}
	if input.Root == "" {
		writeError(w, 400, "bad_request", "请选择存储目录")
		return
	}
	op := "files." + strings.TrimPrefix(r.URL.Path, "/api/v1/files/")
	args := operations.NewArgs().Str("root", input.Root)
	var action func(context.Context) (any, error)
	switch op {
	case "files.mkdir":
		args.Str("path", input.Path)
		action = func(ctx context.Context) (any, error) {
			return map[string]string{"path": input.Path}, s.Files.Mkdir(ctx, input.Root, input.Path)
		}
	case "files.rename":
		args.Str("from", input.From).Str("to", input.To)
		action = func(ctx context.Context) (any, error) {
			return map[string]string{"path": input.To}, s.Files.Rename(ctx, input.Root, input.From, input.To)
		}
	case "files.trash":
		args.Str("path", input.Path)
		action = func(ctx context.Context) (any, error) { return s.Files.Trash(ctx, input.Root, input.Path) }
	case "files.restore":
		args.Str("id", input.ID)
		if input.To != "" {
			args.Str("to", input.To)
		}
		action = func(ctx context.Context) (any, error) {
			p, err := s.Files.Restore(ctx, input.Root, input.ID, input.To)
			return map[string]string{"path": p}, err
		}
	default:
		writeError(w, 404, "not_found", "文件操作不存在")
		return
	}
	s.runFileWrite(w, r, op, args, action)
}
func (s *Server) handleFileTrashList(w http.ResponseWriter, r *http.Request) {
	if !s.fileOperator(w, r, false) {
		return
	}
	result, err := s.Files.ListTrash(r.URL.Query().Get("root"), intParam(r, "page", 1), intParam(r, "page_size", 50))
	if err != nil {
		s.fileWriteError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

type uploadStream struct {
	r          io.Reader
	controller *http.ResponseController
	until      time.Time
}

func (u uploadStream) Read(b []byte) (int, error) {
	deadline := time.Now().Add(30 * time.Second)
	if deadline.After(u.until) {
		deadline = u.until
	}
	_ = u.controller.SetReadDeadline(deadline)
	return u.r.Read(b)
}
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	if !s.fileOperator(w, r, true) {
		return
	}
	if !s.mediaSlot(w, r) {
		return
	}
	defer func() { <-s.streamSlots }()
	root, p := r.URL.Query().Get("root"), r.URL.Query().Get("path")
	if r.ContentLength > s.Files.Limits().UploadMaxBytes {
		s.fileWriteError(w, files.ErrTooLarge)
		return
	}
	controller := http.NewResponseController(w)
	until := time.Now().Add(30 * time.Minute)
	_ = controller.SetWriteDeadline(until)
	defer func() { _ = controller.SetReadDeadline(time.Time{}); _ = controller.SetWriteDeadline(time.Time{}) }()
	body := http.MaxBytesReader(w, r.Body, s.Files.Limits().UploadMaxBytes+1)
	args := operations.NewArgs().Str("root", root).Str("path", p).Int("size", r.ContentLength).Str("overwrite", "refuse")
	s.runFileWrite(w, r, "files.upload", args, func(ctx context.Context) (any, error) {
		n, err := s.Files.Upload(ctx, root, p, uploadStream{body, controller, until}, r.ContentLength)
		return map[string]any{"path": p, "bytes": n}, err
	})
}
