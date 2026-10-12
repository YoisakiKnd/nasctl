// Package server is the read-only web service.
//
// What this package must never do, and how that is enforced:
//
//   - It never decodes an image, never runs an external program, never starts a
//     scan. There is no import of os/exec, image/*, internal/thumbs,
//     internal/probe or internal/scan anywhere in its import graph, and
//     arch_test.go fails the build if one appears.
//   - It never writes to the index. It holds a Store opened with SQLite's
//     query_only pragma and every mutating store method returns ErrReadOnly.
//   - It never loads the library into memory. Every query is paged with an
//     explicit LIMIT, and a page is capped at config.Server.PageMax.
//   - It never streams a whole original into memory. Originals are served with
//     http.ServeContent over an *os.File, which reads with a bounded copy
//     buffer and answers Range requests without a full read.
//
// Access control is by network placement, and the service says so out loud: it
// refuses to bind to a non-private address unless the operator opts in, and it
// emits a warning when no token is configured. A private address is not a
// guarantee that every device on the network is trustworthy, and the response
// says as much rather than implying otherwise.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/buildinfo"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
	"github.com/YoisakiKnd/nasctl/internal/logx"
	"github.com/YoisakiKnd/nasctl/internal/mimetype"
	"github.com/YoisakiKnd/nasctl/internal/resource"
	"github.com/YoisakiKnd/nasctl/internal/store"
)

//go:embed web/*
var webFS embed.FS

// Server is the HTTP service.
type Server struct {
	Cfg     *config.Config
	Store   *store.Store
	Log     *logx.Logger
	Started time.Time

	handler http.Handler
	sem     chan struct{}
	stream  chan struct{}
	counts  *countCache
	client  *http.Client // never used to fetch remote content; reserved for tests
}

// New builds the service.
func New(cfg *config.Config, st *store.Store, log *logx.Logger) (*Server, error) {
	s := &Server{
		Cfg:     cfg,
		Store:   st,
		Log:     log,
		Started: time.Now(),
		sem:     make(chan struct{}, cfg.Server.MaxConcurrentRequests),
		stream:  make(chan struct{}, cfg.Server.MaxConcurrentStreams),
		counts:  newCountCache(32, 5*time.Second),
	}
	if st == nil {
		return nil, errors.New("server: a store handle is required")
	}
	if !st.ReadOnly() {
		return nil, errors.New("server: refusing to run with a writable store handle; " +
			"the web service must not be able to modify the index")
	}
	s.handler = s.routes()
	return s, nil
}

// Handler exposes the middleware-wrapped handler for tests.
func (s *Server) Handler() http.Handler { return s.handler }

// routes wires the API and the embedded assets.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/collections", s.handleCollections)
	mux.HandleFunc("GET /api/v1/media", s.handleMediaList)
	mux.HandleFunc("GET /api/v1/media/{id}", s.handleMediaItem)
	mux.HandleFunc("GET /api/v1/media/{id}/thumb", s.handleThumb)
	mux.HandleFunc("GET /api/v1/media/{id}/raw", s.handleStream(false))
	mux.HandleFunc("HEAD /api/v1/media/{id}/raw", s.handleStream(false))
	mux.HandleFunc("GET /api/v1/media/{id}/download", s.handleStream(true))
	mux.HandleFunc("HEAD /api/v1/media/{id}/download", s.handleStream(true))
	mux.HandleFunc("GET /api/v1/timeline", s.handleTimeline)
	mux.HandleFunc("GET /api/v1/cache", s.handleCache)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("server: embedded assets are missing: " + err.Error())
	}
	static := http.FileServer(http.FS(sub))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		} else if strings.HasPrefix(r.URL.Path, "/app.") {
			w.Header().Set("Cache-Control",
				fmt.Sprintf("public, max-age=%d", s.Cfg.Server.StaticCacheSeconds))
		}
		static.ServeHTTP(w, r)
	}))

	return s.middleware(mux)
}

