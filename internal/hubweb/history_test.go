package hubweb

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
)

func TestHistoryExportAndConfirmedCleanupHTTPPreserveOriginals(t *testing.T) {
	r := nativeRig(t, nil)
	_, _ = seedPlayback(t, r)
	response, body := r.get(t, "/api/v1/anime/history/export?limit=20000&subscription=1")
	if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatal(response.StatusCode, string(body))
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	var footer map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &footer); err != nil || footer["complete"] != true || footer["episodes"] != float64(1) {
		t.Fatal(footer, err)
	}
	response, _ = r.get(t, "/api/v1/anime/history/export?limit=20001")
	if response.StatusCode != 400 {
		t.Fatal("export cap ignored")
	}
	if _, err := r.srv.Anime.SetSubscriptionArchived(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	endpoint := "/api/v1/anime/subscriptions/1/history/cleanup"
	input := `{"confirm_title":"Library Fixture","limit":1}`
	code, _ := fileRequest(t, r, "POST", endpoint, input, "", "", false)
	if code != 400 {
		t.Fatal("unconfirmed cleanup", code)
	}
	code, b := fileRequest(t, r, "POST", endpoint, input, "", "", true)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	job := waitFileJob(t, r, b)
	if job.State != domain.JobCompleted {
		t.Fatal(job)
	}
	var status struct{ Episodes, Dedup int64 }
	response = r.getJSON(t, "/api/v1/anime/subscriptions/1/history", &status)
	if response.StatusCode != 200 || status.Episodes != 0 || status.Dedup != 1 {
		t.Fatal(status)
	}
	path := filepath.Join(r.srv.Anime.Settings().LibraryDir, "Library Fixture", "Season 1", "Library Fixture S01E01.mp4")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "0123456789" {
		t.Fatal("history cleanup changed video", string(data), err)
	}
	code, b = fileRequest(t, r, "POST", endpoint, `{"confirm_title":"Library Fixture","limit":1,"remove_subscription":true}`, "", "", true)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	job = waitFileJob(t, r, b)
	if job.State != domain.JobCompleted {
		t.Fatal(job)
	}
	response, _ = r.get(t, "/api/v1/anime/subscriptions/1/history")
	if response.StatusCode != 404 {
		t.Fatal("subscription not removed", response.StatusCode)
	}
}

func TestHistoryExportIsOperatorOnlyAndCleanupHonorsReadOnly(t *testing.T) {
	r := nativeRig(t, func(c *Config, j *jobs.Config) {
		if j != nil {
			j.ReadOnlyConsole = true
		} else {
			c.Token = "operator"
			c.Agents = []AgentCredential{{Name: "helper", Token: "agent", AllowHeavy: true}}
		}
	})
	for _, path := range []string{"/api/v1/anime/history/export?limit=20000", "/api/v1/anime/subscriptions/1/history"} {
		code, _ := nativeRequest(t, r, "GET", path, "", "agent", "")
		if code != 403 {
			t.Fatal("agent history access", code)
		}
	}
	code, _ := fileRequest(t, r, "POST", "/api/v1/anime/subscriptions/1/history/cleanup", `{"limit":1}`, "operator", "", true)
	if code != 403 {
		t.Fatal("readonly cleanup", code)
	}
}
