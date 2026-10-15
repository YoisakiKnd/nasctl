package nasctl

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/accountslocal"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
)

func TestInitializePrivateEmptyNASAndLocalAccountWorkflow(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "config.toml")
	state := filepath.Join(base, "state")
	var out, errs bytes.Buffer
	run := func(input string, args ...string) int {
		t.Helper()
		out.Reset()
		errs.Reset()
		return Main(append([]string{"--config", source, "--json"}, args...), Env{Stdin: strings.NewReader(input), Stdout: &out, Stderr: &errs})
	}
	if code := run("", "init", "--state-dir", state); code != 2 {
		t.Fatal("unconfirmed init", code)
	}
	if _, e := os.Stat(source); !os.IsNotExist(e) {
		t.Fatal("unconfirmed init wrote config")
	}
	if code := run("", "init", "--state-dir", state, "--confirm"); code != 0 {
		t.Fatal(code, errs.String())
	}
	cfg, e := config.LoadNAS(source)
	if e != nil || !cfg.Hub.Setup || len(cfg.Hub.Storage) != 0 {
		t.Fatal("empty secure NAS config", e)
	}
	original, e := os.ReadFile(source)
	if e != nil {
		t.Fatal(e)
	}
	key, e := os.ReadFile(filepath.Join(state, accounts.SetupFile))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(out.String(), strings.TrimSpace(string(key))) {
		t.Fatal("init exposed setup credential")
	}
	if i, e := os.Stat(source); e != nil || i.Mode().Perm() != 0600 {
		t.Fatal("config permissions", e)
	}
	if code := run("", "init", "--state-dir", state, "--confirm"); code == 0 {
		t.Fatal("init replaced config")
	}
	b, _ := os.ReadFile(source)
	if !bytes.Equal(b, original) {
		t.Fatal("existing config changed")
	}
	password := "generated initial owner password"
	if code := run(password+"\n", "users", "create", "--username", "owner", "--role", "admin", "--password-stdin", "--confirm"); code != 0 {
		t.Fatal(code, errs.String())
	}
	if strings.Contains(out.String()+errs.String(), password) {
		t.Fatal("CLI exposed new password")
	}
	if code := run("", "users", "setup-code"); code == 0 {
		t.Fatal("CLI reused consumed bootstrap")
	}
	if code := run("", "mode", "pause"); code != 0 {
		t.Fatal(code, errs.String())
	}
	next := "generated replacement owner password"
	if code := run(next+"\n", "users", "password", "--username", "owner", "--password-stdin", "--confirm"); code != 0 {
		t.Fatal("password recovery must work while heavy work paused", code, errs.String())
	}
	c, e := accountslocal.Open(state)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Authenticate(context.Background(), "owner", password); e == nil {
		t.Fatal("old password survived CLI recovery")
	}
	if _, e = c.Authenticate(context.Background(), "owner", next); e != nil {
		t.Fatal(e)
	}
	c.Close()
	if code := run("", "users", "remove", "--username", "owner", "--confirm-username", "owner", "--confirm"); code == 0 {
		t.Fatal("CLI deleted last admin")
	}
	if code := run("", "users", "list"); code != 0 {
		t.Fatal(code, errs.String())
	}
	var list accounts.Snapshot
	if e = json.Unmarshal(out.Bytes(), &list); e != nil || len(list.Users) != 1 || list.Users[0].Role != "admin" {
		t.Fatal("CLI list", e)
	}
	if code := run("", "--agent", "test-helper", "users", "list"); code == 0 {
		t.Fatal("agent accessed local account administration")
	}
}

func TestLocalRepairsWorkWithoutSavedSettingsOrJobDatabase(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "config.toml")
	state := filepath.Join(base, "state")
	var out, errs bytes.Buffer
	run := func(input string, args ...string) int {
		out.Reset()
		errs.Reset()
		return Main(append([]string{"--config", source, "--json"}, args...), Env{Stdin: strings.NewReader(input), Stdout: &out, Stderr: &errs})
	}
	if n := run("", "init", "--state-dir", state, "--confirm"); n != 0 {
		t.Fatal(n, errs.String())
	}
	original, _ := os.ReadFile(source)
	brokenSettings := []byte("invalid private saved settings")
	if e := os.WriteFile(filepath.Join(state, config.NASSettingsFile), brokenSettings, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(state, "hub.db"), []byte("invalid job DB"), 0600); e != nil {
		t.Fatal(e)
	}
	if n := run("", "settings", "reset"); n != 2 {
		t.Fatal("unconfirmed reset", n, errs.String())
	}
	if n := run("", "settings", "reset", "--confirm"); n != 0 {
		t.Fatal(n, errs.String())
	}
	var reset struct {
		Quarantine string `json:"quarantine"`
		Completed  bool   `json:"completed"`
	}
	if e := json.Unmarshal(out.Bytes(), &reset); e != nil || !reset.Completed {
		t.Fatal(out.String(), e)
	}
	kept, e := os.ReadFile(filepath.Join(reset.Quarantine, "original.raw"))
	if e != nil || !bytes.Equal(kept, brokenSettings) {
		t.Fatal("settings original", e)
	}
	if _, e = config.LoadNAS(source); e != nil {
		t.Fatal("settings still invalid", e)
	}
	if e = os.WriteFile(filepath.Join(state, accounts.FileName), []byte("invalid user JSON"), 0600); e != nil {
		t.Fatal(e)
	}
	if n := run("generated recovered owner password\n", "users", "recover", "--username", "owner", "--password-stdin", "--confirm"); n != 0 {
		t.Fatal(n, errs.String())
	}
	c, e := accountslocal.Open(state)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if _, e = c.Authenticate(context.Background(), "owner", "generated recovered owner password"); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(source)
	if !bytes.Equal(b, original) {
		t.Fatal("repair changed base config")
	}
	b, _ = os.ReadFile(filepath.Join(state, "hub.db"))
	if string(b) != "invalid job DB" {
		t.Fatal("repair touched unrelated DB")
	}
}

func TestDuplicateConsoleRefusesBeforeOpeningDatabase(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "config.toml")
	state := filepath.Join(base, "state")
	var out, errs bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errs}
	if code := Main([]string{"--config", source, "init", "--state-dir", state, "--confirm"}, env); code != 0 {
		t.Fatal(code, errs.String())
	}
	lease, e := statelease.AcquireService(state)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	if code := Main([]string{"--config", source, "serve"}, env); code != 1 || !strings.Contains(errs.String(), "already owns") {
		t.Fatal("duplicate console reached startup", code, errs.String())
	}
	if _, e = os.Stat(filepath.Join(state, "hub.db")); !os.IsNotExist(e) {
		t.Fatal("duplicate startup opened/migrated task DB", e)
	}
}
