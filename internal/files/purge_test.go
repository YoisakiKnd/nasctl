package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPurgeExactConfirmationAndPreflightPreserveContents(t *testing.T) {
	m, root, _ := writableTree(t)
	ctx := context.Background()
	item, err := m.Trash(ctx, "data", "sub")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Purge(ctx, "data", item.ID, "other", 100, nil); !errors.Is(err, ErrConfirmation) {
		t.Fatal(err)
	}
	if _, err = m.Purge(ctx, "data", item.ID, item.Name, 1, nil); !errors.Is(err, ErrTreeLimit) {
		t.Fatal(err)
	}
	assertContent(t, filepath.Join(root, trashDirectory, item.ID, "entry", "c.txt"), "ccccc")
	assertAbsent(t, filepath.Join(root, trashDirectory, item.ID, ".deleting"))
	result, err := m.Purge(ctx, "data", item.ID, item.Name, 100, nil)
	if err != nil || result.Total != result.Removed || result.Removed < 3 {
		t.Fatal(result, err)
	}
	listing, err := m.ListTrash("data", 1, 50)
	if err != nil || len(listing.Items) != 0 {
		t.Fatal(listing, err)
	}
	assertAbsent(t, filepath.Join(root, trashDirectory, item.ID))
}

func TestPurgeCancellationMarksPartialContentsAndCanResumeOrRestore(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "restore"}[restore], func(t *testing.T) {
			m, root, _ := writableTree(t)
			item, err := m.Trash(context.Background(), "data", "sub")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result, err := m.Purge(ctx, "data", item.ID, item.Name, 100, func(done, _ int64) {
				if done == 1 {
					cancel()
				}
			})
			if !errors.Is(err, context.Canceled) || result.Removed != 1 {
				t.Fatal(result, err)
			}
			listing, err := m.ListTrash("data", 1, 50)
			if err != nil || len(listing.Items) != 1 || !listing.Items[0].PurgeStarted {
				t.Fatal(listing, err)
			}
			if restore {
				if _, err = m.Restore(context.Background(), "data", item.ID, ""); err != nil {
					t.Fatal(err)
				}
				if _, err = os.Stat(filepath.Join(root, "sub")); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = m.Purge(context.Background(), "data", item.ID, item.Name, 100, nil); err != nil {
					t.Fatal(err)
				}
			}
			listing, err = m.ListTrash("data", 1, 50)
			if err != nil || len(listing.Items) != 0 {
				t.Fatal(listing, err)
			}
		})
	}
}

func TestPurgeUnlinksSymlinkOnlyAndNeverAcceptsNormalPaths(t *testing.T) {
	m, root, outside := writableTree(t)
	ctx := context.Background()
	item, err := m.Trash(ctx, "data", "escape-dir")
	if err != nil {
		t.Fatal(err)
	}
	result, err := m.Purge(ctx, "data", item.ID, item.Name, 1, nil)
	if err != nil || result.Removed != 1 {
		t.Fatal(result, err)
	}
	assertContent(t, filepath.Join(outside, "secret.txt"), "SECRET-CONTENT")
	if _, err = m.Purge(ctx, "data", "a.txt", "a.txt", 100, nil); !errors.Is(err, ErrBadPath) {
		t.Fatal(err)
	}
	assertContent(t, filepath.Join(root, "a.txt"), "aaa")
}
