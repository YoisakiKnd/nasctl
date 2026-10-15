package hubstore

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/statelease"
)

func TestDatabaseLifetimeBlocksOfflineRestore(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "hub.db"), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = statelease.Acquire(dir, true); !errors.Is(err, statelease.ErrBusy) {
		db.Close()
		t.Fatal("live hub state allowed restore", err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	restore, err := statelease.Acquire(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer restore.Close()
	if db, err = Open(filepath.Join(dir, "hub.db"), DefaultLimits()); !errors.Is(err, statelease.ErrBusy) {
		if db != nil {
			db.Close()
		}
		t.Fatal("writer opened while restore active", err)
	}
}
