package album

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLocalTransportRangesHeadersAndNoCredentials(t *testing.T) {
	c, err := NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("browser credential forwarded")
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("ETag", `"test"`)
		http.ServeContent(w, r, "sample.mp4", time.Unix(123, 0), bytes.NewReader([]byte("abcdef")))
		w.Header().Set("X-Late", "must-not-race")
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, tc := range []struct {
		method, span, etag string
		code               int
		body               string
	}{
		{"GET", "bytes=1-3", "", 206, "bcd"}, {"HEAD", "", "", 200, ""}, {"GET", "", `"test"`, 304, ""}, {"GET", "bytes=100-200", "", 416, ""},
	} {
		resp, err := c.Stream(context.Background(), "1", "raw", tc.method, http.Header{"Range": {tc.span}, "If-None-Match": {tc.etag}, "Cookie": {"private=cookie"}, "Authorization": {"Bearer secret"}})
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != tc.code || (tc.code != 416 && string(b) != tc.body) || resp.Header.Get("X-Late") != "" {
			t.Fatal(tc, resp.StatusCode, string(b), err)
		}
		if tc.code == 206 && resp.ContentLength != 3 {
			t.Fatal(resp.ContentLength)
		}
	}
}

func TestLocalTransportBackpressureCloseAndDeadlineReleaseHandler(t *testing.T) {
	for _, cancelInstead := range []bool{false, true} {
		done := make(chan struct{})
		c, _ := NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(done)
			w.Header().Set("Content-Length", "1073741824")
			w.WriteHeader(200)
			// This first chunk must block until the consumer reads, cancels or
			// closes. A recorder-based transport would allocate/consume it all.
			chunk := make([]byte, 64<<10)
			for i := 0; i < 16384; i++ {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}))
		ctx, cancel := context.WithCancel(context.Background())
		resp, err := c.Stream(ctx, "1", "raw", "GET", nil)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			t.Fatal("original buffered without consumer")
		default:
		}
		if cancelInstead {
			cancel()
		} else {
			resp.Body.Close()
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("local handler leaked after abandoned body")
		}
		resp.Body.Close()
		cancel()
		c.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	c, _ := NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); <-r.Context().Done() }))
	if _, err := c.Status(ctx); err == nil {
		t.Fatal("deadline ignored")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not receive cancellation")
	}
}

func TestLocalTransportPanicAndJSONLimits(t *testing.T) {
	for _, h := range []http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { panic("private path or secret") },
		func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat(" ", MaxJSONBytes+1)) },
	} {
		c, _ := NewHandler(h)
		if _, err := c.Status(context.Background()); err == nil || strings.Contains(err.Error(), "private path") {
			t.Fatal(err)
		}
		c.Close()
	}
}
