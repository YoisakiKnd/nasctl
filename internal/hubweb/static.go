package hubweb

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// The front end is embedded in the binary. There is no front-end toolchain on the
// target machine and no build step at run time: the assets are compiled in, which
// is why the console needs no Node.js, no bundler and no asset directory on the
// NAS.
//
//go:embed web/index.html web/app.css web/app.js web/manifest.webmanifest web/icon-192.png web/icon-512.png web/icon-maskable.png web/apple-touch-icon.png
var assets embed.FS

// staticAsset is one embedded file with validators computed once at startup.
// The gzip variant is built on first use and kept: it is a fraction of the
// original's size, and compressing on every request would cost CPU the NAS
// needs for other work.
type staticAsset struct {
	path        string
	contentType string
	body        []byte
	hash        string // hex SHA-256 of body
	etag        string

	gzipOnce sync.Once
	gzipBody []byte // nil when compression would not help
}

// assetTable maps a request path to an embedded file. The table is explicit
// rather than a directory listing so that adding a file to the web directory
// cannot accidentally publish it.
var assetTable = map[string]*staticAsset{
	"/":           {path: "web/index.html", contentType: "text/html; charset=utf-8"},
	"/index.html": {path: "web/index.html", contentType: "text/html; charset=utf-8"},
	"/app.css":    {path: "web/app.css", contentType: "text/css; charset=utf-8"},
	"/app.js":     {path: "web/app.js", contentType: "text/javascript; charset=utf-8"},
	// Installable web app: the manifest and icons are public so the browser
	// can fetch them without credentials, and contain nothing private.
	"/manifest.webmanifest": {path: "web/manifest.webmanifest", contentType: "application/manifest+json"},
	"/icon-192.png":         {path: "web/icon-192.png", contentType: "image/png"},
	"/icon-512.png":         {path: "web/icon-512.png", contentType: "image/png"},
	"/icon-maskable.png":    {path: "web/icon-maskable.png", contentType: "image/png"},
	"/apple-touch-icon.png": {path: "web/apple-touch-icon.png", contentType: "image/png"},
}

func init() {
	for key, a := range assetTable {
		body, err := assets.ReadFile(a.path)
		if err != nil {
			panic("hubweb: embedded asset missing for " + key + ": " + err.Error())
		}
		sum := sha256.Sum256(body)
		a.body, a.hash = body, hex.EncodeToString(sum[:])
		a.etag = `"` + a.hash + `"`
	}
}

// gzipped returns the compressed body, or nil when the client should get the
// original.
func (a *staticAsset) gzipped() []byte {
	a.gzipOnce.Do(func() {
		if len(a.body) < 1024 {
			return
		}
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return
		}
		if _, err = zw.Write(a.body); err != nil || zw.Close() != nil {
			return
		}
		if buf.Len() < len(a.body) {
			a.gzipBody = buf.Bytes()
		}
	})
	return a.gzipBody
}

// handleIndex serves the application shell.
//
// It answers only the exact paths it knows. A request for anything else that is
// not an API route gets the shell, because the UI is a single page and a deep
// link must work; but a request that looks like a file gets a 404 rather than the
// shell, so a typo in an asset URL is visible instead of silently returning HTML
// that the browser then fails to parse.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		// An unknown API path must be a 404, not the shell. Returning HTML for
		// /api/v1/shell would tell a caller that an endpoint exists, and a client
		// that parsed it as JSON would report a confusing decode error instead of
		// "no such endpoint".
		if strings.HasPrefix(r.URL.Path, "/api/") || looksLikeAsset(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
	}
	s.serveAsset(w, r, "/")
}

// handleStatic serves one embedded asset.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	s.serveAsset(w, r, r.URL.Path)
}

// looksLikeAsset reports whether a path names a file rather than a page route.
func looksLikeAsset(p string) bool {
	i := strings.LastIndex(p, "/")
	last := p[i+1:]
	return strings.Contains(last, ".")
}

// acceptsGzip reports whether the request accepts a gzip response, honouring
// an explicit q=0 refusal.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		q := strings.TrimSpace(params)
		if v, ok := strings.CutPrefix(q, "q="); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f == 0 {
				return false
			}
		}
		return true
	}
	return false
}

// serveAsset writes an embedded asset.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, key string) {
	a, ok := assetTable[key]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.contentType)
	h.Set("Vary", "Accept-Encoding")
	// Embedded files have zero modification times, so a timestamp validator
	// would keep an older binary's UI forever; content hashes are used instead.
	// The shell always revalidates. The build stamps the script and stylesheet
	// URLs with ?v=<first 12 hex of their SHA-256>; a request carrying the
	// matching stamp names immutable content and may be cached for a year,
	// while an unstamped or stale URL still revalidates.
	if v := r.URL.Query().Get("v"); key != "/" && key != "/index.html" && len(v) == 12 && strings.HasPrefix(a.hash, v) {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	body, etag := a.body, a.etag
	if acceptsGzip(r) {
		if gz := a.gzipped(); gz != nil {
			body, etag = gz, `"`+a.hash+`-gz"`
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("ETag", etag)
	for _, tag := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		tag = strings.TrimSpace(tag)
		if strings.TrimPrefix(tag, "W/") == etag || tag == "*" {
			h.Del("Content-Encoding")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}
