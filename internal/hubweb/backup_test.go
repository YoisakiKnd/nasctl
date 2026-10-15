package hubweb

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/backuplocal"
	"github.com/YoisakiKnd/nasctl/internal/backups"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
)

func backupRig(t *testing.T, tune func(*Config, *jobs.Config)) (*rig, string, *backuplocal.Control) {
	r := writeRig(t, tune)
	archive, recovered := t.TempDir(), t.TempDir()
	fm, err := files.New([]config.HubStorage{{ID: "media", Path: r.root, Writable: true}, {ID: "archive", Path: archive, Writable: true}, {ID: "recovered", Path: recovered, Writable: true}}, files.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	c, err := backuplocal.New(t.TempDir(), fm)
	if err != nil {
		t.Fatal(err)
	}
	r.srv.Files = fm
	r.srv.Backups = c
	t.Cleanup(func() { c.Close(); fm.Close() })
	t.Cleanup(r.srv.WaitWork)
	if err = os.Mkdir(filepath.Join(r.root, "backup-source"), 0750); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(r.root, "backup-source", "data.bin"), []byte("HTTP backup fixture"), 0640); err != nil {
		t.Fatal(err)
	}
	return r, recovered, c
}
func backupResult(t *testing.T, r *rig, job *hubstore.Job) backups.Result {
	t.Helper()
	events, err := r.store.JobEvents(context.Background(), job.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		var v struct {
			Kind   string         `json:"kind"`
			Result backups.Result `json:"result"`
		}
		if json.Unmarshal([]byte(event.Note), &v) == nil && v.Kind == "backup.result" {
			return v.Result
		}
	}
	t.Fatal("missing durable backup result", events)
	return backups.Result{}
}
func TestBackupHTTPConfigureRunVerifyRestoreAndDelete(t *testing.T) {
	r, recovered, c := backupRig(t, nil)
	p := backups.Plan{ID: "daily", Name: "HTTP daily", Root: "media", Path: "backup-source", ToRoot: "archive", Limit: 100, MaxBytes: 1024, Keep: 2, MaxRuntime: "1m"}
	body, _ := json.Marshal(p)
	code, _ := fileRequest(t, r, "POST", "/api/v1/backup/plans", string(body), "", "", false)
	if code != 400 || len(c.Settings().Plans) != 0 {
		t.Fatal("unconfirmed", code)
	}
	code, b := fileRequest(t, r, "POST", "/api/v1/backup/plans", string(body), "", "", true)
	if code != 200 {
		t.Fatalf("save %d %s", code, b)
	}
	var settings backups.Settings
	if err := json.Unmarshal(b, &settings); err != nil || len(settings.Plans) != 1 || settings.Plans[0].SourceScope == "" || settings.Digests[p.ID] != settings.Plans[0].Digest() {
		t.Fatal(settings, err)
	}
	run, _ := json.Marshal(map[string]string{"plan": p.ID, "plan_sha256": settings.Digests[p.ID]})
	code, b = fileRequest(t, r, "POST", "/api/v1/backup/run", string(run), "", "", true)
	if code != 202 {
		t.Fatalf("run %d %s", code, b)
	}
	job := waitFileJob(t, r, b)
	if job.State != domain.JobCompleted {
		t.Fatal(job)
	}
	result := backupResult(t, r, job)
	if !result.Published || result.Snapshot == nil {
		t.Fatal(result)
	}
	snapshot := *result.Snapshot
	code, b = fileRequest(t, r, "GET", "/api/v1/backup/history?root=archive", "", "", "", false)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	var history backups.History
	if err := json.Unmarshal(b, &history); err != nil || len(history.Snapshots) != 1 || history.Snapshots[0].ID != snapshot.ID {
		t.Fatal(history, err)
	}
	code, _ = fileRequest(t, r, "GET", "/api/v1/files/download?root=archive&path=.nasctl-backups/"+snapshot.ID+"/data/data.bin", "", "", "", false)
	if code != 403 {
		t.Fatal("private namespace", code)
	}
	verify, _ := json.Marshal(map[string]string{"root": "archive", "id": snapshot.ID})
	code, b = fileRequest(t, r, "POST", "/api/v1/backup/verify", string(verify), "", "", true)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	if job = waitFileJob(t, r, b); job.State != domain.JobCompleted {
		t.Fatal(job)
	}
	restore, _ := json.Marshal(backups.Restore{Root: "archive", ID: snapshot.ID, ToRoot: "recovered", To: "copy", ManifestSHA256: snapshot.ManifestSHA256})
	code, b = fileRequest(t, r, "POST", "/api/v1/backup/restore", string(restore), "", "", true)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	job = waitFileJob(t, r, b)
	if job.State != domain.JobCompleted || !backupResult(t, r, job).Published {
		t.Fatal(job)
	}
	bytes, err := os.ReadFile(filepath.Join(recovered, "copy", "data.bin"))
	if err != nil || string(bytes) != "HTTP backup fixture" {
		t.Fatal(string(bytes), err)
	}
	code, b = fileRequest(t, r, "POST", "/api/v1/backup/restore", string(restore), "", "", true)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	if job = waitFileJob(t, r, b); job.State != domain.JobFailed {
		t.Fatal("overwrite", job)
	}
	remove, _ := json.Marshal(map[string]string{"root": "archive", "id": snapshot.ID, "confirm_id": "wrong", "manifest_sha256": snapshot.ManifestSHA256})
	code, _ = fileRequest(t, r, "POST", "/api/v1/backup/delete", string(remove), "", "", true)
	if code != 400 {
		t.Fatal("delete confirmation", code)
	}
	remove, _ = json.Marshal(map[string]string{"root": "archive", "id": snapshot.ID, "confirm_id": snapshot.ID, "manifest_sha256": snapshot.ManifestSHA256})
	code, b = fileRequest(t, r, "POST", "/api/v1/backup/delete", string(remove), "", "", true)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	if job = waitFileJob(t, r, b); job.State != domain.JobCompleted {
		t.Fatal(job)
	}
	for _, path := range []string{filepath.Join(r.root, "backup-source", "data.bin"), filepath.Join(recovered, "copy", "data.bin")} {
		bytes, err = os.ReadFile(path)
		if err != nil || string(bytes) != "HTTP backup fixture" {
			t.Fatal("delete affected original/restored", string(bytes), err)
		}
	}
}

func TestBackupHTTPReadOnlyAndAIIdentity(t *testing.T) {
	for _, readonly := range []bool{true, false} {
		r, _, _ := backupRig(t, func(c *Config, j *jobs.Config) {
			if readonly && j != nil {
				j.ReadOnlyConsole = true
			}
			if !readonly && c != nil {
				c.Token = "operator-backup-token"
				c.Agents = []AgentCredential{{Name: "helper", Token: "helper-backup-token"}}
			}
		})
		token := ""
		if !readonly {
			token = "helper-backup-token"
		}
		code, _ := fileRequest(t, r, "POST", "/api/v1/backup/run", `{"plan":"daily","plan_sha256":"scope"}`, token, "", true)
		if code != 403 {
			t.Fatal("write identity", code)
		}
		code, _ = fileRequest(t, r, "GET", "/api/v1/backup/settings", "", token, "", false)
		want := 200
		if !readonly {
			want = 403
		}
		if code != want {
			t.Fatal("read identity", code, want)
		}
	}
}
