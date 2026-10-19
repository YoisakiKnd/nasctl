package files

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

type zipEntry struct {
	name, body string
	header     func(*zip.FileHeader)
}

func writeZip(t *testing.T, path string, entries ...zipEntry) {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.header != nil {
			e.header(h)
		}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func unicodePath(raw, name string) []byte {
	field := binary.LittleEndian.AppendUint16(nil, 0x7075)
	field = binary.LittleEndian.AppendUint16(field, uint16(5+len(name)))
	field = append(field, 1)
	field = binary.LittleEndian.AppendUint32(field, crc32.ChecksumIEEE([]byte(raw)))
	return append(field, name...)
}

func TestExtractZipUnpacksIntoANewFolder(t *testing.T) {
	m, root, _ := writableTree(t)
	ctx := context.Background()
	old := syscall.Umask(0o002)
	defer syscall.Umask(old)
	gbk, _ := simplifiedchinese.GBK.NewEncoder().String("照片/说明.txt")
	writeZip(t, filepath.Join(root, "photos.zip"),
		zipEntry{name: "a/b/deep.txt", body: "deep"}, // folders only implied
		zipEntry{name: gbk, body: "gbk", header: func(h *zip.FileHeader) { h.NonUTF8 = true }},
		zipEntry{name: `win\path.txt`, body: "win"},
		// The raw name is a lossy fallback; the Unicode Path field is the
		// real one, valid only while its CRC matches the raw name.
		zipEntry{name: "____.txt", body: "uni", header: func(h *zip.FileHeader) { h.Extra = unicodePath("____.txt", "统一.txt") }},
		zipEntry{name: "__MACOSX/._deep.txt", body: "fork"},
		zipEntry{name: "link", body: "/etc/passwd", header: func(h *zip.FileHeader) { h.SetMode(os.ModeSymlink | 0o777) }},
		zipEntry{name: "empty/", header: func(h *zip.FileHeader) { h.Method = zip.Store }},
	)
	var reported int64
	result, err := m.ExtractZip(ctx, "data", "photos.zip", "photos", 100, 1<<20, func(done, total int64) { reported = total })
	if err != nil || !result.Published || result.Links != 1 || result.Bytes != 13 || reported != 13 {
		t.Fatalf("extract = %+v, total %d, %v", result, reported, err)
	}
	for name, want := range map[string]string{"a/b/deep.txt": "deep", "照片/说明.txt": "gbk", "win/path.txt": "win", "统一.txt": "uni"} {
		if got, err := os.ReadFile(filepath.Join(root, "photos", name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
	}
	for _, name := range []string{"__MACOSX", "link"} {
		if _, err := os.Lstat(filepath.Join(root, "photos", name)); !os.IsNotExist(err) {
			t.Errorf("%s was extracted: %v", name, err)
		}
	}
	if info, err := os.Stat(filepath.Join(root, "photos", "empty")); err != nil || !info.IsDir() {
		t.Errorf("empty folder: %v", err)
	}
	// Extracted content is shared like anything else the service writes, not
	// private as a restored backup is.
	if info, _ := os.Stat(filepath.Join(root, "photos", "a", "b", "deep.txt")); info.Mode().Perm() != 0o664 {
		t.Errorf("file mode %v, want 0664 under umask 002", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Join(root, "photos", "a")); info.Mode().Perm() != 0o775 {
		t.Errorf("folder mode %v, want 0775 under umask 002", info.Mode().Perm())
	}
	if _, err = m.ExtractZip(ctx, "data", "photos.zip", "photos", 100, 1<<20, nil); !errors.Is(err, ErrExists) {
		t.Fatalf("extracted over an existing folder: %v", err)
	}
}

func TestExtractZipRefusesUnsafeOrBrokenArchives(t *testing.T) {
	m, root, _ := writableTree(t)
	ctx := context.Background()
	refuse := func(label string, want error, limit int, maxBytes int64, entries ...zipEntry) {
		t.Helper()
		writeZip(t, filepath.Join(root, "in.zip"), entries...)
		if _, err := m.ExtractZip(ctx, "data", "in.zip", "out", limit, maxBytes, nil); !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", label, err, want)
		}
		if _, err := os.Lstat(filepath.Join(root, "out")); !os.IsNotExist(err) {
			t.Errorf("%s: a refused archive was published", label)
		}
	}
	refuse("zip slip", ErrBadPath, 100, 1<<20, zipEntry{name: "../escape.txt", body: "x"})
	refuse("absolute", ErrBadPath, 100, 1<<20, zipEntry{name: "/etc/x", body: "x"})
	refuse("internal name", ErrOutsideRoot, 100, 1<<20, zipEntry{name: ".nasctl-staging/x", body: "x"})
	refuse("too many entries", ErrTreeLimit, 2, 1<<20, zipEntry{name: "1", body: "x"}, zipEntry{name: "2", body: "x"}, zipEntry{name: "3", body: "x"})
	refuse("too large", ErrTreeLimit, 100, 8, zipEntry{name: "big", body: "0123456789"})
	refuse("encrypted", ErrZipUnsupported, 100, 1<<20, zipEntry{name: "secret", body: "x", header: func(h *zip.FileHeader) { h.Flags |= 0x1 }})
	refuse("duplicate", ErrBadPath, 100, 1<<20, zipEntry{name: "same", body: "1"}, zipEntry{name: "same", body: "2"})

	// Not a ZIP at all, and one whose data is damaged after the directory
	// was read.
	if err := os.WriteFile(filepath.Join(root, "in.zip"), []byte("plain text, not an archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ExtractZip(ctx, "data", "in.zip", "out", 100, 1<<20, nil); !errors.Is(err, ErrNotZip) {
		t.Errorf("not a zip: %v", err)
	}
	writeZip(t, filepath.Join(root, "in.zip"), zipEntry{name: "a.txt", body: string(bytes.Repeat([]byte("abcdefgh"), 512)), header: func(h *zip.FileHeader) { h.Method = zip.Store }})
	b, _ := os.ReadFile(filepath.Join(root, "in.zip"))
	copy(b[100:], "corrupted")
	if err := os.WriteFile(filepath.Join(root, "in.zip"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ExtractZip(ctx, "data", "in.zip", "out", 100, 1<<20, nil); !errors.Is(err, ErrNotZip) {
		t.Errorf("damaged data: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "out")); !os.IsNotExist(err) {
		t.Error("a damaged archive was published")
	}
	if entries, _ := os.ReadDir(filepath.Join(root, copyDirectory)); len(entries) > 1 {
		t.Errorf("staging left behind: %v", entries)
	}
}

func TestZipDirectoryIsCheckedBeforeItIsRead(t *testing.T) {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for i := 0; i < 5; i++ {
		if _, err := w.Create(string(rune('a' + i))); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close()
	r := bytes.NewReader(b.Bytes())
	if err := zipDirectoryFits(r, int64(b.Len()), 5); err != nil {
		t.Fatalf("fits: %v", err)
	}
	if err := zipDirectoryFits(r, int64(b.Len()), 2); !errors.Is(err, ErrTreeLimit) {
		t.Fatalf("five entries passed a budget of two (doubled to four): %v", err)
	}
	if err := zipDirectoryFits(bytes.NewReader([]byte("short")), 5, 5); !errors.Is(err, ErrNotZip) {
		t.Fatalf("short input: %v", err)
	}
}
