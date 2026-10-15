package hubweb

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/accountslocal"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
)

const accountFixturePassword = "generated account passphrase"

type passwordRoleRace struct {
	accounts.Control
	change func() error
}

func (c *passwordRoleRace) Authenticate(ctx context.Context, name, password string) (accounts.User, error) {
	u, e := c.Control.Authenticate(ctx, name, password)
	if e == nil {
		e = c.change()
	}
	return u, e
}

func accountRig(t *testing.T) (*rig, *accountslocal.Store, string) {
	t.Helper()
	r := writeRig(t, nil)
	c, e := accountslocal.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = c.EnsureSetupKey(context.Background()); e != nil {
		t.Fatal(e)
	}
	key, e := c.SetupKey(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	r.srv.Accounts = c
	r.srv.Cfg.RequireSetup = true
	t.Cleanup(func() { c.Close() })
	t.Cleanup(r.srv.WaitWork)
	return r, c, key
}
func accountRequest(t *testing.T, r *rig, method, path string, body any, cookie *http.Cookie, confirm bool) (int, []byte, []*http.Cookie) {
	t.Helper()
	var data []byte
	if body != nil {
		var e error
		data, e = json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
	}
	req, e := http.NewRequest(method, r.ts.URL+path, strings.NewReader(string(data)))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if confirm {
		req.Header.Set("X-Nasctl-Confirm", "1")
	}
	res, e := r.client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	b, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	return res.StatusCode, b, res.Cookies()
}
func setupOwner(t *testing.T, r *rig, key string) (accounts.User, *http.Cookie) {
	t.Helper()
	code, b, cookies := accountRequest(t, r, "POST", "/api/v1/auth/setup", accounts.Setup{Code: key, Username: "owner", DisplayName: "Owner", Password: accountFixturePassword}, nil, true)
	if code != 200 {
		t.Fatalf("setup %d %s", code, b)
	}
	var out struct {
		User accounts.User `json:"user"`
	}
	if e := json.Unmarshal(b, &out); e != nil {
		t.Fatal(e)
	}
	for _, cookie := range cookies {
		if cookie.Name == sessionCookie {
			return out.User, cookie
		}
	}
	t.Fatal("no account session")
	return accounts.User{}, nil
}
func accountLogin(t *testing.T, r *rig, name, password string) *http.Cookie {
	t.Helper()
	code, b, cookies := accountRequest(t, r, "POST", "/api/v1/auth/login", map[string]string{"username": name, "password": password}, nil, false)
	if code != 200 {
		t.Fatalf("account login %d %s", code, b)
	}
	for _, cookie := range cookies {
		if cookie.Name == sessionCookie {
			return cookie
		}
	}
	t.Fatal("no login cookie")
	return nil
}
func TestAccountSetupConsumesChallengeAndEnablesAuthenticatedMode(t *testing.T) {
	r, c, key := accountRig(t)
	code, b, _ := accountRequest(t, r, "GET", "/api/v1/auth/status", nil, nil, false)
	if code != 200 || !strings.Contains(string(b), `"setup_available":true`) {
		t.Fatal("setup discovery", code, string(b))
	}
	code, _, _ = accountRequest(t, r, "POST", "/api/v1/auth/setup", accounts.Setup{Code: key, Username: "owner", Password: accountFixturePassword}, nil, false)
	if code != 400 {
		t.Fatal("unconfirmed setup", code)
	}
	owner, cookie := setupOwner(t, r, key)
	if owner.Role != domain.RoleAdmin || owner.Epoch != "" {
		t.Fatal("public setup user", owner)
	}
	code, b, _ = accountRequest(t, r, "GET", "/api/v1/auth/status", nil, cookie, false)
	if code != 200 || !strings.Contains(string(b), `"role":"admin"`) || !strings.Contains(string(b), `"required":true`) {
		t.Fatal("named admin status", code, string(b))
	}
	code, _, _ = accountRequest(t, r, "GET", "/api/v1/files/roots", nil, nil, false)
	if code != 401 {
		t.Fatal("anonymous fallback remained after setup", code)
	}
	if _, e := c.SetupKey(context.Background()); e == nil {
		t.Fatal("bootstrap challenge not consumed")
	}
	code, b, _ = accountRequest(t, r, "GET", "/api/v1/users", nil, cookie, false)
	if code != 200 || strings.Contains(string(b), accountFixturePassword) || strings.Contains(string(b), `"hash"`) || strings.Contains(string(b), key) {
		t.Fatal("account list leaked secrets", code)
	}
	code, _, _ = accountRequest(t, r, "POST", "/api/v1/auth/login", map[string]string{"username": "owner", "password": "wrong"}, nil, false)
	if code != 401 {
		t.Fatal("wrong password", code)
	}
	audit, e := r.store.Audit(context.Background(), 50, 0)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, row := range audit {
		if row.Op == "users.setup" {
			found = true
		}
	}
	if !found {
		t.Fatal("bootstrap was not audited")
	}
}

func TestLegacyAccountEnrollmentRequiresPersistentSourceGuard(t *testing.T) {
	r, c, key := accountRig(t)
	r.srv.Cfg.RequireSetup = false
	r.srv.Cfg.Token = ""
	code, b, _ := accountRequest(t, r, "GET", "/api/v1/auth/status", nil, nil, false)
	if code != 200 || !strings.Contains(string(b), `"setup_requires_config":true`) || strings.Contains(string(b), `"setup_available":true`) {
		t.Fatal(code, string(b))
	}
	code, b, _ = accountRequest(t, r, "POST", "/api/v1/auth/setup", accounts.Setup{Code: key, Username: "owner", Password: accountFixturePassword}, nil, true)
	if code != 409 || !strings.Contains(string(b), "setup_requires_config") {
		t.Fatal(code, string(b))
	}
	snap, e := c.Snapshot(context.Background())
	if e != nil || snap.Enabled {
		t.Fatal("guardless enrollment wrote accounts", e)
	}
}

func TestViewerChangesOnlyOwnPasswordAndRevokesAllCookies(t *testing.T) {
	r, c, key := accountRig(t)
	setupOwner(t, r, key)
	ctx := context.Background()
	snapshot, e := c.Snapshot(ctx)
	if e != nil {
		t.Fatal(e)
	}
	viewer, e := c.Save(ctx, accounts.Request{Revision: snapshot.Revision, Username: "reader", Role: domain.RoleViewer, Password: accountFixturePassword})
	if e != nil {
		t.Fatal(e)
	}
	cookie := accountLogin(t, r, "reader", accountFixturePassword)
	second := accountLogin(t, r, "reader", accountFixturePassword)
	next := "generated updated reader password"
	input := map[string]string{"current_password": "wrong current password", "password": next}
	code, _, _ := accountRequest(t, r, "POST", "/api/v1/auth/password", input, cookie, true)
	if code != 401 {
		t.Fatal("wrong current password", code)
	}
	input["current_password"] = accountFixturePassword
	input["user_id"] = "owner" // No caller-selected target is accepted.
	code, _, _ = accountRequest(t, r, "POST", "/api/v1/auth/password", input, cookie, true)
	if code != 400 {
		t.Fatal("accepted a password target", code)
	}
	delete(input, "user_id")
	input["password"] = ""
	code, _, _ = accountRequest(t, r, "POST", "/api/v1/auth/password", input, cookie, true)
	if code != 400 {
		t.Fatal("empty password silently retained", code)
	}
	input["password"] = next
	code, _, _ = accountRequest(t, r, "POST", "/api/v1/auth/password", input, cookie, false)
	if code != 400 {
		t.Fatal("unconfirmed password change", code)
	}
	code, b, _ := accountRequest(t, r, "POST", "/api/v1/auth/password", input, cookie, true)
	if code != 200 {
		t.Fatal("viewer own password denied", code, string(b))
	}
	for _, old := range []*http.Cookie{cookie, second} {
		code, _, _ = accountRequest(t, r, "GET", "/api/v1/files/roots", nil, old, false)
		if code != 401 {
			t.Fatal("old reader cookie survived", code)
		}
	}
	u, e := c.Authenticate(ctx, "reader", next)
	if e != nil || u.ID != viewer.ID || u.Role != domain.RoleViewer {
		t.Fatal("password change altered identity or role", u, e)
	}
	rows, e := r.store.Audit(ctx, 100, 0)
	if e != nil {
		t.Fatal(e)
	}
	encoded, _ := json.Marshal(rows)
	if strings.Contains(string(encoded), accountFixturePassword) || strings.Contains(string(encoded), next) {
		t.Fatal("password reached audit")
	}
}

func TestOwnPasswordCannotOverwriteConcurrentAdministratorRoleChange(t *testing.T) {
	r, c, key := accountRig(t)
	setupOwner(t, r, key)
	ctx := context.Background()
	snap, e := c.Snapshot(ctx)
	if e != nil {
		t.Fatal(e)
	}
	u, e := c.Save(ctx, accounts.Request{Revision: snap.Revision, Username: "member", Role: domain.RoleOperator, Password: accountFixturePassword})
	if e != nil {
		t.Fatal(e)
	}
	r.srv.Accounts = &passwordRoleRace{Control: c, change: func() error {
		snap, e := c.Snapshot(ctx)
		if e != nil {
			return e
		}
		_, e = c.Save(ctx, accounts.Request{Revision: snap.Revision, ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: domain.RoleViewer})
		return e
	}}
	input, _ := json.Marshal(map[string]string{"current_password": accountFixturePassword, "password": "generated concurrent password update"})
	req := httptest.NewRequest("POST", "/api/v1/auth/password", strings.NewReader(string(input)))
	req = req.WithContext(withPrincipal(ctx, accountPrincipal(u)))
	req.Header.Set("X-Nasctl-Confirm", "1")
	w := httptest.NewRecorder()
	r.srv.handlePassword(w, req)
	if w.Code != 409 {
		t.Fatal("stale password proof overwrote an account change", w.Code, w.Body.String())
	}
	current, e := c.Lookup(ctx, u.ID)
	if e != nil || current.Role != domain.RoleViewer {
		t.Fatal("administrator role change not preserved", current, e)
	}
	if _, e = c.Authenticate(ctx, u.Username, accountFixturePassword); e != nil {
		t.Fatal("rejected request still changed password", e)
	}
}

func TestViewerLibraryDoesNotDiscloseProviderCredentials(t *testing.T) {
	r := nativeRig(t, nil)
	ctx := context.Background()
	c, e := accountslocal.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close() })
	if e = c.EnsureSetupKey(ctx); e != nil {
		t.Fatal(e)
	}
	key, e := c.SetupKey(ctx)
	if e != nil {
		t.Fatal(e)
	}
	r.srv.Accounts = c
	r.srv.Cfg.RequireSetup = true
	_, owner := setupOwner(t, r, key)
	snap, e := c.Snapshot(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Save(ctx, accounts.Request{Revision: snap.Revision, Username: "viewer", Role: domain.RoleViewer, Password: accountFixturePassword}); e != nil {
		t.Fatal(e)
	}
	viewer := accountLogin(t, r, "viewer", accountFixturePassword)
	id, episodePath := seedPlayback(t, r)
	settings := r.srv.Anime.Settings()
	db, e := sql.Open("sqlite", filepath.Join(filepath.Dir(settings.LibraryDir), "state", "anime.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	secret := "generated-private-provider-passkey"
	if _, e = db.Exec("UPDATE subscriptions SET feed_url=?,last_error=?", "https://example.com/rss?passkey="+secret, "provider failure "+secret); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("UPDATE episodes SET url=?,error=?,hash=? WHERE id=?", "https://example.com/torrent?passkey="+secret, "provider failure "+secret, strings.Repeat("a", 40), id); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/api/v1/anime/subscriptions", "/api/v1/anime/episodes", episodePath + "/playback"} {
		code, b, _ := accountRequest(t, r, "GET", path, nil, viewer, false)
		if code != 200 || strings.Contains(string(b), secret) || strings.Contains(string(b), strings.Repeat("a", 40)) {
			t.Fatal("viewer provider disclosure", path, code, string(b))
		}
	}
	for _, path := range []string{"/api/v1/anime/settings", "/api/v1/anime/history/export", "/api/v1/anime/subscriptions/1/history", "/api/v1/anime/metadata/backups", "/api/v1/anime/metadata/search?q=fixture"} {
		code, _, _ := accountRequest(t, r, "GET", path, nil, viewer, false)
		if code != 403 {
			t.Fatal("viewer private management read", path, code)
		}
	}
	code, b, _ := accountRequest(t, r, "GET", "/api/v1/anime/subscriptions", nil, owner, false)
	if code != 200 || !strings.Contains(string(b), secret) {
		t.Fatal("viewer projection changed the owner's subscription data", code, string(b))
	}
}
func TestViewerOperatorRolesAndTaskOwnership(t *testing.T) {
	r, c, key := accountRig(t)
	_, adminCookie := setupOwner(t, r, key)
	ctx := context.Background()
	snap, _ := c.Snapshot(ctx)
	viewer, e := c.Save(ctx, accounts.Request{Revision: snap.Revision, Username: "reader", Role: "viewer", Password: accountFixturePassword})
	if e != nil {
		t.Fatal(e)
	}
	snap, _ = c.Snapshot(ctx)
	operator, e := c.Save(ctx, accounts.Request{Revision: snap.Revision, Username: "organizer", Role: "operator", Password: accountFixturePassword})
	if e != nil {
		t.Fatal(e)
	}
	viewerCookie := accountLogin(t, r, viewer.Username, accountFixturePassword)
	operatorCookie := accountLogin(t, r, operator.Username, accountFixturePassword)
	for _, path := range []string{"/api/v1/users", "/api/v1/settings", "/api/v1/audit", "/api/v1/backup/settings", "/api/v1/approvals"} {
		code, _, _ := accountRequest(t, r, "GET", path, nil, viewerCookie, false)
		if code != 403 {
			t.Fatal("viewer admin read", path, code)
		}
	}
	code, b, _ := accountRequest(t, r, "GET", "/api/v1/files/roots", nil, viewerCookie, false)
	var roots rootsResponse
	if e = json.Unmarshal(b, &roots); e != nil || code != 200 || roots.Writable || roots.Roots[0].Writable {
		t.Fatal("viewer advertised writable root", code, e)
	}
	code, _, _ = accountRequest(t, r, "POST", "/api/v1/files/mkdir", map[string]string{"root": "media", "path": "viewer-must-not-create"}, viewerCookie, true)
	if code != 403 {
		t.Fatal("viewer wrote files", code)
	}
	code, b, _ = accountRequest(t, r, "POST", "/api/v1/files/mkdir", map[string]string{"root": "media", "path": "operator-created"}, operatorCookie, true)
	if code != 200 {
		t.Fatal("operator file write rejected", code, string(b))
	}
	code, _, _ = accountRequest(t, r, "POST", "/api/v1/modes", map[string]string{"mode": "manual_pause"}, operatorCookie, true)
	if code != 403 {
		t.Fatal("operator changed global mode", code)
	}
	own, e := r.store.CreateJob(ctx, hubstore.NewJob{ID: "j-account-owned", Op: "files.copy", Principal: accountPrincipal(operator).String(), Target: "owned"})
	if e != nil {
		t.Fatal(e)
	}
	other, e := r.store.CreateJob(ctx, hubstore.NewJob{ID: "j-account-foreign", Op: "backup.run", Principal: "user:another-account#foreign", Target: "foreign"})
	if e != nil {
		t.Fatal(e)
	}
	stopped := false
	r.srv.registerActive(other.ID, func() { stopped = true })
	code, _, _ = accountRequest(t, r, "GET", "/api/v1/jobs/"+other.ID, nil, operatorCookie, false)
	if code != 404 {
		t.Fatal("operator read another user's job", code)
	}
	code, _, _ = accountRequest(t, r, "POST", "/api/v1/jobs/"+other.ID+"/cancel", nil, operatorCookie, true)
	if code != 404 || stopped {
		t.Fatal("operator cancelled another user's job", code, stopped)
	}
	code, b, _ = accountRequest(t, r, "GET", "/api/v1/jobs", nil, operatorCookie, false)
	if code != 200 || !strings.Contains(string(b), own.ID) || strings.Contains(string(b), other.ID) {
		t.Fatal("operator job list scope", code, string(b))
	}
	code, _, _ = accountRequest(t, r, "GET", "/api/v1/jobs/"+other.ID, nil, adminCookie, false)
	if code != 200 {
		t.Fatal("admin lost task visibility", code)
	}
	code, b, _ = accountRequest(t, r, "GET", "/api/v1/overview", nil, viewerCookie, false)
	if code != 200 || !strings.Contains(string(b), `"read_only":true`) || strings.Contains(string(b), other.ID) {
		t.Fatal("viewer overview leaked foreign job or writable status", code, string(b))
	}
}
func TestAccountRoleChangeRevokesSessionAndLastAdminCannotBeRemoved(t *testing.T) {
	r, c, key := accountRig(t)
	owner, adminCookie := setupOwner(t, r, key)
	ctx := context.Background()
	snap, _ := c.Snapshot(ctx)
	code, b, _ := accountRequest(t, r, "POST", "/api/v1/users", accounts.Request{Revision: snap.Revision, Username: "reader", Role: "viewer", Password: accountFixturePassword}, adminCookie, true)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	snap, _ = c.Snapshot(ctx)
	var reader accounts.User
	for _, u := range snap.Users {
		if u.Username == "reader" {
			reader = u
		}
	}
	cookie := accountLogin(t, r, "reader", accountFixturePassword)
	code, _, _ = accountRequest(t, r, "POST", "/api/v1/users", accounts.Request{Revision: snap.Revision, ID: reader.ID, Username: reader.Username, Role: "operator"}, adminCookie, true)
	if code != 200 {
		t.Fatal("role change", code)
	}
	code, _, _ = accountRequest(t, r, "GET", "/api/v1/files/roots", nil, cookie, false)
	if code != 401 {
		t.Fatal("old session survived role change", code)
	}
	snap, _ = c.Snapshot(ctx)
	code, _, _ = accountRequest(t, r, "DELETE", "/api/v1/users/"+owner.ID, map[string]string{"revision": snap.Revision, "confirm_username": owner.Username}, adminCookie, true)
	if code != 409 {
		t.Fatal("last admin removal", code)
	}
	audit, _ := r.store.Audit(ctx, 100, 0)
	found := false
	for _, row := range audit {
		if row.Op == "users.configure" {
			found = true
			b, _ := json.Marshal(row)
			if strings.Contains(string(b), accountFixturePassword) {
				t.Fatal("password in account audit")
			}
		}
	}
	if !found {
		t.Fatal("account changes were not audited")
	}
}
func TestFreshNASRequiresSetupBeforePrivateData(t *testing.T) {
	r, c, key := accountRig(t)
	_ = c
	r.srv.Cfg.RequireSetup = true
	code, b, _ := accountRequest(t, r, "GET", "/api/v1/auth/status", nil, nil, false)
	if code != 200 || !strings.Contains(string(b), `"setup_required":true`) || !strings.Contains(string(b), `"authenticated":false`) {
		t.Fatal("fresh setup state", code, string(b))
	}
	code, _, _ = accountRequest(t, r, "GET", "/api/v1/files/roots", nil, nil, false)
	if code != 401 {
		t.Fatal("fresh NAS exposed private data", code)
	}
	_, cookie := setupOwner(t, r, key)
	code, b, _ = accountRequest(t, r, "GET", "/api/v1/auth/status", nil, cookie, false)
	if code != 200 || strings.Contains(string(b), `"setup_required":true`) || !strings.Contains(string(b), `"authenticated":true`) {
		t.Fatal("setup did not unlock owner session", code, string(b))
	}
}
