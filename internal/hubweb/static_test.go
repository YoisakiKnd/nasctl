package hubweb

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbeddedAssetsRevalidateByContent(t *testing.T) {
	s := &Server{}
	for _, path := range []string{"/", "/app.js", "/app.css"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("If-Modified-Since", "Thu, 01 Jan 1970 00:00:00 GMT")
		res := httptest.NewRecorder()
		s.serveAsset(res, req, path)
		if res.Code != 200 || res.Body.Len() == 0 || res.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: %d %v", path, res.Code, res.Header())
		}
		etag := res.Header().Get("ETag")
		if etag == "" {
			t.Fatal("missing content validator")
		}
		for _, tc := range []struct {
			tag    string
			status int
		}{{etag, 304}, {"W/" + etag, 304}, {`"old-build"`, 200}} {
			req = httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("If-None-Match", tc.tag)
			res = httptest.NewRecorder()
			s.serveAsset(res, req, path)
			if res.Code != tc.status {
				t.Fatalf("%s: tag %s status %d want %d", path, tc.tag, res.Code, tc.status)
			}
		}
	}
}
