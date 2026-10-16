package photovault

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVaultBackupContainsOnlyRecoverableCiphertext(t *testing.T) {
	s, token, password, _ := vaultRig(t)
	id, original := putPhoto(t, s, token)
	if _, e := s.BackupScope(); e == nil {
		t.Fatal("pending plaintext cleanup exported")
	}
	if e := s.Complete(token, "owner", id); e != nil {
		t.Fatal(e)
	}
	s.Lock(token, "owner")
	if _, e := s.BackupScope(); e != nil {
		t.Fatal(e)
	}
	var b bytes.Buffer
	result, e := s.ArchiveBackup(context.Background(), &b, 100, 1<<20, nil)
	if e != nil || result.Entries != 5 {
		t.Fatal(result, e)
	}
	for _, value := range []string{password, "PRIVATE-NAME", "PRIVATE-PATH", "PRIVATE-PHOTO-CONTENT", "PRIVATE-THUMB"} {
		if bytes.Contains(b.Bytes(), []byte(value)) {
			t.Fatal("backup exposed plaintext", value)
		}
	}
	state := t.TempDir()
	if e = os.Mkdir(filepath.Join(state, Directory), 0700); e != nil {
		t.Fatal(e)
	}
	reader := tar.NewReader(bytes.NewReader(b.Bytes()))
	for {
		h, e := reader.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		name := strings.TrimPrefix(h.Name, "data/")
		body, e := io.ReadAll(reader)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(state, Directory, name), body, 0600); e != nil {
			t.Fatal(e)
		}
	}
	restored, e := Open(state)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	newToken, e := restored.Unlock(password, "owner")
	if e != nil {
		t.Fatal(e)
	}
	media, _, _, e := restored.OpenMedia(context.Background(), newToken, "owner", id, "raw")
	if e != nil {
		t.Fatal(e)
	}
	defer media.Close()
	body, e := io.ReadAll(media)
	if e != nil || !bytes.Equal(body, original) {
		t.Fatal("ciphertext backup cannot recover original", e)
	}
}
