package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/logx"
	"github.com/YoisakiKnd/nasctl/internal/store"
)

// A file the walk saw but could not re-index must stay out of the "missing"
// list. This is the difference between a photo that is being copied in right now
// and a photo that has gone: flagging the first as absent hides it from the
// library and stops serving its original until the next scan.
//
// The deferral is produced here by making the entry disagree with the file on
// disk, which is exactly what the walk observes for a file that grows while a
// scan is running.

func newTestRunner(t *testing.T, root string) (*Runner, *store.Store, int64, int64) {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Default()
	cfg.Source = "test"
	cfg.Paths.StateDir = filepath.Join(dir, "state")
	cfg.Paths.CacheDir = filepath.Join(dir, "cache")
	cfg.Paths.LockDir = filepath.Join(dir, "state", "locks")
	cfg.Paths.IndexFile = filepath.Join(dir, "state", "index.sqlite3")
	if err := os.MkdirAll(cfg.Paths.LockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.Paths.CacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Collections = []config.Collection{{
		Code: "test", Name: "Test", Root: root, Enabled: true,
		AllowSharedVolume: true,
	}}

	st, err := store.Open(cfg.Paths.IndexFile, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	colID, err := st.UpsertCollection(context.Background(), "test", "Test", root, "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.NextScanToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{Cfg: &cfg, Store: st, Log: logx.Discard()}, st, colID, token
}

func TestADeferredFileIsNotMarkedMissing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "IMG_0001.jpg")
	writeFile(t, path, "0123456789")

	r, st, colID, token := newTestRunner(t, root)
	ctx := context.Background()

	// First pass: a normal, stable entry, so a row exists.
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	res := &Result{}
	if err := r.record(ctx, colID, "test", token, Entry{
		AbsPath: path, RelPath: "IMG_0001.jpg", Name: "IMG_0001.jpg",
		Size: info.Size(), MtimeNS: info.ModTime().UnixNano(),
	}, res); err != nil {
		t.Fatal(err)
	}
	if res.Inserted != 1 {
		t.Fatalf("Inserted = %d, want 1", res.Inserted)
	}

	// Second pass, new token: the same file, but the entry claims an older size
	// and mtime, as the walk would have captured if the file were still being
	// written.
	token2, err := st.NextScanToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res2 := &Result{}
	if err := r.record(ctx, colID, "test", token2, Entry{
		AbsPath: path, RelPath: "IMG_0001.jpg", Name: "IMG_0001.jpg",
		Size: info.Size() + 4096, MtimeNS: info.ModTime().Add(-time.Hour).UnixNano(),
	}, res2); err != nil {
		t.Fatal(err)
	}
	if res2.Deferred != 1 {
		t.Fatalf("Deferred = %d, want 1 (the file did not match the entry)", res2.Deferred)
	}
	if res2.Inserted+res2.Changed+res2.Unchanged != 0 {
		t.Fatalf("a deferred file must not be counted as indexed: %+v", res2)
	}
	if len(res2.Notes) == 0 {
		t.Error("a deferral must leave a note saying which file and why")
	}

	// The row must still be considered present, and must still carry its
	// original size rather than the stale number from the entry.
	marked, err := st.MarkMissing(ctx, colID, token2)
	if err != nil {
		t.Fatal(err)
	}
	if marked != 0 {
		t.Fatalf("MarkMissing flagged %d row(s); a deferred file is present and must not be "+
			"recorded as gone", marked)
	}
	tt, err := st.Totals(ctx, colID)
	if err != nil {
		t.Fatal(err)
	}
	if tt.Missing != 0 {
		t.Errorf("Missing = %d, want 0", tt.Missing)
	}

	// And the same walk, once the file is stable again, must still agree it is
	// there — the deferral must not have left the row in a half-written state.
	token3, err := st.NextScanToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	info2, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	res3 := &Result{}
	if err := r.record(ctx, colID, "test", token3, Entry{
		AbsPath: path, RelPath: "IMG_0001.jpg", Name: "IMG_0001.jpg",
		Size: info2.Size(), MtimeNS: info2.ModTime().UnixNano(),
	}, res3); err != nil {
		t.Fatal(err)
	}
	if res3.Unchanged != 1 {
		t.Fatalf("Unchanged = %d, want 1: the row's size must not have been rewritten by the "+
			"deferred pass", res3.Unchanged)
	}
}

func TestADeferredFileWithNoRowDoesNotInventOne(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "IMG_0002.jpg")
	writeFile(t, path, "x")

	r, st, colID, token := newTestRunner(t, root)
	ctx := context.Background()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	res := &Result{}
	if err := r.record(ctx, colID, "test", token, Entry{
		AbsPath: path, RelPath: "IMG_0002.jpg", Name: "IMG_0002.jpg",
		Size: info.Size() + 1, MtimeNS: info.ModTime().UnixNano(),
	}, res); err != nil {
		t.Fatal(err)
	}
	if res.Deferred != 1 {
		t.Fatalf("Deferred = %d, want 1", res.Deferred)
	}
	cols, err := st.Collections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || cols[0].MediaCount != 0 {
		t.Fatalf("a deferred file with no row must not create one: %+v", cols)
	}
}

