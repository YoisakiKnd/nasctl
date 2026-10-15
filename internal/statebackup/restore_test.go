package statebackup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/accountslocal"
	"github.com/YoisakiKnd/nasctl/internal/anime"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
	"github.com/YoisakiKnd/nasctl/internal/store"
)

func sqlWork(t *testing.T, p, q string) {
	t.Helper()
	u := url.URL{Scheme: "file", Path: p}
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(q); e != nil {
		t.Fatal(e)
	}
}
func scalar(t *testing.T, p, q string) int64 {
	t.Helper()
	u := url.URL{Scheme: "file", Path: p}
	u.RawQuery = "mode=ro"
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var n int64
	if e = db.QueryRow(q).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func fixture(t *testing.T) (Layout, string) {
	t.Helper()
	base, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(base, 0700); e != nil {
		t.Fatal(e)
	}
	state := filepath.Join(base, "state")
	if e = os.Mkdir(state, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := config.Default()
	cfg.Hub.Setup = true
	media := filepath.Join(base, "media")
	if e = os.Mkdir(media, 0750); e != nil {
		t.Fatal(e)
	}
	cfg.Hub.Storage = []config.HubStorage{{ID: "data", Path: media, Label: "Files"}}
	cfg.Paths.StateDir = state
	cfg.Paths.CacheDir = filepath.Join(base, "cache")
	cfg.Paths.IndexFile = filepath.Join(state, "index.sqlite3")
	cfg.Paths.LockDir = filepath.Join(state, "locks")
	cfg.Paths.RunDir = state
	cfg.Paths.LogFile = filepath.Join(state, "album.log")
	cfg.Hub.Album.Mode = "local"
	if e = cfg.Finalize(); e != nil {
		t.Fatal(e)
	}
	var b bytes.Buffer
	if e = toml.NewEncoder(&b).Encode(cfg); e != nil {
		t.Fatal(e)
	}
	cp := filepath.Join(base, "config.toml")
	if e = os.WriteFile(cp, b.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	l := NewLayout(&cfg, cp, "")
	hub, e := hubstore.Open(l.Hub, hubstore.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	if e = hub.Close(); e != nil {
		t.Fatal(e)
	}
	an, e := anime.OpenStore(l.targets()["anime"])
	if e != nil {
		t.Fatal(e)
	}
	if e = an.Close(); e != nil {
		t.Fatal(e)
	}
	al, e := store.Open(l.Album, store.Options{})
	if e != nil {
		t.Fatal(e)
	}
	if e = al.Close(); e != nil {
		t.Fatal(e)
	}
	sqlWork(t, l.targets()["anime"], `INSERT INTO subscriptions(id,title,feed_url) VALUES(1,'Old','https://example.com/rss');UPDATE anime_sequences SET next_id=2 WHERE name='subscriptions'`)
	if e = os.WriteFile(l.targets()["album-directories"], []byte("[]\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(l.targets()["metadata"], 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(l.targets()["metadata"], strings.Repeat("0", 64)+".backup"), []byte("original metadata bytes"), 0600); e != nil {
		t.Fatal(e)
	}
	return l, filepath.Join(base, "package")
}
func advance(t *testing.T, l Layout) {
	t.Helper()
	sqlWork(t, l.targets()["anime"], `INSERT INTO subscriptions(id,title,feed_url) VALUES(900,'New','https://example.com/new');UPDATE anime_sequences SET next_id=1000 WHERE name='subscriptions';UPDATE anime_sequences SET next_id=5000 WHERE name='episodes'`)
	if e := os.WriteFile(l.targets()["album-directories"], []byte("[{}]\n"), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestOfflinePackageRestorePreservesHighwaterAndCheckpoint(t *testing.T) {
	ctx := context.Background()
	l, p := fixture(t)
	shared, e := statelease.Acquire(l.State, false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Create(ctx, l, p, nil); !errors.Is(e, statelease.ErrBusy) {
		t.Fatal("live writer accepted", e)
	}
	shared.Close()
	r, e := Create(ctx, l, p, nil)
	if e != nil {
		t.Fatal(e)
	}
	if r.Files != 6 || r.Bytes == 0 {
		t.Fatal(r)
	}
	beforeConfig, e := os.ReadFile(l.Config)
	if e != nil {
		t.Fatal(e)
	}
	advance(t, l)
	if _, e = Restore(ctx, l, p, strings.Repeat("0", 64), nil); e == nil {
		t.Fatal("stale consent accepted")
	}
	got, e := Restore(ctx, l, p, r.ManifestSHA256, nil)
	if e != nil {
		t.Fatal(e)
	}
	if got.Checkpoint == "" || got.ConfigRestored {
		t.Fatal(got)
	}
	if scalar(t, l.targets()["anime"], "SELECT count(*) FROM subscriptions") != 1 || scalar(t, l.targets()["anime"], "SELECT next_id FROM anime_sequences WHERE name='subscriptions'") != 1000 || scalar(t, l.targets()["anime"], "SELECT next_id FROM anime_sequences WHERE name='episodes'") != 5000 {
		t.Fatal("rows or highwater wrong")
	}
	b, _ := os.ReadFile(l.targets()["album-directories"])
	if string(b) != "[]\n" {
		t.Fatal(string(b))
	}
	b, _ = os.ReadFile(l.Config)
	if !bytes.Equal(b, beforeConfig) {
		t.Fatal("active config replaced")
	}
	if _, e = Verify(ctx, got.Checkpoint); e != nil {
		t.Fatal(e)
	}
	checkpointAnime := payload(got.Checkpoint, "anime")
	if scalar(t, checkpointAnime, "SELECT count(*) FROM subscriptions") != 2 {
		t.Fatal("checkpoint lost newer records")
	}
	lease, e := statelease.Acquire(l.State, false)
	if e != nil {
		t.Fatal(e)
	}
	lease.Close()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, e = os.Lstat(l.targets()["anime"] + suffix); !os.IsNotExist(e) {
			t.Fatal("old SQLite sidecar survived publication", suffix, e)
		}
	}
}
func TestRestoreFaultRollsBackEveryTarget(t *testing.T) {
	ctx := context.Background()
	l, p := fixture(t)
	r, e := Create(ctx, l, p, nil)
	if e != nil {
		t.Fatal(e)
	}
	advance(t, l)
	targets := l.targets()
	before := map[string]string{}
	for k, v := range targets {
		before[k], e = digestPath(ctx, v)
		if e != nil {
			t.Fatal(e)
		}
	}
	_, e = restore(ctx, l, p, r.ManifestSHA256, nil, func(i int) error {
		if i == 3 {
			return errors.New("injected publication failure")
		}
		return nil
	})
	if e == nil || !strings.Contains(e.Error(), "rolled back") {
		t.Fatal(e)
	}
	for k, v := range targets {
		got, e := digestPath(ctx, v)
		if e != nil || got != before[k] {
			t.Fatal("rollback bytes", k, got, before[k], e)
		}
	}
	if _, e = os.Lstat(filepath.Join(l.State, journalName)); !os.IsNotExist(e) {
		t.Fatal("journal survived successful rollback", e)
	}
	if scalar(t, targets["anime"], "SELECT count(*) FROM subscriptions") != 2 {
		t.Fatal("new records lost on failed restore")
	}
}
func TestStateRestoreCrashHelper(t *testing.T) {
	cp := os.Getenv("NASCTL_RESTORE_CRASH_CONFIG")
	if cp == "" {
		return
	}
	cfg, e := config.Load(cp)
	if e != nil {
		t.Fatal(e)
	}
	l := NewLayout(cfg, cp, "")
	_, e = restore(context.Background(), l, os.Getenv("NASCTL_RESTORE_CRASH_PACKAGE"), os.Getenv("NASCTL_RESTORE_CRASH_DIGEST"), nil, func(i int) error {
		if i == 2 {
			os.Exit(73)
		}
		return nil
	})
	t.Fatal("child failed to crash", e)
}
func TestProcessDeathBlocksWritersUntilRecovery(t *testing.T) {
	ctx := context.Background()
	l, p := fixture(t)
	r, e := Create(ctx, l, p, nil)
	if e != nil {
		t.Fatal(e)
	}
	advance(t, l)
	cmd := exec.Command(os.Args[0], "-test.run=^TestStateRestoreCrashHelper$")
	cmd.Env = append(os.Environ(), "NASCTL_RESTORE_CRASH_CONFIG="+l.Config, "NASCTL_RESTORE_CRASH_PACKAGE="+p, "NASCTL_RESTORE_CRASH_DIGEST="+r.ManifestSHA256)
	out, e := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(e, &exit) || exit.ExitCode() != 73 {
		t.Fatalf("child: %v %s", e, out)
	}
	if _, e = statelease.Acquire(l.State, false); !errors.Is(e, statelease.ErrRecoveryRequired) {
		t.Fatal("writer accepted partial restore", e)
	}
	if _, e = Create(ctx, l, filepath.Join(filepath.Dir(p), "blocked"), nil); !errors.Is(e, statelease.ErrRecoveryRequired) {
		t.Fatal("new backup accepted partial restore", e)
	}
	if e = Recover(ctx, l); e != nil {
		t.Fatal(e)
	}
	if scalar(t, l.targets()["anime"], "SELECT count(*) FROM subscriptions") != 2 {
		t.Fatal("crash recovery lost records")
	}
	lease, e := statelease.Acquire(l.State, false)
	if e != nil {
		t.Fatal(e)
	}
	lease.Close()
}
func TestPackageCorruptionAliasesAndUnlistedFilesRefused(t *testing.T) {
	ctx := context.Background()
	l, p := fixture(t)
	r, e := Create(ctx, l, p, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(p, "data", "unlisted"), []byte("unexpected"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = Verify(ctx, p); e == nil {
		t.Fatal("unlisted payload accepted")
	}
	os.Remove(filepath.Join(p, "data", "unlisted"))
	if e = os.WriteFile(payload(p, "album-directories"), []byte("corrupted"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = Restore(ctx, l, p, r.ManifestSHA256, nil); e == nil {
		t.Fatal("corrupt package accepted")
	}
	if scalar(t, l.targets()["anime"], "SELECT count(*) FROM subscriptions") != 1 {
		t.Fatal("corrupt restore mutated original")
	}
	l2, p2 := fixture(t)
	r2, e := Create(ctx, l2, p2, nil)
	if e != nil {
		t.Fatal(e)
	}
	target := payload(p2, "config")
	if e = os.Remove(target); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(l2.Config, target); e != nil {
		t.Fatal(e)
	}
	if _, e = Restore(ctx, l2, p2, r2.ManifestSHA256, nil); e == nil {
		t.Fatal("payload symlink accepted")
	}
}

func TestCommittedRecoveryCompletesWithoutUndoingPublication(t *testing.T) {
	ctx := context.Background()
	l, _ := fixture(t)
	id, e := nonce()
	if e != nil {
		t.Fatal(e)
	}
	target := l.targets()["album-directories"]
	old, e := digestPath(ctx, target)
	if e != nil {
		t.Fatal(e)
	}
	j := journal{Version: 1, ID: id, LayoutSHA256: l.fingerprint(), Committed: true, Changes: []change{{Key: "album-directories", Target: target, OldSHA256: old}}}
	c := j.Changes[0]
	if e = os.Rename(target, j.old(c)); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(target, []byte("[{}]\n"), 0600); e != nil {
		t.Fatal(e)
	}
	j.Changes[0].NewSHA256, e = digestPath(ctx, target)
	if e != nil {
		t.Fatal(e)
	}
	if e = writeJournal(l, j); e != nil {
		t.Fatal(e)
	}
	if e = markPending(l, j); e != nil {
		t.Fatal(e)
	}
	if e = Recover(ctx, l); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(target)
	if string(b) != "[{}]\n" {
		t.Fatal("committed publication undone")
	}
	if _, e = os.Lstat(j.old(c)); !os.IsNotExist(e) {
		t.Fatal("old file not cleaned", e)
	}
}
func TestIncompleteJournalBlocksBeforeFirstPendingMarker(t *testing.T) {
	l, _ := fixture(t)
	if e := os.WriteFile(filepath.Join(l.State, journalName), []byte("incomplete"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := statelease.Acquire(l.State, false); !errors.Is(e, statelease.ErrRecoveryRequired) {
		t.Fatal(e)
	}
	if e := Recover(context.Background(), l); e == nil {
		t.Fatal("invalid recovery journal accepted")
	}
	if _, e := os.Lstat(filepath.Join(l.State, journalName)); e != nil {
		t.Fatal("invalid journal removed", e)
	}
}
func TestWALCrashHelper(t *testing.T) {
	p := os.Getenv("NASCTL_STATE_WAL_CRASH_DB")
	if p == "" {
		return
	}
	u := url.URL{Scheme: "file", Path: p}
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`PRAGMA journal_mode=WAL;PRAGMA wal_autocheckpoint=0;INSERT INTO subscriptions(id,title,feed_url) VALUES(777,'Committed WAL','https://example.com/wal');UPDATE anime_sequences SET next_id=778 WHERE name='subscriptions'`); e != nil {
		t.Fatal(e)
	}
	os.Exit(74)
}
func TestClosedCrashWALIsCapturedAndDetachedOnRestore(t *testing.T) {
	ctx := context.Background()
	l, p := fixture(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestWALCrashHelper$")
	cmd.Env = append(os.Environ(), "NASCTL_STATE_WAL_CRASH_DB="+l.targets()["anime"])
	out, e := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(e, &exit) || exit.ExitCode() != 74 {
		t.Fatalf("WAL child: %v %s", e, out)
	}
	if i, e := os.Stat(l.targets()["anime"] + "-wal"); e != nil || i.Size() == 0 {
		t.Fatal("fixture did not leave committed WAL", e)
	}
	r, e := Create(ctx, l, p, nil)
	if e != nil {
		t.Fatal(e)
	}
	if scalar(t, payload(p, "anime"), "SELECT count(*) FROM subscriptions") != 2 {
		t.Fatal("committed WAL omitted")
	}
	got, e := Restore(ctx, l, p, r.ManifestSHA256, nil)
	if e != nil {
		t.Fatal(e)
	}
	if got.Checkpoint == "" || scalar(t, l.targets()["anime"], "SELECT count(*) FROM subscriptions") != 2 {
		t.Fatal("WAL restore lost rows")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, e = os.Lstat(l.targets()["anime"] + suffix); !os.IsNotExist(e) {
			t.Fatal("stale WAL retained", e)
		}
	}
}
func TestBackupCancellationDoesNotPublishAndAuditIsBounded(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	l, p := fixture(t)
	_, e := Create(ctx, l, p, func(phase string) {
		if phase == "snapshot anime" {
			stop()
		}
	})
	if !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e = os.Lstat(p); !os.IsNotExist(e) {
		t.Fatal("cancelled package published", e)
	}
	close, e := l.lock(false)
	if e != nil {
		t.Fatal(e)
	}
	defer close()
	for i := 0; i < 110; i++ {
		if e = audit(l, "bounded-test", Result{}, nil); e != nil {
			t.Fatal(e)
		}
	}
	b, e := readPrivate(filepath.Join(l.State, "offline-state-audit.json"), 65536)
	if e != nil {
		t.Fatal(e)
	}
	var rows []auditRow
	if e = decode(b, &rows); e != nil || len(rows) != 100 {
		t.Fatal(len(rows), e)
	}
}
func TestPrivateSettingsOverlayParticipatesInStateRestore(t *testing.T) {
	ctx := context.Background()
	l, p := fixture(t)
	cfg, e := config.Load(l.Config)
	if e != nil {
		t.Fatal(e)
	}
	options := config.Options(cfg)
	options.Files.UploadMaxBytes = 1111
	options.Anime.Password = "private-overlay-secret"
	b, e := json.Marshal(options)
	if e != nil {
		t.Fatal(e)
	}
	settingsPath := filepath.Join(l.State, config.NASSettingsFile)
	if e = os.WriteFile(settingsPath, b, 0600); e != nil {
		t.Fatal(e)
	}
	r, e := Create(ctx, l, p, nil)
	if e != nil {
		t.Fatal(e)
	}
	if r.Files != 7 {
		t.Fatal("settings not captured", r)
	}
	manifest, e := os.ReadFile(filepath.Join(p, "manifest.json"))
	if e != nil || strings.Contains(string(manifest), "private-overlay-secret") {
		t.Fatal("secret in manifest", e)
	}
	options.Files.UploadMaxBytes = 2222
	b, _ = json.Marshal(options)
	if e = os.WriteFile(settingsPath, b, 0600); e != nil {
		t.Fatal(e)
	}
	got, e := Restore(ctx, l, p, r.ManifestSHA256, nil)
	if e != nil {
		t.Fatal(e)
	}
	active, e := config.LoadNAS(l.Config)
	if e != nil || active.Hub.Files.UploadMaxBytes != 1111 || active.Hub.Anime.Password != "private-overlay-secret" {
		t.Fatal("saved overlay not restored", e)
	}
	before, e := config.DecodeNASOptions(mustRead(t, payload(got.Checkpoint, "settings")))
	if e != nil || before.Files.UploadMaxBytes != 2222 {
		t.Fatal("checkpoint omitted newer overlay", e)
	}
}
func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestAccountStateBackupCannotRestoreAnonymousMode(t *testing.T) {
	ctx := context.Background()
	l, preUsers := fixture(t)
	old, e := Create(ctx, l, preUsers, nil)
	if e != nil {
		t.Fatal(e)
	}
	users, e := accountslocal.Open(l.State)
	if e != nil {
		t.Fatal(e)
	}
	if e = users.EnsureSetupKey(ctx); e != nil {
		t.Fatal(e)
	}
	key, e := users.SetupKey(ctx)
	if e != nil {
		t.Fatal(e)
	}
	owner, e := users.CreateFirst(ctx, accounts.Setup{Code: key, Username: "owner", Password: "generated backup owner password"})
	if e != nil {
		t.Fatal(e)
	}
	users.Close()
	if _, e = Restore(ctx, l, preUsers, old.ManifestSHA256, nil); e == nil {
		t.Fatal("pre-account package erased enabled account protection")
	}
	if _, e = os.Stat(filepath.Join(l.State, "restore-checkpoints")); !os.IsNotExist(e) {
		t.Fatal("invalid restore consumed checkpoint budget", e)
	}
	withUsers := filepath.Join(filepath.Dir(preUsers), "with-users")
	saved, e := Create(ctx, l, withUsers, nil)
	if e != nil {
		t.Fatal(e)
	}
	originalConfig, e := os.ReadFile(l.Config)
	if e != nil {
		t.Fatal(e)
	}
	current, e := config.Load(l.Config)
	if e != nil {
		t.Fatal(e)
	}
	current.Hub.Setup = false
	var unguarded bytes.Buffer
	if e = toml.NewEncoder(&unguarded).Encode(current); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(l.Config, unguarded.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	originalUsers, e := os.ReadFile(filepath.Join(l.State, accounts.FileName))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Restore(ctx, l, withUsers, saved.ManifestSHA256, nil); e == nil || !strings.Contains(e.Error(), "current base configuration") {
		t.Fatal("saved guard was incorrectly treated as active policy", e)
	}
	if _, e = os.Stat(filepath.Join(l.State, "restore-checkpoints")); !os.IsNotExist(e) {
		t.Fatal("unguarded restore consumed checkpoint budget", e)
	}
	unchangedUsers, e := os.ReadFile(filepath.Join(l.State, accounts.FileName))
	if e != nil || !bytes.Equal(originalUsers, unchangedUsers) {
		t.Fatal("unguarded restore changed accounts", e)
	}
	if e = os.WriteFile(l.Config, originalConfig, 0600); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(withUsers, "manifest.json"))
	if e != nil || strings.Contains(string(b), "generated backup owner password") {
		t.Fatal("credential in manifest", e)
	}
	users, e = accountslocal.Open(l.State)
	if e != nil {
		t.Fatal(e)
	}
	snap, e := users.Snapshot(ctx)
	if e != nil {
		t.Fatal(e)
	}
	_, e = users.Save(ctx, accounts.Request{Revision: snap.Revision, ID: owner.ID, Username: owner.Username, Role: "admin", Password: "new generated owner password"})
	if e != nil {
		t.Fatal(e)
	}
	users.Close()
	got, e := Restore(ctx, l, withUsers, saved.ManifestSHA256, nil)
	if e != nil {
		t.Fatal(e)
	}
	users, e = accountslocal.Open(l.State)
	if e != nil {
		t.Fatal(e)
	}
	defer users.Close()
	if _, e = users.Authenticate(ctx, "owner", "generated backup owner password"); e != nil {
		t.Fatal("restored account failed", e)
	}
	if e = accountslocal.ValidateFile(payload(got.Checkpoint, "users")); e != nil {
		t.Fatal("newer account checkpoint invalid", e)
	}
}

func TestStateRestoreCannotResurrectPreVaultAlbumMetadata(t *testing.T) {
	ctx := context.Background()
	layout, dir := fixture(t)
	snapshot, e := Create(ctx, layout, dir, nil)
	if e != nil {
		t.Fatal(e)
	}
	vault := filepath.Join(layout.State, ".nasctl-photo-vault")
	if e = os.Mkdir(vault, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(vault, "vault.json"), []byte("configured-vault-marker"), 0600); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(layout.Album)
	if e != nil {
		t.Fatal(e)
	}
	_, e = Restore(ctx, layout, dir, snapshot.ManifestSHA256, nil)
	if e == nil || !strings.Contains(e.Error(), "encrypted photo vault") {
		t.Fatal("restore bypassed vault protection", e)
	}
	after, e := os.ReadFile(layout.Album)
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("refused restore changed album index", e)
	}
	if _, e = os.Stat(filepath.Join(layout.State, "restore-checkpoints")); !os.IsNotExist(e) {
		t.Fatal("refusal consumed a checkpoint", e)
	}
}
