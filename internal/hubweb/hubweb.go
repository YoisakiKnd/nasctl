// Package hubweb is the console's unified web layer: one page, one set of design
// tokens, one navigation, and one HTTP API that every part of the UI is built
// from.
//
// File management is read-only unless a root explicitly enables operator writes.
// Native anime work is explicit, bounded
// and tracked through the job layer. Requests never start a scan, decode images
// or launch external programs; qBittorrent performs downloads independently.
//
// The boundary is enforced by the package's import graph and checked by a test:
// nothing here imports a decoder, the scanner or the thumbnail generator. That is
// what makes "an HTTP request cannot start a full-library scan" a property of the
// build rather than a promise in a document.
package hubweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/anime"
	"github.com/YoisakiKnd/nasctl/internal/backups"
	"github.com/YoisakiKnd/nasctl/internal/buildinfo"
	"github.com/YoisakiKnd/nasctl/internal/cloudbackup"
	"github.com/YoisakiKnd/nasctl/internal/desktopapps"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
	"github.com/YoisakiKnd/nasctl/internal/photovault"
	"github.com/YoisakiKnd/nasctl/internal/settings"
	"github.com/YoisakiKnd/nasctl/internal/suzu"
	"github.com/YoisakiKnd/nasctl/internal/sysinfo"
)

// Config wires the web layer.
type Config struct {
	// Bind is the listen address. It is validated at startup, not per request.
	Bind string
	// Token, when set, is required as a bearer token or cookie. Empty means the
	// bind address is the only access control, which is why CheckBind refuses a
	// public address.
	Token        string
	RequireSetup bool
	// Agents maps a credential to the agent it identifies. This is what makes the
	// AI distinction enforceable: the credential decides the principal, so an
	// agent cannot escape its restrictions by not announcing itself.
	Agents []AgentCredential

	MaxConcurrentRequests int
	MaxConcurrentStreams  int
	MaxResponseBytes      int64

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// PageDefault and PageMax bound every listing.
	PageDefault int
	PageMax     int
	// PreviewMaxBytes caps what the preview endpoint will pass through.
	PreviewMaxBytes int64
}

// AgentCredential is one agent's identity and token.
type AgentCredential struct {
	Name       string
	Token      string
	AllowHeavy bool
}

// DefaultConfig returns the shipped bounds. They are deliberately small: this
// console runs on a machine with 981 MiB of memory and a handful of file
// descriptors to spare.
func DefaultConfig() Config {
	return Config{
		Bind:                  "127.0.0.1:8090",
		MaxConcurrentRequests: 8,
		MaxConcurrentStreams:  3,
		MaxResponseBytes:      2 << 20,
		ReadHeaderTimeout:     10 * time.Second,
		ReadTimeout:           30 * time.Second,
		WriteTimeout:          60 * time.Second,
		IdleTimeout:           60 * time.Second,
		PageDefault:           50,
		PageMax:               200,
		PreviewMaxBytes:       32 << 20,
	}
}

// Server is the console's HTTP service.
type Server struct {
	Cfg          Config
	Files        *files.Manager
	Jobs         *jobs.Manager
	Suzu         *suzu.Client
	Log          *slog.Logger
	Version      string
	Anime        *anime.Engine
	Album        *album.Client
	AlbumControl album.Control
	PhotoVault   *photovault.Store
	VaultMover   photovault.Mover
	vaultAuth    browserAuth
	CloudBackups cloudbackup.Control
	Apps         desktopapps.Control
	Wallpaper    Wallpapers
	System       *sysinfo.Sampler
	Storage      StorageHelper
	Backups      backups.Control
	Settings     settings.Control
	Accounts     accounts.Control
	WorkContext  context.Context
	work         sync.WaitGroup
	workMu       sync.Mutex
	workStopping bool
	activeWork   map[string]context.CancelFunc

	// requestSlots bounds concurrent requests; streamSlots bounds concurrent
	// file transfers. Two limits rather than one, because a slow download must
	// not consume the slots that keep the UI responsive.
	requestSlots chan struct{}
	streamSlots  chan struct{}

	// previews caches small derived facts (not file contents) to avoid
	// re-stat'ing on every thumbnail request.
	mu   sync.Mutex
	auth browserAuth
}