func TestAVanishedFileIsMarkedMissingOnACompleteWalk(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "IMG_0003.jpg")
	gone := filepath.Join(root, "IMG_0004.jpg")
	writeFile(t, keep, "keep")
	writeFile(t, gone, "gone")

	r, st, colID, token := newTestRunner(t, root)
	ctx := context.Background()

	for _, p := range []string{keep, gone} {
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.record(ctx, colID, "test", token, Entry{
			AbsPath: p, RelPath: filepath.Base(p), Name: filepath.Base(p),
			Size: info.Size(), MtimeNS: info.ModTime().UnixNano(),
		}, &Result{}); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	// The next complete walk sees only the surviving file.
	token2, err := st.NextScanToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(keep)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.record(ctx, colID, "test", token2, Entry{
		AbsPath: keep, RelPath: "IMG_0003.jpg", Name: "IMG_0003.jpg",
		Size: info.Size(), MtimeNS: info.ModTime().UnixNano(),
	}, &Result{}); err != nil {
		t.Fatal(err)
	}
	marked, err := st.MarkMissing(ctx, colID, token2)
	if err != nil {
		t.Fatal(err)
	}
	if marked != 1 {
		t.Fatalf("MarkMissing flagged %d row(s), want exactly the removed file", marked)
	}
	tt, err := st.Totals(ctx, colID)
	if err != nil {
		t.Fatal(err)
	}
	if tt.Missing != 1 {
		t.Fatalf("Missing = %d, want 1", tt.Missing)
	}
	if tt.Media != 2 {
		t.Errorf("Media = %d, want 2: a missing row must be flagged, never deleted", tt.Media)
	}
}

// Files already in the index do not count against the limit, so running the
// same limited scan again works through the library instead of stopping at
// its first files every time.
func TestRepeatedLimitedScansCoverTheWholeCollection(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 12; i++ {
		writeFile(t, filepath.Join(root, "IMG_"+string(rune('a'+i))+".jpg"), "not really a jpeg")
	}
	r, st, colID, _ := newTestRunner(t, root)
	r.Cfg.Collections[0].EnabledKinds = []string{"image"}
	ctx := context.Background()
	var inserted int64
	for run := 1; run <= 3; run++ {
		res, err := r.Run(ctx, Options{CollectionCode: "test", Limit: 5, MaxRuntime: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		inserted += res.Inserted
		if want := run < 3; res.More != want {
			t.Fatalf("run %d: More=%v, want %v\n%s", run, res.More, want, res.HumanReport())
		}
	}
	if inserted != 12 {
		t.Fatalf("three runs of 5 indexed %d of 12 files", inserted)
	}
	totals, err := st.Totals(ctx, colID)
	if err != nil || totals.Media != 12 {
		t.Fatal(totals, err)
	}
}
