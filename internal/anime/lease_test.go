package anime

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/statelease"
)

func TestAnimeLifetimeBlocksOfflineRestore(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenStore(filepath.Join(dir, "anime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = statelease.Acquire(dir, true); !errors.Is(err, statelease.ErrBusy) {
		db.Close()
		t.Fatal("live anime state allowed restore", err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	restore, err := statelease.Acquire(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer restore.Close()
	if db, err = OpenStore(filepath.Join(dir, "anime.sqlite")); !errors.Is(err, statelease.ErrBusy) {
		if db != nil {
			db.Close()
		}
		t.Fatal("anime opened during restore", err)
	}
}