// New builds the server.
//
// The suzu client is a parameter rather than a field set afterwards so that a
// caller cannot forget it: the first version opened the integration in the
// serving command but never handed it over, so the CLI reported the library while
// the web page said "not configured" - a discrepancy that is invisible until
// somebody compares the two.
func New(cfg Config, fm *files.Manager, jm *jobs.Manager, sc *suzu.Client, log *slog.Logger) *Server {
	d := DefaultConfig()
	if cfg.Bind == "" {
		cfg.Bind = d.Bind
	}
	if cfg.MaxConcurrentRequests <= 0 {
		cfg.MaxConcurrentRequests = d.MaxConcurrentRequests
	}
	if cfg.MaxConcurrentStreams <= 0 {
		cfg.MaxConcurrentStreams = d.MaxConcurrentStreams
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = d.MaxResponseBytes
	}
	if cfg.ReadHeaderTimeout <= 0 {
		cfg.ReadHeaderTimeout = d.ReadHeaderTimeout
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = d.ReadTimeout
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = d.WriteTimeout
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = d.IdleTimeout
	}
	if cfg.PageDefault <= 0 {
		cfg.PageDefault = d.PageDefault
	}
	if cfg.PageMax <= 0 {
		cfg.PageMax = d.PageMax
	}
	if cfg.PreviewMaxBytes <= 0 {
		cfg.PreviewMaxBytes = d.PreviewMaxBytes
	}
	return &Server{System: sysinfo.New(),
		Cfg:          cfg,
		Files:        fm,
		Jobs:         jm,
		Suzu:         sc,
		Log:          log,
		Version:      buildinfo.Version,
		requestSlots: make(chan struct{}, cfg.MaxConcurrentRequests),
		streamSlots:  make(chan struct{}, cfg.MaxConcurrentStreams),
	}
}

