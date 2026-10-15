package domain

import "strings"

const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// DeviceAdmin preserves the configured-token/local-operator identity. Named
// accounts always carry a nonempty role resolved from the private user store.
func (p Principal) DeviceAdmin() bool {
	return p.Kind == PrincipalLocal || (p.Kind == PrincipalUser && (p.Role == RoleAdmin || p.Role == "" && p.UserID == ""))
}
func (p Principal) ReadOnlyUser() bool { return p.Kind == PrincipalUser && p.Role == RoleViewer }

// RoleAllowsOperation is an additional gate, not an approval or resource bypass.
// AI/helper policy is applied separately by the existing permission machinery.
func (p Principal) RoleAllowsOperation(id string, readOnly bool) bool {
	if p.Kind != PrincipalUser {
		return true
	}
	if p.DeviceAdmin() {
		return true
	}
	if p.Role != RoleOperator && p.Role != RoleViewer {
		return false
	}
	if readOnly {
		return true
	}
	if id == "users.password" && p.UserID != "" {
		return true
	}
	if p.Role == RoleViewer {
		return false
	}
	if strings.HasPrefix(id, "files.") || strings.HasPrefix(id, "anime.") {
		return true
	}
	switch id {
	case "album.scan", "album.thumbs", "album.pause", "album.resume", "media.progress":
		return true
	}
	return false
}
