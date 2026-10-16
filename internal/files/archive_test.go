package files

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tarFixture(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, h := range headers {
		if e := w.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		if h.Size > 0 {
			w.Write(make([]byte, h.Size))
		}
	}
	w.Close()
	return b.Bytes()
}
func TestArchiveRestoreRefusesUnverifiedOrUnsafeTrees(t *testing.T) {
	m, source, destination := copyRig(t, false)
	mustWrite(t, filepath.Join(source, "plain"), "original")
	var b bytes.Buffer
	if _, e := m.Archive(context.Background(), "source", ".", &b, 100, 1024, nil); e != nil {
		t.Fatal(e)
	}
	bad := errors.New("transport failed")
	if result, e := m.ImportArchive(context.Background(), "destination", "unverified", bytes.NewReader(b.Bytes()), 100, 1024, func() error { return bad }, nil); !errors.Is(e, bad) || result.Published {
		t.Fatal(result, e)
	}
	assertAbsent(t, filepath.Join(destination, "unverified"))
	for _, h := range []*tar.Header{{Name: "data/../../escape", Typeflag: tar.TypeReg}, {Name: "data/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, {Name: "data/hard", Typeflag: tar.TypeLink, Linkname: "data/file"}, {Name: "data/.nasctl-photo-vault/secret", Typeflag: tar.TypeReg}, {Name: "/absolute", Typeflag: tar.TypeDir}} {
		data := tarFixture(t, []*tar.Header{{Name: "data/", Typeflag: tar.TypeDir, Mode: 0700}, h})
		if result, e := m.ImportArchive(context.Background(), "destination", "unsafe", bytes.NewReader(data), 100, 1024, nil, nil); e == nil || result.Published {
			t.Fatal(h, result, e)
		}
		assertAbsent(t, filepath.Join(destination, "unsafe"))
	}
	if _, e := os.Stat(filepath.Join(source, "plain")); e != nil {
		t.Fatal(e)
	}
}

func TestArchiveRestorePreservesFileTimestamp(t *testing.T) {
	m, source, destination := copyRig(t, false)
	name := filepath.Join(source, "photo")
	mustWrite(t, name, "original timestamp")
	modified := time.Unix(1600000000, 0)
	if e := os.Chtimes(name, modified, modified); e != nil {
		t.Fatal(e)
	}
	var body bytes.Buffer
	if _, e := m.Archive(context.Background(), "source", ".", &body, 100, 1024, nil); e != nil {
		t.Fatal(e)
	}
	if _, e := m.ImportArchive(context.Background(), "destination", "restored", bytes.NewReader(body.Bytes()), 100, 1024, nil, nil); e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(filepath.Join(destination, "restored/photo"))
	if e != nil || !info.ModTime().Equal(modified) {
		t.Fatal(info, e)
	}
}