// Handler returns the routed handler with the middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Pages. The console is a single-page application: one shell, and the
	// fragments the UI fetches are data, not markup.
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /app.css", s.handleStatic)
	mux.HandleFunc("GET /app.js", s.handleStatic)
	mux.HandleFunc("GET /manifest.webmanifest", s.handleStatic)
	mux.HandleFunc("GET /icon-192.png", s.handleStatic)
	mux.HandleFunc("GET /icon-512.png", s.handleStatic)
	mux.HandleFunc("GET /icon-maskable.png", s.handleStatic)
	mux.HandleFunc("GET /apple-touch-icon.png", s.handleStatic)
	mux.HandleFunc("GET /api/v1/auth/status", s.handleAuthStatus)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	mux.HandleFunc("POST /api/v1/auth/setup", s.handleAccountSetup)
	mux.HandleFunc("POST /api/v1/auth/password", s.handlePassword)
	mux.HandleFunc("GET /api/v1/users", s.handleUsers)
	mux.HandleFunc("POST /api/v1/users", s.handleUsers)
	mux.HandleFunc("DELETE /api/v1/users/{id}", s.handleUsers)

	// API: reads.
	mux.HandleFunc("GET /api/v1/apps", s.handleApps)
	mux.HandleFunc("PUT /api/v1/apps", s.handleApps)
	mux.HandleFunc("GET /api/v1/overview", s.handleOverview)
	mux.HandleFunc("GET /api/v1/system/status", s.handleSystemStatus)
	mux.HandleFunc("GET /api/v1/desktop/wallpaper", s.handleWallpaper)
	mux.HandleFunc("PUT /api/v1/desktop/wallpaper", s.handleWallpaper)
	mux.HandleFunc("DELETE /api/v1/desktop/wallpaper", s.handleWallpaper)
	mux.HandleFunc("GET /api/v1/storage", s.handleStorage)
	mux.HandleFunc("GET /api/v1/storage/smart", s.handleStorageSmart)
	mux.HandleFunc("POST /api/v1/storage/{action}", s.handleStorageAction)
	mux.HandleFunc("GET /api/v1/ops", s.handleOps)
	mux.HandleFunc("GET /api/v1/audit", s.handleAudit)
	mux.HandleFunc("GET /api/v1/modes", s.handleModes)
	mux.HandleFunc("GET /api/v1/approvals", s.handleApprovals)
	mux.HandleFunc("GET /api/v1/settings", s.handleSettings)
	mux.HandleFunc("POST /api/v1/settings/validate", s.handleSettings)
	mux.HandleFunc("POST /api/v1/settings", s.handleSettings)

	mux.HandleFunc("GET /api/v1/files/roots", s.handleRoots)
	mux.HandleFunc("GET /api/v1/files/list", s.handleFileList)
	mux.HandleFunc("GET /api/v1/files/stat", s.handleFileStat)
	mux.HandleFunc("GET /api/v1/files/search", s.handleFileSearch)
	mux.HandleFunc("GET /api/v1/files/preview", s.handleFilePreview)
	mux.HandleFunc("GET /api/v1/files/text", s.handleFileText)
	mux.HandleFunc("PUT /api/v1/files/text", s.handleFileTextSave)
	mux.HandleFunc("GET /api/v1/files/download", s.handleFileDownload)
	mux.HandleFunc("HEAD /api/v1/files/download", s.handleFileDownload)
	mux.HandleFunc("GET /api/v1/files/archive", s.handleFolderDownload)
	mux.HandleFunc("HEAD /api/v1/files/archive", s.handleFolderDownload)
	mux.HandleFunc("POST /api/v1/files/mkdir", s.handleFileMutation)
	mux.HandleFunc("POST /api/v1/files/rename", s.handleFileMutation)
	mux.HandleFunc("POST /api/v1/files/trash", s.handleFileMutation)
	mux.HandleFunc("POST /api/v1/files/restore", s.handleFileMutation)
	mux.HandleFunc("GET /api/v1/files/trash", s.handleFileTrashList)
	mux.HandleFunc("PUT /api/v1/files/upload", s.handleFileUpload)
	mux.HandleFunc("POST /api/v1/files/copy", s.handleFileBackground)
	mux.HandleFunc("POST /api/v1/files/move", s.handleFileBackground)
	mux.HandleFunc("POST /api/v1/files/purge", s.handleFileBackground)
	mux.HandleFunc("POST /api/v1/files/batch", s.handleFileBatch)
	mux.HandleFunc("GET /api/v1/backup/webdav", s.handleCloudSettings)
	mux.HandleFunc("PUT /api/v1/backup/webdav", s.handleCloudSettings)
	mux.HandleFunc("POST /api/v1/backup/webdav/probe", s.handleCloudProbe)
	mux.HandleFunc("POST /api/v1/backup/webdav/{action}", s.handleCloudWork)
	mux.HandleFunc("GET /api/v1/backup/webdav/{id}/download", s.handleCloudDownload)
	mux.HandleFunc("HEAD /api/v1/backup/webdav/{id}/download", s.handleCloudDownload)
	mux.HandleFunc("GET /api/v1/backup/settings", s.handleBackupSettings)
	mux.HandleFunc("POST /api/v1/backup/plans", s.handleBackupPlan)
	mux.HandleFunc("DELETE /api/v1/backup/plans/{id}", s.handleBackupPlan)
	mux.HandleFunc("GET /api/v1/backup/history", s.handleBackupHistory)
	mux.HandleFunc("POST /api/v1/backup/run", s.handleBackupWork)
	mux.HandleFunc("POST /api/v1/backup/verify", s.handleBackupWork)
	mux.HandleFunc("POST /api/v1/backup/restore", s.handleBackupWork)
	mux.HandleFunc("POST /api/v1/backup/delete", s.handleBackupWork)
	mux.HandleFunc("POST /api/v1/backup/cleanup", s.handleBackupWork)
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.handleCancelWork)

	mux.HandleFunc("GET /api/v1/jobs", s.handleJobs)
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.handleJob)

	mux.HandleFunc("GET /api/v1/media/status", s.handleMediaStatus)
	mux.HandleFunc("GET /api/v1/album/vault/status", s.handleVaultStatus)
	mux.HandleFunc("POST /api/v1/album/vault/{operation}", s.handleVaultPassword)
	mux.HandleFunc("POST /api/v1/album/vault/move", s.handleVaultMove)
	mux.HandleFunc("GET /api/v1/album/vault/media", s.handleVaultList)
	mux.HandleFunc("GET /api/v1/album/vault/media/{id}", s.handleVaultDetails)
	mux.HandleFunc("GET /api/v1/album/vault/media/{id}/{part}", s.handleVaultStream)
	mux.HandleFunc("GET /api/v1/album/status", s.handleAlbumStatus)
	mux.HandleFunc("GET /api/v1/album/settings", s.handleAlbumSettings)
	mux.HandleFunc("POST /api/v1/album/directories", s.handleAlbumDirectory)
	mux.HandleFunc("DELETE /api/v1/album/directories/{code}", s.handleAlbumDirectory)
	mux.HandleFunc("POST /api/v1/album/tasks/{operation}", s.handleAlbumTask)
	mux.HandleFunc("GET /api/v1/album/collections", s.handleAlbumCollections)
	mux.HandleFunc("GET /api/v1/album/media", s.handleAlbumList)
	mux.HandleFunc("GET /api/v1/album/media/{id}", s.handleAlbumDetails)
	mux.HandleFunc("GET /api/v1/album/media/{id}/{part}", s.handleAlbumStream)
	mux.HandleFunc("GET /api/v1/album/private/collections", s.handleAlbumCollections)
	mux.HandleFunc("GET /api/v1/album/private/media", s.handleAlbumList)
	mux.HandleFunc("GET /api/v1/album/private/media/{id}", s.handleAlbumDetails)
	mux.HandleFunc("GET /api/v1/album/private/media/{id}/{part}", s.handleAlbumStream)
	mux.HandleFunc("GET /api/v1/anime/status", s.handleAnimeStatus)
	mux.HandleFunc("GET /api/v1/anime/settings", s.handleAnimeSettings)
	mux.HandleFunc("GET /api/v1/anime/history/export", s.handleAnimeHistoryExport)
	mux.HandleFunc("GET /api/v1/anime/subscriptions/{id}/history", s.handleAnimeHistoryStatus)
	mux.HandleFunc("POST /api/v1/anime/subscriptions/{id}/history/cleanup", s.handleAnimeHistoryCleanup)
	mux.HandleFunc("POST /api/v1/anime/subscriptions/{id}/metadata", s.handleAnimeMetadataRefresh)
	mux.HandleFunc("GET /api/v1/anime/metadata/backups", s.handleAnimeMetadataBackups)
	mux.HandleFunc("GET /api/v1/anime/metadata/backups/{id}/download", s.handleAnimeMetadataBackupExport)
	mux.HandleFunc("POST /api/v1/anime/metadata/backups/{id}/restore", s.handleAnimeMetadataRestore)
	mux.HandleFunc("DELETE /api/v1/anime/metadata/backups/{id}", s.handleAnimeMetadataBackupDelete)
	mux.HandleFunc("POST /api/v1/anime/preview", s.handleAnimePreview)
	mux.HandleFunc("POST /api/v1/anime/connection", s.handleAnimeConnection)
	mux.HandleFunc("POST /api/v1/anime/sync", s.handleAnimeSync)
	mux.HandleFunc("POST /api/v1/anime/episodes/{id}/ignore", s.handleAnimeIgnore)
	mux.HandleFunc("POST /api/v1/anime/episodes/{id}/download", s.handleAnimeDownloadControl)
	mux.HandleFunc("GET /api/v1/anime/subscriptions", s.handleAnimeSubscriptions)
	mux.HandleFunc("POST /api/v1/anime/subscriptions", s.handleSaveAnimeSubscription)
	mux.HandleFunc("PUT /api/v1/anime/subscriptions/{id}", s.handleSaveAnimeSubscription)
	mux.HandleFunc("POST /api/v1/anime/subscriptions/{id}/enabled", s.handleAnimeSubscriptionState)
	mux.HandleFunc("POST /api/v1/anime/subscriptions/{id}/archive", s.handleAnimeSubscriptionState)
	mux.HandleFunc("GET /api/v1/anime/metadata/search", s.handleAnimeMetadataSearch)
	mux.HandleFunc("GET /api/v1/anime/metadata/series/{id}", s.handleAnimeMetadataDetails)
	mux.HandleFunc("GET /api/v1/anime/sources/search", s.handleAnimeSources)
	mux.HandleFunc("GET /api/v1/anime/sources/mikan/{id}", s.handleMikanSource)
	mux.HandleFunc("GET /api/v1/anime/episodes", s.handleAnimeEpisodes)
	mux.HandleFunc("POST /api/v1/anime/refresh", s.handleAnimeRefresh)
	mux.HandleFunc("POST /api/v1/anime/episodes/{id}/queue", s.handleAnimeQueue)
	mux.HandleFunc("GET /api/v1/anime/episodes/{id}/video", s.handleAnimeVideo)
	mux.HandleFunc("GET /api/v1/anime/subscriptions/{id}/poster", s.handleAnimePoster)
	mux.HandleFunc("GET /api/v1/anime/episodes/{id}/playback", s.handleAnimePlayback)
	mux.HandleFunc("PUT /api/v1/anime/episodes/{id}/playback", s.handleSaveAnimePlayback)
	mux.HandleFunc("GET /api/v1/anime/episodes/{id}/subtitles/{name}", s.handleAnimeSubtitle)

	// API: governance controls change the console's modes and approvals.
	mux.HandleFunc("POST /api/v1/modes", s.handleSetMode)
	mux.HandleFunc("DELETE /api/v1/modes/{mode}", s.handleClearMode)

	return s.middleware(mux)
}

