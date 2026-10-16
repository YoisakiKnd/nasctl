package hubweb

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/domain"
)

func TestRememberedSessionsSurviveRestartUntilRevoked(t *testing.T) {
	r, c, key := accountRig(t)
	setupOwner(t, r, key)
	ctx := context.Background()
	snapshot, e := c.Snapshot(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Save(ctx, accounts.Request{Revision: snapshot.Revision, Username: "reader", Role: domain.RoleViewer, Password: accountFixturePassword}); e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	if e = r.srv.EnableRememberedSessions(dir); e != nil {
		t.Fatal(e)
	}
	login := func(remember bool) *http.Cookie {
		t.Helper()
		code, b, cookies := accountRequest(t, r, "POST", "/api/v1/auth/login", map[string]any{"username": "reader", "password": accountFixturePassword, "remember": remember}, nil, false)
		if code != 200 {
			t.Fatalf("login %d %s", code, b)
		}
		for _, cookie := range cookies {
			if cookie.Name == sessionCookie {
				return cookie
			}
		}
		t.Fatal("no cookie")
		return nil
	}
	kept, plain := login(true), login(false)
	if kept.MaxAge != int(rememberTTL.Seconds()) || plain.MaxAge != int(sessionTTL.Seconds()) {
		t.Fatalf("cookie lifetimes: kept %d plain %d", kept.MaxAge, plain.MaxAge)
	}
	path := filepath.Join(dir, rememberFile)
	info, e := os.Stat(path)
	if e != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("kept-session file %v %v", info, e)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), kept.Value) || strings.Count(string(raw), `"key"`) != 1 {
		t.Fatalf("file must hold one digest and never the cookie: %s", raw)
	}
	restart := func() {
		t.Helper()
		r.srv.auth = browserAuth{}
		if e := r.srv.EnableRememberedSessions(dir); e != nil {
			t.Fatal(e)
		}
	}
	restart()
	if code, _, _ := accountRequest(t, r, "GET", "/api/v1/files/roots", nil, kept, false); code != 200 {
		t.Fatal("kept session lost on restart", code)
	}
	if code, _, _ := accountRequest(t, r, "GET", "/api/v1/files/roots", nil, plain, false); code != 401 {
		t.Fatal("ordinary session survived restart", code)
	}
	// Logging out removes the kept session from disk too.
	if code, _, _ := accountRequest(t, r, "POST", "/api/v1/auth/logout", nil, kept, false); code != 200 {
		t.Fatal("logout", code)
	}
	restart()
	if code, _, _ := accountRequest(t, r, "GET", "/api/v1/files/roots", nil, kept, false); code != 401 {
		t.Fatal("logged-out session came back after restart", code)
	}
	// A password change revokes a kept session, before and after a restart.
	kept = login(true)
	code, b, _ := accountRequest(t, r, "POST", "/api/v1/auth/password", map[string]string{"current_password": accountFixturePassword, "password": "generated rotated reader password"}, kept, true)
	if code != 200 {
		t.Fatal("password change", code, string(b))
	}
	restart()
	if code, _, _ := accountRequest(t, r, "GET", "/api/v1/files/roots", nil, kept, false); code != 401 {
		t.Fatal("kept session survived a password change", code)
	}
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), `"key"`) {
		t.Fatalf("revoked session left on disk: %s", raw)
	}
	// Token logins are never kept.
	r.srv.Cfg.Token = "operator-secret"
	code, _, cookies := accountRequest(t, r, "POST", "/api/v1/auth/login", map[string]any{"token": "operator-secret", "remember": true}, nil, false)
	if code != 200 || len(cookies) == 0 || cookies[0].MaxAge != int(sessionTTL.Seconds()) {
		t.Fatal("token session must not be kept", code)
	}
	// A tampered file signs everyone out instead of failing open.
	if e = os.WriteFile(path, []byte(`{"version":1,"sessions":[{"key":"zz"}],"extra":1}`), 0o600); e != nil {
		t.Fatal(e)
	}
	r.srv.auth = browserAuth{}
	if e = r.srv.EnableRememberedSessions(dir); e == nil {
		t.Fatal("invalid kept-session file accepted")
	}
}
