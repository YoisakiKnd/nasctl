package scan

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The walk stats are the only account an operator gets of what a scan skipped,
// so they have to add up. Three separate counters exist precisely because
// lumping files into "excluded directories" once made a report claim that
// thousands of files had been dropped from a library that had not lost anything.

func writeFile(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mediaExt mirrors the real filter's contract: the extension arrives
// lower-cased and without the leading dot.
func mediaExt(ext string) bool {
	switch ext {
	case "jpg", "jpeg", "png":
		return true
	}
	return false
}

func collect(t *testing.T, root string, opts WalkOptions, exclude []string) (WalkStats, []string) {
	t.Helper()
	opts.Exclude = exclude
	var names []string
	st, err := WalkStream(root, opts, mediaExt, func(e Entry) error {
		names = append(names, e.RelPath)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkStream: %v", err)
	}
	return st, names
}

func TestTheWalkCountersAddUp(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a", "IMG_0001.jpg"), "x")
	writeFile(t, filepath.Join(root, "a", "notes.txt"), "x")
	writeFile(t, filepath.Join(root, "a", "IMG_0002.jpg.tmp"), "x")
	writeFile(t, filepath.Join(root, "a", ".DS_Store"), "x")
	writeFile(t, filepath.Join(root, "a", "._IMG_0003.jpg"), "x")
	writeFile(t, filepath.Join(root, "skipme", "IMG_0004.jpg"), "x")
	writeFile(t, filepath.Join(root, "b", "IMG_0005.png"), "x")
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	st, names := collect(t, root, WalkOptions{}, []string{"skipme"})

	if len(names) != 2 {
		t.Fatalf("kept %d files, want 2 (the two real pictures): %v", len(names), names)
	}
	// FilesSeen counts the files the walker actually considered: 5 inside "a"
	// plus 1 inside "b", minus the 2 dot-prefixed names that are skipped before
	// they are counted at all. Hidden names are reported on their own line so
	// that "files seen" and its breakdown stay consistent.
	if st.FilesSeen != 4 {
		t.Errorf("FilesSeen = %d, want 4", st.FilesSeen)
	}
	if st.FilesKept != 2 {
		t.Errorf("FilesKept = %d, want 2", st.FilesKept)
	}
	// "notes.txt" and "IMG_0002.jpg.tmp" are not media by extension.
	if st.NotMedia != 2 {
		t.Errorf("NotMedia = %d, want 2", st.NotMedia)
	}
	// The AppleDouble sidecar has a media extension and must still be skipped,
	// with .DS_Store. Both are counted as hidden, not as media.
	if st.Hidden != 2 {
		t.Errorf("Hidden = %d, want 2 (.DS_Store and ._IMG_0003.jpg)", st.Hidden)
	}
	// Only the *directory* named skipme is an exclusion. The picture inside it
	// is never seen, so it must not appear in any file counter.
	if st.Excluded != 1 {
		t.Errorf("Excluded = %d, want 1 (the directory)", st.Excluded)
	}
	if st.FilesByName != 0 {
		t.Errorf("FilesByName = %d, want 0", st.FilesByName)
	}
	if st.Symlinks != 0 || st.DepthCut != 0 || st.OtherFS != 0 || st.ReadErrors != 0 {
		t.Errorf("unexpected counters: symlinks=%d depth=%d otherfs=%d readerr=%d",
			st.Symlinks, st.DepthCut, st.OtherFS, st.ReadErrors)
	}
	// Every directory that was visited, including the empty one.
	if st.Dirs != 4 {
		t.Errorf("Dirs = %d, want 4 (root, a, b, empty)", st.Dirs)
	}
}

func TestAnExcludedFileNameIsCountedSeparatelyFromADirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "IMG_0001.jpg"), "x")
	writeFile(t, filepath.Join(root, "Thumbs.db"), "x")

	st, names := collect(t, root, WalkOptions{}, []string{"Thumbs.db"})
	if len(names) != 1 {
		t.Fatalf("kept %d, want 1: %v", len(names), names)
	}
	if st.FilesByName != 1 {
		t.Errorf("FilesByName = %d, want 1", st.FilesByName)
	}
	if st.Excluded != 0 {
		t.Errorf("Excluded = %d, want 0: a file must not be counted as an excluded directory", st.Excluded)
	}
}

func TestTheDepthLimitStopsTheWalkAndSaysSo(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "one", "two", "three", "four")
	writeFile(t, filepath.Join(deep, "IMG_0001.jpg"), "x")
	writeFile(t, filepath.Join(root, "one", "IMG_0002.jpg"), "x")

	st, names := collect(t, root, WalkOptions{MaxDepth: 2}, nil)

	if len(names) != 1 {
		t.Fatalf("kept %d files, want only the shallow one: %v", len(names), names)
	}
	if st.DepthCut != 1 {
		t.Errorf("DepthCut = %d, want 1", st.DepthCut)
	}
	if len(st.Notes) == 0 {
		t.Error("a truncated walk must leave a note; a silent depth cut looks like an empty directory")
	}
}

func TestSymlinksAreCountedAndNotFollowed(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "IMG_9999.jpg"), "outside")
	writeFile(t, filepath.Join(root, "IMG_0001.jpg"), "x")
	if err := os.Symlink(filepath.Join(outside, "IMG_9999.jpg"), filepath.Join(root, "link.jpg")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}

	st, names := collect(t, root, WalkOptions{}, nil)

	for _, n := range names {
		if n == "link.jpg" || n == "linkdir/IMG_9999.jpg" {
			t.Fatalf("a symlink was followed out of the collection: %v", names)
		}
	}
	if st.Symlinks != 2 {
		t.Errorf("Symlinks = %d, want 2", st.Symlinks)
	}
}

func TestAStopRequestEndsTheWalkWithoutAnError(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		writeFile(t, filepath.Join(root, n), "x")
	}
	seen := 0
	st, err := WalkStream(root, WalkOptions{}, mediaExt, func(Entry) error {
		seen++
		return StopWalk()
	})
	if err != nil {
		t.Fatalf("a stop must not be reported as an error: %v", err)
	}
	if seen != 1 {
		t.Errorf("saw %d entries after a stop, want 1", seen)
	}
	if st.FilesKept != 1 {
		t.Errorf("FilesKept = %d, want 1", st.FilesKept)
	}
}

func TestAWalkErrorIsNotAStop(t *testing.T) {
	boom := errors.New("boom")
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.jpg"), "x")
	_, err := WalkStream(root, WalkOptions{}, mediaExt, func(Entry) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the callback's own error", err)
	}
}

func TestAMissingRootIsAReadErrorNotSilence(t *testing.T) {
	st, err := WalkStream(filepath.Join(t.TempDir(), "nope"), WalkOptions{}, mediaExt, func(Entry) error { return nil })
	if err != nil {
		t.Fatalf("WalkStream: %v", err)
	}
	if st.ReadErrors != 1 {
		t.Errorf("ReadErrors = %d, want 1: an unreadable root must be visible in the report", st.ReadErrors)
	}
	if st.FilesKept != 0 {
		t.Errorf("FilesKept = %d, want 0", st.FilesKept)
	}
}