// middleware applies the security headers, the concurrency bound, the token
// check and panic recovery, in that order.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		// No external origins at all: the console loads nothing from the
		// network, so a policy that forbids it cannot break a legitimate page.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: blob: https://lain.bgm.tv https://mikanani.me; media-src 'self'; "+
				"script-src 'self'; style-src 'self'; connect-src 'self'; "+
				"font-src 'self'; object-src 'none'; base-uri 'none'; "+
				"form-action 'none'; frame-ancestors 'none'")

		// Authorisation resolves the credential to a principal. With no token and
		// no agents configured, the bind address is the only control - which is
		// why CheckBind refuses a public address.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		p, ok := s.authenticate(r)
		if !ok && !publicRoute(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nasctl"`)
			writeError(w, http.StatusUnauthorized, "unauthorised",
				"this console requires authentication; sign in or complete local administrator setup")
			return
		}
		r = r.WithContext(withPrincipal(r.Context(), p))
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
				writeError(w, http.StatusForbidden, "cross_origin", "cross-origin writes are refused")
				return
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				writeError(w, http.StatusForbidden, "cross_origin", "cross-site writes are refused")
				return
			}
		}

		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				if s.Log != nil {
					s.Log.Error("handler panic", "path", r.URL.Path, "panic", fmt.Sprint(rec))
				}
				writeError(w, http.StatusInternalServerError, "internal_error",
					"the request could not be completed")
			}
		}()

		// Bound concurrent requests so a browser with many tabs cannot consume
		// the machine. The wait is bounded by the request's own context, so a
		// shutdown does not leave a request parked here.
		select {
		case s.requestSlots <- struct{}{}:
			defer func() { <-s.requestSlots }()
		case <-r.Context().Done():
			return
		}
		if !roleRouteAllowed(p, r) {
			s.roleDenied(r)
			writeError(w, 403, "role_denied", "此账号角色无权执行该操作")
			return
		}

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK, detail: p.DeviceAdmin()}
		next.ServeHTTP(sw, r)
		if s.Log != nil && r.URL.Path != "/api/v1/overview" {
			s.Log.Debug("request", "method", r.Method, "path", r.URL.Path,
				"status", sw.code, "bytes", sw.n, "ms", time.Since(start).Milliseconds())
		}
		s.auditAgentRead(r, sw.code)
	})
}

