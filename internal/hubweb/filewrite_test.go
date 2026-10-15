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

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
)

func writeRig(t *testing.T, tune func(*Config, *jobs.Config)) *rig {
	r := newRig(t, tune)
	limits := files.DefaultLimits()
	limits.UploadMaxBytes = 32
	fm, err := files.New([]config.HubStorage{{ID: "media", Path: r.root, Writable: true}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	r.srv.Files = fm
	t.Cleanup(func() { fm.Close() })
	return r
}
func fileRequest(t *testing.T, r *rig, method, path, body, token, origin string, confirmed bool) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, r.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if confirmed {
		request.Header.Set("X-Nasctl-Confirm", "1")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := r.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	b, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, b
}
func TestFileManagementHTTPWorkflowAndConfirmation(t *testing.T) {
	r := writeRig(t, nil)
	code, _ := fileRequest(t, r, "POST", "/api/v1/files/mkdir", `{"root":"media","path":"new"}`, "", "", false)
	if code != 400 {
		t.Fatal("unconfirmed write ran", code)
	}
	code, b := fileRequest(t, r, "POST", "/api/v1/files/mkdir", `{"root":"media","path":"new"}`, "", "", true)
	if code != 200 {
		t.Fatalf("mkdir: %d %s", code, b)
	}
	code, b = fileRequest(t, r, "PUT", "/api/v1/files/upload?root=media&path=new/hello.txt", "hello", "", "", true)
	if code != 200 {
		t.Fatalf("upload: %d %s", code, b)
	}
	code, _ = fileRequest(t, r, "PUT", "/api/v1/files/upload?root=media&path=new/hello.txt", "other", "", "", true)
	if code != 409 {
		t.Fatal("same-name upload overwritten", code)
	}
	code, b = fileRequest(t, r, "POST", "/api/v1/files/rename", `{"root":"media","from":"new/hello.txt","to":"new/renamed.txt"}`, "", "", true)
	if code != 200 {
		t.Fatalf("rename: %d %s", code, b)
	}
	code, b = fileRequest(t, r, "POST", "/api/v1/files/trash", `{"root":"media","path":"new/renamed.txt"}`, "", "", true)
	if code != 200 {
		t.Fatalf("trash: %d %s", code, b)
	}
	var result struct {
		Result files.TrashItem `json:"result"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	res, b := r.get(t, "/api/v1/files/trash?root=media")
	if res.StatusCode != 200 || !strings.Contains(string(b), result.Result.ID) {
		t.Fatalf("trash listing: %d %s", res.StatusCode, b)
	}
	code, b = fileRequest(t, r, "POST", "/api/v1/files/restore", `{"root":"media","id":"`+result.Result.ID+`"}`, "", "", true)
	if code != 200 {
		t.Fatalf("restore: %d %s", code, b)
	}
	content, err := os.ReadFile(filepath.Join(r.root, "new", "renamed.txt"))
	if err != nil || string(content) != "hello" {
		t.Fatal("restored content changed", err)
	}
	res, b = r.get(t, "/api/v1/files/roots")
	if res.StatusCode != 200 || !strings.Contains(string(b), `"writable":true`) {
		t.Fatal("capabilities absent")
	}
	code, _ = fileRequest(t, r, "PUT", "/api/v1/files/upload?root=media&path=too-large", strings.Repeat("x", 33), "", "", true)
	if code != 413 {
		t.Fatal("upload size bound absent", code)
	}
	code, _ = fileRequest(t, r, "POST", "/api/v1/files/mkdir", `{"root":"media","path":"../outside/changed"}`, "", "", true)
	if code != 403 {
		t.Fatal("outside write accepted", code)
	}
	audit, err := r.store.Audit(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range audit {
		if entry.Action == "approval.confirm" && entry.Op == "files.upload" {
			found = true
		}
	}
	if !found {
		t.Fatal("operator confirmation not audited")
	}
}
func TestFileWritesRefuseAgentReadonlyAndCrossOrigin(t *testing.T) {
	r := writeRig(t, func(c *Config, _ *jobs.Config) {
		c.Token = "operator-token-value"
		c.Agents = []AgentCredential{{Name: "agent", Token: "agent-token-value", AllowHeavy: true}}
	})
	for _, endpoint := range []string{"mkdir", "rename", "trash", "restore", "copy", "move", "purge"} {
		code, _ := fileRequest(t, r, "POST", "/api/v1/files/"+endpoint, `{"root":"media","path":"new"}`, "agent-token-value", "", true)
		if code != 403 {
			t.Fatal("AI confirmation bypassed", endpoint, code)
		}
	}
	code, _ := fileRequest(t, r, "PUT", "/api/v1/files/upload?root=media&path=new", "content", "agent-token-value", "", true)
	if code != 403 {
		t.Fatal("AI uploaded", code)
	}
	code, _ = fileRequest(t, r, "POST", "/api/v1/files/mkdir", `{"root":"media","path":"new"}`, "operator-token-value", "https://evil.example", true)
	if code != 403 {
		t.Fatal("cross-origin write accepted", code)
	}
	readonly := writeRig(t, func(_ *Config, j *jobs.Config) {
		if j != nil {
			j.ReadOnlyConsole = true
		}
	})
	code, _ = fileRequest(t, readonly, "POST", "/api/v1/files/mkdir", `{"root":"media","path":"new"}`, "", "", true)
	if code != 403 {
		t.Fatal("global readonly bypassed", code)
	}
	plain := newRig(t, nil)
	code, _ = fileRequest(t, plain, "POST", "/api/v1/files/mkdir", `{"root":"media","path":"new"}`, "", "", true)
	if code != 403 {
		t.Fatal("root readonly bypassed", code)
	}
}

func TestStoppingServiceInterruptsUploadAndRecordsCancellation(t *testing.T) {
	r := writeRig(t, nil)
	work, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	r.srv.WorkContext = work
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	request, err := http.NewRequest("PUT", r.ts.URL+"/api/v1/files/upload?root=media&path=unfinished.txt", reader)
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = 16
	request.Header.Set("X-Nasctl-Confirm", "1")
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		response, err := r.client.Do(request)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	if _, err = writer.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, _ := os.ReadDir(filepath.Join(r.root, ".nasctl-staging"))
		if len(entries) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upload did not enter staging")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelWork()
	finished := make(chan struct{})
	go func() { r.srv.WaitWork(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not interrupt a blocked upload")
	}
	_ = writer.Close()
	select {
	case <-clientDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not finish")
	}
	if _, err = os.Stat(filepath.Join(r.root, "unfinished.txt")); !os.IsNotExist(err) {
		t.Fatal("partial file published")
	}
	jobs, err := r.store.Jobs(context.Background(), hubstore.JobFilter{Op: "files.upload", Limit: 10})
	if err != nil || len(jobs) != 1 || jobs[0].State != domain.JobCancelled {
		t.Fatalf("upload terminal state: %+v %v", jobs, err)
	}
}
