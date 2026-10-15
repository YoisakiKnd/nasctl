package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/config"
)

func TestSealRefusesLinksReadonlyChangedAndFailedEncryption(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "photo.jpg")
	os.WriteFile(source, []byte("photo"), 0600)
	info, _ := os.Stat(source)
	fm, e := New([]config.HubStorage{{ID: "data", Path: root, Writable: true}}, DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer fm.Close()
	ctx := context.Background()
	failure := errors.New("encryption failed")
	e = fm.ConsumeSealed(ctx, "data", "photo.jpg", 5, info.ModTime().UnixNano(), func(*os.File) (string, error) { return "", failure }, "")
	if !errors.Is(e, failure) {
		t.Fatal(e)
	}
	if _, e = os.Stat(source); e != nil {
		t.Fatal("failed encryption lost source", e)
	}
	os.Link(source, filepath.Join(root, "other.jpg"))
	called := false
	e = fm.ConsumeSealed(ctx, "data", "photo.jpg", 5, info.ModTime().UnixNano(), func(*os.File) (string, error) { called = true; return "", nil }, "")
	if !errors.Is(e, ErrSourceChanged) || called {
		t.Fatal("hardlink accepted", e)
	}
	os.Remove(filepath.Join(root, "other.jpg"))
	e = fm.ConsumeSealed(ctx, "data", "photo.jpg", 5, info.ModTime().UnixNano(), func(f *os.File) (string, error) {
		b, _ := io.ReadAll(f)
		hash := sha256.Sum256(b)
		os.WriteFile(source, []byte("CHANGED"), 0600)
		return hex.EncodeToString(hash[:]), nil
	}, "")
	if !errors.Is(e, ErrSourceChanged) {
		t.Fatal("source replacement not detected", e)
	}
	if b, _ := os.ReadFile(source); string(b) != "CHANGED" {
		t.Fatal("changed original removed")
	}
	readonly, e := New([]config.HubStorage{{ID: "data", Path: root}}, DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer readonly.Close()
	e = readonly.ConsumeSealed(ctx, "data", "photo.jpg", 7, info.ModTime().UnixNano(), func(*os.File) (string, error) { t.Fatal("readonly encryptor invoked"); return "", nil }, "")
	if !errors.Is(e, ErrReadOnly) {
		t.Fatal("readonly mutation", e)
	}
}
