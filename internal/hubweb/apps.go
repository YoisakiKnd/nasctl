package hubweb

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/YoisakiKnd/nasctl/internal/desktopapps"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
	"io"
	"net/http"
	"time"
)

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	if principalOf(r.Context()).Kind!=domain.PrincipalUser{writeError(w,403,"denied","桌面应用需要 NAS 成员身份");return}
 if s.Apps == nil {
		if r.Method != "GET" {
			writeError(w, 503, "not_configured", "桌面应用设置尚未初始化")
			return
		}
		writeJSON(w, 200, desktopapps.View{Apps: []desktopapps.App{}})
		return
	}
	if r.Method == "GET" {
		v, e := s.Apps.Get(r.Context())
		if e != nil {
			writeError(w, 500, "apps_error", "应用快捷方式暂时无法读取")
			return
		}
		writeJSON(w, 200, v)
		return
	}
	p := principalOf(r.Context())
	if p.Kind != domain.PrincipalUser || !p.DeviceAdmin() {
		writeError(w, 403, "denied", "仅管理员可以编辑桌面应用")
		return
	}
	if s.Jobs == nil || s.Jobs.ReadOnlyConsole() {
		writeError(w, 403, "read_only", "当前不能修改应用")
		return
	}
	if r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请核对应用名称和地址")
		return
	}
	var input desktopapps.View
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, 400, "bad_request", "应用列表格式无效或超过上限")
		return
	}
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	defer s.work.Done()
	ctx, stop := context.WithTimeout(r.Context(), 10*time.Second)
	defer stop()
	var view desktopapps.View
	out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: p, OpID: "apps.configure", Args: operations.NewArgs().Str("revision", input.Revision), Target: "desktop", OperatorConsent: true, Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		var e error
		view, e = s.Apps.Save(ctx, input.Revision, input.Apps)
		return jobs.Result{Completed: e == nil, Verified: e == nil}, e
	}})
	if out.Err != nil {
		if errors.Is(out.Err, desktopapps.ErrConflict) {
			writeError(w, 409, "apps_changed", "应用列表已更改，请重新读取后编辑")
		} else {
			writeError(w, 400, "apps_invalid", "请检查重复编号、应用地址、图标和名称")
		}
		return
	}
	writeJSON(w, 200, view)
}
