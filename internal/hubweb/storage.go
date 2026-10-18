package hubweb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/storage"
)

// StorageHelper is the root storage helper behind its socket.
type StorageHelper interface {
	Available() bool
	Call(ctx context.Context, req storage.Request, out any) error
}

// storageOverview is everything the storage page shows at once.
type storageOverview struct {
	Helper bool            `json:"helper"`
	Disks  []storage.Disk  `json:"disks"`
	Status *storage.Status `json:"status"`
	Error  string          `json:"error,omitempty"`
}

// storageActions maps the URL action to the helper operation and how long it
// may take. Formatting includes mkfs on a whole disk.
var storageActions = map[string]struct {
	op      string
	timeout time.Duration
}{
	"mount":          {"mount", time.Minute},
	"unmount":        {"unmount", time.Minute},
	"format":         {"format", 25 * time.Minute},
	"automount":      {"automount.set", 10 * time.Second},
	"shares":         {"shares.set", time.Minute},
	"share-password": {"shares.password", 30 * time.Second},
	"net-add":        {"net.add", time.Minute},
	"net-remove":     {"net.remove", time.Minute},
}

func (s *Server) storageAdmin(w http.ResponseWriter, r *http.Request) bool {
	p := principalOf(r.Context())
	if p.Kind != domain.PrincipalUser || !p.DeviceAdmin() {
		writeError(w, 403, "denied", "存储管理需要管理员身份")
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if s.Jobs != nil && s.Jobs.ReadOnlyConsole() {
			writeError(w, 403, "read_only", "当前为只读模式")
			return false
		}
		if r.Header.Get("X-Nasctl-Confirm") != "1" {
			writeError(w, 400, "confirmation_required", "请确认这项存储操作")
			return false
		}
	}
	return true
}

func (s *Server) storageError(w http.ResponseWriter, err error) {
	var he *storage.HelperError
	switch {
	case errors.Is(err, storage.ErrHelperMissing) || s.Storage == nil:
		writeError(w, 503, "storage_helper_missing", "存储助手没有安装或没有启动，请按 STORAGE.md 启用 nasctl-storage.socket")
	case errors.As(err, &he):
		writeErrorDetail(w, 400, "storage_refused", he.Message, he.Detail)
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, 504, "storage_timeout", "存储助手没有及时完成，请稍后刷新查看结果")
	default:
		writeErrorDetail(w, 502, "storage_failed", "存储助手没有回应", err.Error())
	}
}

func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	if !s.storageAdmin(w, r) {
		return
	}
	out := storageOverview{Disks: []storage.Disk{}}
	if s.Storage == nil || !s.Storage.Available() {
		writeJSON(w, 200, out)
		return
	}
	out.Helper = true
	ctx, stop := context.WithTimeout(r.Context(), 15*time.Second)
	defer stop()
	var st storage.Status
	if err := s.Storage.Call(ctx, storage.Request{Op: "disks"}, &out.Disks); err != nil {
		out.Error = err.Error()
	} else if err := s.Storage.Call(ctx, storage.Request{Op: "status"}, &st); err != nil {
		out.Error = err.Error()
	} else {
		out.Status = &st
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleStorageSmart(w http.ResponseWriter, r *http.Request) {
	if !s.storageAdmin(w, r) {
		return
	}
	if s.Storage == nil {
		s.storageError(w, storage.ErrHelperMissing)
		return
	}
	ctx, stop := context.WithTimeout(r.Context(), 40*time.Second)
	defer stop()
	var out storage.SMART
	if err := s.Storage.Call(ctx, storage.Request{Op: "smart", Device: r.URL.Query().Get("device")}, &out); err != nil {
		s.storageError(w, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleStorageAction(w http.ResponseWriter, r *http.Request) {
	if !s.storageAdmin(w, r) {
		return
	}
	action, ok := storageActions[r.PathValue("action")]
	if !ok {
		writeError(w, 404, "not_found", "没有这项存储操作")
		return
	}
	var req storage.Request
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF {
		writeError(w, 400, "bad_request", "存储请求必须是单个 JSON 对象，且不能包含未知字段")
		return
	}
	req.Op = action.op
	if s.Storage == nil {
		s.storageError(w, storage.ErrHelperMissing)
		return
	}
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	defer s.work.Done()
	// A disk operation is finished by the helper even if the browser goes
	// away, so the answer is recorded either way.
	ctx, stop := context.WithTimeout(context.WithoutCancel(r.Context()), action.timeout)
	defer stop()
	err := s.Storage.Call(ctx, req, nil)
	s.auditStorage(r, req, err)
	if err != nil {
		s.storageError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) auditStorage(r *http.Request, req storage.Request, err error) {
	if s.Jobs == nil {
		return
	}
	target := req.Device
	if target == "" {
		target = req.Name
	}
	if req.Net != nil {
		target = req.Net.Kind + "://" + req.Net.Server + "/" + req.Net.Share + " as " + req.Net.Name
	}
	outcome, note := "ok", ""
	if err != nil {
		outcome, note = "failed", err.Error()
	}
	if len(note) > 300 {
		note = note[:300]
	}
	ctx, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
	defer stop()
	_ = s.Jobs.Store().Record(ctx, hubstore.AuditEntry{At: time.Now(), Principal: principalOf(r.Context()).String(), Action: "call", Op: "storage." + req.Op, Target: target, Outcome: outcome, Note: note})
}
