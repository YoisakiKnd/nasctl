package album

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/config"
)

func TestClientUsesConfiguredCredentialAndRewritesMediaURLs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer album-secret" || r.Header.Get("Cookie") != "" {
			t.Error("wrong upstream identity", r.Header)
		}
		switch r.URL.Path {
		case "/gallery/api/v1/status":
			io.WriteString(w, `{"version":"test","paused":true}`)
		case "/gallery/api/v1/collections":
			io.WriteString(w, `{"items":[{"code":"photos","name":"Photos","media_count":1}]}`)
		case "/gallery/api/v1/media":
			if r.URL.Query().Get("q") != "name & title" || r.URL.Query().Get("limit") != "1" {
				t.Error(r.URL)
			}
			io.WriteString(w, `{"items":[{"id":7,"name":"<img>.jpg","thumb_url":"https://evil.invalid/steal","raw_url":"javascript:bad"}],"next_cursor":"opaque","total_estimate":2}`)
		case "/gallery/api/v1/media/7":
			io.WriteString(w, `{"item":{"id":7,"name":"<img>.jpg"},"paths":{"display":"Photos/<img>.jpg"},"exif":{"Camera":"Test"}}`)
		case "/gallery/api/v1/media/7/raw":
			w.Header().Set("Content-Type", "video/mp4")
			w.Write([]byte("stream"))
		default:
			t.Error("unexpected endpoint", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := New(config.Album{APIBase: server.URL + "/gallery", Token: "album-secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	status, err := client.Status(context.Background())
	if err != nil || !status.Paused {
		t.Fatal(status, err)
	}
	collections, err := client.Collections(context.Background())
	if err != nil || len(collections.Items) != 1 {
		t.Fatal(collections, err)
	}
	page, err := client.List(context.Background(), Query{Q: "name & title", Limit: 1})
	if err != nil || page.Items[0].ThumbURL != "/api/v1/album/media/7/thumb" || page.Items[0].RawURL != "/api/v1/album/media/7/raw" {
		t.Fatal(page, err)
	}
	detail, err := client.Details(context.Background(), "7")
	if err != nil || detail.Item.ThumbURL != "/api/v1/album/media/7/thumb" {
		t.Fatal(detail, err)
	}
	response, err := client.Stream(context.Background(), "7", "raw", "GET", http.Header{"Authorization": {"Bearer operator-secret"}, "Cookie": {"secret=cookie"}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestClientRefusesRedirectsBadBodiesAndUnsafeParameters(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true; w.WriteHeader(500) }))
	defer target.Close()
	for _, scenario := range []string{"redirect", "credential", "large", "bad-json", "wrong-service", "too-many-items", "wrong-id"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch scenario {
				case "redirect":
					w.Header().Set("Location", target.URL)
					w.WriteHeader(302)
				case "credential":
					w.WriteHeader(401)
					io.WriteString(w, "album-secret operator-secret /private/state")
				case "large":
					io.WriteString(w, strings.Repeat("x", MaxJSONBytes+1))
				case "bad-json":
					io.WriteString(w, `<html>Wrong service</html>`)
				case "wrong-service":
					io.WriteString(w, `{}`)
				case "too-many-items":
					io.WriteString(w, `{"items":[{"id":1,"name":"one"},{"id":2,"name":"two"}]}`)
				case "wrong-id":
					io.WriteString(w, `{"item":{"id":2,"name":"different"}}`)
				}
			}))
			defer server.Close()
			client, _ := New(config.Album{APIBase: server.URL, Token: "album-secret"})
			defer client.Close()
			var err error
			if scenario == "too-many-items" {
				_, err = client.List(context.Background(), Query{Limit: 1})
			} else if scenario == "wrong-id" {
				_, err = client.Details(context.Background(), "1")
			} else {
				_, err = client.Status(context.Background())
			}
			if err == nil || strings.Contains(err.Error(), "album-secret") || strings.Contains(err.Error(), "operator-secret") || strings.Contains(err.Error(), "/private/state") {
				t.Fatal("unsafe failure", err)
			}
		})
	}
	if redirected {
		t.Fatal("credential redirect followed")
	}
	for _, q := range []Query{{Q: strings.Repeat("x", 257)}, {Limit: 101}, {Kind: "command"}, {Sort: "sql"}, {From: "2026-02-30"}, {From: "2026-02-01", To: "2025-01-01"}, {Cursor: strings.Repeat("x", 513)}} {
		if _, err := q.Values(); err == nil {
			t.Fatal("bad album query", q)
		}
	}
	for _, id := range []string{"0", "01", "+1", "../1", "1/raw", "9999999999999999999"} {
		if ValidID(id) {
			t.Fatal("bad media id", id)
		}
	}
}

func TestClientCancellationStopsUpstreamRequest(t *testing.T) {
	entered, stopped := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(stopped) }))
	defer server.Close()
	client, _ := New(config.Album{APIBase: server.URL})
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := client.Status(ctx); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not cancel")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("upstream still running")
	}
}
