package hubweb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/cloudbackup"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

func (s *Server) cloudOperator(w http.ResponseWriter, r *http.Request) bool {
	if s.CloudBackups == nil || s.Jobs == nil {
		writeError(w, 503, "not_configured", "WebDAV 备份暂未初始化")
		return false
	}
	p := principalOf(r.Context())
	if p.Kind != domain.PrincipalUser || !p.DeviceAdmin() {
		writeError(w, 403, "denied", "WebDAV 备份需要管理员身份")
		return false
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		if s.Jobs.ReadOnlyConsole() {
			writeError(w, 403, "read_only", "当前为只读模式")
			return false
		}
		if r.Header.Get("X-Nasctl-Confirm") != "1" {
			writeError(w, 400, "confirmation_required", "请核对备份方式、源目录和 WebDAV 位置")
			return false
		}
	}
	return true
}
func (s *Server) cloudError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, cloudbackup.ErrBusy):
		writeError(w, 409, "busy", "已有 WebDAV 备份正在处理")
	case errors.Is(e, cloudbackup.ErrPassword):
		writeError(w, 400, "archive_password", "密码错误，或加密备份包未通过校验")
	case errors.Is(e, cloudbackup.ErrInvalid):
		writeError(w, 400, "webdav_scope", "配置或备份范围已变更，请重新读取并核对")
	case errors.Is(e, cloudbackup.ErrRemote):
		msg, detail := "WebDAV 请求或回读校验失败，请检查地址、账号、权限及 MOVE 支持", ""
		if summary, d, ok := cloudbackup.Explain(e); ok {
			msg, detail = "WebDAV 请求失败："+summary, d
		}
		writeErrorDetail(w, 502, "webdav_failed", msg, detail)
	default:
		s.fileWriteError(w, e)
	}
}
func (s *Server) handleCloudSettings(w http.ResponseWriter, r *http.Request) {
	if !s.cloudOperator(w, r) {
		return
	}
	if r.Method == "GET" {
		writeJSON(w, 200, s.CloudBackups.Settings())
		return
	}
	var in cloudbackup.ConnectionRequest
	if !backupInput(w, r, &in) {
		return
	}
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	defer s.work.Done()
	ctx, stop := context.WithTimeout(r.Context(), 10*time.Second)
	defer stop()
	var view cloudbackup.View
	out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: principalOf(r.Context()), OpID: "backup.webdav.configure", Args: operations.NewArgs().Str("revision", in.Revision), Target: "webdav", OperatorConsent: true, Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		var e error
		view, e = s.CloudBackups.Save(ctx, in)
		in.Password = ""
		return jobs.Result{Completed: e == nil, Verified: e == nil}, e
	}})
	if out.Err != nil {
		s.cloudError(w, out.Err)
		return
	}
	writeJSON(w, 200, view)
}
func (s *Server) handleCloudProbe(w http.ResponseWriter, r *http.Request) {
	if !s.cloudOperator(w, r) {
		return
	}
	var in struct {
		Revision string `json:"revision"`
	}
	if !backupInput(w, r, &in) {
		return
	}
	ctx, stop := context.WithTimeout(r.Context(), 15*time.Second)
	defer stop()
	if e := s.CloudBackups.Probe(ctx, in.Revision); e != nil {
		s.cloudError(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"connected": true})
}
func (s *Server) handleCloudWork(w http.ResponseWriter, r *http.Request) {
	if !s.cloudOperator(w, r) {
		return
	}
	var in struct {
		cloudbackup.RunRequest
		ID     string `json:"id"`
		ToRoot string `json:"to_root"`
		To     string `json:"to"`
	}
	if !backupInput(w, r, &in) {
		return
	}
	action := r.PathValue("action")
	op := "backup.webdav." + action
	scopeBody, _ := json.Marshal(struct {
		Input                    cloudbackup.Input
		ID, ToRoot, To, Revision string
	}{in.Input, in.ID, in.ToRoot, in.To, in.Revision})
	scopeHash := sha256.Sum256(scopeBody)
	args := operations.NewArgs().Str("scope", hex.EncodeToString(scopeHash[:]))
	if action != "run" && action != "retry" && action != "restore" {
		writeError(w, 404, "not_found", "备份任务不存在")
		return
	}
	s.startBackground(w, r, op, args, "webdav", true, func(ctx context.Context, job *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		last := time.Time{}
		phase := ""
		report := func(p cloudbackup.Progress) {
			if p.Phase == phase && time.Since(last) < 500*time.Millisecond {
				return
			}
			last = time.Now()
			_ = s.Jobs.Store().Progress(ctx, job.ID, 0, 0, p.Bytes, p.Total, "bytes")
			if p.Phase != phase {
				phase = p.Phase
				_ = s.Jobs.Store().Transition(ctx, job.ID, domain.JobRunning, map[string]string{"pack": "正在压缩并加密私密备份", "upload": "正在上传并回读校验", "publish": "正在发布已校验的备份", "restore": "正在校验并恢复到新文件夹"}[phase])
			}
		}
		var e error
		var summary any
		switch action {
		case "run":
			record, err := s.CloudBackups.Run(ctx, in.RunRequest, report)
			e = err
			summary = map[string]any{"id": record.ID, "mode": record.Input.Mode, "entries": record.Entries, "bytes": record.Bytes, "state": record.State}
		case "retry":
			record, err := s.CloudBackups.Retry(ctx, in.ID, report)
			e = err
			summary = map[string]any{"id": record.ID, "state": record.State}
		case "restore":
			result, err := s.CloudBackups.Restore(ctx, in.ID, in.ToRoot, in.To, in.Password, report)
			e = err
			summary = result
		}
		in.Password = ""
		finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		note, _ := json.Marshal(map[string]any{"kind": "webdav.result", "result": summary})
		_ = s.Jobs.Store().Transition(finish, job.ID, domain.JobRunning, string(note))
		return jobs.Result{Completed: e == nil, Verified: e == nil}, cloudWorkError(e)
	})
}
func (s *Server) handleCloudDownload(w http.ResponseWriter, r *http.Request) {
	if !s.cloudOperator(w, r) {
		return
	}
	select {
	case s.streamSlots <- struct{}{}:
		defer func() { <-s.streamSlots }()
	default:
		writeError(w, 429, "busy", "已有文件传输正在进行")
		return
	}
	ctx, stop := context.WithTimeout(r.Context(), 2*time.Hour)
	defer stop()
	body, size, name, e := s.CloudBackups.Download(ctx, r.PathValue("id"))
	if e != nil {
		s.cloudError(w, e)
		return
	}
	defer body.Close()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Hour))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	if r.Method == "HEAD" {
		return
	}
	if _, e = io.CopyBuffer(w, io.LimitReader(body, size+1), make([]byte, 64<<10)); e != nil {
		panic(http.ErrAbortHandler)
	}
}

