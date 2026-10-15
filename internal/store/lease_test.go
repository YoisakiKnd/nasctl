package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/statelease"
)

func TestAlbumWriterLifetimeBlocksOfflineRestore(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "index.sqlite"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = statelease.Acquire(dir, true); !errors.Is(err, statelease.ErrBusy) {
		db.Close()
		t.Fatal("album writer allowed restore", err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	restore, err := statelease.Acquire(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer restore.Close()
	if db, err = Open(filepath.Join(dir, "index.sqlite"), Options{}); !errors.Is(err, statelease.ErrBusy) {
		if db != nil {
			db.Close()
		}
		t.Fatal("album writer opened during restore", err)
	}
}
