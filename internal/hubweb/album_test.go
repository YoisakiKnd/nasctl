package hubweb

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
)

func TestAlbumProxyPreservesRangesAndNeverForwardsOperatorSecrets(t *testing.T) {
	r := newRig(t, func(c *Config, _ *jobs.Config) { c.Token = "operator-secret-value" })
	client, _ := album.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			t.Error("operator secret crossed service boundary", request.Header)
		}
		w.Header().Set("Set-Cookie", "untrusted=upstream")
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Disposition", `inline; filename*=UTF-8''%E6%B5%8B%E8%AF%95.mp4`)
		w.Header().Set("ETag", `"test"`)
		http.ServeContent(w, request, "video.mp4", time.Unix(123, 0), bytes.NewReader([]byte("abcdef")))
	}))
	r.srv.Album = client
	defer client.Close()
	for _, test := range []struct {
		method, part, rangeHeader, etag string
		code                            int
		body                            string
	}{{"GET", "raw", "bytes=1-3", "", 206, "bcd"}, {"HEAD", "raw", "", "", 200, ""}, {"GET", "download", "", "", 200, "abcdef"}, {"GET", "raw", "", `"test"`, 304, ""}, {"GET", "raw", "bytes=100-200", "", 416, ""}} {
		req, _ := http.NewRequest(test.method, r.ts.URL+"/api/v1/album/media/1/"+test.part, nil)
		req.Header.Set("Authorization", "Bearer operator-secret-value")
		req.Header.Set("Cookie", "nasctl_token=operator-cookie")
		req.Header.Set("Range", test.rangeHeader)
		req.Header.Set("If-None-Match", test.etag)
		response, err := r.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != test.code {
			t.Fatal(test, response.StatusCode, string(body), err)
		}
		if test.code != 416 && string(body) != test.body {
			t.Fatal("proxy bytes", string(body), test)
		}
		if response.Header.Get("Set-Cookie") != "" || !strings.Contains(response.Header.Get("Cache-Control"), "no-store") {
			t.Fatal("unsafe response headers", response.Header)
		}
		if test.part == "download" && !strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment;") {
			t.Fatal("download is not attachment")
		}
	}
}

func TestAlbumStatusDoesNotLogOutConsoleOnAlbumAuthFailure(t *testing.T) {
	r := newRig(t, nil)
	client, _ := album.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, "secret-upstream-token /private/path")
	}))
	defer client.Close()
	r.srv.Album = client
	response, b := r.get(t, "/api/v1/album/status")
	if response.StatusCode != 200 || !strings.Contains(string(b), `"connected":false`) || strings.Contains(string(b), "secret-upstream-token") {
		t.Fatal(response.StatusCode, string(b))
	}
	response, b = r.get(t, "/api/v1/album/collections")
	if response.StatusCode != 502 || strings.Contains(string(b), "/private/path") {
		t.Fatal(response.StatusCode, string(b))
	}
	r.srv.Album = nil
	response, b = r.get(t, "/api/v1/album/status")
	var state map[string]any
	json.Unmarshal(b, &state)
	if response.StatusCode != 200 || state["configured"] != false {
		t.Fatal(string(b))
	}
	response, _ = r.get(t, "/api/v1/album/media")
	if response.StatusCode != 503 {
		t.Fatal(response.StatusCode)
	}
}

func TestAlbumThumbnailProxyRefusesUnexpectedContentAndSize(t *testing.T) {
	for _, scenario := range []string{"html", "large", "svg"} {
		t.Run(scenario, func(t *testing.T) {
			r := newRig(t, nil)
			client, _ := album.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch scenario {
				case "html":
					w.Header().Set("Content-Type", "text/html")
					io.WriteString(w, "<script>unsafe</script>")
				case "large":
					w.Header().Set("Content-Type", "image/jpeg")
					w.Header().Set("Content-Length", "2097153")
				case "svg":
					w.Header().Set("Content-Type", "image/svg+xml")
					io.WriteString(w, `<svg xmlns="http://www.w3.org/2000/svg"></svg>`)
				}
			}))
			defer client.Close()
			r.srv.Album = client
			response, b := r.get(t, "/api/v1/album/media/1/thumb")
			if scenario == "svg" {
				if response.StatusCode != 200 || !strings.Contains(string(b), "<svg") {
					t.Fatal(response.StatusCode, string(b))
				}
				if !strings.Contains(response.Header.Get("Content-Security-Policy"), "sandbox") {
					t.Fatal("direct thumbnail navigation must isolate upstream SVG content")
				}
			} else if response.StatusCode != 502 || strings.Contains(string(b), "<script>") {
				t.Fatal(response.StatusCode, string(b))
			}
		})
	}
}
