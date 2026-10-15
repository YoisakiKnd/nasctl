package hubweb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/anime"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
)

func nativeRig(t *testing.T, tune func(*Config, *jobs.Config)) *rig {
	t.Helper()
	r := newRig(t, tune)
	base := t.TempDir()
	download := filepath.Join(base, "downloads")
	library := filepath.Join(base, "library")
	for _, p := range []string{download, library} {
		if err := os.Mkdir(p, 0750); err != nil {
			t.Fatal(err)
		}
	}
	e, err := anime.Open(config.Anime{Enabled: true, QBittorrentURL: "http://127.0.0.1:9999", DownloadDir: download, LibraryDir: library}, filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	r.srv.Anime = e
	ctx, cancel := context.WithCancel(context.Background())
	r.srv.WorkContext = ctx
	t.Cleanup(func() { cancel(); r.srv.WaitWork(); e.Close() })
	return r
}

func nativeRequest(t *testing.T, r *rig, method, path, body, token, origin string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, r.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	res, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, b
}

func TestNativeSubscriptionsArePersistedAndValidated(t *testing.T) {
	r := nativeRig(t, nil)
	code, b := nativeRequest(t, r, "POST", "/api/v1/anime/subscriptions", `{"title":"Test Show","feed_url":"https://example.com/rss","season":1,"enabled":true}`, "", "")
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, b)
	}
	var sub anime.Subscription
	if err := json.Unmarshal(b, &sub); err != nil {
		t.Fatal(err)
	}
	if sub.AutoDownload || sub.ID == 0 {
		t.Fatalf("sub=%+v", sub)
	}
	res, _ := r.get(t, "/api/v1/anime/subscriptions")
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	code, b = nativeRequest(t, r, "POST", "/api/v1/anime/subscriptions", `{"title":"../escape","feed_url":"https://example.com/rss","season":1}`, "", "")
	if code != 400 {
		t.Fatalf("unsafe title code=%d body=%s", code, b)
	}
	code, _ = nativeRequest(t, r, "POST", "/api/v1/anime/subscriptions", `{"title":"Test","feed_url":"https://example.com/rss","unknown":true}`, "", "")
	if code != 400 {
		t.Fatal("unknown fields accepted")
	}
	audit, err := r.store.Audit(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audit {
		if a.Op == "anime.subscription" {
			found = true
		}
	}
	if !found {
		t.Fatal("subscription save was not audited")
	}
}

