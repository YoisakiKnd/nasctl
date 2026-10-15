package hubweb

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
)

func TestFileBatchHTTPMixedResultsPersistAndRefusals(t *testing.T) {
	r := writeRig(t, nil)
	t.Cleanup(r.srv.WaitWork)
	for _, name := range []string{"first.txt", "second.txt"} {
		if err := os.WriteFile(filepath.Join(r.root, name), []byte(name), 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(r.root, "target"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.root, "target", "first.txt"), []byte("preserve"), 0640); err != nil {
		t.Fatal(err)
	}
	body := `{"action":"copy","to_root":"media","directory":"target","items":[{"root":"media","path":"first.txt"},{"root":"media","path":"second.txt"}]}`
	code, _ := fileRequest(t, r, "POST", "/api/v1/files/batch", body, "", "", false)
	if code != 400 {
		t.Fatal(code)
	}
	code, b := fileRequest(t, r, "POST", "/api/v1/files/batch", body, "", "", true)
	if code != 202 {
		t.Fatalf("%d %s", code, b)
	}
	job := waitFileJob(t, r, b)
	if job.State != domain.JobFailed || job.Progress != 2 || job.Total != 2 {
		t.Fatal(job)
	}
	events, err := r.store.JobEvents(context.Background(), job.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var result files.BatchResult
	found := false
	for _, event := range events {
		var value struct {
			Kind   string            `json:"kind"`
			Result files.BatchResult `json:"result"`
		}
		if json.Unmarshal([]byte(event.Note), &value) == nil && value.Kind == "files.batch" {
			result = value.Result
			found = true
		}
	}
	if !found || result.Completed != 1 || result.Failed != 1 || result.Items[0].Code != "exists" {
		t.Fatal(result, events)
	}
	bytes, err := os.ReadFile(filepath.Join(r.root, "target", "first.txt"))
	if err != nil || string(bytes) != "preserve" {
		t.Fatal(string(bytes), err)
	}
	bytes, err = os.ReadFile(filepath.Join(r.root, "target", "second.txt"))
	if err != nil || string(bytes) != "second.txt" {
		t.Fatal(string(bytes), err)
	}
	for _, bad := range []string{strings.Replace(body, `"first.txt"`, `"../escape"`, 1), body + "{}", strings.Replace(body, `"action":"copy"`, `"action":"purge"`, 1)} {
		code, _ = fileRequest(t, r, "POST", "/api/v1/files/batch", bad, "", "", true)
		if code < 400 {
			t.Fatal("invalid accepted", code)
		}
	}
}

func TestFileBatchReadOnlyAIAndConfigLimits(t *testing.T) {
	for _, readonly := range []bool{true, false} {
		r := writeRig(t, func(c *Config, j *jobs.Config) {
			if readonly && j != nil {
				j.ReadOnlyConsole = true
			} else if !readonly && c != nil {
				c.Token = "operator-token-long"
				c.Agents = []AgentCredential{{Name: "helper", Token: "helper-token-long"}}
			}
		})
		token := ""
		if !readonly {
			token = "helper-token-long"
		}
		code, _ := fileRequest(t, r, "POST", "/api/v1/files/batch", `{"action":"trash","items":[{"root":"media","path":"none"}]}`, token, "", true)
		if code != 403 {
			t.Fatal("identity refusal", code)
		}
	}
	r := writeRig(t, nil)
	l := files.DefaultLimits()
	l.CopyMaxEntries = 3
	l.CopyMaxBytes = 10
	l.BatchMaxItems = 2
	l.PreviewMaxBytes = 1
	l.MaxPageSize = 2
	l.DefaultPageSize = 1
	fm, err := files.New([]config.HubStorage{{ID: "media", Path: r.root, Writable: true}}, l)
	if err != nil {
		t.Fatal(err)
	}
	r.srv.Files = fm
	t.Cleanup(func() { fm.Close() })
	for _, p := range []string{"1.jpg", "2.txt", "3.txt"} {
		if err := os.WriteFile(filepath.Join(r.root, p), []byte("xx"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	code, b := fileRequest(t, r, "GET", "/api/v1/files/list?root=media", "", "", "", false)
	if code != 200 || !strings.Contains(string(b), `"page_size":1`) {
		t.Fatal(code, string(b))
	}
	code, _ = fileRequest(t, r, "GET", "/api/v1/files/preview?root=media&path=1.jpg", "", "", "", false)
	if code != 413 {
		t.Fatal("preview bypass", code)
	}
	code, _ = fileRequest(t, r, "POST", "/api/v1/files/copy", `{"root":"media","to_root":"media","from":"1.jpg","to":"4.jpg","limit":4,"max_bytes":10}`, "", "", true)
	if code != 400 {
		t.Fatal("copy bypass", code)
	}
	code, _ = fileRequest(t, r, "POST", "/api/v1/files/batch", `{"action":"trash","items":[{"root":"media","path":"1.jpg"},{"root":"media","path":"2.txt"},{"root":"media","path":"3.txt"}]}`, "", "", true)
	if code != 400 {
		t.Fatal("selection bypass", code)
	}
}
