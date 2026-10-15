package files

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateVaultAliasesCannotReadListCopyOrWrite(t *testing.T) {
	m, src, dst := copyRig(t, true)
	vault, err := m.BackupVault(context.Background(), "source", true)
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	f, err := vault.OpenFile("secret", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("private-state")
	f.Close()
	if err = os.Symlink(".nasctl-backups", filepath.Join(src, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = m.Open("source", "alias/secret"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatal("read private alias", err)
	}
	if _, err = m.List("source", ListOptions{Path: "alias"}); !errors.Is(err, ErrOutsideRoot) {
		t.Fatal("list private alias", err)
	}
	if _, err = m.Stat("source", "alias/secret"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatal("stat private alias", err)
	}
	if _, err = m.Copy(context.Background(), copyOptions("alias/secret", "stolen"), nil); !errors.Is(err, ErrOutsideRoot) {
		t.Fatal("copy private alias", err)
	}
	if err = m.Mkdir(context.Background(), "source", "alias/new"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatal("mkdir private alias", err)
	}
	if _, err = m.Upload(context.Background(), "source", "alias/new", strings.NewReader("bad"), 3); !errors.Is(err, ErrOutsideRoot) {
		t.Fatal("upload private alias", err)
	}
	assertAbsent(t, filepath.Join(dst, "stolen"))
	b, err := os.ReadFile(filepath.Join(src, ".nasctl-backups", "secret"))
	if err != nil || string(b) != "private-state" {
		t.Fatal(string(b), err)
	}
	// Regular links and parent-relative links still work inside the public root.
	if err = os.Mkdir(filepath.Join(src, "folder"), 0750); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, "public"), "readable")
	if err = os.Symlink("../public", filepath.Join(src, "folder", "link")); err != nil {
		t.Fatal(err)
	}
	f, _, err = m.Open("source", "folder/link")
	if err != nil {
		t.Fatal(err)
	}
	b, err = io.ReadAll(f)
	f.Close()
	if err != nil || string(b) != "readable" {
		t.Fatal(string(b), err)
	}
}

func TestVaultCopyWholeRootAndVerifiedRestoreRejectOverwrite(t *testing.T) {
	m, src, dst := copyRig(t, true)
	mustWrite(t, filepath.Join(src, "original"), "bytes")
	vault, err := m.BackupVault(context.Background(), "destination", true)
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	if err = vault.Mkdir("work", 0700); err != nil {
		t.Fatal(err)
	}
	result, err := m.CopyToVault(context.Background(), vault, "source", ".", "work/data", 100, 1024, nil)
	if err != nil || !result.Published {
		t.Fatal(result, err)
	}
	if _, err = m.List("destination", ListOptions{Path: ".nasctl-backups"}); !errors.Is(err, ErrOutsideRoot) {
		t.Fatal(err)
	}
	verify := func(context.Context, *os.Root, string) error { return ErrSourceChanged }
	result, err = m.RestoreFromVault(context.Background(), vault, "work/data", "source", "refused", 100, 1024, verify, nil)
	if !errors.Is(err, ErrSourceChanged) || result.Published {
		t.Fatal(result, err)
	}
	assertAbsent(t, filepath.Join(src, "refused"))
	ctx, cancel := context.WithCancel(context.Background())
	result, err = m.RestoreFromVault(ctx, vault, "work/data", "source", "cancelled", 100, 1024, func(context.Context, *os.Root, string) error { cancel(); return nil }, nil)
	if !errors.Is(err, context.Canceled) || result.Published {
		t.Fatal("published after verification cancellation", result, err)
	}
	assertAbsent(t, filepath.Join(src, "cancelled"))
	result, err = m.RestoreFromVault(context.Background(), vault, "work/data", "source", "restored", 100, 1024, nil, nil)
	if err != nil || !result.Published {
		t.Fatal(result, err)
	}
	assertContent(t, filepath.Join(src, "restored", "original"), "bytes")
	if _, err = m.RestoreFromVault(context.Background(), vault, "work/data", "source", "restored", 100, 1024, nil, nil); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	assertAbsent(t, filepath.Join(dst, "original"))
}
