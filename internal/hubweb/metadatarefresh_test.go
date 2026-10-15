package hubweb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/anime"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
)

func TestMetadataRefreshRestoreAndBackupExportHTTP(t *testing.T) {
	r := nativeRig(t, nil)
	_, _ = seedPlayback(t, r)
	series := filepath.Join(r.srv.Anime.Settings().LibraryDir, "Library Fixture", "tvshow.nfo")
	old := "<tvshow>old custom metadata</tvshow>"
	if err := os.WriteFile(series, []byte(old), 0640); err != nil {
		t.Fatal(err)
	}
	endpoint := "/api/v1/anime/subscriptions/1/metadata"
	for _, body := range []string{`{"limit":0}`, `{"limit":51}`, `{"limit":1,"unknown":true}`} {
		code, _ := fileRequest(t, r, "POST", endpoint, body, "", "", true)
		if code != 400 {
			t.Fatal(body, code)
		}
	}
	code, _ := fileRequest(t, r, "POST", endpoint, `{"limit":1}`, "", "", false)
	if code != 400 {
		t.Fatal("unconfirmed", code)
	}
	code, b := fileRequest(t, r, "POST", endpoint, `{"limit":1,"replace":true}`, "", "", true)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	job := waitFileJob(t, r, b)
	if job.State != domain.JobCompleted {
		t.Fatal(job)
	}
	var backups struct {
		Items []anime.MetadataBackup `json:"backups"`
	}
	resp := r.getJSON(t, "/api/v1/anime/metadata/backups", &backups)
	if resp.StatusCode != 200 || len(backups.Items) != 1 {
		t.Fatal(resp.StatusCode, backups)
	}
	id := backups.Items[0].ID
	response, data := r.get(t, "/api/v1/anime/metadata/backups/"+id+"/download")
	if response.StatusCode != 200 || string(data) != old || !strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatal(response.StatusCode, string(data))
	}
	code, b = fileRequest(t, r, "POST", "/api/v1/anime/metadata/backups/"+id+"/restore", `{}`, "", "", true)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	job = waitFileJob(t, r, b)
	if job.State != domain.JobCompleted {
		t.Fatal(job)
	}
	if data, _ := os.ReadFile(series); string(data) != old {
		t.Fatal("restore bytes", string(data))
	}
	code, b = fileRequest(t, r, "DELETE", "/api/v1/anime/metadata/backups/"+id, `{"sha256":"`+backups.Items[0].SHA256+`"}`, "", "", true)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	if _, err := r.srv.Anime.Store.Episode(context.Background(), 1); err != nil {
		t.Fatal("metadata work deleted episode", err)
	}
}

func TestMetadataMutationCannotBypassReadOnlyOrAgent(t *testing.T) {
	for _, readonly := range []bool{false, true} {
		r := nativeRig(t, func(c *Config, j *jobs.Config) {
			if j != nil {
				j.ReadOnlyConsole = readonly
			} else {
				c.Token = "operator"
				c.Agents = []AgentCredential{{Name: "helper", Token: "agent", AllowHeavy: true}}
			}
		})
		token := "agent"
		if readonly {
			token = "operator"
		}
		for _, path := range []string{"/api/v1/anime/subscriptions/1/metadata", "/api/v1/anime/metadata/backups/" + strings.Repeat("a", 64) + "/restore"} {
			code, _ := fileRequest(t, r, "POST", path, `{"limit":1}`, token, "", true)
			if code != 403 {
				t.Fatal(readonly, path, code)
			}
		}
	}
}