func TestNativeRefreshCreatesFinishedBackgroundJob(t *testing.T) {
	r := nativeRig(t, nil)
	code, b := nativeRequest(t, r, "POST", "/api/v1/anime/refresh", "", "", "")
	if code != 202 {
		t.Fatalf("code=%d body=%s", code, b)
	}
	var accepted struct {
		ID string `json:"job_id"`
	}
	if err := json.Unmarshal(b, &accepted); err != nil || accepted.ID == "" {
		t.Fatalf("body=%s err=%v", b, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := r.store.Job(context.Background(), accepted.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.State == domain.JobCompleted {
			return
		}
		if job.State.Terminal() {
			t.Fatalf("unexpected job state: %+v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("refresh job did not complete")
}

func TestNativeWritesRefuseAgentsCrossOriginAndReadOnly(t *testing.T) {
	r := nativeRig(t, func(c *Config, j *jobs.Config) {
		c.Token = "operator-token-value"
		c.Agents = []AgentCredential{{Name: "agent", Token: "agent-token-value"}}
	})
	body := `{"title":"Test","feed_url":"https://example.com/rss","season":1}`
	for _, path := range []string{"/api/v1/anime/subscriptions", "/api/v1/anime/refresh", "/api/v1/anime/sync", "/api/v1/anime/preview", "/api/v1/anime/connection", "/api/v1/anime/episodes/1/ignore", "/api/v1/anime/episodes/1/queue", "/api/v1/anime/subscriptions/1/enabled", "/api/v1/anime/subscriptions/1/archive"} {
		code, b := nativeRequest(t, r, "POST", path, body, "agent-token-value", "")
		if code != 403 {
			t.Fatalf("path=%s code=%d body=%s", path, code, b)
		}
	}
	for _, path := range []string{"/api/v1/anime/metadata/search?q=Show", "/api/v1/anime/metadata/series/42"} {
		code, _ := nativeRequest(t, r, "GET", path, "", "agent-token-value", "")
		if code != 403 {
			t.Fatal("agent metadata lookup", code)
		}
	}
	for _, path := range []string{"/api/v1/anime/subscriptions/1/enabled", "/api/v1/anime/subscriptions/1/archive"} {
		code, _ := nativeRequest(t, r, "POST", path, body, "operator-token-value", "https://evil.example")
		if code != 403 {
			t.Fatal("cross origin state change", code)
		}
	}
	code, _ := nativeRequest(t, r, "POST", "/api/v1/anime/subscriptions", body, "operator-token-value", "https://evil.example")
	if code != 403 {
		t.Fatal("cross-origin write accepted")
	}
	ro := nativeRig(t, func(c *Config, j *jobs.Config) {
		if j != nil {
			j.ReadOnlyConsole = true
		}
	})
	code, _ = nativeRequest(t, ro, "POST", "/api/v1/anime/subscriptions", body, "", "")
	if code != 403 {
		t.Fatal("read-only console accepted subscription write")
	}
}

func TestAnimeManagementValidatesBodiesAndHidesCredentials(t *testing.T) {
	r := nativeRig(t, nil)
	code, b := nativeRequest(t, r, "GET", "/api/v1/anime/settings", "", "", "")
	if code != 200 {
		t.Fatalf("settings: %d %s", code, b)
	}
	for _, key := range []string{`"password"`, `"username"`, `"tmdb_token"`} {
		if strings.Contains(string(b), key) {
			t.Fatalf("settings exposed %s", key)
		}
	}
	for _, path := range []string{"/api/v1/anime/episodes?subscription=oops", "/api/v1/anime/refresh?subscription=oops", "/api/v1/anime/sync?subscription=-1"} {
		method := "POST"
		if strings.Contains(path, "episodes?") {
			method = "GET"
		}
		code, _ = nativeRequest(t, r, method, path, "", "", "")
		if code != 400 {
			t.Fatalf("%s accepted invalid subscription: %d", path, code)
		}
	}
	for _, body := range []string{
		`{"title":"Test","feed_url":"https://example.com/rss","include":"["}`,
		`{"title":"Test","feed_url":"https://example.com/rss","start_episode":5,"end_episode":2}`,
		`{"title":"Test","feed_url":"https://example.com/rss"} {}`,
	} {
		code, b = nativeRequest(t, r, "POST", "/api/v1/anime/preview", body, "", "")
		if code != 400 {
			t.Fatalf("preview accepted invalid draft: %d %s", code, b)
		}
	}
	code, b = nativeRequest(t, r, "POST", "/api/v1/anime/preview", `{"title":"Test","feed_url":"http://127.0.0.1:1/rss"}`, "", "")
	if code != 400 || !strings.Contains(string(b), "RSS") {
		t.Fatalf("private RSS not explained: %d %s", code, b)
	}
	rows, err := r.srv.Anime.Store.Subscriptions(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("preview persisted data: %+v %v", rows, err)
	}
}

func TestAnimeSyncCreatesJobWithoutFetchingFeed(t *testing.T) {
	r := nativeRig(t, nil)
	code, b := nativeRequest(t, r, "POST", "/api/v1/anime/subscriptions", `{"title":"Offline Feed","feed_url":"http://127.0.0.1:1/rss","enabled":true}`, "", "")
	if code != 200 {
		t.Fatalf("save: %d %s", code, b)
	}
	code, b = nativeRequest(t, r, "POST", "/api/v1/anime/sync", "", "", "")
	if code != 202 {
		t.Fatalf("sync: %d %s", code, b)
	}
	var accepted struct {
		ID string `json:"job_id"`
	}
	if err := json.Unmarshal(b, &accepted); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := r.store.Job(context.Background(), accepted.ID)
		if err == nil && job.State == domain.JobCompleted {
			if job.Op != "anime.sync" {
				t.Fatalf("wrong operation: %s", job.Op)
			}
			return
		}
		if err == nil && job.State == domain.JobFailed {
			t.Fatalf("sync contacted offline RSS: %+v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sync did not finish")
}
