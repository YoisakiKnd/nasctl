package hubweb

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
)

// Role checks apply to authenticated human accounts. AI policy and the legacy
// configured operator keep their existing, independently enforced boundaries.
func roleRouteAllowed(p domain.Principal, r *http.Request) bool {
	if p.Kind != domain.PrincipalUser || p.DeviceAdmin() || publicRoute(r) {
		return true
	}
	if p.Role != domain.RoleViewer && p.Role != domain.RoleOperator {
		return false
	}
	path := r.URL.Path
	if p.Role == domain.RoleViewer && (path == "/api/v1/anime/settings" || strings.HasPrefix(path, "/api/v1/anime/metadata/") || strings.HasPrefix(path, "/api/v1/anime/sources/") || path == "/api/v1/anime/history/export" || strings.HasPrefix(path, "/api/v1/anime/subscriptions/") && strings.HasSuffix(path, "/history")) {
		return false
	}
	for _, prefix := range []string{"/api/v1/users", "/api/v1/settings", "/api/v1/approvals", "/api/v1/audit", "/api/v1/backup", "/api/v1/ops"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return false
		}
	}
	if path == "/api/v1/auth/password" && r.Method == http.MethodPost && p.UserID != "" {
		return true
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if read {
		if p.Role == domain.RoleViewer && (path == "/api/v1/jobs" || strings.HasPrefix(path, "/api/v1/jobs/")) {
			return false
		}
		return true
	}
	if p.Role == domain.RoleViewer {
		return false
	}
	if strings.HasPrefix(path, "/api/v1/files/") || strings.HasPrefix(path, "/api/v1/anime/") {
		return true
	}
	if strings.HasPrefix(path, "/api/v1/album/tasks/") {
		operation := strings.TrimPrefix(path, "/api/v1/album/tasks/")
		return operation == "scan" || operation == "thumbs" || operation == "pause" || operation == "resume"
	}
	return strings.HasPrefix(path, "/api/v1/jobs/") && strings.HasSuffix(path, "/cancel")
}
func canReadJob(p domain.Principal, j hubstore.Job) bool {
	if p.Kind != domain.PrincipalUser || p.DeviceAdmin() {
		return true
	}
	return p.Role == domain.RoleOperator && p.UserID != "" && j.Principal == p.String()
}
func canStopJob(p domain.Principal, j hubstore.Job) bool {
	return p.Kind == domain.PrincipalUser && (p.DeviceAdmin() || p.Role == domain.RoleOperator && p.UserID != "" && j.Principal == p.String())
}
func (s *Server) roleDenied(r *http.Request) {
	if s.Jobs == nil {
		return
	}
	ctx, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
	defer stop()
	target := r.URL.Path
	if len(target) > 256 {
		target = target[:256]
	}
	_ = s.Jobs.Store().Record(ctx, hubstore.AuditEntry{Principal: principalOf(r.Context()).String(), Action: "refused", Op: "access.role", Target: target, Outcome: "refused", Note: "user role cannot access this route"})
}