// middleware applies access control, concurrency limits, security headers and
// panic recovery.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Concurrency cap. A slow phone on a flaky link can hold several
		// connections open; on a small machine the number of simultaneous
		// requests is a resource, so it is capped and refused politely.
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			w.Header().Set("Retry-After", "2")
			writeError(w, http.StatusServiceUnavailable, "server_busy",
				"the web service is handling its maximum number of requests; retry shortly")
			return
		}

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; media-src 'self'; "+
				"script-src 'self'; style-src 'self'; connect-src 'self'; "+
				"font-src 'self'; object-src 'none'; base-uri 'none'; "+
				"form-action 'none'; frame-ancestors 'none'")

		if s.Cfg.Server.Token != "" && !s.authorised(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="albumctl"`)
			writeError(w, http.StatusUnauthorized, "unauthorised",
				"this albumctl instance requires a token")
			return
		}

		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("handler panic", "path", r.URL.Path, "panic", fmt.Sprint(rec))
				writeError(w, http.StatusInternalServerError, "internal_error",
					"the request could not be completed")
			}
		}()

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		if s.Log != nil && r.URL.Path != "/api/v1/status" {
			s.Log.Debug("request",
				"method", r.Method, "path", r.URL.Path, "status", sw.code,
				"bytes", sw.n, "ms", time.Since(start).Milliseconds())
		}
	})
}

func (s *Server) authorised(r *http.Request) bool {
	tok := s.Cfg.Server.Token
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") && subtleEqual(strings.TrimSpace(h[7:]), tok) {
		return true
	}
	if c, err := r.Cookie("albumctl_token"); err == nil && subtleEqual(c.Value, tok) {
		return true
	}
	return false
}

// subtleEqual is a constant-time comparison so that a token cannot be recovered
// by timing a sequence of guesses.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		// Still walk the longer string so the timing does not leak its length.
		var n int
		if len(a) > len(b) {
			n = len(a)
		} else {
			n = len(b)
		}
		var diff byte
		for i := 0; i < n; i++ {
			var x, y byte
			if i < len(a) {
				x = a[i]
			}
			if i < len(b) {
				y = b[i]
			}
			diff |= x ^ y
		}
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

type statusWriter struct {
	http.ResponseWriter
	code int
	n    int64
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

// CheckBind refuses to serve on a public address unless explicitly allowed.
//
// "Default to my Tailscale devices only" is implemented as a check rather than
// a comment: a typo in a bind address is a silent exposure, and this turns it
// into a startup failure.
func CheckBind(cfg *config.Config) error {
	host, port, err := net.SplitHostPort(cfg.Server.Bind)
	if err != nil {
		return err
	}
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("server.bind port %q is not numeric", port)
	}
	if host == "" {
		return errors.New("server.bind has no address; refusing to listen on every interface")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("server.bind host %q is not an IP address; "+
			"use the Tailscale address or 127.0.0.1 so the exposure is explicit", host)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return nil
	}
	// 100.64.0.0/10 is the Tailscale carrier-grade NAT range; Go's IsPrivate
	// does not include it.
	if ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1] >= 64 && ip.To4()[1] <= 127 {
		return nil
	}
	if cfg.Server.AllowNonPrivateBind {
		return nil
	}
	return fmt.Errorf("server.bind %s is not a loopback, private or Tailscale address; "+
		"either bind to a private address or set server.allow_non_private_bind = true to accept the exposure", ip)
}

// ---- JSON helpers -------------------------------------------------------

// writeJSON encodes with HTML escaping on, which turns <, > and & inside
// metadata and file names into \u003c etc. The frontend additionally builds DOM
// nodes with textContent, so escaping happens twice on purpose.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorEnvelope{Error: apiError{Code: code, Message: msg}})
}

func (s *Server) fail(w http.ResponseWriter, status int, code string, err error) {
	msg := "the request could not be completed"
	if err != nil {
		msg = err.Error()
	}
	if status >= 500 && s.Log != nil {
		s.Log.Error("api error", "code", code, "err", msg)
	}
	writeError(w, status, code, msg)
}

// ---- small caches -------------------------------------------------------

// countCache memoises the totals behind `status` and the per-filter estimate
// for a few seconds.
//
// It exists because a page request should not trigger two full aggregates over
// the media table, and because a live photo grid issues many page requests in a
// row. It is small and bounded so it can never itself become a memory problem.
type countCache struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	entries map[string]countEntry
}

type countEntry struct {
	v  int64
	at time.Time
}

func newCountCache(max int, ttl time.Duration) *countCache {
	return &countCache{max: max, ttl: ttl, entries: map[string]countEntry{}}
}

func (c *countCache) get(k string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok || time.Since(e.at) > c.ttl {
		return 0, false
	}
	return e.v, true
}

func (c *countCache) put(k string, v int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		// Drop the oldest entry rather than growing without bound.
		var oldest string
		var oldestAt time.Time
		for kk, ee := range c.entries {
			if oldest == "" || ee.at.Before(oldestAt) {
				oldest, oldestAt = kk, ee.at
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[k] = countEntry{v: v, at: time.Now()}
}

// ---- status -------------------------------------------------------------

type collectionStatus struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	MediaCount int64  `json:"media_count"`
	MountOK    bool   `json:"mount_ok"`
	MountNote  string `json:"mount_note"`
}

type totalsStatus struct {
	Media       int64 `json:"media"`
	Images      int64 `json:"images"`
	Videos      int64 `json:"videos"`
	Other       int64 `json:"other"`
	Probed      int64 `json:"probed"`
	ThumbOK     int64 `json:"thumb_ok"`
	ThumbDue    int64 `json:"thumb_pending"`
	ThumbFailed int64 `json:"thumb_failed"`
	Unsupported int64 `json:"unsupported"`
	Undated     int64 `json:"undated"`
	WithGPS     int64 `json:"with_gps"`
	Missing     int64 `json:"missing"`
}

type taskStatus struct {
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
	Detail    string `json:"detail"`
	EndedAt   int64  `json:"ended_at"`
	Processed int64  `json:"processed"`
	Skipped   int64  `json:"skipped"`
	Failed    int64  `json:"failed"`
}

type cacheStatus struct {
	Files    int64   `json:"files"`
	Bytes    int64   `json:"bytes"`
	MaxBytes int64   `json:"max_bytes"`
	UsedPct  float64 `json:"used_pct"`
}

type resourceStatus struct {
	MemAvailableKB     int64 `json:"mem_available_kb"`
	MemTotalKB         int64 `json:"mem_total_kb"`
	SwapFreeKB         int64 `json:"swap_free_kb"`
	StartThresholdKB   int64 `json:"start_threshold_kb"`
	WarnThresholdKB    int64 `json:"warn_threshold_kb"`
	StopThresholdKB    int64 `json:"stop_threshold_kb"`
	PressureTripped    bool  `json:"pressure_tripped"`
	PressureTrippedAt  int64 `json:"pressure_tripped_at"`
	ReadOnly           bool  `json:"read_only"`
	WebServiceRSSBytes int64 `json:"web_service_rss_bytes"`
}

type statusResponse struct {
	Version     string             `json:"version"`
	Schema      int                `json:"schema"`
	Now         int64              `json:"now"`
	Paused      bool               `json:"paused"`
	PauseReason string             `json:"pause_reason"`
	PauseSince  int64              `json:"pause_since"`
	Collections []collectionStatus `json:"collections"`
	Totals      totalsStatus       `json:"totals"`
	TaskRunning *taskStatus        `json:"running"`
	TaskLast    *taskStatus        `json:"last"`
	Cache       cacheStatus        `json:"cache"`
	Resource    *resourceStatus    `json:"resource"`
	Warnings    []string           `json:"warnings"`
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var resp statusResponse
	resp.Version = buildinfo.Version
	resp.Schema = buildinfo.Schema
	resp.Now = time.Now().Unix()
	resp.Collections = []collectionStatus{}
	resp.Warnings = []string{}

	ps, err := s.Store.PauseState(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "state_error", err)
		return
	}
	resp.Paused, resp.PauseReason, resp.PauseSince = ps.Paused, ps.Reason, ps.Since

	cols, err := s.Store.Collections(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "collections_error", err)
		return
	}
	for _, c := range cols {
		resp.Collections = append(resp.Collections, collectionStatus{
			Code: c.Code, Name: c.Name, Enabled: c.Enabled, MediaCount: c.MediaCount,
			MountOK: c.LastCheckOK, MountNote: c.LastCheckNote,
		})
	}

	t, _, err := s.totals(ctx)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "totals_error", err)
		return
	}
	resp.Totals = t

	files, bytes, err := s.Store.SumThumbBytes(ctx)
	if err == nil {
		resp.Cache = cacheStatus{Files: files, Bytes: bytes, MaxBytes: s.Cfg.Cache.MaxBytes}
		if s.Cfg.Cache.MaxBytes > 0 {
			resp.Cache.UsedPct = float64(bytes) / float64(s.Cfg.Cache.MaxBytes) * 100
		}
	}

	if running, err := s.Store.RunningTasks(ctx); err == nil && len(running) > 0 {
		resp.TaskRunning = toTaskStatus(running[len(running)-1])
	}
	if last, err := s.Store.LastTask(ctx, ""); err == nil {
		resp.TaskLast = toTaskStatus(last)
	}
	if pr, err := s.Store.Pressure(ctx); err == nil && pr.Tripped {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf(
			"a previous run was stopped by resource pressure (%s); heavy tasks stay disabled until `albumctl resume`",
			orDefault(pr.Reason, "reason not recorded")))
	}

	if s.Cfg.Server.ExposeResourceSnapshot {
		rs := &resourceStatus{
			StartThresholdKB: s.Cfg.Guard.StartMemAvailableKB,
			WarnThresholdKB:  s.Cfg.Guard.WarnMemAvailableKB,
			StopThresholdKB:  s.Cfg.Guard.StopMemAvailableKB,
			ReadOnly:         true,
		}
		if mi, err := resource.ReadMemInfo("/proc/meminfo"); err == nil {
			rs.MemAvailableKB = mi.MemAvailableKB
			rs.MemTotalKB = mi.MemTotalKB
			rs.SwapFreeKB = mi.SwapFreeKB
		}
		if pr, err := s.Store.Pressure(ctx); err == nil {
			rs.PressureTripped, rs.PressureTrippedAt = pr.Tripped, pr.At
		}
		if rss, _, err := resource.SelfRSSBytes("/proc"); err == nil {
			rs.WebServiceRSSBytes = rss
		}
		resp.Resource = rs
	}

	if s.Cfg.Server.Token == "" {
		resp.Warnings = append(resp.Warnings,
			"no token is configured: access control relies entirely on the bind address and the network in front of it")
	}
	if resp.TaskRunning != nil {
		resp.Warnings = append(resp.Warnings,
			"a task is recorded as running. The web service never starts or resumes work and cannot stop it")
	}
	writeJSON(w, http.StatusOK, resp)
}

func toTaskStatus(t store.Task) *taskStatus {
	return &taskStatus{
		Kind: t.Kind, State: t.State, Reason: t.Reason, Detail: t.Detail,
		EndedAt: t.EndedAt, Processed: t.Processed, Skipped: t.Skipped, Failed: t.Failed,
	}
}

func (s *Server) totals(ctx context.Context) (totalsStatus, bool, error) {
	tt, err := s.Store.Totals(ctx, 0)
	if err != nil {
		return totalsStatus{}, false, err
	}
	return totalsStatus{
		Media: tt.Media, Images: tt.Images, Videos: tt.Videos, Other: tt.Other,
		Probed: tt.Probed, ThumbOK: tt.ThumbOK, ThumbDue: tt.ThumbDue,
		ThumbFailed: tt.ThumbFailed, Unsupported: tt.Unsupported,
		Undated: tt.Undated, WithGPS: tt.WithGPS, Missing: tt.Missing,
	}, true, nil
}

func (s *Server) handleCollections(w http.ResponseWriter, r *http.Request) {
	cols, err := s.Store.Collections(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "collections_error", err)
		return
	}
	type item struct {
		Code       string `json:"code"`
		Name       string `json:"name"`
		Enabled    bool   `json:"enabled"`
		MediaCount int64  `json:"media_count"`
		MountOK    bool   `json:"mount_ok"`
		MountNote  string `json:"mount_note"`
		FirstSeen  int64  `json:"first_seen"`
		LastScan   int64  `json:"last_scan"`
	}
	out := struct {
		Items  []item `json:"items"`
		Paused bool   `json:"paused"`
	}{Items: []item{}}
	for _, c := range cols {
		out.Items = append(out.Items, item{
			Code: c.Code, Name: c.Name, Enabled: c.Enabled, MediaCount: c.MediaCount,
			MountOK: c.LastCheckOK, MountNote: c.LastCheckNote,
			FirstSeen: c.FirstSeen, LastScan: c.LastScanAt,
		})
	}
	ps, _ := s.Store.PauseState(r.Context())
	out.Paused = ps.Paused
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCache(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// The numbers come from the cache directory itself, not from summing the
	// index, so this report and `albumctl cache-report` can never disagree
	// about how much space is in use. A file that was removed by hand, or a
	// temporary file left by an interrupted write, shows up here as the space
	// it actually occupies.
	//
	// The walk is bounded by the configured file ceiling: cache.max_files caps
	// how many files the cache may hold, so this cannot become an unbounded
	// scan of a growing tree.
	files, bytes, err := fsutil.DirUsage(s.Cfg.Paths.CacheDir)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "cache_error", err)
		return
	}
	indexed, _, err := s.Store.SumThumbBytes(ctx)
	if err != nil {
		indexed = -1
	}
	type resp struct {
		Files      int      `json:"files"`
		Bytes      int64    `json:"bytes"`
		MaxBytes   int64    `json:"max_bytes"`
		MaxFiles   int      `json:"max_files"`
		UsedPct    float64  `json:"used_pct"`
		ShardDepth int      `json:"shard_depth"`
		Indexed    int64    `json:"indexed_thumbnails"`
		Source     string   `json:"source"`
		Notes      []string `json:"notes"`
	}
	out := resp{
		Files: files, Bytes: bytes,
		MaxBytes: s.Cfg.Cache.MaxBytes, MaxFiles: s.Cfg.Cache.MaxFiles,
		ShardDepth: s.Cfg.Cache.ShardDepth,
		Indexed:    maxInt64(indexed, 0),
		Source:     "cache directory",
		Notes: []string{
			"cache pruning is manual only: `albumctl cache-gc`",
			"the web service never adds to the cache and never removes from it",
		},
	}
	if s.Cfg.Cache.MaxBytes > 0 {
		out.UsedPct = float64(bytes) / float64(s.Cfg.Cache.MaxBytes) * 100
	}
	if indexed >= 0 && indexed != int64(files) {
		out.Notes = append(out.Notes,
			"the index records thumbnails for a different number of files than the directory holds; "+
				"a difference is expected if temporary files or orphans are present, and nothing is "+
				"removed automatically")
	}
	writeJSON(w, http.StatusOK, out)
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

// rawMIMEFor is the serving allowlist. An extension outside it is served as
// application/octet-stream rather than sniffed, so a crafted file cannot be
// served as text/html from the album's own origin.
func rawMIMEFor(ext string) string { return mimetype.ForExt(ext) }
