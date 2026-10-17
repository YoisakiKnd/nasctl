package hubweb

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
)

type albumControlFake struct {
	mu         sync.Mutex
	configured int
	started    chan struct{}
	block      bool
}

func (f *albumControlFake) Configuration() album.Configuration {
	return album.Configuration{Directories: []album.Directory{}, WorkerAvailable: true}
}
func (f *albumControlFake) Configure(context.Context, album.Directory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configured++
	return nil
}
func (f *albumControlFake) RemoveDirectory(context.Context, string) error { return nil }
func (f *albumControlFake) PrivateFolders() []album.Folder                { return nil }
func (f *albumControlFake) Run(ctx context.Context, w album.Work, report func(album.WorkProgress)) (album.WorkResult, error) {
	if report != nil {
		report(album.WorkProgress{Processed: 2, Total: 3, Detail: "fixture progress"})
	}
	if f.started != nil {
		close(f.started)
	}
	if f.block {
		<-ctx.Done()
		return album.WorkResult{}, ctx.Err()
	}
	return album.WorkResult{Completed: true, Detail: "fixture complete"}, nil
}

func TestAlbumOperatorConfigurationValidationAndAudit(t *testing.T) {
	r := newRig(t, nil)
	fake := &albumControlFake{}
	r.srv.AlbumControl = fake
	t.Cleanup(r.srv.WaitWork)
	body := `{"code":"photos","name":"Photos","root":"media","path":"sub","enabled":true,"allow_shared_volume":false}`
	code, _ := fileRequest(t, r, "POST", "/api/v1/album/directories", body, "", "", false)
	if code != 400 {
		t.Fatal("unconfirmed", code)
	}
	code, b := fileRequest(t, r, "POST", "/api/v1/album/directories", body, "", "", true)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	fake.mu.Lock()
	count := fake.configured
	fake.mu.Unlock()
	if count != 1 {
		t.Fatal(count)
	}
	code, _ = fileRequest(t, r, "POST", "/api/v1/album/directories", `{"code":"photos","unexpected":true}`, "", "", true)
	if code != 400 {
		t.Fatal("unknown field", code)
	}
	for _, input := range []string{`{"collection":"photos","limit":0,"max_runtime":"1s"}`, `{"collection":"photos","limit":1,"max_runtime":"13h"}`, `{"collection":"photos","limit":1,"max_runtime":"1s","Operation":"album.resume"}`} {
		code, _ = fileRequest(t, r, "POST", "/api/v1/album/tasks/scan", input, "", "", true)
		if code != 400 {
			t.Fatal(input, code)
		}
	}
	_, audit := r.get(t, "/api/v1/audit?limit=50")
	var log struct {
		Entries []struct{ Action, Op, Params string }
	}
	if err := json.Unmarshal(audit, &log); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range log.Entries {
		if row.Op == "album.configure" && row.Action == "approval.confirm" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing configuration consent audit", string(audit))
	}
}

func TestAlbumTaskTrackedProgressAndCancellation(t *testing.T) {
	r := newRig(t, nil)
	fake := &albumControlFake{block: true, started: make(chan struct{})}
	r.srv.AlbumControl = fake
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.srv.WorkContext = ctx
	t.Cleanup(func() { cancel(); r.srv.WaitWork() })
	code, b := fileRequest(t, r, "POST", "/api/v1/album/tasks/scan", `{"collection":"photos","limit":3,"max_runtime":"10s"}`, "", "", true)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	var accepted struct {
		ID string `json:"job_id"`
	}
	if err := json.Unmarshal(b, &accepted); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	job, err := r.store.Job(context.Background(), accepted.ID)
	if err != nil || job.Progress != 2 || job.Total != 3 {
		t.Fatal(job, err)
	}
	code, _ = fileRequest(t, r, "POST", "/api/v1/jobs/"+accepted.ID+"/cancel", `{}`, "", "", true)
	if code != 202 {
		t.Fatal(code)
	}
	job = waitFileJob(t, r, b)
	if job.State != domain.JobCancelled {
		t.Fatal(job)
	}
}

func TestAlbumManagementCannotBypassReadOnlyOrAgentIdentity(t *testing.T) {
	for _, readonly := range []bool{false, true} {
		r := newRig(t, func(c *Config, j *jobs.Config) {
			if j != nil {
				j.ReadOnlyConsole = readonly
			} else {
				c.Token = "operator"
				c.Agents = []AgentCredential{{Name: "helper", Token: "agent", AllowHeavy: true}}
			}
		})
		r.srv.AlbumControl = &albumControlFake{}
		token := "agent"
		if readonly {
			token = "operator"
		}
		code, _ := fileRequest(t, r, "POST", "/api/v1/album/tasks/pause", `{}`, token, "", true)
		if code != 403 {
			t.Fatal(readonly, code)
		}
	}
}
