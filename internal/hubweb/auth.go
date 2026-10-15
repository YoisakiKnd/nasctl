package hubweb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/domain"
)

const sessionCookie = "nasctl_session"
const sessionTTL = 12 * time.Hour

// Browser sessions contain random credentials, never the configured API token.
// Both tables are bounded. Restarting the process invalidates all sessions.
type browserAuth struct {
	mu       sync.Mutex
	sessions map[[32]byte]time.Time
	failures map[string]loginFailures
	users    map[[32]byte]userSession
}
type userSession struct{ ID, Epoch string }
type loginFailures struct {
	since time.Time
	count int
}

func publicRoute(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		switch r.URL.Path {
		case "/", "/index.html", "/app.js", "/app.css", "/api/v1/auth/status":
			return true
		}
	case http.MethodPost:
		return r.URL.Path == "/api/v1/auth/login" || r.URL.Path == "/api/v1/auth/logout" || r.URL.Path == "/api/v1/auth/setup"
	}
	return false
}

func (a *browserAuth) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || len(cookie.Value) != 43 {
		return false
	}
	key := sha256.Sum256([]byte(cookie.Value))
	a.mu.Lock()
	defer a.mu.Unlock()
	expires, ok := a.sessions[key]
	if !ok {
		return false
	}
	if !time.Now().Before(expires) {
		delete(a.sessions, key)
		delete(a.users, key)
		return false
	}
	return true
}

func (a *browserAuth) create(now time.Time) (string, error) {
	return a.createSession(now, nil)
}
func (a *browserAuth) createSession(now time.Time, user *userSession) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	value := base64.RawURLEncoding.EncodeToString(raw[:])
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sessions == nil {
		a.sessions = make(map[[32]byte]time.Time)
	}
	for key, expires := range a.sessions {
		if !now.Before(expires) {
			delete(a.sessions, key)
			delete(a.users, key)
		}
	}
	// Least remaining lifetime is the oldest session; make room without growth.
	if len(a.sessions) >= 64 {
		var oldest [32]byte
		var deadline time.Time
		for key, expires := range a.sessions {
			if deadline.IsZero() || expires.Before(deadline) {
				oldest, deadline = key, expires
			}
		}
		delete(a.sessions, oldest)
		delete(a.users, oldest)
	}
	key := sha256.Sum256([]byte(value))
	a.sessions[key] = now.Add(sessionTTL)
	if user != nil {
		if a.users == nil {
			a.users = map[[32]byte]userSession{}
		}
		a.users[key] = *user
	}
	return value, nil
}

