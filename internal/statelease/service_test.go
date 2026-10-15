package statelease

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestServiceLeaseIsExclusiveWithoutBlockingCLILeases(t *testing.T) {
	state := t.TempDir()
	first, e := AcquireService(state)
	if e != nil {
		t.Fatal(e)
	}
	defer first.Close()
	if second, e := AcquireService(state); !errors.Is(e, ErrServiceRunning) {
		if second != nil {
			second.Close()
		}
		t.Fatal("duplicate service admitted", e)
	}
	shared, e := Acquire(state, false)
	if e != nil {
		t.Fatal("service blocked ordinary CLI", e)
	}
	shared.Close()
	first.Close()
	if _, e = os.Stat(filepath.Join(state, ".nasctl-service.lock")); e != nil {
		t.Fatal("shutdown unlinked shared lock inode", e)
	}
	second, e := AcquireService(state)
	if e != nil {
		t.Fatal("clean restart blocked", e)
	}
	second.Close()
}

func TestServiceLeaseRejectsUnsafeLockFiles(t *testing.T) {
	state := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, nil, 0600)
	if e := os.Symlink(outside, filepath.Join(state, ".nasctl-service.lock")); e != nil {
		t.Fatal(e)
	}
	if l, e := AcquireService(state); e == nil {
		l.Close()
		t.Fatal("followed unsafe service lock")
	}
}
