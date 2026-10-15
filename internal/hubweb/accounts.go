package hubweb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

func accountInput(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		writeError(w, 400, "bad_request", "账号请求必须是一个 JSON 对象，且不能包含未知字段")
		return false
	}
	return true
}
func accountError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, accounts.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeError(w, 429, "accounts_busy", "设备正在处理密码，请稍后重试")
	case errors.Is(e, accounts.ErrCredential):
		writeError(w, 401, "invalid_credentials", "首次设置密钥或账号凭据不正确")
	case errors.Is(e, accounts.ErrConflict):
		writeError(w, 409, "accounts_changed", "账号状态已更改，请重新读取列表后核对")
	case errors.Is(e, accounts.ErrLastAdmin):
		writeError(w, 409, "last_admin", "至少保留一个已启用的管理员账号")
	case errors.Is(e, accounts.ErrInvalid):
		writeError(w, 400, "accounts_invalid", "请检查用户名、显示名称、角色和账号数量；新密码至少 12 个字符")
	default:
		writeError(w, 503, "accounts_unavailable", "账号状态不可读写，请在 NAS 本机检查或恢复")
	}
}
func (s *Server) handleAccountSetup(w http.ResponseWriter, r *http.Request) {
	if !s.Cfg.RequireSetup && s.Cfg.Token == "" {
		writeError(w, 409, "setup_requires_config", "请先在本机配置 [hub] setup=true 并重启，确保状态盘丢失时仍要求登录")
		return
	}
	if s.Accounts == nil || s.Jobs == nil {
		writeError(w, 503, "not_configured", "本机账号尚未初始化")
		return
	}
	if principalOf(r.Context()).Kind == domain.PrincipalAI {
		writeError(w, 403, "denied", "程序凭据不能设置管理员")
		return
	}
	if s.Jobs.ReadOnlyConsole() {
		writeError(w, 403, "read_only", "只读控制台不能设置账号")
		return
	}
	if r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请核对首次管理员设置并确认")
		return
	}
	now := time.Now()
	peer := loginPeer(r)
	if s.auth.limited(peer, now) {
		w.Header().Set("Retry-After", "60")
		writeError(w, 429, "rate_limited", "尝试次数过多，请稍后重试")
		return
	}
	var req accounts.Setup
	if !accountInput(w, r, &req) {
		return
	}
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	defer s.work.Done()
	ctx, stop := context.WithTimeout(r.Context(), 15*time.Second)
	defer stop()
	u, e := s.Accounts.CreateFirst(ctx, req)
	if e != nil {
		if errors.Is(e, accounts.ErrCredential) {
			s.auth.failed(peer, now)
		}
		accountError(w, e)
		return
	}
	// Bootstrap authority comes only from the private, single-use local key. The
	// account is saved before the audit; a failed audit never produces a session.
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if e = s.Jobs.Store().Record(auditCtx, hubstore.AuditEntry{Principal: accountPrincipal(u).String(), Action: "setup", Op: "users.setup", Target: u.ID, Outcome: "ok", Note: "first administrator verified with private local challenge"}); e != nil {
		writeError(w, 503, "audit_failed", "管理员已保存，但审计记录失败；请使用刚设置的账号登录后检查")
		return
	}
	value, e := s.auth.createUser(now, u)
	if e != nil {
		accountError(w, e)
		return
	}
	setSessionCookie(w, r, value, now)
	writeJSON(w, 200, map[string]any{"authenticated": true, "user": u, "expires_at": now.Add(sessionTTL)})
}
func setSessionCookie(w http.ResponseWriter, r *http.Request, value string, now time.Time) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: int(sessionTTL.Seconds()), Expires: now.Add(sessionTTL)})
	clearLegacyCookie(w, r)
}
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if s.Accounts == nil || s.Jobs == nil {
		writeError(w, 503, "not_configured", "本机账号尚未初始化")
		return
	}
	if !principalOf(r.Context()).DeviceAdmin() {
		writeError(w, 403, "denied", "账号管理需要管理员身份")
		return
	}
	ctx, stop := context.WithTimeout(r.Context(), 15*time.Second)
	defer stop()
	if r.Method == http.MethodGet {
		out, e := s.Accounts.Snapshot(ctx)
		if e != nil {
			accountError(w, e)
			return
		}
		if !out.Enabled && !s.Cfg.RequireSetup && s.Cfg.Token == "" {
			out.SetupAvailable = false
			out.SetupRequiresConfig = true
		}
		writeJSON(w, 200, out)
		return
	}
	if s.Jobs.ReadOnlyConsole() {
		writeError(w, 403, "read_only", "只读控制台不能修改账号")
		return
	}
	if r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请确认本次账号权限或密码修改")
		return
	}
	snapshot, e := s.Accounts.Snapshot(ctx)
	if e != nil {
		accountError(w, e)
		return
	}
	if !snapshot.Enabled {
		writeError(w, 409, "setup_required", "请先使用 NAS 本机的首次设置密钥创建管理员")
		return
	}
	var req accounts.Request
	remove := r.Method == http.MethodDelete
	var confirmName string
	if remove {
		var input struct {
			Revision        string `json:"revision"`
			ConfirmUsername string `json:"confirm_username"`
		}
		if !accountInput(w, r, &input) {
			return
		}
		req.ID = r.PathValue("id")
		req.Revision = input.Revision
		confirmName = input.ConfirmUsername
		for _, u := range snapshot.Users {
			if u.ID == req.ID {
				req.Username = u.Username
				req.Role = u.Role
				break
			}
		}
		if req.Username == "" || req.Username != confirmName {
			writeError(w, 400, "confirmation_required", "删除账号需要准确输入用户名")
			return
		}
	} else if !accountInput(w, r, &req) {
		return
	}
	action := "save"
	if remove {
		action = "remove"
	}
	args := operations.NewArgs().Str("user", req.Username).Str("revision", req.Revision).Str("action", action).Str("role", req.Role)
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	defer s.work.Done()
	out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: principalOf(r.Context()), OpID: "users.configure", Args: args, Target: req.Username, OperatorConsent: true, Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		var e error
		if remove {
			e = s.Accounts.Remove(ctx, req.ID, req.Revision)
		} else {
			_, e = s.Accounts.Save(ctx, req)
		}
		return jobs.Result{Completed: e == nil, Verified: e == nil}, e
	}})
	if out.Err != nil {
		if errors.Is(out.Err, jobs.ErrDenied) || errors.Is(out.Err, jobs.ErrGated) {
			s.fileWriteError(w, out.Err)
		} else {
			accountError(w, out.Err)
		}
		return
	}
	saved, e := s.Accounts.Snapshot(ctx)
	if e != nil {
		accountError(w, e)
		return
	}
	writeJSON(w, 200, saved)
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r.Context())
	if p.Kind != domain.PrincipalUser || p.UserID == "" || s.Accounts == nil || s.Jobs == nil {
		writeError(w, 403, "denied", "修改自己的密码需要账号登录")
		return
	}
	if s.Jobs.ReadOnlyConsole() {
		writeError(w, 403, "read_only", "此控制台为只读，请联系本机管理员修改密码")
		return
	}
	if r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请确认修改密码并重新登录")
		return
	}
	now := time.Now()
	peer := loginPeer(r)
	if s.auth.limited(peer, now) {
		w.Header().Set("Retry-After", "60")
		writeError(w, 429, "rate_limited", "尝试次数过多，请稍后重试")
		return
	}
	var input struct {
		Current  string `json:"current_password"`
		Password string `json:"password"`
	}
	if !accountInput(w, r, &input) {
		return
	}
	if input.Password == "" {
		accountError(w, accounts.ErrInvalid)
		return
	}
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	defer s.work.Done()
	ctx, stop := context.WithTimeout(r.Context(), 15*time.Second)
	defer stop()
	u, e := s.Accounts.Authenticate(ctx, p.Name, input.Current)
	if e != nil || u.ID != p.UserID {
		if errors.Is(e, accounts.ErrCredential) || e == nil {
			s.auth.failed(peer, now)
			accountError(w, accounts.ErrCredential)
		} else {
			accountError(w, e)
		}
		return
	}
	snapshot, e := s.Accounts.Snapshot(ctx)
	if e != nil {
		accountError(w, e)
		return
	}
	// Bind the password proof to the same account version that will be saved.
	// Otherwise an administrator changing its role between authentication and
	// this snapshot could have that change overwritten by stale user metadata.
	matched := false
	for _, current := range snapshot.Users {
		if current.ID == u.ID && current.Epoch == u.Epoch {
			matched = true
			break
		}
	}
	if !matched {
		accountError(w, accounts.ErrConflict)
		return
	}
	out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: p, OpID: "users.password", Args: operations.NewArgs().Str("user", u.Username).Str("revision", snapshot.Revision), Target: u.Username, OperatorConsent: true, Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		_, e := s.Accounts.Save(ctx, accounts.Request{Revision: snapshot.Revision, ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role, Disabled: u.Disabled, Password: input.Password})
		return jobs.Result{Completed: e == nil, Verified: e == nil}, e
	}})
	if out.Err != nil {
		if errors.Is(out.Err, jobs.ErrDenied) || errors.Is(out.Err, jobs.ErrGated) {
			s.fileWriteError(w, out.Err)
		} else {
			accountError(w, out.Err)
		}
		return
	}
	// Account publication has rotated the epoch, revoking every existing cookie.
	// Reuse logout to remove this in-memory session and clear its browser cookie.
	s.handleLogout(w, r)
}
