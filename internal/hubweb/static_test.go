package hubweb

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

func TestEmbeddedAssetsCompressAndCacheStampedURLs(t *testing.T) {
	s := &Server{}
	get := func(path, encoding, inm string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if encoding != "" {
			req.Header.Set("Accept-Encoding", encoding)
		}
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		res := httptest.NewRecorder()
		key, _, _ := strings.Cut(path, "?")
		s.serveAsset(res, req, key)
		return res
	}
	js := assetTable["/app.js"]
	plain := get("/app.js", "", "")
	if plain.Header().Get("Content-Encoding") != "" || !bytes.Equal(plain.Body.Bytes(), js.body) || plain.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatal("uncompressed response", plain.Header())
	}
	zipped := get("/app.js", "br, gzip;q=0.8", "")
	if zipped.Header().Get("Content-Encoding") != "gzip" || zipped.Body.Len() >= len(js.body) || zipped.Header().Get("Content-Length") != strconv.Itoa(zipped.Body.Len()) {
		t.Fatal("gzip response", zipped.Header(), zipped.Body.Len())
	}
	zr, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(zr); err != nil || !bytes.Equal(got, js.body) {
		t.Fatal("gzip body does not decode to the original", err)
	}
	if zipped.Header().Get("ETag") == plain.Header().Get("ETag") {
		t.Fatal("encoded and identity representations share a validator")
	}
	if res := get("/app.js", "gzip", zipped.Header().Get("ETag")); res.Code != 304 || res.Body.Len() != 0 {
		t.Fatal("gzip validator", res.Code)
	}
	if res := get("/app.js", "gzip;q=0", ""); res.Header().Get("Content-Encoding") != "" {
		t.Fatal("explicit gzip refusal ignored")
	}
	if res := get("/app.js?v="+js.hash[:12], "", ""); res.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatal("stamped asset is not immutable", res.Header())
	}
	for _, path := range []string{"/app.js?v=000000000000", "/app.js?v=" + js.hash, "/?v=" + assetTable["/"].hash[:12]} {
		if res := get(path, "", ""); res.Header().Get("Cache-Control") != "no-cache" {
			t.Fatal(path, "must revalidate", res.Header())
		}
	}
	// The shell references exactly the stamps that make the assets immutable.
	shell := string(assetTable["/"].body)
	for _, name := range []string{"app.js", "app.css"} {
		if !strings.Contains(shell, `"/`+name+`?v=`+assetTable["/"+name].hash[:12]+`"`) {
			t.Fatalf("index.html does not stamp %s with its content hash", name)
		}
	}
}

func TestErrorDetailIsOnlyForDeviceAdministrators(t *testing.T) {
	s := &Server{}
	for _, admin := range []bool{true, false} {
		res := httptest.NewRecorder()
		s.fail(&statusWriter{ResponseWriter: res, code: 200, detail: admin}, 500, "state_error", errors.New("open /srv/private/state.db: permission denied"))
		body := res.Body.String()
		if !strings.Contains(body, "读取控制台状态失败") {
			t.Fatalf("message not localised: %s", body)
		}
		if strings.Contains(body, "/srv/private") != admin {
			t.Fatalf("admin=%v detail exposure wrong: %s", admin, body)
		}
	}
}