func cloudWorkError(e error) error {
	if e == nil || errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
		return e
	}
	message := "备份未完成，请检查存储位置、可用空间和连接。"
	switch {
	case errors.Is(e, cloudbackup.ErrPendingVault):
		message = "请先完成保险箱中未完成的移入，再直接上传普通文件。"
	case errors.Is(e, cloudbackup.ErrPassword):
		message = "密码错误，或加密包未通过校验。"
	case errors.Is(e, cloudbackup.ErrRemote):
		message = "WebDAV 上传或回读校验失败，请检查连接、写入权限和 MOVE 支持。"
		if summary, detail, ok := cloudbackup.Explain(e); ok {
			// The first line is the summary; the rest is shown on demand.
			message = "WebDAV " + summary + "\n" + detail
		}
	case errors.Is(e, cloudbackup.ErrInvalid):
		message = "备份范围或连接已变更，请重新读取并核对。"
	case errors.Is(e, files.ErrExists):
		message = "目标文件夹已经存在，请换一个新位置。"
	case errors.Is(e, files.ErrTreeLimit):
		message = "备份超出条目、大小或目录深度限制，请缩小范围。"
	case errors.Is(e, files.ErrNoSpace):
		message = "可用空间不足，已保留原文件并停止备份。"
	case errors.Is(e, files.ErrReadOnly):
		message = "目标存储位置只读，请选择可写位置。"
	default:
		// Backups are administrator work; the underlying cause helps them.
		message += "\n错误：" + domain.Redact(e.Error())
	}
	return errors.New(message)
}
