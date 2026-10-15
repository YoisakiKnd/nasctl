package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBatchConflictsContinueAndSharedBudgetStops(t *testing.T) {
	m, src, dst := copyRig(t, true)
	for _, name := range []string{"first", "conflict", "third", "fourth"} {
		mustWrite(t, filepath.Join(src, name), "1234")
	}
	mustWrite(t, filepath.Join(dst, "conflict"), "existing")
	o := BatchOptions{Action: "copy", ToRoot: "destination", Directory: ".", Limit: 3, MaxBytes: 8, Items: []BatchItem{{Root: "source", Path: "first"}, {Root: "source", Path: "conflict"}, {Root: "source", Path: "third"}, {Root: "source", Path: "fourth"}}}
	result, err := m.Batch(context.Background(), o, nil)
	if !errors.Is(err, ErrBatchIncomplete) || result.Completed != 2 || result.Failed != 1 || result.NotStarted != 1 || result.Bytes != 8 || result.Entries != 2 {
		t.Fatalf("%+v %v", result, err)
	}
	if result.Items[1].Code != "exists" || result.Items[3].Code != "budget_exhausted" {
		t.Fatal(result)
	}
	assertContent(t, filepath.Join(dst, "conflict"), "existing")
	assertContent(t, filepath.Join(dst, "third"), "1234")
	assertAbsent(t, filepath.Join(dst, "fourth"))
	// Entries are shared too: directory + file consumes the whole batch budget.
	if err := os.Mkdir(filepath.Join(src, "tree"), 0750); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, "tree", "data"), "tree")
	o.Items = []BatchItem{{Root: "source", Path: "tree"}, {Root: "source", Path: "fourth"}}
	o.Limit = 2
	o.MaxBytes = 100
	result, err = m.Batch(context.Background(), o, nil)
	if !errors.Is(err, ErrBatchIncomplete) || result.Completed != 1 || result.NotStarted != 1 || result.Entries != 2 {
		t.Fatalf("%+v %v", result, err)
	}
	assertAbsent(t, filepath.Join(dst, "fourth"))
}

func TestBatchCancelTrashRestoreAndMonotonicResults(t *testing.T) {
	m, src, _ := copyRig(t, true)
	for _, name := range []string{"a", "b", "c"} {
		mustWrite(t, filepath.Join(src, name), name)
	}
	o := BatchOptions{Action: "trash", Items: []BatchItem{{Root: "source", Path: "a"}, {Root: "source", Path: "b"}, {Root: "source", Path: "c"}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := m.Batch(ctx, o, func(p BatchResult) {
		if p.Completed == 1 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || result.Completed != 1 || result.NotStarted != 2 || len(result.Items) != 3 {
		t.Fatalf("%+v %v", result, err)
	}
	assertAbsent(t, filepath.Join(src, "a"))
	assertContent(t, filepath.Join(src, "b"), "b")
	trash, err := m.ListTrash("source", 1, 50)
	if err != nil || len(trash.Items) != 1 {
		t.Fatal(trash, err)
	}
	mustWrite(t, filepath.Join(src, "a"), "new")
	o = BatchOptions{Action: "restore", Items: []BatchItem{{Root: "source", ID: trash.Items[0].ID}}}
	result, err = m.Batch(context.Background(), o, nil)
	if !errors.Is(err, ErrBatchIncomplete) || result.Items[0].Code != "exists" {
		t.Fatal(result, err)
	}
	assertContent(t, filepath.Join(src, "a"), "new")
	if err = os.Remove(filepath.Join(src, "a")); err != nil {
		t.Fatal(err)
	}
	result, err = m.Batch(context.Background(), o, nil)
	if err != nil || result.Completed != 1 {
		t.Fatal(result, err)
	}
	assertContent(t, filepath.Join(src, "a"), "a")
}

func TestBatchValidatesAllPathsBeforeAnyWrite(t *testing.T) {
	m, src, dst := copyRig(t, true)
	mustWrite(t, filepath.Join(src, "data"), "keep")
	base := BatchOptions{Action: "copy", ToRoot: "destination", Directory: ".", Items: []BatchItem{{Root: "source", Path: "data"}}}
	for _, bad := range []BatchOptions{
		{Action: "purge", Items: base.Items},
		{Action: "copy", ToRoot: "destination", Directory: ".", Items: []BatchItem{{Root: "source", Path: "data"}, {Root: "source", Path: "../outside"}}},
		{Action: "trash", Items: []BatchItem{{Root: "source", Path: "tree"}, {Root: "source", Path: "tree/child"}}},
		{Action: "copy", ToRoot: "destination", Directory: ".", Items: base.Items, Limit: 10001},
		{Action: "restore", Items: []BatchItem{{Root: "source", ID: "bad"}}},
	} {
		if _, err := m.Batch(context.Background(), bad, nil); err == nil {
			t.Fatal("invalid batch accepted", bad)
		}
	}
	assertAbsent(t, filepath.Join(dst, "data"))
	assertContent(t, filepath.Join(src, "data"), "keep")
	m.limits.CopyMaxBytes = 3
	m.limits.CopyMaxEntries = 2
	m.limits.BatchMaxItems = 1
	if _, err := m.Copy(context.Background(), copyOptions("data", "data"), nil); !errors.Is(err, ErrTreeLimit) {
		t.Fatal("local policy bypassed", err)
	}
	base.MaxBytes = 4
	if _, err := m.Batch(context.Background(), base, nil); !errors.Is(err, ErrTreeLimit) {
		t.Fatal("batch local bytes bypassed", err)
	}
	base.MaxBytes = 3
	result, err := m.Batch(context.Background(), base, nil)
	if !errors.Is(err, ErrBatchIncomplete) || result.Items[0].Code != "budget_exhausted" {
		t.Fatal(result, err)
	}
	assertAbsent(t, filepath.Join(dst, "data"))
	base.Items = append(base.Items, base.Items[0])
	if _, err := m.Batch(context.Background(), base, nil); !errors.Is(err, ErrTreeLimit) {
		t.Fatal("selection limit bypassed", err)
	}
}

func TestCopyGrowthDoesNotWriteBeyondSharedByteBudget(t *testing.T) {
	m, src, dst := copyRig(t, true)
	p := filepath.Join(src, "growing")
	mustWrite(t, p, "1234")
	o := copyOptions("growing", "target")
	o.MaxBytes = 4
	changed := false
	result, err := m.Copy(context.Background(), o, func(_, bytes int64, _ string) {
		if bytes == 4 && !changed {
			changed = true
			mustWrite(t, p, "12345")
		}
	})
	if !errors.Is(err, ErrSourceChanged) || result.Bytes != 4 || result.Published {
		t.Fatal(result, err)
	}
	assertAbsent(t, filepath.Join(dst, "target"))
}
