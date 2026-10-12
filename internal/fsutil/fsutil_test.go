package fsutil

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSafeRelJoinRejectsEscapes is the path-traversal gate. Every served
// original and every decoded file passes through this function, so it is the
// single place where a crafted relative path could reach outside a collection.
func TestSafeRelJoinRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		rel  string
		want bool // true means "must be rejected"
	}{
		{"a/b/c.jpg", false},
		{"IMG_0001.jpg", false},
		{"nested/中文.jpg", false},
		{"", true},   // an empty relative path is a bug in the caller
		{"..", true}, // the parent of the root
		{"./a.jpg", false},

		{"../etc/passwd", true},
		{"../../etc/passwd", true},
		{"a/../../etc/passwd", true},
		{"a/b/../../../etc/passwd", true},
		{"/etc/passwd", true},
		{"//etc/passwd", true},
		{"a/../../../../../../../../etc/passwd", true},
		{"..\\..\\windows\\system32", false}, // backslashes are ordinary name characters on Linux
	}
	for _, c := range cases {
		got, err := SafeRelJoin(root, c.rel)
		if c.want {
			if err == nil {
				t.Errorf("SafeRelJoin(%q) was accepted as %q; it must be refused", c.rel, got)
			} else if !errors.Is(err, ErrOutsideRoot) && err.Error() != "fsutil: empty relative path" {
				t.Errorf("SafeRelJoin(%q) failed with %v; an escape must report ErrOutsideRoot so "+
					"callers can distinguish it from a bug", c.rel, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("SafeRelJoin(%q) refused a legitimate path: %v", c.rel, err)
			continue
		}
		if !IsWithin(root, got) {
			t.Errorf("SafeRelJoin(%q) = %q which is outside %q", c.rel, got, root)
		}
	}
}

func TestSafeRelJoinRefusesAbsoluteSecondArgument(t *testing.T) {
	root := t.TempDir()
	if got, err := SafeRelJoin(root, root+"/x"); err == nil {
		t.Fatalf("an absolute path was accepted as %q", got)
	}
	if got, err := SafeRelJoin(root, "/etc/passwd"); err == nil {
		t.Fatalf("an absolute path outside the root was accepted as %q", got)
	}
}

// TestResolveWithinRefusesSymlinkEscape is the second gate. A relative path can
// be perfectly well formed and still land outside the root, because a directory
// inside the collection is a symlink to somewhere else.
func TestResolveWithinRefusesSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.jpg")
	if err := os.WriteFile(secret, []byte("not really a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.jpg"), []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "direct.jpg")); err != nil {
		t.Fatal(err)
	}
	// A symlink that stays inside the root is fine.
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inner.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(sub, "inner.jpg"), filepath.Join(root, "inside.jpg")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		rel  string
		want bool
	}{
		{"ok.jpg", true},
		{"inside.jpg", true},
		{"link/secret.jpg", false}, // escapes through a symlinked directory
		{"direct.jpg", false},      // a symlink pointing out of the tree
	}
	for _, c := range cases {
		joined, err := SafeRelJoin(root, c.rel)
		if err != nil {
			if c.want {
				t.Errorf("%s: SafeRelJoin refused a legitimate path: %v", c.rel, err)
			}
			continue
		}
		got, err := ResolveWithin(root, joined)
		if c.want {
			if err != nil {
				t.Errorf("%s: ResolveWithin refused a legitimate path: %v", c.rel, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: ResolveWithin accepted %q, which is outside %q", c.rel, got, root)
		}
	}
}

func TestResolveWithinRefusesNonexistentTarget(t *testing.T) {
	root := t.TempDir()
	// A missing file must not be reported as a resolved path; callers rely on
	// the error to distinguish "gone" from "present".
	if _, err := ResolveWithin(root, filepath.Join(root, "nope.jpg")); err == nil {
		t.Fatal("ResolveWithin accepted a path that does not exist")
	}
}

// TestAtomicWriteLeavesNoPartialFile covers the interrupted-write case: a cache
// entry that exists must always be complete, so a failed write must leave the
// destination absent or untouched.
func TestAtomicWriteLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "thumb.jpg")

	// The destination already holds a valid previous value.
	if err := AtomicWrite(dst, 0o640, func(w io.Writer) error {
		_, err := w.Write([]byte("OLD"))
		return err
	}); err != nil {
		t.Fatalf("seeding the destination: %v", err)
	}

	boom := errors.New("decoder exploded halfway through")
	err := AtomicWrite(dst, 0o640, func(w io.Writer) error {
		_, _ = w.Write([]byte("PARTIAL"))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("AtomicWrite returned %v, want the writer's error", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading destination after a failed write: %v", err)
	}
	if string(got) != "OLD" {
		t.Fatalf("a failed atomic write replaced the destination with %q; it must be left alone", got)
	}
	assertNoTempArtifacts(t, dir)

	// And a successful write does replace it.
	if err := AtomicWrite(dst, 0o640, func(w io.Writer) error {
		_, err := w.Write([]byte("NEW"))
		return err
	}); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	got, _ = os.ReadFile(dst)
	if string(got) != "NEW" {
		t.Fatalf("destination is %q after a successful write", got)
	}
	assertNoTempArtifacts(t, dir)
}

func TestAtomicWriteCreatesMissingParentDirectories(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "a", "b", "c", "t.jpg")
	if err := AtomicWrite(dst, 0o640, func(w io.Writer) error {
		_, err := w.Write([]byte("x"))
		return err
	}); err != nil {
		t.Fatalf("AtomicWrite into a missing tree: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode is %v, want 0640", fi.Mode().Perm())
	}
}

