package hubweb

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestFolderDownloadStreamsAZipWithoutLinksOrHiddenFiles(t *testing.T) {
	r := newRig(t, nil)
	writeFile(t, filepath.Join(r.root, "sub", ".hidden"), "secret dotfile")
	res, body := r.get(t, "/api/v1/files/archive?root=media&path=/sub")
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/zip" || !strings.Contains(res.Header.Get("Content-Disposition"), "sub.zip") {
		t.Fatalf("status %d headers %v body %.200s", res.StatusCode, res.Header, body)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.Name == "sub/inner.txt" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			if string(b) != "inner" || f.Method != zip.Store {
				t.Fatalf("inner.txt %q method %d", b, f.Method)
			}
		}
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "sub/,sub/inner.txt" {
		t.Fatalf("entries %v", names)
	}

	res, body = r.get(t, "/api/v1/files/archive?root=media&path=/")
	if res.StatusCode != 200 {
		t.Fatalf("root folder: %d %s", res.StatusCode, body)
	}
	zr, err = zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if strings.Contains(f.Name, "escape") || strings.Contains(f.Name, "SECRET") || strings.HasPrefix(filepath.Base(f.Name), ".") {
			t.Fatalf("archive followed a link or exposed a hidden file: %s", f.Name)
		}
	}

	if res, _ := r.get(t, "/api/v1/files/archive?root=media&path=/notes.txt"); res.StatusCode != 400 {
		t.Fatalf("a file is not a folder: %d", res.StatusCode)
	}
	if res, _ := r.get(t, "/api/v1/files/archive?root=media&path=/../outside"); res.StatusCode < 400 {
		t.Fatalf("path escape accepted: %d", res.StatusCode)
	}
}

func TestFolderDownloadRefusesOversizedFoldersBeforeSending(t *testing.T) {
	r := newRig(t, nil)
	big := filepath.Join(r.root, "big")
	if err := os.MkdirAll(big, 0o755); err != nil {
		t.Fatal(err)
	}
	limit := r.srv.Files.Limits().CopyMaxEntries
	for i := 0; i <= limit; i++ {
		f, err := os.Create(filepath.Join(big, fmt.Sprintf("f%05d", i)))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	res, body := r.get(t, "/api/v1/files/archive?root=media&path=/big")
	if res.StatusCode != 413 || !strings.Contains(string(body), "打包下载上限") {
		t.Fatalf("oversized folder: %d %s", res.StatusCode, body)
	}
}
