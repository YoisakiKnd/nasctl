package anime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func metadataResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}
}

func TestMetadataSearchAndSeasonSelectionAreBoundedAndReadOnly(t *testing.T) {
	e, down, cfg := newEngine(t, sampleRSS)
	e.cfg.TMDBToken = "lookup-secret"
	before, _ := e.Store.Subscriptions(context.Background())
	calls := 0
	e.feedClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Scheme != "https" || r.URL.Host != "api.themoviedb.org" || r.Header.Get("Authorization") != "Bearer lookup-secret" {
			t.Fatal("credential origin", r.URL)
		}
		switch r.URL.Path {
		case "/3/search/tv":
			if r.URL.Query().Get("query") != "番剧 & 雨" || r.URL.Query().Get("page") != "2" || r.URL.Query().Get("language") != "zh-CN" || r.URL.Query().Get("include_adult") != "false" {
				t.Fatal(r.URL)
			}
			items := []string{`{"id":42,"name":"番剧 <title>","original_name":"Original","overview":"` + strings.Repeat("文", 1000) + `","first_air_date":"2026-01-01"}`, `{"id":42,"name":"duplicate"}`, `{"id":-2,"name":"invalid"}`}
			for i := 0; i < 30; i++ {
				items = append(items, fmt.Sprintf(`{"id":%d,"name":"Show %d"}`, 100+i, i))
			}
			return metadataResponse(r, 200, `{"total_pages":30,"results":[`+strings.Join(items, ",")+`]}`), nil
		case "/3/tv/42":
			return metadataResponse(r, 200, `{"id":42,"name":"番剧","seasons":[{"season_number":2,"name":"第二季","episode_count":12},{"season_number":0,"name":"Specials"},{"season_number":1,"episode_count":24},{"season_number":2,"name":"duplicate"},{"season_number":100,"episode_count":1}]}`), nil
		default:
			t.Fatal("unexpected request", r.URL)
			return nil, nil
		}
	})}
	result, err := e.SearchMetadata(context.Background(), "  番剧 & 雨  ", 2)
	if err != nil || len(result.Results) != 20 || !result.HasMore || !result.Truncated || result.Page != 2 || result.Results[0].ID != 42 || len([]rune(result.Results[0].Overview)) != 801 {
		t.Fatal(result, err)
	}
	details, err := e.MetadataDetails(context.Background(), 42)
	if err != nil || len(details.Seasons) != 2 || details.Seasons[0].Number != 1 || details.Seasons[1].Number != 2 {
		t.Fatal(details, err)
	}
	after, _ := e.Store.Subscriptions(context.Background())
	if !reflect.DeepEqual(before, after) || down.adds != 0 || calls != 2 {
		t.Fatal("lookup mutated subscription/downloader")
	}
	entries, err := os.ReadDir(cfg.LibraryDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("lookup wrote library", entries, err)
	}
}

func TestMetadataRefusesBadInputsUnavailableCredentialsAndUpstreamFailures(t *testing.T) {
	e, _, _ := newEngine(t, sampleRSS)
	calls := 0
	e.feedClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { calls++; return metadataResponse(r, 200, `{}`), nil })}
	for _, q := range []string{"", strings.Repeat("x", 201), "bad\nquery"} {
		if _, err := e.SearchMetadata(context.Background(), q, 1); err == nil {
			t.Fatal("bad query accepted")
		}
	}
	for _, page := range []int{0, 21} {
		if _, err := e.SearchMetadata(context.Background(), "valid", page); err == nil {
			t.Fatal("bad page")
		}
	}
	if _, err := e.MetadataDetails(context.Background(), 0); err == nil {
		t.Fatal("bad id")
	}
	if _, err := e.SearchMetadata(context.Background(), "valid", 1); err == nil {
		t.Fatal("missing credential")
	}
	if calls != 0 {
		t.Fatal("network called without credentials")
	}
	e.cfg.TMDBToken = "secret-never-return"
	for _, scenario := range []string{"auth", "rate", "not-found", "large", "bad-json", "wrong-series", "redirect", "transport"} {
		t.Run(scenario, func(t *testing.T) {
			calls = 0
			e.feedClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				switch scenario {
				case "auth":
					return metadataResponse(r, 401, "secret-never-return"), nil
				case "rate":
					return metadataResponse(r, 429, "secret-never-return"), nil
				case "not-found":
					return metadataResponse(r, 404, "secret-never-return"), nil
				case "large":
					return metadataResponse(r, 200, strings.Repeat("x", maxMetadataBytes+1)), nil
				case "bad-json":
					return metadataResponse(r, 200, `{invalid`), nil
				case "wrong-series":
					return metadataResponse(r, 200, `{"id":43}`), nil
				case "redirect":
					response := metadataResponse(r, 302, "")
					response.Header.Set("Location", "https://evil.invalid/steal")
					return response, nil
				default:
					return nil, errors.New("secret-never-return")
				}
			})}
			_, err := e.MetadataDetails(context.Background(), 42)
			if err == nil || strings.Contains(err.Error(), "secret-never-return") || calls != 1 {
				t.Fatalf("%v calls=%d", err, calls)
			}
		})
	}
}

func TestMetadataConcurrentLookupAndCancellation(t *testing.T) {
	e, _, _ := newEngine(t, sampleRSS)
	e.cfg.TMDBToken = "test-secret"
	entered := make(chan struct{})
	e.feedClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := e.SearchMetadata(ctx, "Show", 1); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	if _, err := e.MetadataDetails(context.Background(), 42); err == nil {
		t.Fatal("concurrent lookup allowed")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("lookup did not stop")
	}
}
