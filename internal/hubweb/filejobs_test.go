package hubweb

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

func waitFileJob(t *testing.T, r *rig, body []byte) *hubstore.Job {
	t.Helper()
	var accepted struct {
		ID string `json:"job_id"`
	}
	if err := json.Unmarshal(body, &accepted); err != nil || accepted.ID == "" {
		t.Fatalf("missing job: %s %v", body, err)
	}
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		job, err := r.store.Job(context.Background(), accepted.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.State.Terminal() {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return nil
}
func TestFileBackgroundCopyMoveAndPurgeHTTP(t *testing.T) {
	r := writeRig(t, nil)
	destination := t.TempDir()
	fm, err := files.New([]config.HubStorage{{ID: "media", Path: r.root, Writable: true}, {ID: "other", Path: destination, Writable: true}}, files.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	r.srv.Files = fm
	t.Cleanup(func() { fm.Close() })
	t.Cleanup(r.srv.WaitWork)
	if err = os.WriteFile(filepath.Join(r.root, "source.txt"), []byte("copy contents"), 0640); err != nil {
		t.Fatal(err)
	}
	copyBody := `{"root":"media","to_root":"other","from":"source.txt","to":"copy.txt","limit":100,"max_bytes":1024}`
	code, _ := fileRequest(t, r, "POST", "/api/v1/files/copy", copyBody, "", "", false)
	if code != 400 {
		t.Fatal("unconfirmed", code)
	}
	code, b := fileRequest(t, r, "POST", "/api/v1/files/copy", copyBody, "", "", true)
	if code != 202 {
		t.Fatalf("%d %s", code, b)
	}
	job := waitFileJob(t, r, b)
	if job.State != domain.JobCompleted || job.Progress != 13 || job.Total != 13 || job.Unit != "bytes" {
		t.Fatalf("%+v", job)
	}
	content, err := os.ReadFile(filepath.Join(destination, "copy.txt"))
	if err != nil || string(content) != "copy contents" {
		t.Fatal(string(content), err)
	}
	code, _ = fileRequest(t, r, "POST", "/api/v1/files/copy", copyBody, "", "", true)
	if code != 409 {
		t.Fatal("overwrite", code)
	}
	code, b = fileRequest(t, r, "POST", "/api/v1/files/move", strings.Replace(copyBody, "copy.txt", "moved.txt", 1), "", "", true)
	if code != 202 {
		t.Fatalf("%d %s", code, b)
	}
	job = waitFileJob(t, r, b)
	if job.State != domain.JobCompleted {
		t.Fatal(job.Err)
	}
	trash, err := fm.ListTrash("media", 1, 50)
	if err != nil || len(trash.Items) != 1 {
		t.Fatal(trash, err)
	}
	item := trash.Items[0]
	purgeBody, _ := json.Marshal(map[string]any{"root": "media", "id": item.ID, "confirm_name": "wrong", "limit": 100})
	code, b = fileRequest(t, r, "POST", "/api/v1/files/purge", string(purgeBody), "", "", true)
	if code != 202 {
		t.Fatalf("%d %s", code, b)
	}
	if job = waitFileJob(t, r, b); job.State != domain.JobFailed {
		t.Fatal("name check missing", job)
	}
	trash, err = fm.ListTrash("media", 1, 50)
	if err != nil || len(trash.Items) != 1 {
		t.Fatal("wrong name deleted", trash, err)
	}
	purgeBody, _ = json.Marshal(map[string]any{"root": "media", "id": item.ID, "confirm_name": item.Name, "limit": 100})
	code, b = fileRequest(t, r, "POST", "/api/v1/files/purge", string(purgeBody), "", "", true)
	if code != 202 {
		t.Fatalf("%d %s", code, b)
	}
	if job = waitFileJob(t, r, b); job.State != domain.JobCompleted || job.Progress != 1 || job.Total != 1 {
		t.Fatal(job)
	}
	trash, err = fm.ListTrash("media", 1, 50)
	if err != nil || len(trash.Items) != 0 {
		t.Fatal(trash, err)
	}
	content, err = os.ReadFile(filepath.Join(destination, "moved.txt"))
	if err != nil || string(content) != "copy contents" {
		t.Fatal("purge affected target", err)
	}
}
func TestCancelOnlyActiveWorkAndPersistCancelledState(t *testing.T) {
	r := writeRig(t, nil)
	t.Cleanup(r.srv.WaitWork)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accepted := make(chan string, 1)
	done := make(chan jobs.Outcome, 1)
	if !r.srv.beginWork() {
		t.Fatal("begin")
	}
	go func() {
		defer r.srv.work.Done()
		var id string
		defer func() { r.srv.unregisterActive(id) }()
		done <- r.srv.Jobs.Run(ctx, jobs.RunOptions{Principal: domain.Principal{Kind: domain.PrincipalUser}, OpID: "files.copy", Target: "media", OperatorConsent: true, Args: operations.NewArgs().Str("root", "media").Str("to_root", "media").Str("from", "from").Str("to", "to").Int("limit", 10).Int("max_bytes", 1024), OnAccepted: func(job *hubstore.Job) { id = job.ID; r.srv.registerActive(id, cancel); accepted <- id }, Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
			<-ctx.Done()
			return jobs.Result{}, ctx.Err()
		}})
	}()
	id := <-accepted
	response, b := r.get(t, "/api/v1/jobs/"+id)
	if response.StatusCode != 200 || !strings.Contains(string(b), `"cancellable":true`) {
		t.Fatal(string(b))
	}
	code, _ := fileRequest(t, r, "POST", "/api/v1/jobs/"+id+"/cancel", "{}", "", "", false)
	if code != 400 {
		t.Fatal(code)
	}
	code, b = fileRequest(t, r, "POST", "/api/v1/jobs/"+id+"/cancel", "{}", "", "", true)
	if code != 202 {
		t.Fatalf("%d %s", code, b)
	}
	select {
	case out := <-done:
		if out.Job == nil || out.Job.State != domain.JobCancelled {
			t.Fatal(out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not finish")
	}
	code, _ = fileRequest(t, r, "POST", "/api/v1/jobs/"+id+"/cancel", "{}", "", "", true)
	if code != 409 {
		t.Fatal("terminal task cancelled", code)
	}
	code, _ = fileRequest(t, r, "POST", "/api/v1/jobs/nonexistent/cancel", "{}", "", "", true)
	if code != 409 {
		t.Fatal(code)
	}
}

func TestFileExtractZipHTTP(t *testing.T) {
	r := writeRig(t, nil)
	t.Cleanup(r.srv.WaitWork)
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	f, _ := zw.Create("album/1.txt")
	_, _ = f.Write([]byte("hello"))
	_ = zw.Close()
	writeFile(t, filepath.Join(r.root, "album.zip"), b.String())
	body := `{"root":"media","from":"album.zip","to":"album","limit":100,"max_bytes":1024}`
	if code, _ := fileRequest(t, r, "POST", "/api/v1/files/extract", body, "", "", false); code != 400 {
		t.Fatal("unconfirmed extract", code)
	}
	code, out := fileRequest(t, r, "POST", "/api/v1/files/extract", body, "", "", true)
	if code != 202 {
		t.Fatalf("%d %s", code, out)
	}
	job := waitFileJob(t, r, out)
	if job.State != domain.JobCompleted || job.Progress != 5 || job.Total != 5 || job.Unit != "bytes" {
		t.Fatalf("%+v", job)
	}
	if got, err := os.ReadFile(filepath.Join(r.root, "album", "album", "1.txt")); err != nil || string(got) != "hello" {
		t.Fatal(string(got), err)
	}
	// The folder now exists; a second extract is refused before it starts.
	if code, out = fileRequest(t, r, "POST", "/api/v1/files/extract", body, "", "", true); code != 409 {
		t.Fatalf("extract over a folder: %d %s", code, out)
	}
	// Over the server's own budget, or from a folder rather than a file.
	if code, _ = fileRequest(t, r, "POST", "/api/v1/files/extract", `{"root":"media","from":"album.zip","to":"x","limit":100000000,"max_bytes":1024}`, "", "", true); code != 400 {
		t.Fatal("over budget", code)
	}
	if code, _ = fileRequest(t, r, "POST", "/api/v1/files/extract", `{"root":"media","from":"album","to":"x","limit":100,"max_bytes":1024}`, "", "", true); code == 202 {
		t.Fatal("extracted a folder")
	}
}
