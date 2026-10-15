package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/config"
)

func writableTree(t *testing.T) (*Manager, string, string) {
	t.Helper()
	_, root, outside := newTree(t)
	limits := DefaultLimits()
	limits.UploadMaxBytes = 32
	limits.TrashMaxEntries = 3
	m, err := New([]config.HubStorage{{ID: "data", Path: root, Writable: true, ExposeHidden: true}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, root, outside
}
func TestFileWritesNeverOverwriteAndStayInsideRoot(t *testing.T) {
	m, root, outside := writableTree(t)
	ctx := context.Background()
	if err := m.Mkdir(ctx, "data", "created"); err != nil {
		t.Fatal(err)
	}
	if err := m.Mkdir(ctx, "data", "created"); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	if _, err := m.Upload(ctx, "data", "created/new.txt", strings.NewReader("new bytes"), 9); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upload(ctx, "data", "created/new.txt", strings.NewReader("overwrite"), 9); !errors.Is(err, ErrExists) {
		t.Fatal("existing upload overwritten", err)
	}
	if err := m.Rename(ctx, "data", "created/new.txt", "a.txt"); !errors.Is(err, ErrExists) {
		t.Fatal("existing rename overwritten", err)
	}
	if err := m.Rename(ctx, "data", "created/new.txt", "created/renamed.txt"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../outside/secret.txt", "/etc/passwd", "escape-dir/secret.txt", trashDirectory + "/entry", stagingDirectory + "/entry", writeLockFile} {
		if _, err := m.Upload(ctx, "data", p, strings.NewReader("changed"), 7); err == nil {
			t.Fatalf("unsafe write accepted %s", p)
		}
	}
	content, _ := os.ReadFile(filepath.Join(outside, "secret.txt"))
	if string(content) != "SECRET-CONTENT" {
		t.Fatal("outside content changed")
	}
	content, _ = os.ReadFile(filepath.Join(root, "a.txt"))
	if string(content) != "aaa" {
		t.Fatal("existing file changed")
	}
	if err := m.Rename(ctx, "data", "created", "created/descendant"); !errors.Is(err, ErrBadPath) {
		t.Fatal("moved directory inside itself")
	}
	listing, err := m.List("data", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range listing.Entries {
		if internalName(entry.Name) {
			t.Fatal("internal state exposed even with expose_hidden")
		}
	}
	for _, p := range []string{trashDirectory, stagingDirectory, writeLockFile} {
		if _, err := m.Stat("data", p); !errors.Is(err, ErrOutsideRoot) {
			t.Fatal("internal state stat allowed", p, err)
		}
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestUploadFailuresLeaveNoPublishedFileAndReleaseLocks(t *testing.T) {
	m, root, _ := writableTree(t)
	ctx := context.Background()
	for name, source := range map[string]io.Reader{"interrupted.txt": failedReader{}, "oversized.txt": strings.NewReader(strings.Repeat("x", 33))} {
		if _, err := m.Upload(ctx, "data", name, source, -1); err == nil {
			t.Fatal("failed stream accepted", name)
		}
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("partial file published", name)
		}
	}
	if _, err := m.Upload(ctx, "data", "size.txt", strings.NewReader("short"), 10); !errors.Is(err, ErrSizeMismatch) {
		t.Fatal("size mismatch accepted", err)
	}
	if _, err := m.Upload(ctx, "data", "works.txt", strings.NewReader("complete"), 8); err != nil {
		t.Fatal("lock not released", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, stagingDirectory))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ".owner" {
		t.Fatal("failed upload left temporary data", entries)
	}
	_, unlock, err := m.lockWrite(ctx, "data")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Mkdir(ctx, "data", "blocked"); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrent write accepted", err)
	}
	unlock()
	other, err := New([]config.HubStorage{{ID: "data", Path: root, Writable: true}}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	_, unlock, err = m.lockWrite(ctx, "data")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Mkdir(ctx, "data", "cross-process"); !errors.Is(err, ErrBusy) {
		t.Fatal("filesystem lock bypassed", err)
	}
	unlock()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Upload(cancelled, "data", "cancelled", strings.NewReader("x"), 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestTrashRecoveryAndNoOverwriteAfterRestart(t *testing.T) {
	m, root, _ := writableTree(t)
	ctx := context.Background()
	item, err := m.Trash(ctx, "data", "sub")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source remains")
	}
	reopened, err := New([]config.HubStorage{{ID: "data", Path: root, Writable: true}}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	listing, err := reopened.ListTrash("data", 1, 50)
	if err != nil || len(listing.Items) != 1 || listing.Items[0].OriginalPath != "sub" {
		t.Fatalf("durable trash: %+v %v", listing, err)
	}
	if err = os.Mkdir(filepath.Join(root, "sub"), 0750); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.Restore(ctx, "data", item.ID, ""); !errors.Is(err, ErrExists) {
		t.Fatal("restore replaced existing directory", err)
	}
	if _, err = reopened.Restore(ctx, "data", item.ID, "alternate/sub"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "alternate", "sub", "c.txt"))
	if err != nil || string(content) != "ccccc" {
		t.Fatal("directory contents lost", err)
	}
	listing, err = reopened.ListTrash("data", 1, 50)
	if err != nil || len(listing.Items) != 0 {
		t.Fatal("restored entry still in trash")
	}
	item, err = m.Trash(ctx, "data", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, trashDirectory, item.ID, "metadata.json"), []byte(`{"version":1,"path":"../outside","deleted_at":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	listing, err = m.ListTrash("data", 1, 50)
	if err != nil || !listing.Items[0].RecoveryRequired {
		t.Fatal("corrupt recovery metadata hidden")
	}
	if _, err = m.Restore(ctx, "data", item.ID, ""); !errors.Is(err, ErrBadPath) {
		t.Fatal("unsafe manifest trusted")
	}
	if _, err = m.Restore(ctx, "data", item.ID, "recovered.txt"); err != nil {
		t.Fatal("explicit recovery failed", err)
	}
}
func TestReadonlyForeignInternalDirectoriesAndTrashBounds(t *testing.T) {
	m, root, _ := newTree(t)
	ctx := context.Background()
	if err := m.Mkdir(ctx, "data", "no"); !errors.Is(err, ErrReadOnly) {
		t.Fatal("readonly root wrote", err)
	}
	writable, err := New([]config.HubStorage{{ID: "data", Path: root, Writable: true}}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer writable.Close()
	if err = os.Mkdir(filepath.Join(root, stagingDirectory), 0750); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, stagingDirectory, strings.Repeat("a", 32))
	if err = os.WriteFile(foreign, []byte("user data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = writable.Upload(ctx, "data", "new.txt", strings.NewReader("x"), 1); !errors.Is(err, ErrUnsupported) {
		t.Fatal("foreign staging directory adopted", err)
	}
	if content, _ := os.ReadFile(foreign); string(content) != "user data" {
		t.Fatal("foreign data removed")
	}
	bounded, _, _ := writableTree(t)
	for _, name := range []string{"a.txt", "b.log", "sub"} {
		if _, err := bounded.Trash(ctx, "data", name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := bounded.Trash(ctx, "data", "inside-link"); !errors.Is(err, ErrTrashFull) {
		t.Fatal("unbounded trash", err)
	}
}

func TestHiddenEntriesConsumeListingAndSearchBudgets(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 25; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf(".hidden-%02d", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "visible.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	limits := DefaultLimits()
	limits.MaxEntries = 3
	limits.SearchMaxExamined = 3
	m, err := New([]config.HubStorage{{ID: "data", Path: root}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	listing, err := m.List("data", ListOptions{})
	if err != nil || !listing.Truncated {
		t.Fatalf("hidden entries bypassed listing cap: %+v %v", listing, err)
	}
	result, err := m.Search("data", SearchOptions{Query: "visible"})
	if err != nil || !result.Truncated || result.Examined != 3 {
		t.Fatalf("hidden entries bypassed search cap: %+v %v", result, err)
	}
}