// presentedToken extracts the credential from the request, if any.
func (s *Server) presentedToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if c, err := r.Cookie("nasctl_token"); err == nil {
		return c.Value
	}
	return ""
}

// authenticate resolves the request's credential to a principal.
//
// The credential decides the identity, and the order matters: an agent token is
// checked first, so a caller holding one is an agent even if it claims otherwise
// with a header. The `X-Nasctl-Agent` header is still honoured, but only to
// *name* an agent that has no configured credential of its own, and only when no
// credentials are configured at all - it is a convenience for a single-operator
// setup, not an authorisation mechanism.
//
// The second return value reports whether the caller may proceed at all. With no
// token and no agents configured, every caller proceeds: the bind address is the
// access control, and that is stated rather than implied.
func (s *Server) authenticate(r *http.Request) (domain.Principal, bool) {
	tok := s.presentedToken(r)
	state, err := s.accountStatus(r.Context())
	if err != nil {
		return domain.Principal{}, false
	}

	if len(s.Cfg.Agents) > 0 || s.Cfg.Token != "" || state.Enabled || s.Cfg.RequireSetup {
		// A credential is required. Check the agents first so that an agent token
		// can never be mistaken for the operator's.
		for _, a := range s.Cfg.Agents {
			if a.Token != "" && subtleEqual(tok, a.Token) {
				return domain.Principal{Kind: domain.PrincipalAI, Name: a.Name}, true
			}
		}
		if s.Cfg.Token != "" && subtleEqual(tok, s.Cfg.Token) {
			return domain.Principal{Kind: domain.PrincipalUser, Name: "operator"}, true
		}
		// An explicitly supplied invalid credential must not fall back to a
		// different browser identity. Cookie sessions are operator-only.
		if tok == "" && r.Header.Get("Authorization") == "" {
			return s.browserPrincipal(r)
		}
		return domain.Principal{}, false
	}

	// Nothing is configured. If the caller names itself as an agent, record it as
	// one - the restrictions that apply to agents then apply, which is the safe
	// direction for a self-declaration to fail.
	if v := strings.TrimSpace(r.Header.Get("X-Nasctl-Agent")); v != "" {
		if len(v) > 64 {
			v = v[:64]
		}
		return domain.Principal{Kind: domain.PrincipalAI, Name: sanitiseName(v)}, true
	}
	return domain.Principal{Kind: domain.PrincipalUser, Name: "operator"}, true
}

// allowHeavyFor reports whether the named agent may start heavy work. An agent
// that self-declared (no configured credential) never may.
func (s *Server) allowHeavyFor(name string) bool {
	for _, a := range s.Cfg.Agents {
		if a.Name == name && a.AllowHeavy {
			return true
		}
	}
	return false
}

// authorised is retained for the token check alone, used by tests and by any
// caller that only needs "is this a valid credential".
func (s *Server) authorised(r *http.Request) bool {
	_, ok := s.authenticate(r)
	return ok
}

// principalContextKey carries the resolved principal through the request.
type principalContextKey struct{}

