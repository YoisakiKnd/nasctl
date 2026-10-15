package nasctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
)

func TestCLIFileWritesConsentNoOverwriteAndAudit(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "files")
	if err := os.Mkdir(root, 0750); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(base, "config.toml")
	doc := fmt.Sprintf("[paths]\nstate_dir=%q\ncache_dir=%q\n[[hub.storage]]\nid=\"data\"\npath=%q\nwritable=true\n[hub.files]\nupload_max_bytes=4\n", filepath.Join(base, "state"), filepath.Join(base, "cache"), root)
	if err := os.WriteFile(cfgPath, []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(want int, args ...string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		code := Main(append([]string{"--config", cfgPath, "--json", "files"}, args...), Env{Stdout: &out, Stderr: &stderr})
		if code != want {
			t.Fatalf("args %v code %d want %d: %s %s", args, code, want, out.String(), stderr.String())
		}
		return out.String()
	}
	run(2, "mkdir", "--root", "data", "--path", "folder")
	if _, err := os.Stat(filepath.Join(root, "folder")); !os.IsNotExist(err) {
		t.Fatal("unconfirmed mutation", err)
	}
	run(0, "mkdir", "--root", "data", "--path", "folder", "--confirm")
	run(0, "mkdir", "-h")
	run(1, "mkdir", "--root", "data", "--path", "ai", "--confirm", "--agent", "helper")
	local := filepath.Join(base, "upload.txt")
	if err := os.WriteFile(local, []byte("data"), 0640); err != nil {
		t.Fatal(err)
	}
	// Upload is async in policy and therefore exercises the real Linux resource
	// gate on Linux; deterministic command mutation tests use synchronous ops.
	if err := os.WriteFile(filepath.Join(root, "folder", "a.txt"), []byte("keep"), 0640); err != nil {
		t.Fatal(err)
	}
	run(0, "rename", "--root", "data", "--from", "folder/a.txt", "--to", "folder/b.txt", "--confirm")
	if err := os.WriteFile(filepath.Join(root, "folder", "a.txt"), []byte("new"), 0640); err != nil {
		t.Fatal(err)
	}
	run(1, "rename", "--root", "data", "--from", "folder/a.txt", "--to", "folder/b.txt", "--confirm")
	b, err := os.ReadFile(filepath.Join(root, "folder", "b.txt"))
	if err != nil || string(b) != "keep" {
		t.Fatal(string(b), err)
	}
	result := run(0, "trash", "--root", "data", "--path", "folder/b.txt", "--confirm")
	var exported struct {
		Result struct {
			ID   string `json:"id"`
			Path string `json:"original_path"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(result), &exported); err != nil || exported.Result.Path != "folder/b.txt" || exported.Result.ID == "" {
		t.Fatal(result, err)
	}
	run(0, "restore", "--root", "data", "--id", exported.Result.ID, "--confirm")
	run(2, "batch", "--batch-file", local, "--confirm")
	st, err := hubstore.Open(filepath.Join(base, "state", "hub.db"), hubstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.Jobs(context.Background(), hubstore.JobFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	completed, failed := 0, 0
	for _, row := range rows {
		if row.State == domain.JobCompleted {
			completed++
		}
		if row.State == domain.JobFailed {
			failed++
		}
	}
	if completed != 4 || failed != 1 {
		t.Fatal("missing audited jobs", rows)
	}
	audit, err := st.Audit(context.Background(), 100, 0)
	if err != nil || len(audit) < 4 {
		t.Fatal("missing audit", audit, err)
	}
}
