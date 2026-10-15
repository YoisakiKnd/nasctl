package accountslocal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/localrepair"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
)

func TestOfflineAccountRecoveryPreservesCorruptionAndRevokesIdentities(t *testing.T) {
	s, old, state := first(t)
	ctx := context.Background()
	if _, e := Recover(ctx, state, "recovered", fixturePassword); !errors.Is(e, statelease.ErrBusy) {
		t.Fatal("live store allowed offline repair", e)
	}
	if _, e := os.Stat(filepath.Join(state, localrepair.Directory)); !os.IsNotExist(e) {
		t.Fatal("busy repair created quarantine")
	}
	s.Close()
	bad := []byte("{broken private account fixture\n")
	if e := os.WriteFile(filepath.Join(state, accounts.FileName), bad, 0600); e != nil {
		t.Fatal(e)
	}
	other := filepath.Join(state, "hub.db")
	if e := os.WriteFile(other, []byte("damaged hub fixture"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Recover(ctx, state, "recovered", "short"); e == nil {
		t.Fatal("weak recovery password accepted")
	}
	before, _ := os.ReadFile(filepath.Join(state, accounts.FileName))
	if !bytes.Equal(before, bad) {
		t.Fatal("invalid request moved original")
	}
	out, e := Recover(ctx, state, "recovered", fixturePassword)
	if e != nil || !out.Completed {
		t.Fatal(out, e)
	}
	kept, e := os.ReadFile(filepath.Join(out.Quarantine, "original.raw"))
	if e != nil || !bytes.Equal(kept, bad) {
		t.Fatal("corrupt original not retained", e)
	}
	for _, name := range []string{"intent.json", "result.json"} {
		b, e := os.ReadFile(filepath.Join(out.Quarantine, name))
		if e != nil || strings.Contains(string(b), fixturePassword) || strings.Contains(string(b), "hash") {
			t.Fatal("repair evidence leaks secrets", e)
		}
	}
	reopened, e := Open(state)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	u, e := reopened.Authenticate(ctx, "recovered", fixturePassword)
	if e != nil || u.Role != "admin" || u.ID == old.ID {
		t.Fatal("replacement identity", u, e)
	}
	if _, e = reopened.Lookup(ctx, old.ID); e == nil {
		t.Fatal("old identity retained")
	}
	unchanged, _ := os.ReadFile(other)
	if string(unchanged) != "damaged hub fixture" {
		t.Fatal("account repair altered hub database")
	}
}

func TestAccountRecoveryRefusesInterruptedStateRestoreAndUnsafeTarget(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	if e := os.WriteFile(filepath.Join(state, statelease.PendingRestore), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Recover(ctx, state, "owner", fixturePassword); !errors.Is(e, statelease.ErrRecoveryRequired) {
		t.Fatal("repair bypassed pending state restore", e)
	}
	os.Remove(filepath.Join(state, statelease.PendingRestore))
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("unchanged"), 0600)
	if e := os.Symlink(outside, filepath.Join(state, accounts.FileName)); e != nil {
		t.Fatal(e)
	}
	if _, e := Recover(ctx, state, "owner", fixturePassword); e == nil {
		t.Fatal("repaired through symlink")
	}
	b, _ := os.ReadFile(outside)
	if string(b) != "unchanged" {
		t.Fatal("unsafe alias modified")
	}
}