// withPrincipal attaches a principal to a context.
func withPrincipal(ctx context.Context, p domain.Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// principalOf returns the resolved principal, or a conservative default.
//
// The default is a *user*, not an agent: a handler reached without the middleware
// has no evidence either way, and the middleware is the only place that can
// authenticate. The jobs layer applies the agent restrictions from the principal
// it is given, so a mislabelled caller is caught there.
func principalOf(ctx context.Context) domain.Principal {
	if p, ok := ctx.Value(principalContextKey{}).(domain.Principal); ok {
		return p
	}
	return domain.Principal{Kind: domain.PrincipalUser, Name: "unknown"}
}

// subtleEqual compares two strings in constant time.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		var n int
		if len(a) > len(b) {
			n = len(a)
		} else {
			n = len(b)
		}
		var diff byte
		for i := 0; i < n; i++ {
			var ca, cb byte
			if i < len(a) {
				ca = a[i]
			}
			if i < len(b) {
				cb = b[i]
			}
			diff |= ca ^ cb
		}
		return false && diff == 0
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// auditAgentRead records what an AI principal read.
//
// Reads by a human are deliberately not audited: the operator browsing their own
// files is not accountability information, and recording every page load would
// bury the entries that are - the mode changes, the approvals, the refusals -
// under thousands of rows nobody will read.
//
// An agent is different. An operator who lets a program read their files wants to
// be able to see what it read, and an agent's request volume is its own business
// rather than a side effect of somebody clicking around. So agent reads are
// recorded, with the operation the route maps to, and the audit retention window
// prunes them like anything else.
func (s *Server) auditAgentRead(r *http.Request, status int) {
	if s.Jobs == nil || s.Jobs.Store() == nil {
		return
	}
	p := s.principal(r)
	if p.Kind != domain.PrincipalAI {
		return
	}
	// The route pattern is populated by the mux during ServeHTTP, so it is
	// available here and names the operation rather than a concrete path: a
	// request for one file and a request for another are the same operation.
	op := r.Pattern
	if op == "" {
		op = r.Method + " " + r.URL.Path
	}
	outcome := "ok"
	if status >= 400 {
		outcome = "refused"
	}
	target := r.URL.Path
	params := domain.Redact(r.URL.RawQuery)

	// A short timeout and a discarded error: failing to write an audit row must
	// not fail the read the caller asked for, and the row is not worth holding a
	// request slot for.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
	defer cancel()
	_ = s.Jobs.Store().Record(ctx, hubstore.AuditEntry{
		At:        time.Now(),
		Principal: p.String(),
		Action:    "read",
		Op:        op,
		Target:    target,
		Params:    params,
		Outcome:   outcome,
	})
}

// principal returns the caller's resolved identity.
//
// It comes from the credential, established once in the middleware. Without
// per-user accounts - which this stage does not have - the honest identity for a
// token holder is "the operator, over a private network", and it is recorded as
// such rather than dressed up as a named user.
func (s *Server) principal(r *http.Request) domain.Principal {
	return principalOf(r.Context())
}

// sanitiseName keeps a caller-supplied name to characters that are safe in a log
// line, a JSON string and a URL segment.
func sanitiseName(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// CheckBind refuses an address that would expose the console.
//
// The console has no per-user authentication, so the address is the access
// control. Rather than documenting that and hoping, the bind is checked: a public
// address is refused unless the operator explicitly allows it for an offline
// test.
func CheckBind(bind string, allowNonPrivate bool) error {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return fmt.Errorf("bind %q is not host:port: %w", bind, err)
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return fmt.Errorf("bind %q has an unusable port: %w", bind, err)
	}
	if host == "" {
		return fmt.Errorf("bind %q names no host; an empty host listens on every "+
			"interface, which would expose the console to the network it is on", bind)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		if host != "localhost" {
			return fmt.Errorf("bind %q must be an IP address or localhost; a name would be "+
				"resolved at listen time, so the interface actually used could differ from "+
				"the one intended", bind)
		}
		return nil
	}
	if ip.IsLoopback() {
		return nil
	}
	// 100.64.0.0/10 is the carrier-grade NAT range Tailscale uses.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return nil
	}
	if ip.IsPrivate() {
		return nil
	}
	if allowNonPrivate {
		return nil
	}
	return fmt.Errorf("bind %q is not a private address; this console has no per-user "+
		"authentication, so binding it publicly would let anyone who can reach the port "+
		"read your files. Use a Tailscale or private address, or set "+
		"allow_non_private_bind for an offline test", bind)
}

// ---------------------------------------------------------------------------
// Response helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_error",
			"the response could not be encoded")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(buf)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeErrorDetail(w, status, code, msg, "")
}

