package hubweb

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/anime"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
)

func seedPlayback(t *testing.T, r *rig) (int64, string) {
	t.Helper()
	settings := r.srv.Anime.Settings()
	sub, err := r.srv.Anime.Store.SaveSubscription(context.Background(), anime.Subscription{Title: "Library Fixture", FeedURL: "https://example.com/feed", Season: 1, Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(settings.LibraryDir), "state", "anime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result, err := db.Exec(`INSERT INTO episodes(subscription_id,guid,title,url,number,season,state,library_path,updated_at) VALUES(?, 'fixture','Fixture - 01','https://example.com/fixture.torrent',1,1,'archived','Library Fixture/Season 1/Library Fixture S01E01.mp4',1)`, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	dir := filepath.Join(settings.LibraryDir, "Library Fixture", "Season 1")
	if err = os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"Library Fixture S01E01.mp4": "0123456789", "Library Fixture S01E01.chs.srt": "1\n00:00:01,250 --> 00:00:02,500\nHello\n", "Library Fixture S01E01.ass": "[Script Info]"} {
		if err = os.WriteFile(filepath.Join(dir, name), []byte(content), 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(settings.LibraryDir, "Library Fixture", "poster.jpg"), []byte{0xff, 0xd8, 0xff, 0xd9}, 0640); err != nil {
		t.Fatal(err)
	}
	return id, fmt.Sprintf("/api/v1/anime/episodes/%d", id)
}

func TestLibraryPlaybackHTTPStateSubtitlesAndRange(t *testing.T) {
	r := nativeRig(t, nil)
	id, base := seedPlayback(t, r)
	code, b := nativeRequest(t, r, "GET", base+"/playback", "", "", "")
	if code != 200 || !strings.Contains(string(b), "chs.srt") {
		t.Fatalf("playback: %d %s", code, b)
	}
	code, b = nativeRequest(t, r, "PUT", base+"/playback", `{"position":30,"duration":120}`, "", "")
	if code != 200 || !strings.Contains(string(b), `"position":30`) {
		t.Fatalf("progress: %d %s", code, b)
	}
	ep, err := r.srv.Anime.Store.Episode(context.Background(), id)
	if err != nil || ep.WatchPosition != 30 || ep.State != "archived" {
		t.Fatalf("persisted: %+v %v", ep, err)
	}
	entries, _ := r.store.Audit(context.Background(), 100, 0)
	if len(entries) != 0 {
		t.Fatal("periodic progress filled audit history")
	}
	code, _ = nativeRequest(t, r, "PUT", base+"/playback", `{"watched":true}`, "", "")
	if code != 200 {
		t.Fatal(code)
	}
	entries, _ = r.store.Audit(context.Background(), 100, 0)
	if len(entries) != 1 || entries[0].Op != "anime.watched" {
		t.Fatal("explicit decision was not audited")
	}
	code, b = nativeRequest(t, r, "GET", "/api/v1/anime/episodes?watch=watched", "", "", "")
	if code != 200 || !strings.Contains(string(b), `"watched":true`) {
		t.Fatalf("watch filter: %d %s", code, b)
	}
	code, _ = nativeRequest(t, r, "GET", "/api/v1/anime/episodes?watch=invalid", "", "", "")
	if code != 400 {
		t.Fatal("invalid watch filter accepted")
	}
	for _, body := range []string{`{}`, `null`, `{"position":1}`, `{"position":3,"duration":2}`, `{"watched":true,"unknown":1}`, `{"watched":true} {}`} {
		code, _ := nativeRequest(t, r, "PUT", base+"/playback", body, "", "")
		if code != 400 {
			t.Fatalf("invalid update %s: %d", body, code)
		}
	}
	subtitlePath := base + "/subtitles/" + url.PathEscape("Library Fixture S01E01.chs.srt")
	res, b := r.get(t, subtitlePath)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/vtt; charset=utf-8" || !strings.Contains(string(b), "00:00:01.250") {
		t.Fatalf("subtitle: %d %q", res.StatusCode, b)
	}
	res, b = r.get(t, subtitlePath+"?download=1")
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Disposition"), "attachment") || strings.HasPrefix(string(b), "WEBVTT") {
		t.Fatal("raw subtitle download changed")
	}
	code, _ = nativeRequest(t, r, "GET", base+"/subtitles/"+url.PathEscape("Library Fixture S01E02.srt"), "", "", "")
	if code != 404 {
		t.Fatal("another episode's subtitle opened")
	}
	res, b = r.get(t, "/api/v1/anime/subscriptions/1/poster")
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/jpeg" || len(b) != 4 {
		t.Fatal("poster response")
	}
	code, b = nativeRequest(t, r, "GET", "/api/v1/anime/subscriptions", "", "", "")
	if code != 200 || !strings.Contains(string(b), `"poster_available":true`) {
		t.Fatal("poster availability absent")
	}
	req, _ := http.NewRequest("GET", r.ts.URL+base+"/video", nil)
	req.Header.Set("Range", "bytes=2-5")
	response, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	b, _ = io.ReadAll(response.Body)
	if response.StatusCode != 206 || string(b) != "2345" {
		t.Fatalf("video Range: %d %q", response.StatusCode, b)
	}
}

func TestPlaybackWritesRetainIdentityAndReadonlyBoundaries(t *testing.T) {
	r := nativeRig(t, func(c *Config, _ *jobs.Config) {
		c.Token = "operator-token-value"
		c.Agents = []AgentCredential{{Name: "agent", Token: "agent-token-value"}}
	})
	_, base := seedPlayback(t, r)
	for _, path := range []string{base + "/playback", "/api/v1/anime/subscriptions/1/poster", base + "/subtitles/" + url.PathEscape("Library Fixture S01E01.chs.srt")} {
		code, _ := nativeRequest(t, r, "GET", path, "", "", "")
		if code != 401 {
			t.Fatalf("private media %s: %d", path, code)
		}
	}
	code, _ := nativeRequest(t, r, "PUT", base+"/playback", `{"watched":true}`, "agent-token-value", "")
	if code != 403 {
		t.Fatal("AI modified watch state")
	}
	code, _ = nativeRequest(t, r, "PUT", base+"/playback", `{"watched":true}`, "operator-token-value", "https://evil.example")
	if code != 403 {
		t.Fatal("cross origin modified state")
	}
	readOnly := nativeRig(t, func(_ *Config, j *jobs.Config) {
		if j != nil {
			j.ReadOnlyConsole = true
		}
	})
	_, base = seedPlayback(t, readOnly)
	code, _ = nativeRequest(t, readOnly, "PUT", base+"/playback", `{"watched":true}`, "", "")
	if code != 403 {
		t.Fatal("readonly modified state")
	}
	for i := 0; i < cap(readOnly.srv.streamSlots); i++ {
		readOnly.srv.streamSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(readOnly.srv.streamSlots); i++ {
			<-readOnly.srv.streamSlots
		}
	}()
	for _, path := range []string{base + "/video", "/api/v1/anime/subscriptions/1/poster", base + "/subtitles/" + url.PathEscape("Library Fixture S01E01.chs.srt")} {
		code, _ := nativeRequest(t, readOnly, "GET", path, "", "", "")
		if code != 503 {
			t.Fatalf("saturated media stream: %d", code)
		}
	}
}
