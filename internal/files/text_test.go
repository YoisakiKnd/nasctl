package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReadTextBoundsAndRecognisesBinary(t *testing.T) {
	m, root, _ := writableTree(t)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("note.md", "# 标题\n正文\n")
	text, err := m.ReadText("data", "note.md", 0)
	if err != nil || string(text.Content) != "# 标题\n正文\n" || text.Binary || text.Truncated || len(text.Revision) != 64 {
		t.Fatalf("text = %+v, %v", text, err)
	}
	write("blob.txt", "PK\x00\x03binary")
	if text, err = m.ReadText("data", "blob.txt", 0); err != nil || !text.Binary || string(text.Content) != "" || text.Revision != "" {
		t.Fatalf("binary = %+v, %v", text, err)
	}
	write("latin1.txt", "caf\xe9")
	if text, _ = m.ReadText("data", "latin1.txt", 0); !text.Binary {
		t.Fatalf("invalid UTF-8 shown as text: %+v", text)
	}
	// A multi-byte character cut at the limit still reads as text.
	write("big.log", strings.Repeat("a", TextMaxBytes-1)+"中文")
	text, err = m.ReadText("data", "big.log", 0)
	if err != nil || !text.Truncated || text.Binary || text.Revision != "" || len(text.Content) != TextMaxBytes-1 {
		t.Fatalf("truncated = binary %v truncated %v rev %q len %d, %v", text.Binary, text.Truncated, text.Revision, len(text.Content), err)
	}
	// An excerpt reads only what it shows.
	if text, err = m.ReadText("data", "note.md", 4); err != nil || string(text.Content) != "# " || !text.Truncated || text.Binary || text.Revision != "" {
		t.Fatalf("excerpt = %+v, %v", text, err)
	}
}

func TestSaveTextCreatesAndReplacesOnlyTheLoadedRevision(t *testing.T) {
	m, root, _ := writableTree(t)
	ctx := context.Background()
	if _, err := m.SaveText(ctx, "data", "notes/new.txt", []byte("x"), ""); err == nil {
		t.Fatal("created a file in a missing folder")
	}
	if _, err := m.SaveText(ctx, "data", "new.txt", []byte("first\n"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SaveText(ctx, "data", "new.txt", []byte("again"), ""); !errors.Is(err, ErrExists) {
		t.Fatalf("create replaced an existing file: %v", err)
	}
	path := filepath.Join(root, "new.txt")
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	loaded, _ := m.ReadText("data", "new.txt", 0)
	// Someone else changes the file after it was loaded.
	if err := os.WriteFile(path, []byte("changed elsewhere\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SaveText(ctx, "data", "new.txt", []byte("mine\n"), loaded.Revision); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("stale revision saved: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "changed elsewhere\n" {
		t.Fatalf("the newer content was lost: %q", b)
	}
	current, _ := m.ReadText("data", "new.txt", 0)
	e, err := m.SaveText(ctx, "data", "new.txt", []byte("mine\n"), current.Revision)
	if err != nil || e.Size != 5 {
		t.Fatalf("save = %+v, %v", e, err)
	}
	info, _ := os.Stat(path)
	if b, _ := os.ReadFile(path); string(b) != "mine\n" || info.Mode().Perm() != 0o664 {
		t.Fatalf("saved %q with mode %v", b, info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(filepath.Join(root, stagingDirectory)); len(entries) != 1 { // only .owner
		t.Fatalf("staging left behind: %v", entries)
	}
}

func TestSaveTextRefusesWhatItCannotSafelyReplace(t *testing.T) {
	m, root, _ := writableTree(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}
	rev := Revision([]byte("real"))
	if err := os.Symlink("real.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SaveText(ctx, "data", "link.txt", []byte("x"), rev); !errors.Is(err, ErrNotText) {
		t.Fatalf("symlink replaced: %v", err)
	}
	if err := os.Link(filepath.Join(root, "real.txt"), filepath.Join(root, "hard.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SaveText(ctx, "data", "hard.txt", []byte("x"), rev); !errors.Is(err, ErrNotText) {
		t.Fatalf("hard link split: %v", err)
	}
	if _, err := m.SaveText(ctx, "data", "bin.txt", []byte("a\x00b"), ""); !errors.Is(err, ErrNotText) {
		t.Fatalf("binary content written: %v", err)
	}
	if _, err := m.SaveText(ctx, "data", "huge.txt", make([]byte, TextMaxBytes+1), ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized content written: %v", err)
	}
	for _, p := range []string{"../outside/secret.txt", stagingDirectory + "/x", trashDirectory + "/x"} {
		if _, err := m.SaveText(ctx, "data", p, []byte("x"), ""); err == nil {
			t.Fatalf("unsafe path accepted: %s", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "real.txt")); string(b) != "real" {
		t.Fatalf("real.txt changed: %q", b)
	}
}

func TestSaveTextNewFileFollowsTheUmask(t *testing.T) {
	m, root, _ := writableTree(t)
	old := syscall.Umask(0o002)
	defer syscall.Umask(old)
	if _, err := m.SaveText(context.Background(), "data", "shared.txt", []byte("x"), ""); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(filepath.Join(root, "shared.txt")); info.Mode().Perm() != 0o664 {
		t.Fatalf("mode = %v, want 0664 under umask 002", info.Mode().Perm())
	}
}