// writeErrorDetail adds an optional technical explanation that the UI shows
// behind an expandable "details" control.
func writeErrorDetail(w http.ResponseWriter, status int, code, msg, detail string) {
	msg, detail = localizeError(code, msg, detail)
	body := map[string]any{"error": code, "message": msg}
	if detail != "" {
		body["detail"] = detail
	}
	writeJSON(w, status, body)
}

// errorText gives a Chinese message for codes whose handlers still describe
// the problem in English. The English text is kept as the detail.
var errorText = map[string]string{
	"bad_request":           "请求参数无效，请刷新页面后重试",
	"denied":                "没有权限执行此操作",
	"role_denied":           "此账号角色无权执行该操作",
	"not_configured":        "该功能尚未启用，请在系统设置中配置",
	"read_only":             "控制台当前为只读模式",
	"not_found":             "没有找到对应的记录，可能已被移动或删除",
	"busy":                  "设备正忙，请稍后重试",
	"no_storage":            "尚未配置存储目录",
	"rate_limited":          "尝试次数过多，请稍后重试",
	"internal_error":        "服务内部出错，请稍后重试",
	"encode_error":          "响应生成失败，请稍后重试",
	"not_settable":          "该设置不能在网页上修改",
	"cross_origin":          "已拒绝来自其他网站的请求",
	"unauthorised":          "请先登录",
	"too_large":             "文件太大，无法在网页中处理",
	"no_preview":            "此文件不支持预览，可以下载后查看",
	"timeout":               "处理超时，请稍后重试",
	"invalid_range":         "请求的文件范围无效",
	"interrupted":           "操作被中断，请刷新后确认结果",
	"not_running":           "任务已经结束",
	"shutting_down":         "服务正在停止",
	"anime_error":           "追番操作没有完成",
	"state_error":           "读取控制台状态失败，请稍后重试",
	"no_such_root":          "没有这个存储目录，请刷新后重试",
	"outside_root":          "路径超出了存储目录范围",
	"confirmation_required": "请确认后再执行此操作",
}

// localizeError keeps Chinese messages as they are. An English message is
// moved into the detail and replaced by the code's Chinese text.
func localizeError(code, msg, detail string) (string, string) {
	for _, r := range msg {
		if r >= 0x4e00 && r <= 0x9fff {
			return msg, detail
		}
	}
	text, ok := errorText[code]
	if !ok {
		text = "操作没有完成，请稍后重试"
	}
	if msg != "" {
		if detail != "" {
			detail = msg + "\n" + detail
		} else {
			detail = msg
		}
	}
	return text, detail
}

