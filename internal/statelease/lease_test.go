package statelease

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLeaseSubprocessHelper(t *testing.T) {
	dir := os.Getenv("NASCTL_LEASE_TEST_DIR")
	if dir == "" {
		return
	}
	lease, err := Acquire(dir, os.Getenv("NASCTL_LEASE_TEST_EXCLUSIVE") == "1")
	if os.Getenv("NASCTL_LEASE_TEST_BUSY") == "1" {
		if !errors.Is(err, ErrBusy) {
			if lease != nil {
				lease.Close()
			}
			t.Fatalf("expected busy, got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseCoordinatesIndependentProcesses(t *testing.T) {
	dir := t.TempDir()
	child := func(exclusive, busy string) {
		t.Helper()
		ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLeaseSubprocessHelper$")
		cmd.Env = append(os.Environ(), "NASCTL_LEASE_TEST_DIR="+dir, "NASCTL_LEASE_TEST_EXCLUSIVE="+exclusive, "NASCTL_LEASE_TEST_BUSY="+busy)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child lease: %s %v", b, err)
		}
	}
	writer, err := Acquire(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	child("1", "1")
	child("0", "0")
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	restore, err := Acquire(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer restore.Close()
	child("0", "1")
	child("1", "1")
	if err = restore.Close(); err != nil {
		t.Fatal(err)
	}
	child("1", "0")
}

func TestOfflineRestoreWaitsForEveryWriterAndKeepsLockIdentity(t *testing.T) {
	dir := t.TempDir()
	a, err := Acquire(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Acquire(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	before, err := os.Stat(filepath.Join(dir, filename))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Acquire(dir, true); !errors.Is(err, ErrBusy) {
		t.Fatal("restore while writers open", err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = Acquire(dir, true); !errors.Is(err, ErrBusy) {
		t.Fatal("restore with one writer still open", err)
	}
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	restore, err := Acquire(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer restore.Close()
	if _, err = Acquire(dir, false); !errors.Is(err, ErrBusy) {
		t.Fatal("writer opened during restore", err)
	}
	if err = restore.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := Acquire(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	after, err := os.Stat(filepath.Join(dir, filename))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("lock inode replaced", err)
	}
}

func TestLeaseRefusesSymlinksDirectoriesAndNonemptyFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "data"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, filename)
			switch kind {
			case "symlink":
				target := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(target, nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, p); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
			case "data":
				if err := os.WriteFile(p, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if lease, err := Acquire(dir, true); err == nil {
				lease.Close()
				t.Fatal("invalid lock accepted")
			}
		})
	}
}
