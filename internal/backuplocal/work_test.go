package backuplocal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/backups"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/files"
)

func fixture(t *testing.T) (*Control, *files.Manager, string, string, string, backups.Plan) {
	t.Helper()
	base := t.TempDir()
	source, target, recovered := filepath.Join(base, "source"), filepath.Join(base, "target"), filepath.Join(base, "recovered")
	for _, p := range []string{source, target, recovered} {
		if err := os.Mkdir(p, 0750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(source, "folder"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "folder", "hello.txt"), []byte("original data"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".hidden"), []byte("hidden bytes"), 0640); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.WriteFile(outside, []byte("never read outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	fm, err := files.New([]config.HubStorage{{ID: "source", Path: source}, {ID: "target", Path: target, Writable: true}, {ID: "recovered", Path: recovered, Writable: true}}, files.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(filepath.Join(base, "state"), fm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); fm.Close() })
	p := backups.Plan{ID: "daily", Name: "Daily documents", Root: "source", Path: ".", ToRoot: "target", Limit: 100, MaxBytes: 1 << 20, Keep: 2, MaxRuntime: "1m"}
	if err = c.Save(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p = c.Settings().Plans[0]
	return c, fm, source, target, recovered, p
}

func TestLargePlanBudgetPersistsAndCannotRaiseCurrentPolicy(t *testing.T) {
	c, _, source, target, recovered, p := fixture(t)
	ctx := context.Background()
	p.MaxBytes = 32 << 30
	if e := c.Save(ctx, p); e == nil {
		t.Fatal("plan raised the default byte policy")
	}
	l := files.DefaultLimits()
	l.CopyMaxBytes = 32 << 30
	fm, e := files.New([]config.HubStorage{{ID: "source", Path: source}, {ID: "target", Path: target, Writable: true}, {ID: "recovered", Path: recovered, Writable: true}}, l)
	if e != nil {
		t.Fatal(e)
	}
	defer fm.Close()
	state := filepath.Join(filepath.Dir(source), "state")
	large, e := New(state, fm)
	if e != nil {
		t.Fatal(e)
	}
	if e = large.Save(ctx, p); e != nil {
		t.Fatal(e)
	}
	p = large.Settings().Plans[0]
	made, e := large.Run(ctx, p.ID, p.Digest(), nil)
	if e != nil || !made.Published {
		t.Fatal("large planned budget blocked bounded backup", made, e)
	}
	large.Close()
	reopened, e := New(state, fm)
	if e != nil {
		t.Fatal("large plan rejected on reload", e)
	}
	defer reopened.Close()
	if reopened.Settings().Plans[0].MaxBytes != 32<<30 {
		t.Fatal("large plan was truncated")
	}
	if _, e = reopened.Verify(ctx, "target", made.Snapshot.ID, nil); e != nil {
		t.Fatal("large-budget manifest rejected", e)
	}
	if _, e = reopened.Restore(ctx, backups.Restore{Root: "target", ID: made.Snapshot.ID, ToRoot: "recovered", To: "copy", ManifestSHA256: made.Snapshot.ManifestSHA256}, nil); e != nil {
		t.Fatal("large-budget restore failed", e)
	}
	if b, e := os.ReadFile(filepath.Join(recovered, "copy/folder/hello.txt")); e != nil || string(b) != "original data" {
		t.Fatal("restored bytes", e)
	}
}

func TestBackupManifestVerifyRestoreAndNoOverwrite(t *testing.T) {
	c, fm, source, target, recovered, p := fixture(t)
	ctx := context.Background()
	result, err := c.Run(ctx, p.ID, p.Digest(), nil)
	if err != nil || !result.Published || result.Snapshot == nil {
		t.Fatal(result, err)
	}
	s := *result.Snapshot
	if s.Entries != 5 || s.Bytes != 25 || len(s.ManifestSHA256) != 64 {
		t.Fatal(s)
	}
	h, err := c.History(ctx, "target")
	if err != nil || len(h.Snapshots) != 1 || len(h.Incomplete) != 0 {
		t.Fatal(h, err)
	}
	if _, err = c.Verify(ctx, "target", s.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err = fm.Open("target", ".nasctl-backups/"+s.ID+"/data/.hidden"); !errors.Is(err, files.ErrOutsideRoot) {
		t.Fatal("private file API", err)
	}
	restored, err := c.Restore(ctx, backups.Restore{Root: "target", ID: s.ID, ToRoot: "recovered", To: "restored", ManifestSHA256: s.ManifestSHA256}, nil)
	if err != nil || !restored.Published {
		t.Fatal(restored, err)
	}
	for _, p := range []string{"folder/hello.txt", ".hidden"} {
		a, e := os.ReadFile(filepath.Join(source, p))
		if e != nil {
			t.Fatal(e)
		}
		b, e := os.ReadFile(filepath.Join(recovered, "restored", p))
		if e != nil || string(a) != string(b) {
			t.Fatal("restored bytes", string(b), e)
		}
	}
	link, err := os.Readlink(filepath.Join(recovered, "restored", "link"))
	if err != nil || link != filepath.Join(filepath.Dir(source), "outside") {
		t.Fatal(link, err)
	}
	if _, err = c.Restore(ctx, backups.Restore{Root: "target", ID: s.ID, ToRoot: "recovered", To: "restored", ManifestSHA256: s.ManifestSHA256}, nil); !errors.Is(err, files.ErrExists) {
		t.Fatal("overwrite", err)
	}
	// Corruption must be detected before a new destination is published.
	if err = os.WriteFile(filepath.Join(target, ".nasctl-backups", s.ID, "data", "folder", "hello.txt"), []byte("corrupt bytes"), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Verify(ctx, "target", s.ID, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatal("corruption not detected", err)
	}
	if _, err = c.Restore(ctx, backups.Restore{Root: "target", ID: s.ID, ToRoot: "recovered", To: "corrupt", ManifestSHA256: s.ManifestSHA256}, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatal("corrupt restore", err)
	}
	if _, err = os.Stat(filepath.Join(recovered, "corrupt")); !os.IsNotExist(err) {
		t.Fatal("corrupt destination published", err)
	}
}

func TestBackupCancelBudgetIncompleteCleanupAndPlanPersistence(t *testing.T) {
	c, fm, source, _, _, p := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	_, err := c.Run(ctx, p.ID, p.Digest(), func(progress backups.Progress) {
		if progress.Phase == "copy" && progress.Bytes > 0 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	h, err := c.History(context.Background(), "target")
	if err != nil || len(h.Snapshots) != 0 || len(h.Incomplete) != 1 {
		t.Fatal(h, err)
	}
	if _, err = c.Cleanup(context.Background(), "target", h.Incomplete[0], nil); err != nil {
		t.Fatal(err)
	}
	h, err = c.History(context.Background(), "target")
	if err != nil || len(h.Incomplete) != 0 {
		t.Fatal(h, err)
	}
	b, err := os.ReadFile(filepath.Join(source, "folder", "hello.txt"))
	if err != nil || string(b) != "original data" {
		t.Fatal(string(b), err)
	}
	p.MaxBytes = 2
	if err = c.Save(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(context.Background(), p.ID, p.Digest(), nil); !errors.Is(err, files.ErrTreeLimit) {
		t.Fatal(err)
	}
	h, err = c.History(context.Background(), "target")
	if err != nil || len(h.Snapshots) != 0 || len(h.Incomplete) != 1 {
		t.Fatal(h, err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(filepath.Join(filepath.Dir(source), "state"), fm)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.Settings().Plans) != 1 || reopened.Settings().Plans[0].MaxBytes != 2 {
		t.Fatal("plans not durable", reopened.Settings())
	}
}

func TestBackupRetentionChangedConsentAndPermanentDeletion(t *testing.T) {
	c, _, source, _, _, p := fixture(t)
	ctx := context.Background()
	p.Keep = 1
	if err := c.Save(ctx, p); err != nil {
		t.Fatal(err)
	}
	first, err := c.Run(ctx, p.ID, p.Digest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, ".hidden"), []byte("new bytes"), 0640); err != nil {
		t.Fatal(err)
	}
	second, err := c.Run(ctx, p.ID, p.Digest(), nil)
	if err != nil || second.Pruned != 1 {
		t.Fatal(second, err)
	}
	h, err := c.History(ctx, "target")
	if err != nil || len(h.Snapshots) != 1 || h.Snapshots[0].ID != second.Snapshot.ID {
		t.Fatal(h, err)
	}
	if _, err = c.Verify(ctx, "target", first.Snapshot.ID, nil); err == nil {
		t.Fatal("retention left old version")
	}
	p.Name = "Changed scope"
	if err = c.Save(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(ctx, p.ID, strings.Repeat("0", 64), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("stale consent", err)
	}
	if _, err = c.Delete(ctx, "target", second.Snapshot.ID, "wrong", nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("delete unbound", err)
	}
	if _, err = c.Delete(ctx, "target", second.Snapshot.ID, second.Snapshot.ManifestSHA256, nil); err != nil {
		t.Fatal(err)
	}
	h, err = c.History(ctx, "target")
	if err != nil || len(h.Snapshots) != 0 {
		t.Fatal(h, err)
	}
	if err = c.RemovePlan(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if len(c.Settings().Plans) != 0 {
		t.Fatal(c.Settings())
	}
}

func TestBackupVaultCrossInstanceLockAndSettingsStayVisible(t *testing.T) {
	c, fm, source, _, _, p := fixture(t)
	other, err := New(filepath.Join(filepath.Dir(source), "state"), fm)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	done := make(chan error, 1)
	go func() {
		once := false
		_, err := c.Run(context.Background(), p.ID, p.Digest(), func(progress backups.Progress) {
			if !once && progress.Phase == "copy" {
				once = true
				close(entered)
				<-release
			}
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("backup never started")
	}
	settings := c.Settings()
	if !settings.Busy || len(settings.Plans) != 1 {
		t.Fatal(settings)
	}
	if _, err = other.Run(context.Background(), p.ID, p.Digest(), nil); !errors.Is(err, ErrBusy) {
		t.Fatal("cross-instance run", err)
	}
	close(release)
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("backup did not finish")
	}
}

func TestChangedStorageScopeCannotReplaceLastGoodBackup(t *testing.T) {
	c, _, source, target, recovered, p := fixture(t)
	p.Keep = 1
	if err := c.Save(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p = c.Settings().Plans[0]
	first, err := c.Run(context.Background(), p.ID, p.Digest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	newSource := t.TempDir()
	fm, err := files.New([]config.HubStorage{{ID: "source", Path: newSource}, {ID: "target", Path: target, Writable: true}, {ID: "recovered", Path: recovered, Writable: true}}, files.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer fm.Close()
	changed, err := New(filepath.Join(filepath.Dir(source), "state"), fm)
	if err != nil {
		t.Fatal(err)
	}
	defer changed.Close()
	if _, err = changed.Run(context.Background(), p.ID, p.Digest(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("changed/missing-mount scope accepted", err)
	}
	h, err := changed.History(context.Background(), "target")
	if err != nil || len(h.Snapshots) != 1 || h.Snapshots[0].ID != first.Snapshot.ID {
		t.Fatal("last good backup removed", h, err)
	}
	if _, err = changed.Verify(context.Background(), "target", first.Snapshot.ID, nil); err != nil {
		t.Fatal(err)
	}
	// Saving explicitly accepts the new configured filesystem/location.
	if err = changed.Save(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	accepted := changed.Settings().Plans[0]
	if accepted.SourceScope == p.SourceScope {
		t.Fatal("source scope did not change")
	}
}

func TestRecreatedEmptyMountDirectoryCannotPruneGoodBackup(t *testing.T) {
	c, _, source, target, recovered, p := fixture(t)
	p.Keep = 1
	if err := c.Save(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p = c.Settings().Plans[0]
	first, err := c.Run(context.Background(), p.ID, p.Digest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(source, source+"-detached"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(source, 0750); err != nil {
		t.Fatal(err)
	}
	fm, err := files.New([]config.HubStorage{{ID: "source", Path: source}, {ID: "target", Path: target, Writable: true}, {ID: "recovered", Path: recovered, Writable: true}}, files.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer fm.Close()
	restarted, err := New(filepath.Join(filepath.Dir(source), "state"), fm)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err = restarted.Run(context.Background(), p.ID, p.Digest(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("empty replacement root accepted", err)
	}
	h, err := restarted.History(context.Background(), "target")
	if err != nil || len(h.Snapshots) != 1 || h.Snapshots[0].ID != first.Snapshot.ID {
		t.Fatal(h, err)
	}
}

func TestCleanupRefusesUnknownPrivateWork(t *testing.T) {
	c, _, _, target, _, p := fixture(t)
	if _, err := c.Run(context.Background(), p.ID, p.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	name := "work-" + strings.Repeat("a", 32)
	dir := filepath.Join(target, ".nasctl-backups", name)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unknown"), []byte("preserve unknown data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Cleanup(context.Background(), "target", name, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatal("unmarked cleanup accepted", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "unknown"))
	if err != nil || string(b) != "preserve unknown data" {
		t.Fatal(string(b), err)
	}
}
