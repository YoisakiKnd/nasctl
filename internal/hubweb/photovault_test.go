package hubweb

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/photovault"
)

type vaultNoopMover struct{}

func (vaultNoopMover) MoveToVault(context.Context, *photovault.Store, string, string, int64, album.Details) (string, error) {
	return "", nil
}
func TestVaultHTTPPasswordCookieCSRFRoleAndRateBounds(t *testing.T) {
	r := newRig(t, nil)
	vault, e := photovault.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer vault.Close()
	r.srv.PhotoVault = vault
	r.srv.VaultMover = vaultNoopMover{}
	jar, _ := cookiejar.New(nil)
	r.client.Jar = jar
	request := func(method, path, body string, confirmed bool, extra map[string]string) (int, string, *http.Response) {
		t.Helper()
		req, _ := http.NewRequest(method, r.ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if confirmed {
			req.Header.Set("X-Nasctl-Confirm", "1")
		}
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		res, e := r.client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b), res
	}
	if code, _, _ := request("GET", "/api/v1/album/vault/media", "", false, nil); code != 403 {
		t.Fatal("locked list", code)
	}
	if code, _, _ := request("POST", "/api/v1/album/vault/create", `{"password":"disposable test password"}`, false, nil); code != 400 {
		t.Fatal("missing consent", code)
	}
	if code, _, _ := request("POST", "/api/v1/album/vault/create", `{"password":"disposable test password"}`, true, map[string]string{"Origin": "https://other.example"}); code != 403 {
		t.Fatal("CSRF", code)
	}
	if code, _, _ := request("POST", "/api/v1/album/vault/create", `{"password":"short"}`, true, nil); code != 400 {
		t.Fatal("weak password", code)
	}
	if code, _, _ := request("POST", "/api/v1/album/vault/create", `{"password":"disposable test password"}`, true, nil); code != 200 {
		t.Fatal("create", code)
	}
	code, _, res := request("POST", "/api/v1/album/vault/unlock", `{"password":"disposable test password"}`, true, nil)
	if code != 200 {
		t.Fatal("unlock", code)
	}
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == vaultCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.Path != "/api/v1" || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != 0 {
		t.Fatal("cookie protections", cookie)
	}
	if code, body, _ := request("GET", "/api/v1/album/vault/media", "", false, nil); code != 200 || strings.Contains(body, "password") {
		t.Fatal("unlocked list", code, body)
	}
	if code, _, _ := request("GET", "/api/v1/album/vault/media", "", false, map[string]string{"Cookie": vaultCookie + "=" + cookie.Value + "; " + sessionCookie + "=another-session"}); code != 403 {
		t.Fatal("vault cookie accepted under another NAS session", code)
	}
	if code, _, _ := request("GET", "/api/v1/album/vault/media", "", false, map[string]string{"X-Nasctl-Agent": "helper"}); code != 403 {
		t.Fatal("AI access", code)
	}
	if code, _, _ := request("POST", "/api/v1/album/vault/lock", "", true, nil); code != 200 {
		t.Fatal("lock", code)
	}
	if code, _, _ := request("GET", "/api/v1/album/vault/media", "", false, nil); code != 403 {
		t.Fatal("revocation", code)
	}
	for i := 0; i < 10; i++ {
		code, _, _ = request("POST", "/api/v1/album/vault/unlock", `{"password":"wrong"}`, true, nil)
		if code != 403 && code != 429 {
			t.Fatal("failure", code)
		}
	}
	if code, _, _ := request("POST", "/api/v1/album/vault/unlock", `{"password":"disposable test password"}`, true, nil); code != 429 {
		t.Fatal("brute-force bound", code)
	}
}
func TestVaultCreationRespectsReadOnlyConsole(t *testing.T) {
	r := newRig(t, func(_ *Config, j *jobs.Config) {
		if j != nil {
			j.ReadOnlyConsole = true
		}
	})
	v, e := photovault.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer v.Close()
	r.srv.PhotoVault = v
	r.srv.VaultMover = vaultNoopMover{}
	code, _ := fileRequest(t, r, "POST", "/api/v1/album/vault/create", `{"password":"disposable test password"}`, "", "", true)
	if code != 403 || v.Status("", "").Configured {
		t.Fatal("readonly created vault", code)
	}
}
