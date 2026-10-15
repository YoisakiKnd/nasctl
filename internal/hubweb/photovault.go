package hubweb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
	"github.com/YoisakiKnd/nasctl/internal/photovault"
)

const vaultCookie = "nasctl_photo_vault"

func (s *Server) vaultIdentity(r *http.Request) (string, string) {
	token := ""
	if c, e := r.Cookie(vaultCookie); e == nil && len(c.Value) == 43 {
		token = c.Value
	}
	credential := s.presentedToken(r)
	if c, e := r.Cookie(sessionCookie); e == nil {
		credential = c.Value
	}
	digest := sha256.Sum256([]byte(credential))
	return token, principalOf(r.Context()).String() + ":" + hex.EncodeToString(digest[:])
}
func (s *Server) vaultAccess(w http.ResponseWriter, r *http.Request) bool {
	if !principalOf(r.Context()).DeviceAdmin() {
		writeError(w, 403, "denied", "照片保险箱仅对本机管理员开放，并需要独立密码")
		return false
	}
	if s.PhotoVault == nil || s.VaultMover == nil {
		writeError(w, 503, "not_configured", "照片保险箱需要启用本地相册")
		return false
	}
	if r.Method == "POST" && r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请确认保险箱操作")
		return false
	}
	return true
}
func (s *Server) vaultError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, photovault.ErrLocked):
		writeError(w, 403, "vault_locked", "保险箱已锁定或密码不正确，请重新解锁")
	case errors.Is(e, photovault.ErrBusy):
		writeError(w, 429, "vault_busy", e.Error())
	case errors.Is(e, os.ErrNotExist):
		writeError(w, 404, "not_found", "保险箱中没有这张照片")
	default:
		writeError(w, 400, "vault_error", "保险箱操作未完成，请检查密码、文件或存储状态")
	}
}
func (s *Server) handleVaultStatus(w http.ResponseWriter, r *http.Request) {
	if !principalOf(r.Context()).DeviceAdmin() {
		writeError(w, 403, "denied", "此账号无权访问照片保险箱")
		return
	}
	if s.PhotoVault == nil {
		writeJSON(w, 200, photovault.Snapshot{})
		return
	}
	token, owner := s.vaultIdentity(r)
	writeJSON(w, 200, s.PhotoVault.Status(token, owner))
}
func (s *Server) handleVaultPassword(w http.ResponseWriter, r *http.Request) {
	if !s.vaultAccess(w, r) {
		return
	}
	operation := r.PathValue("operation")
	token, owner := s.vaultIdentity(r)
	if operation == "lock" {
		s.PhotoVault.Lock(token, owner)
		http.SetCookie(w, &http.Cookie{Name: vaultCookie, Value: "", Path: "/api/v1/album/vault", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: -1})
		writeJSON(w, 200, s.PhotoVault.Status("", owner))
		return
	}
	var input struct {
		Password    string `json:"password"`
		NewPassword string `json:"new_password"`
	}
	if !albumInput(w, r, &input) {
		return
	}
	if operation != "create" && operation != "unlock" && operation != "password" {
		writeError(w, 404, "not_found", "没有此保险箱操作")
		return
	}
	if operation != "unlock" && (s.Jobs == nil || s.Jobs.ReadOnlyConsole()) {
		writeError(w, 403, "read_only", "只读控制台不能设置保险箱密码")
		return
	}
	peer := "vault:" + loginPeer(r)
	now := time.Now()
	if s.vaultAuth.limited(peer, now) {
		w.Header().Set("Retry-After", "60")
		writeError(w, 429, "rate_limited", "尝试次数过多，请一分钟后重试")
		return
	}
	var e error
	switch operation {
	case "create":
		e = s.PhotoVault.Create(input.Password)
	case "password":
		e = s.PhotoVault.ChangePassword(token, owner, input.Password, input.NewPassword)
	case "unlock":
		token, e = s.PhotoVault.Unlock(input.Password, owner)
	}
	if e != nil {
		s.vaultAuth.failed(peer, now)
		s.vaultError(w, e)
		return
	}
	if operation == "unlock" {
		http.SetCookie(w, &http.Cookie{Name: vaultCookie, Value: token, Path: "/api/v1/album/vault", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 900})
	}
	writeJSON(w, 200, s.PhotoVault.Status(token, owner))
}
func (s *Server) handleVaultList(w http.ResponseWriter, r *http.Request) {
	if !s.vaultAccess(w, r) {
		return
	}
	page := 1
	var e error
	if value := r.URL.Query().Get("page"); value != "" {
		page, e = strconv.Atoi(value)
	}
	if e != nil {
		writeError(w, 400, "bad_request", "页码无效")
		return
	}
	token, owner := s.vaultIdentity(r)
	items, total, e := s.PhotoVault.List(r.Context(), token, owner, page)
	if e != nil {
		s.vaultError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "total_estimate": total, "page": page, "has_next": page*48 < total})
}
func (s *Server) handleVaultDetails(w http.ResponseWriter, r *http.Request) {
	if !s.vaultAccess(w, r) {
		return
	}
	token, owner := s.vaultIdentity(r)
	item, record, e := s.PhotoVault.Details(r.Context(), token, owner, r.PathValue("id"))
	if e != nil {
		s.vaultError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"item": item, "exif": record.Details.Exif, "paths": map[string]string{"display": "照片保险箱"}, "warnings": []string{}})
}
func (s *Server) handleVaultStream(w http.ResponseWriter, r *http.Request) {
	if !s.vaultAccess(w, r) {
		return
	}
	part := r.PathValue("part")
	if part != "thumb" {
		if !s.mediaSlot(w, r) {
			return
		}
		defer func() { <-s.streamSlots }()
	}
	token, owner := s.vaultIdentity(r)
	reader, item, mime, e := s.PhotoVault.OpenMedia(r.Context(), token, owner, r.PathValue("id"), part)
	if e != nil {
		s.vaultError(w, e)
		return
	}
	defer reader.Close()
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", safeAlbumType(mime, part))
	if part == "thumb" {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	} else {
		disposition := "inline"
		if part == "download" {
			disposition = "attachment"
		}
		w.Header().Set("Content-Disposition", albumDisposition(disposition+"; filename=\"photo\"", part, item.ID))
		if part == "download" {
			w.Header().Set("Content-Disposition", contentDisposition(item.Name))
		}
	}
	http.ServeContent(w, r, item.Name, time.Time{}, reader)
}
func (s *Server) handleVaultMove(w http.ResponseWriter, r *http.Request) {
	if !s.vaultAccess(w, r) {
		return
	}
	if s.Jobs == nil || s.Jobs.ReadOnlyConsole() {
		writeError(w, 403, "read_only", "只读控制台不能移动照片")
		return
	}
	var input struct {
		ID int64 `json:"id"`
	}
	if !albumInput(w, r, &input) {
		return
	}
	if input.ID <= 0 || input.ID > 9007199254740991 {
		writeError(w, 400, "bad_request", "照片编号无效")
		return
	}
	token, owner := s.vaultIdentity(r)
	if !s.PhotoVault.Valid(token, owner) {
		s.vaultError(w, photovault.ErrLocked)
		return
	}
	// On retry the original index may already be redacted. Use encrypted metadata.
	_, record, exists, e := s.PhotoVault.Find(token, owner, input.ID)
	if e != nil {
		s.vaultError(w, e)
		return
	}
	details := record.Details
	if !exists {
		details, e = s.Album.Details(r.Context(), strconv.FormatInt(input.ID, 10))
		if e != nil {
			s.albumError(w, e)
			return
		}
	}
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	defer s.work.Done()
	ctx, stop := context.WithTimeout(r.Context(), 50*time.Second)
	defer stop()
	vaultID := ""
	out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: principalOf(r.Context()), OpID: "album.vault.move", Args: operations.NewArgs().Int("id", input.ID), Target: "photo", OperatorConsent: true, Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		var e error
		vaultID, e = s.VaultMover.MoveToVault(ctx, s.PhotoVault, token, owner, input.ID, details)
		return jobs.Result{Completed: e == nil, Detail: "照片保险箱移入操作"}, e
	}})
	if out.Err != nil {
		if vaultID != "" {
			writeJSON(w, 409, map[string]any{"error": "vault_pending", "message": "加密副本已保存，但原件清理未完成。原件可能仍存在，请在保险箱中重试完成移入。"})
			return
		}
		writeError(w, 400, "vault_move_failed", "未移入保险箱：请确认照片不超过 64 MiB、目录允许写入、没有重叠的相册目录，且没有其他相册任务或文件写入正在进行")
		return
	}
	writeJSON(w, 200, map[string]any{"id": vaultID, "moved": true})
}