func loginPeer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	// Never trust caller-supplied Forwarded / X-Forwarded-For for limits.
	return host
}
func (a *browserAuth) limited(peer string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.failures[peer]
	return f.count >= 10 && now.Sub(f.since) < time.Minute
}
func (a *browserAuth) failed(peer string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failures == nil {
		a.failures = make(map[string]loginFailures)
	}
	for key, f := range a.failures {
		if now.Sub(f.since) >= time.Minute {
			delete(a.failures, key)
		}
	}
	if _, found := a.failures[peer]; !found && len(a.failures) >= 256 {
		// Fixed memory even if the server receives many distinct clients.
		var oldest string
		var since time.Time
		for key, f := range a.failures {
			if since.IsZero() || f.since.Before(since) {
				oldest, since = key, f.since
			}
		}
		delete(a.failures, oldest)
	}
	f := a.failures[peer]
	if f.since.IsZero() {
		f.since = now
	}
	f.count++
	a.failures[peer] = f
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	state, err := s.accountStatus(r.Context())
	if err != nil {
		writeError(w, 503, "accounts_unavailable", "账号状态不可读，请在 NAS 本机检查或恢复账号文件")
		return
	}
	p, ok := s.authenticate(r)
	response := map[string]any{"required": s.Cfg.Token != "" || len(s.Cfg.Agents) > 0 || state.Enabled, "login_available": s.Cfg.Token != "" || state.Enabled, "authenticated": ok, "accounts_enabled": state.Enabled, "setup_available": state.SetupAvailable, "token_login_available": s.Cfg.Token != ""}
	response["setup_required"] = s.Cfg.RequireSetup && !state.Enabled && s.Cfg.Token == ""
	if !state.Enabled && !s.Cfg.RequireSetup && s.Cfg.Token == "" {
		response["setup_available"] = false
		response["setup_requires_config"] = true
	}
	if s.Cfg.RequireSetup {
		response["required"] = true
	}
	if ok {
		response["principal"] = p.Kind
		response["name"] = p.Name
		response["role"] = p.Role
		response["user_id"] = p.UserID
		response["can_manage_device"] = p.DeviceAdmin()
		response["read_only_user"] = p.ReadOnlyUser()
		if p.UserID != "" && s.Accounts != nil {
			if u, e := s.Accounts.Lookup(r.Context(), p.UserID); e == nil {
				response["display_name"] = u.DisplayName
			}
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if principalOf(r.Context()).Kind == domain.PrincipalAI {
		writeError(w, 403, "denied", "程序访问凭据不能创建操作者会话")
		return
	}
	state, stateErr := s.accountStatus(r.Context())
	if stateErr != nil {
		writeError(w, 503, "accounts_unavailable", "账号状态不可读，请在本机恢复")
		return
	}
	if s.Cfg.Token == "" && !state.Enabled {
		writeError(w, 403, "login_unavailable", "尚未配置可用账号或访问令牌")
		return
	}
	now := time.Now()
	peer := loginPeer(r)
	if s.auth.limited(peer, now) {
		w.Header().Set("Retry-After", "60")
		writeError(w, 429, "rate_limited", "尝试次数过多，请一分钟后重试")
		return
	}
	var input struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		writeError(w, 400, "bad_request", "请输入访问令牌")
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeError(w, 400, "bad_request", "登录请求必须是单个 JSON 对象")
		return
	}
	var value string
	var err error
	if input.Username != "" {
		if input.Token != "" || s.Accounts == nil || !state.Enabled {
			writeError(w, 400, "bad_request", "请选择账号或令牌中的一种登录方式")
			return
		}
		ctx, stop := context.WithTimeout(r.Context(), 15*time.Second)
		defer stop()
		user, e := s.Accounts.Authenticate(ctx, input.Username, input.Password)
		if e != nil {
			if errors.Is(e, accounts.ErrBusy) {
				w.Header().Set("Retry-After", "1")
				writeError(w, 429, "login_busy", "设备正在验证登录，请稍后重试")
				return
			}
			s.auth.failed(peer, now)
			writeError(w, 401, "invalid_credentials", "用户名或密码不正确")
			return
		}
		value, err = s.auth.createUser(now, user)
	} else {
		if input.Password != "" {
			writeError(w, 400, "bad_request", "请输入用户名")
			return
		}
		valid := input.Token != "" && s.Cfg.Token != "" && subtleEqual(input.Token, s.Cfg.Token)
		for _, agent := range s.Cfg.Agents {
			if agent.Token != "" && subtleEqual(input.Token, agent.Token) {
				valid = false
			}
		}
		if !valid {
			s.auth.failed(peer, now)
			writeError(w, 401, "invalid_token", "访问令牌不正确")
			return
		}
		value, err = s.auth.create(now)
	}
	if err != nil {
		s.fail(w, 500, "internal_error", err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: int(sessionTTL.Seconds()), Expires: now.Add(sessionTTL)})
	clearLegacyCookie(w, r)
	writeJSON(w, 200, map[string]any{"authenticated": true, "expires_at": now.Add(sessionTTL)})
}

func clearLegacyCookie(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie("nasctl_token"); err == nil {
		http.SetCookie(w, &http.Cookie{Name: "nasctl_token", Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: -1, Expires: time.Unix(1, 0)})
	}
}
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.auth.mu.Lock()
		delete(s.auth.sessions, sha256.Sum256([]byte(c.Value)))
		delete(s.auth.users, sha256.Sum256([]byte(c.Value)))
		s.auth.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: -1, Expires: time.Unix(1, 0)})
	clearLegacyCookie(w, r)
	writeJSON(w, 200, map[string]bool{"authenticated": false})
}

func sessionPrincipal() domain.Principal {
	return domain.Principal{Kind: domain.PrincipalUser, Name: "operator"}
}
