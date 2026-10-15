package hubweb

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func authRequest(h http.Handler, method, path, body string, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://nas.local"+path, strings.NewReader(body))
	if cookie != nil {
		r.AddCookie(cookie)
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func loginCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	w := authRequest(h, "POST", "/api/v1/auth/login", `{"token":"operator-secret"}`, nil, nil)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie
		}
	}
	t.Fatal("missing session cookie")
	return nil
}

func TestBrowserLoginLogoutAndRevocation(t *testing.T) {
	s := New(Config{Token: "operator-secret", Agents: []AgentCredential{{Name: "agent", Token: "agent-secret"}}}, nil, nil, nil, nil)
	h := s.Handler()
	for _, path := range []string{"/", "/index.html", "/app.js", "/app.css", "/api/v1/auth/status"} {
		w := authRequest(h, "GET", path, "", nil, nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), "operator-secret") {
			t.Fatalf("public shell %s: %d", path, w.Code)
		}
	}
	w := authRequest(h, "GET", "/api/v1/anime/status", "", nil, nil)
	if w.Code != 401 {
		t.Fatalf("private read without login: %d", w.Code)
	}
	cookie := loginCookie(t, h)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.MaxAge != 43200 || strings.Contains(cookie.Value, "operator-secret") {
		t.Fatalf("unsafe cookie: %+v", cookie)
	}
	w = authRequest(h, "GET", "/api/v1/auth/status", "", cookie, nil)
	var status struct {
		Authenticated bool   `json:"authenticated"`
		Principal     string `json:"principal"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || !status.Authenticated || status.Principal != "user" {
		t.Fatalf("status: %s %v", w.Body.String(), err)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("auth status is cacheable")
	}
	if w := authRequest(h, "GET", "/api/v1/anime/status", "", cookie, nil); w.Code != 200 {
		t.Fatalf("cookie not usable: %d", w.Code)
	}
	for _, bearer := range []string{"Bearer wrong", "Bearer ", "Basic invalid"} {
		if w := authRequest(h, "GET", "/api/v1/anime/status", "", cookie, map[string]string{"Authorization": bearer}); w.Code != 401 {
			t.Fatalf("invalid explicit auth accepted: %q %d", bearer, w.Code)
		}
	}
	w = authRequest(h, "POST", "/api/v1/auth/logout", "", cookie, nil)
	if w.Code != 200 {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := authRequest(h, "GET", "/api/v1/anime/status", "", cookie, nil); w.Code != 401 {
		t.Fatal("copied session credential remains usable after logout")
	}
	// API and agent credentials keep their original identities.
	if w := authRequest(h, "GET", "/api/v1/anime/status", "", nil, map[string]string{"Authorization": "Bearer operator-secret"}); w.Code != 200 {
		t.Fatal("bearer compatibility lost")
	}
	w = authRequest(h, "GET", "/api/v1/auth/status", "", nil, map[string]string{"Authorization": "Bearer agent-secret"})
	if !strings.Contains(w.Body.String(), `"principal":"ai"`) {
		t.Fatalf("agent identity: %s", w.Body.String())
	}
}

func TestBrowserLoginRefusesAgentsCrossSiteAndMalformedBodies(t *testing.T) {
	s := New(Config{Token: "operator-secret", Agents: []AgentCredential{{Name: "agent", Token: "agent-secret"}}}, nil, nil, nil, nil)
	h := s.Handler()
	if w := authRequest(h, "POST", "/api/v1/auth/login", `{"token":"operator-secret"}`, nil, map[string]string{"Authorization": "Bearer agent-secret"}); w.Code != 403 {
		t.Fatal("explicit agent created operator session")
	}
	for _, body := range []string{`{"token":"agent-secret"}`, `{"token":"wrong"}`} {
		w := authRequest(h, "POST", "/api/v1/auth/login", body, nil, nil)
		if w.Code != 401 || len(w.Result().Cookies()) != 0 {
			t.Fatalf("bad credential login: %d", w.Code)
		}
	}
	for _, headers := range []map[string]string{{"Origin": "https://evil.test"}, {"Sec-Fetch-Site": "cross-site"}} {
		for _, endpoint := range []string{"login", "logout"} {
			w := authRequest(h, "POST", "/api/v1/auth/"+endpoint, `{"token":"operator-secret"}`, nil, headers)
			if w.Code != 403 || len(w.Result().Cookies()) != 0 {
				t.Fatalf("cross-site %s: %d", endpoint, w.Code)
			}
		}
	}
	for _, body := range []string{`null`, `{"token":"operator-secret","extra":1}`, `{"token":"operator-secret"} {}`, `{"token":"` + strings.Repeat("x", 5000) + `"}`} {
		w := authRequest(h, "POST", "/api/v1/auth/login", body, nil, nil)
		if w.Code < 400 || len(w.Result().Cookies()) != 0 {
			t.Fatalf("malformed login accepted: %d", w.Code)
		}
	}
	// A valid same-origin request still works after refused cross-site attempts.
	if w := authRequest(h, "POST", "/api/v1/auth/login", `{"token":"operator-secret"}`, nil, map[string]string{"Origin": "http://nas.local"}); w.Code != 200 {
		t.Fatalf("same origin: %d", w.Code)
	}
}

func TestBrowserSessionExpiryRestartBoundsAndTLS(t *testing.T) {
	s := New(Config{Token: "operator-secret"}, nil, nil, nil, nil)
	h := s.Handler()
	cookie := loginCookie(t, h)
	s.auth.mu.Lock()
	s.auth.sessions[sha256.Sum256([]byte(cookie.Value))] = time.Now().Add(-time.Second)
	s.auth.mu.Unlock()
	if w := authRequest(h, "GET", "/api/v1/anime/status", "", cookie, nil); w.Code != 401 {
		t.Fatal("expired session accepted")
	}
	cookie = loginCookie(t, h)
	other := New(Config{Token: "operator-secret"}, nil, nil, nil, nil)
	if w := authRequest(other.Handler(), "GET", "/api/v1/anime/status", "", cookie, nil); w.Code != 401 {
		t.Fatal("session survives restart")
	}
	cookie.Value = cookie.Value[:42] + "!"
	if w := authRequest(h, "GET", "/api/v1/anime/status", "", cookie, nil); w.Code != 401 {
		t.Fatal("tampered session accepted")
	}
	now := time.Now()
	for i := 0; i < 100; i++ {
		if _, err := s.auth.create(now.Add(time.Duration(i) * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.auth.sessions) != 64 {
		t.Fatalf("session table not bounded: %d", len(s.auth.sessions))
	}
	for i := 0; i < 300; i++ {
		s.auth.failed(fmt.Sprint(i), now)
	}
	if len(s.auth.failures) != 256 {
		t.Fatal("login failure table not bounded")
	}
	r := httptest.NewRequest("POST", "https://nas.local/api/v1/auth/login", strings.NewReader(`{"token":"operator-secret"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !w.Result().Cookies()[0].Secure {
		t.Fatal("HTTPS session cookie missing Secure")
	}
}

func TestBrowserLoginRateLimitAndUnauthenticatedMode(t *testing.T) {
	s := New(Config{Token: "operator-secret"}, nil, nil, nil, nil)
	h := s.Handler()
	for i := 0; i < 10; i++ {
		if w := authRequest(h, "POST", "/api/v1/auth/login", `{"token":"wrong"}`, nil, nil); w.Code != 401 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := authRequest(h, "POST", "/api/v1/auth/login", `{"token":"operator-secret"}`, nil, map[string]string{"X-Forwarded-For": "198.51.100.10"})
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("missing rate limit: %d", w.Code)
	}
	s.auth.failed("old", time.Now().Add(-2*time.Minute))
	if s.auth.limited("old", time.Now()) {
		t.Fatal("old failures never expire")
	}
	for _, cfg := range []Config{{}, {Agents: []AgentCredential{{Name: "agent", Token: "agent-secret"}}}} {
		h := New(cfg, nil, nil, nil, nil).Handler()
		w := authRequest(h, "POST", "/api/v1/auth/login", `{"token":"anything"}`, nil, nil)
		if w.Code != 403 {
			t.Fatalf("no operator token: %d", w.Code)
		}
		w = authRequest(h, "GET", "/api/v1/auth/status", "", nil, nil)
		if len(cfg.Agents) == 0 && !strings.Contains(w.Body.String(), `"authenticated":true`) {
			t.Fatal("local no-token mode broken")
		}
	}
}
