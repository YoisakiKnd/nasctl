package hubweb

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"net/http"
	"strings"
)

// The front end is embedded in the binary. There is no front-end toolchain on the
// target machine and no build step at run time: the assets are compiled in, which
// is why the console needs no Node.js, no bundler and no asset directory on the
// NAS.
//
//go:embed web/index.html web/app.css web/app.js
var assets embed.FS

// assetInfo describes one embedded file.
type assetInfo struct {
	path        string
	contentType string
}

// assetTable maps a request path to an embedded file. The table is explicit
// rather than a directory listing so that adding a file to the web directory
// cannot accidentally publish it.
var assetTable = map[string]assetInfo{
	"/":           {"web/index.html", "text/html; charset=utf-8"},
	"/index.html": {"web/index.html", "text/html; charset=utf-8"},
	"/app.css":    {"web/app.css", "text/css; charset=utf-8"},
	"/app.js":     {"web/app.js", "text/javascript; charset=utf-8"},
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

// serveAsset writes an embedded asset.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, key string) {
	info, ok := assetTable[key]
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := assets.ReadFile(info.path)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "internal_error", err)
		return
	}
	w.Header().Set("Content-Type", info.contentType)
	// Embedded files have zero modification times. A timestamp validator would
	// incorrectly keep an older binary's UI forever. Revalidate all three assets
	// by content so an upgrade also picks up the matching script and stylesheet.
	w.Header().Set("Cache-Control", "no-cache")
	etag := fmt.Sprintf(`"%x"`, sha256.Sum256(body))
	w.Header().Set("ETag", etag)
	for _, tag := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if strings.TrimPrefix(strings.TrimSpace(tag), "W/") == etag || strings.TrimSpace(tag) == "*" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}
