package hubweb

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/settings"
	"github.com/YoisakiKnd/nasctl/internal/settingslocal"
)

func configureSettingsRig(t *testing.T, r *rig) string {
	t.Helper()
	base := t.TempDir()
	state := filepath.Join(base, "state")
	if e := os.Mkdir(state, 0700); e != nil {
		t.Fatal(e)
	}
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	source := filepath.Join(base, "config.toml")
	doc := "[paths]\nstate_dir=" + quote(state) + "\ncache_dir=" + quote(filepath.Join(base, "cache")) + "\n[hub.anime]\npassword=\"fixture-private-secret\"\n[[hub.storage]]\nid=\"media\"\npath=" + quote(r.root) + "\n"
	if e := os.WriteFile(source, []byte(doc), 0600); e != nil {
		t.Fatal(e)
	}
	cfg, e := config.LoadNAS(source)
	if e != nil {
		t.Fatal(e)
	}
	r.srv.Settings = settingslocal.New(cfg)
	return filepath.Join(state, config.NASSettingsFile)
}
func TestUnifiedSettingsHTTPValidationConfirmationSecretsAndAudit(t *testing.T) {
	r := newRig(t, nil)
	path := configureSettingsRig(t, r)
	var snapshot settings.Snapshot
	r.getJSON(t, "/api/v1/settings", &snapshot)
	req := settings.Request{Revision: snapshot.Revision, Settings: snapshot.Saved, Secrets: map[string]settings.Secret{"password": {Action: "set", Value: "http-replacement-secret"}}}
	req.Settings.Files.UploadMaxBytes = 5000
	b, _ := json.Marshal(req)
	code, body := fileRequest(t, r, "POST", "/api/v1/settings/validate", string(b), "", "", false)
	if code != 200 || strings.Contains(string(body), "http-replacement-secret") || strings.Contains(string(body), "fixture-private-secret") {
		t.Fatalf("validation response %d %s", code, body)
	}
	if _, e := os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("validation wrote overlay", e)
	}
	code, _ = fileRequest(t, r, "POST", "/api/v1/settings", string(b), "", "", false)
	if code != 400 {
		t.Fatal("unconfirmed settings save", code)
	}
	code, body = fileRequest(t, r, "POST", "/api/v1/settings", string(b), "", "", true)
	if code != 200 {
		t.Fatalf("save %d %s", code, body)
	}
	if strings.Contains(string(body), "http-replacement-secret") || strings.Contains(string(body), "fixture-private-secret") {
		t.Fatal("secret in response")
	}
	var saved settings.Snapshot
	if e := json.Unmarshal(body, &saved); e != nil || !saved.RestartRequired || saved.Active.Files.UploadMaxBytes == 5000 || saved.Saved.Files.UploadMaxBytes != 5000 {
		t.Fatal("restart response", saved, e)
	}
	audit, e := r.store.Audit(context.Background(), 50, 0)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, row := range audit {
		if row.Op == "settings.configure" {
			found = true
			b, _ := json.Marshal(row)
			if strings.Contains(string(b), "http-replacement-secret") {
				t.Fatal("secret in audit")
			}
		}
	}
	if !found {
		t.Fatal("settings save unaudited")
	}
	code, _ = fileRequest(t, r, "POST", "/api/v1/settings", string(b), "", "", true)
	if code != 409 {
		t.Fatal("stale revision not rejected", code)
	}
}
func TestUnifiedSettingsReadOnlyAndAgentRestrictions(t *testing.T) {
	for _, readonly := range []bool{true, false} {
		r := newRig(t, func(c *Config, j *jobs.Config) {
			if readonly && j != nil {
				j.ReadOnlyConsole = true
			}
			if !readonly && c != nil {
				c.Token = "settings-operator-token"
				c.Agents = []AgentCredential{{Name: "helper", Token: "settings-agent-token", AllowHeavy: true}}
			}
		})
		path := configureSettingsRig(t, r)
		token := ""
		if !readonly {
			token = "settings-agent-token"
		}
		code, _ := fileRequest(t, r, "POST", "/api/v1/settings/validate", `{}`, token, "", false)
		if code != 403 {
			t.Fatal("validation access", readonly, code)
		}
		code, _ = fileRequest(t, r, "POST", "/api/v1/settings", `{}`, token, "", true)
		if code != 403 {
			t.Fatal("write access", readonly, code)
		}
		code, _ = fileRequest(t, r, "GET", "/api/v1/settings", "", token, "", false)
		want := 200
		if !readonly {
			want = 403
		}
		if code != want {
			t.Fatal("read access", readonly, code)
		}
		if _, e := os.Stat(path); !os.IsNotExist(e) {
			t.Fatal("unauthorized settings written", e)
		}
	}
}
