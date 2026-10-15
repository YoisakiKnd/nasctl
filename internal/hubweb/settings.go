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
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
	"github.com/YoisakiKnd/nasctl/internal/settings"
)

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if s.Settings == nil || s.Jobs == nil {
		writeError(w, 503, "not_configured", "统一设置尚未初始化")
		return
	}
	if principalOf(r.Context()).Kind != domain.PrincipalUser {
		writeError(w, 403, "denied", "统一设置需要操作者身份")
		return
	}
	ctx, stop := context.WithTimeout(r.Context(), 10*time.Second)
	defer stop()
	fail := func(e error) {
		if errors.Is(e, settings.ErrConflict) {
			writeError(w, 409, "settings_changed", "配置已被其他窗口或本机修改，请保留草稿并重新读取")
		} else {
			writeError(w, 400, "settings_invalid", "配置未通过校验；请检查路径、重复目录、各项上限和资源阈值")
		}
	}
	if r.Method == "GET" {
		out, e := s.Settings.Get(ctx)
		if e != nil {
			fail(e)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	if s.Jobs.ReadOnlyConsole() {
		writeError(w, 403, "read_only", "控制台处于只读模式")
		return
	}
	var input settings.Request
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF {
		writeError(w, 400, "bad_request", "设置请求必须为一个 JSON 对象，且不能包含未知字段")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/validate") {
		out, e := s.Settings.Validate(ctx, input)
		if e != nil {
			fail(e)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	if r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请先核对配置和重启影响后确认保存")
		return
	}
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	defer s.work.Done()
	var saved settings.Snapshot
	out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: principalOf(r.Context()), OpID: "settings.configure", Args: operations.NewArgs().Str("revision", input.Revision), Target: "next-start", OperatorConsent: true, Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		var e error
		saved, e = s.Settings.Save(ctx, input)
		return jobs.Result{Completed: e == nil, Verified: e == nil}, e
	}})
	if out.Err != nil {
		fail(out.Err)
		return
	}
	writeJSON(w, 200, saved)
}
