package settingslocal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/settings"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
)

func fixture(t *testing.T) (*config.Config, *Control) {
	t.Helper()
	p := t.TempDir()
	state := filepath.Join(p, "state")
	data := filepath.Join(p, "data")
	for _, d := range []string{state, data} {
		if e := os.Mkdir(d, 0700); e != nil {
			t.Fatal(e)
		}
	}
	src := filepath.Join(p, "config.toml")
	doc := "[paths]\nstate_dir=" + quote(state) + "\ncache_dir=" + quote(filepath.Join(p, "cache")) + "\n[hub.anime]\nusername=\"private-user-secret\"\npassword=\"private-password-secret\"\ntmdb_token=\"private-tmdb-secret\"\n[[hub.storage]]\nid=\"data\"\npath=" + quote(data) + "\n"
	if e := os.WriteFile(src, []byte(doc), 0600); e != nil {
		t.Fatal(e)
	}
	cfg, e := config.LoadNAS(src)
	if e != nil {
		t.Fatal(e)
	}
	return cfg, New(cfg)
}
func quote(s string) string { b, _ := json.Marshal(s); return string(b) }
func TestValidationSaveRestartAndSecrets(t *testing.T) {
	cfg, c := fixture(t)
	ctx := context.Background()
	original, _ := os.ReadFile(cfg.Source)
	s, e := c.Get(ctx)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "private-") {
		t.Fatal("secret leaked", string(b))
	}
	req := settings.Request{Revision: s.Revision, Settings: s.Saved, Secrets: map[string]settings.Secret{"password": {Action: "set", Value: "replacement-secret"}, "tmdb_token": {Action: "clear"}}}
	req.Settings.Files.UploadMaxBytes = 12345
	if _, e = c.Validate(ctx, req); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(cfg.Paths.StateDir, config.NASSettingsFile)); !os.IsNotExist(e) {
		t.Fatal("validation wrote settings", e)
	}
	saved, e := c.Save(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	if !saved.RestartRequired || saved.Active.Files.UploadMaxBytes == 12345 || saved.Saved.Files.UploadMaxBytes != 12345 {
		t.Fatal("runtime/restart semantics", saved)
	}
	b, _ = json.Marshal(saved)
	if strings.Contains(string(b), "replacement-secret") || strings.Contains(string(b), "private-") {
		t.Fatal("response leaked secrets")
	}
	if !saved.Secrets["password"] || saved.Secrets["tmdb_token"] {
		t.Fatal("secret presence wrong")
	}
	loaded, e := config.LoadNAS(cfg.Source)
	if e != nil {
		t.Fatal(e)
	}
	if loaded.Hub.Files.UploadMaxBytes != 12345 || loaded.Hub.Anime.Username != "private-user-secret" || loaded.Hub.Anime.Password != "replacement-secret" || loaded.Hub.Anime.TMDBToken != "" {
		t.Fatal("overlay not applied/preserved")
	}
	child, e := config.Load(cfg.Source)
	if e != nil {
		t.Fatal(e)
	}
	if child.Hub.Files.UploadMaxBytes == 12345 || child.Hub.Anime.Password == "replacement-secret" {
		t.Fatal("album child unexpectedly applied unsanitized NAS override")
	}
	after, _ := os.ReadFile(cfg.Source)
	if !bytes.Equal(original, after) {
		t.Fatal("base TOML modified")
	}
	i, _ := os.Stat(filepath.Join(cfg.Paths.StateDir, config.NASSettingsFile))
	if i.Mode().Perm() != 0600 {
		t.Fatal("overlay permissions")
	}
	fresh := New(loaded)
	active, e := fresh.Get(ctx)
	if e != nil || active.RestartRequired {
		t.Fatal("restart did not activate saved values", e)
	}
	if _, e = c.Save(ctx, req); !errors.Is(e, settings.ErrConflict) {
		t.Fatal("stale revision accepted", e)
	}
}
func TestInvalidConfigurationAndManagedRootRemovalRefused(t *testing.T) {
	cfg, c := fixture(t)
	ctx := context.Background()
	s, e := c.Get(ctx)
	if e != nil {
		t.Fatal(e)
	}
	req := settings.Request{Revision: s.Revision, Settings: s.Saved}
	req.Settings.Storage = append(req.Settings.Storage, req.Settings.Storage[0])
	if _, e = c.Save(ctx, req); e == nil {
		t.Fatal("duplicate root accepted")
	}
	req.Settings = s.Saved
	req.Settings.GuardMemory.StopKB = req.Settings.GuardMemory.StartKB
	if _, e = c.Validate(ctx, req); e == nil {
		t.Fatal("invalid memory thresholds accepted")
	}
	req.Settings = s.Saved
	req.Secrets = map[string]settings.Secret{"password": {Action: "keep", Value: "must-not-pass"}}
	if _, e = c.Save(ctx, req); e == nil {
		t.Fatal("keep with value accepted")
	}
	req.Secrets = nil
	req.Settings = s.Saved
	req.Settings.Storage = []config.HubStorage{{ID: "other", Path: s.Saved.Storage[0].Path}}
	if e = os.WriteFile(filepath.Join(cfg.Paths.StateDir, "album-directories.json"), []byte(`[{"code":"photos","name":"Photos","root":"data","path":".","enabled":true,"allow_shared_volume":true,"managed":true}]`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Save(ctx, req); e == nil {
		t.Fatal("managed root removal would break album startup")
	}
	if _, e = os.Stat(filepath.Join(cfg.Paths.StateDir, config.NASSettingsFile)); !os.IsNotExist(e) {
		t.Fatal("invalid save wrote overlay", e)
	}
}
func TestOfflineRestoreExcludesSettingsWriter(t *testing.T) {
	cfg, c := fixture(t)
	ctx := context.Background()
	s, e := c.Get(ctx)
	if e != nil {
		t.Fatal(e)
	}
	lease, e := statelease.Acquire(cfg.Paths.StateDir, true)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	if _, e = c.Save(ctx, settings.Request{Revision: s.Revision, Settings: s.Saved}); !errors.Is(e, statelease.ErrBusy) {
		t.Fatal("offline lease ignored", e)
	}
}