// detailAllowed reports whether internal error text may be shown to this
// caller. Only device administrators see it: it can name paths on the NAS.
func detailAllowed(w http.ResponseWriter) bool {
	for {
		switch v := w.(type) {
		case *statusWriter:
			return v.detail
		case interface{ Unwrap() http.ResponseWriter }:
			w = v.Unwrap()
		default:
			return false
		}
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, code string, err error) {
	if s.Log != nil {
		s.Log.Error("request failed", "code", code, "error", err.Error())
	}
	// The internal error text can contain a path, so only a device
	// administrator gets it, as an expandable detail; it is always logged.
	detail := ""
	if err != nil && detailAllowed(w) {
		detail = domain.Redact(err.Error())
	}
	writeErrorDetail(w, status, code, userMessage(code), detail)
}

// userMessage maps an error code to text safe to show a caller.
func userMessage(code string) string {
	switch code {
	case "no_such_root":
		return "没有这个存储目录，请刷新后重试"
	case "outside_root":
		return "路径超出了存储目录范围"
	case "not_found":
		return "文件或目录不存在，可能已被移动或删除"
	case "bad_request":
		return "请求参数无效，请刷新页面后重试"
	case "state_error":
		return "读取控制台状态失败，请稍后重试"
	case "busy":
		return "设备正忙，请稍后重试"
	default:
		return "操作没有完成，请稍后重试"
	}
}

// mapFileErr turns a files-package error into a status and a code.
//
// A refusal is a 403 and a genuine absence is a 404. Keeping them distinct is
// deliberate: a caller probing for paths must not be able to tell "you may not"
// from "it is not there" by the status code, so the two are the same for
// traversal, and different only where the caller already named a real root.
func mapFileErr(err error) (int, string) {
	switch {
	case errors.Is(err, files.ErrNoSuchRoot):
		return http.StatusNotFound, "no_such_root"
	case errors.Is(err, files.ErrRootUnavailable):
		return http.StatusServiceUnavailable, "root_unavailable"
	case errors.Is(err, files.ErrOutsideRoot):
		return http.StatusForbidden, "outside_root"
	case errors.Is(err, files.ErrBadPath):
		return http.StatusBadRequest, "bad_request"
	case errors.Is(err, files.ErrNotExist):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, files.ErrNotRegular):
		return http.StatusForbidden, "not_regular"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

// pageParams reads page and page_size, clamped to the configured bounds.
func (s *Server) pageParams(r *http.Request) (page, size int) {
	page = intParam(r, "page", 1)
	if page < 1 {
		page = 1
	}
	size = intParam(r, "page_size", s.Cfg.PageDefault)
	if size <= 0 {
		size = s.Cfg.PageDefault
	}
	if size > s.Cfg.PageMax {
		size = s.Cfg.PageMax
	}
	return page, size
}

// intParam reads an integer query parameter, returning def when absent or
// malformed. A malformed value is treated as absent rather than as an error,
// because every one of these parameters is a bound and the default is the safe
// value.
func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// statusWriter records the status and size for the access log.
type statusWriter struct {
	http.ResponseWriter
	code   int
	n      int64
	wrote  bool
	detail bool // the caller may see internal error details
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.code = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	w.wrote = true
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

// Flush forwards to the underlying writer when it supports flushing, so that a
// streaming response is not buffered by this wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// escapeHTML is used where a value must be embedded in markup rather than JSON.
func escapeHTML(s string) string { return html.EscapeString(s) }

// streamCopy copies a file to the response with a bounded buffer. It never reads
// the whole file into memory, so a 4 GiB video costs the same as a 4 KiB text
// file.
func streamCopy(w io.Writer, r io.Reader) error {
	buf := make([]byte, 64<<10)
	_, err := io.CopyBuffer(w, r, buf)
	return err
}

// contextWithTimeout is a small helper so handlers do not each import context
// for one call.
func contextWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}

// jobSummary is the shape the UI consumes for one job.
type jobSummary struct {
	ID          string          `json:"id"`
	Op          string          `json:"op"`
	State       domain.JobState `json:"state"`
	Principal   string          `json:"principal"`
	Target      string          `json:"target,omitempty"`
	Params      string          `json:"params,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	EndedAt     *time.Time      `json:"ended_at,omitempty"`
	Progress    int64           `json:"progress"`
	Total       int64           `json:"total"`
	Unit        string          `json:"unit,omitempty"`
	StopReason  string          `json:"stop_reason,omitempty"`
	Error       string          `json:"error,omitempty"`
	Verified    bool            `json:"verified"`
	Attention   bool            `json:"needs_attention"`
	Cancellable bool            `json:"cancellable"`
}

// toJobSummary renders a job for the UI, making explicit which states need
// attention so that the page cannot accidentally render "unknown" as healthy.
func toJobSummary(j hubstore.Job) jobSummary {
	js := jobSummary{
		ID: j.ID, Op: j.Op, State: j.State, Principal: j.Principal,
		Target: j.Target, Params: j.Params, CreatedAt: j.CreatedAt,
		Progress: j.Progress, Total: j.Total, Unit: j.Unit,
		StopReason: j.StopReason, Error: j.Err, Verified: j.Verified,
		Attention: j.State.NeedsAttention(),
	}
	if !j.StartedAt.IsZero() {
		t := j.StartedAt
		js.StartedAt = &t
	}
	if !j.EndedAt.IsZero() {
		t := j.EndedAt
		js.EndedAt = &t
	}
	return js
}

// argsFromQuery builds an argument set from query parameters, keeping only the
// names the operation declares.
//
// An undeclared parameter is dropped here rather than refused, because this is a
// read path whose declarations are the authority: the operation check that
// refuses undeclared parameters runs for calls that do something. For a read, an
// unexpected parameter is noise, and refusing the request would make the API
// brittle without making it safer.
func argsFromQuery(op operations.Operation, r *http.Request) *operations.Args {
	args := operations.NewArgs()
	q := r.URL.Query()
	for _, p := range op.Params {
		v, ok := q[p.Name]
		if !ok || len(v) == 0 {
			continue
		}
		val := v[0]
		switch p.Kind {
		case operations.ParamInt:
			if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
				args.Int(p.Name, n)
			}
		case operations.ParamBool:
			switch strings.ToLower(strings.TrimSpace(val)) {
			case "1", "true", "yes", "on":
				args.Bool(p.Name, true)
			case "0", "false", "no", "off", "":
				args.Bool(p.Name, false)
			}
		default:
			args.Str(p.Name, val)
		}
	}
	return args
}

// decodeBody reads a JSON request body under a size cap.
//
// The cap is enforced by the reader rather than by inspecting the request: a
// Content-Length header is a claim, and a claim is not a bound.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	const maxBody = 64 << 10
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"the request body could not be read as JSON with the expected fields")
		return err
	}
	return nil
}
