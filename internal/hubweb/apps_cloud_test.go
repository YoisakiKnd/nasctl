package hubweb

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/YoisakiKnd/nasctl/internal/cloudbackup"
	"github.com/YoisakiKnd/nasctl/internal/desktopapps"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"io"
	"testing"
)

func TestDesktopApplicationAndWebDAVCredentialBoundaries(t *testing.T) {
	r := writeRig(t, nil)
	r.srv.Apps = desktopapps.New(t.TempDir())
	cloud, e := cloudbackup.New(t.TempDir(), r.srv.Files)
	if e != nil {
		t.Fatal(e)
	}
	defer cloud.Close()
	r.srv.CloudBackups = cloud
	code, b := fileRequest(t, r, "GET", "/api/v1/apps", "", "", "", false)
	var apps desktopapps.View
	if code != 200 || json.Unmarshal(b, &apps) != nil {
		t.Fatal(code, string(b))
	}
	body, _ := json.Marshal(desktopapps.View{Revision: apps.Revision, Apps: []desktopapps.App{{ID: "video", Name: "Video", URL: "http://{host}:8096/", Icon: "play", Color: "blue"}}})
	code, _ = fileRequest(t, r, "PUT", "/api/v1/apps", string(body), "", "", false)
	if code != 400 {
		t.Fatal("unconfirmed app saved", code)
	}
	code, b = fileRequest(t, r, "PUT", "/api/v1/apps", string(body), "", "", true)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	secret := "generated-webdav-login-secret"
	request := `{"url":"https://example.com/dav/","username":"fixture","password":"` + secret + `","password_action":"replace","allow_http":false,"revision":""}`
	code, b = fileRequest(t, r, "PUT", "/api/v1/backup/webdav", request, "", "", true)
	if code != 200 || bytes.Contains(b, []byte(secret)) {
		t.Fatal(code, string(b))
	}
	code, b = fileRequest(t, r, "GET", "/api/v1/backup/webdav", "", "", "", false)
	if code != 200 || bytes.Contains(b, []byte(secret)) {
		t.Fatal(code, string(b))
	}
	_, b = fileRequest(t, r, "GET", "/api/v1/jobs?limit=100", "", "", "", false)
	if bytes.Contains(b, []byte(secret)) {
		t.Fatal("job leaked password")
	}
}
func TestWebDAVAndAppsRefuseAIAndReadOnlyWrites(t *testing.T) {
	for _, readonly := range []bool{true, false} {
		r := writeRig(t, func(c *Config, j *jobs.Config) {
			if readonly && j != nil {
				j.ReadOnlyConsole = true
			}
			if !readonly && c != nil {
				c.Token = "generated-operator-token"
				c.Agents = []AgentCredential{{Name: "helper", Token: "generated-helper-token"}}
			}
		})
		r.srv.Apps = desktopapps.New(t.TempDir())
		cloud, e := cloudbackup.New(t.TempDir(), r.srv.Files)
		if e != nil {
			t.Fatal(e)
		}
		defer cloud.Close()
		r.srv.CloudBackups = cloud
		token := ""
		if !readonly {
			token = "generated-helper-token"
		}
		for _, target := range []string{"/api/v1/apps", "/api/v1/backup/webdav"} {
			code, _ := fileRequest(t, r, "PUT", target, `{}`, token, "", true)
			if code != 403 {
				t.Fatal(target, code)
			}
		}
		if !readonly {
			code, _ := fileRequest(t, r, "GET", "/api/v1/backup/webdav", "", token, "", false)
			if code != 403 {
				t.Fatal("AI can read WebDAV connection", code)
			}
		}
	}
}

type corruptArchiveDownload struct{ cloudbackup.Control }

func (corruptArchiveDownload) Download(context.Context, string) (io.ReadCloser, int64, string, error) {
	return io.NopCloser(brokenArchiveReader{}), 100, "generated.tar.zst.age", nil
}

type brokenArchiveReader struct{}

func (brokenArchiveReader) Read(p []byte) (int, error) {
	return copy(p, []byte("partial")), cloudbackup.ErrRemote
}
func TestCorruptArchiveDownloadAbortsInsteadOfAppendingJSON(t *testing.T) {
	r := writeRig(t, nil)
	r.srv.CloudBackups = corruptArchiveDownload{}
	response, e := r.client.Get(r.ts.URL + "/api/v1/backup/webdav/0123456789abcdef0123456789abcdef/download")
	if e != nil {
		return
	}
	defer response.Body.Close()
	if _, e = io.ReadAll(response.Body); e == nil {
		t.Fatal("corrupt encrypted download completed as a normal response")
	}
}