func assertNoTempArtifacts(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if IsTempArtifact(e.Name()) {
			t.Errorf("a temporary artifact %q survived; an interrupted write must not leave one behind", e.Name())
		}
	}
}

func TestRemoveTempArtifactsRespectsAge(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, ".albumctl.tmp-old")
	fresh := filepath.Join(dir, ".albumctl.tmp-fresh")
	keep := filepath.Join(dir, "thumb_512.jpg")
	for _, p := range []string{old, fresh, keep} {
		if err := os.WriteFile(p, []byte("12345"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	removed, freed, err := RemoveTempArtifacts(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || freed != 5 {
		t.Fatalf("removed %d files (%d bytes), want 1 (5)", removed, freed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("the stale temp file survived")
	}
	for _, p := range []string{fresh, keep} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed but should have been kept: %v", filepath.Base(p), err)
		}
	}
}

// TestFileUnchangedDetectsReplacement covers "the file changed while we were
// working on it". Writers that replace a file in place keep the size and the
// mtime unless the filesystem reports sub-second resolution, so both are
// checked and the mtime check must be able to fail on its own.
func TestFileUnchangedDetectsReplacement(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.jpg")
	if err := os.WriteFile(p, []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := FileUnchanged(p, fi.Size(), fi.ModTime().UnixNano())
	if err != nil || !ok {
		t.Fatalf("FileUnchanged said %v/%v about a file that has not changed", ok, err)
	}

	// Size change.
	if err := os.WriteFile(p, []byte("aaaaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := FileUnchanged(p, fi.Size(), fi.ModTime().UnixNano()); ok {
		t.Error("FileUnchanged missed a size change")
	}

	// mtime change with the same size.
	other := time.Now().Add(-90 * time.Minute)
	if err := os.Chtimes(p, other, other); err != nil {
		t.Fatal(err)
	}
	if ok, _ := FileUnchanged(p, int64(len("aaaaaa")), fi.ModTime().UnixNano()); ok {
		t.Error("FileUnchanged missed an mtime change")
	}
}

func TestDeviceIDAndFSIDAreReadable(t *testing.T) {
	dir := t.TempDir()
	dev, err := DeviceID(dir)
	if err != nil {
		t.Fatalf("DeviceID: %v", err)
	}
	if dev == 0 {
		t.Error("DeviceID returned 0 for an existing directory")
	}
	fsid, err := FSID(dir)
	if err != nil {
		t.Skipf("this filesystem does not expose an fsid: %v", err)
	}
	if fsid == "" {
		t.Error("FSID returned an empty identity for an existing directory")
	}
	key, err := Identify(dir)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if key.FSID != fsid && key.FSID != "" {
		t.Errorf("Identify and FSID disagree: %q vs %q", key.FSID, fsid)
	}
}

func TestIsWithin(t *testing.T) {
	base := "/mnt/raid0/photos"
	cases := map[string]bool{
		"/mnt/raid0/photos":        true,
		"/mnt/raid0/photos/a/b":    true,
		"/mnt/raid0/photos-other":  false, // a sibling with a shared prefix
		"/mnt/raid0":               false,
		"/etc":                     false,
		"/mnt/raid0/photos/../etc": false,
	}
	for p, want := range cases {
		if got := IsWithin(base, p); got != want {
			t.Errorf("IsWithin(%q, %q) = %v, want %v", base, p, got, want)
		}
	}
}

func TestDirUsageAndFreeBytes(t *testing.T) {
	dir := t.TempDir()
	for i, n := range []int{10, 20, 30} {
		sub := filepath.Join(dir, "s"+string(rune('a'+i)))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "f"), make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, bytes, err := DirUsage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if files != 3 || bytes != 60 {
		t.Errorf("DirUsage = %d files, %d bytes; want 3, 60", files, bytes)
	}
	free, total, err := FreeBytes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 || free > total {
		t.Errorf("FreeBytes returned free=%d total=%d", free, total)
	}
}

func TestIsTempArtifactRecognisesOurOwnNames(t *testing.T) {
	ours := []string{".albumctl.tmp-123", ".albumctl.tmp-abcdef"}
	for _, n := range ours {
		if !IsTempArtifact(n) {
			t.Errorf("%q should be recognised as a temporary artifact", n)
		}
	}
	notOurs := []string{"thumb_512.jpg", "IMG_0001.jpg", "", ".hidden"}
	for _, n := range notOurs {
		if IsTempArtifact(n) {
			t.Errorf("%q must not be treated as a temporary artifact; "+
				"cache-gc would then be able to delete something that is not ours", n)
		}
	}
}

func TestSafeRelJoinAcceptsNULFreeOnly(t *testing.T) {
	root := t.TempDir()
	if _, err := SafeRelJoin(root, "a\x00b.jpg"); err == nil {
		t.Error("a relative path containing NUL was accepted")
	}
	if _, err := SafeRelJoin(root, "a/b/c.jpg"); err != nil {
		t.Errorf("an ordinary nested path was refused: %v", err)
	}
}
