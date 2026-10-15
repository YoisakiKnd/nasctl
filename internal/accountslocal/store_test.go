package accountslocal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
)

const fixturePassword = "generated correct passphrase"

func TestMalformedLocalSetupChallengeIsRefused(t *testing.T) {
	state := t.TempDir()
	s, e := Open(state)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = os.WriteFile(filepath.Join(state, accounts.SetupFile), []byte(strings.Repeat("!", 43)+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = s.EnsureSetupKey(context.Background()); !errors.Is(e, accounts.ErrInvalid) {
		t.Fatal("malformed challenge accepted", e)
	}
	if _, e = s.SetupKey(context.Background()); !errors.Is(e, accounts.ErrInvalid) {
		t.Fatal("malformed challenge exposed", e)
	}
}

func first(t *testing.T) (*Store, accounts.User, string) {
	t.Helper()
	state := t.TempDir()
	s, e := Open(state)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	if e = s.EnsureSetupKey(ctx); e != nil {
		t.Fatal(e)
	}
	key, e := s.SetupKey(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if len(key) != 43 {
		t.Fatal("setup key length")
	}
	u, e := s.CreateFirst(ctx, accounts.Setup{Code: key, Username: "owner", DisplayName: "Owner", Password: fixturePassword})
	if e != nil {
		t.Fatal(e)
	}
	return s, u, state
}
func TestFirstAccountRequiresPrivateChallengeAndNeverReturnsPasswords(t *testing.T) {
	state := t.TempDir()
	s, e := Open(state)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	if e = s.EnsureSetupKey(ctx); e != nil {
		t.Fatal(e)
	}
	key, e := s.SetupKey(ctx)
	if e != nil {
		t.Fatal(e)
	}
	snap, e := s.Snapshot(ctx)
	if e != nil || snap.Enabled || !snap.SetupAvailable {
		t.Fatal(snap, e)
	}
	if _, e = s.CreateFirst(ctx, accounts.Setup{Code: strings.Repeat("x", 43), Username: "owner", Password: fixturePassword}); !errors.Is(e, accounts.ErrCredential) {
		t.Fatal("wrong owner challenge accepted", e)
	}
	if _, e = os.Stat(filepath.Join(state, accounts.FileName)); !os.IsNotExist(e) {
		t.Fatal("failed setup published account file")
	}
	u, e := s.CreateFirst(ctx, accounts.Setup{Code: key, Username: "owner", Password: fixturePassword})
	if e != nil {
		t.Fatal(e)
	}
	if u.Role != "admin" || u.Disabled || u.Epoch == "" {
		t.Fatal(u)
	}
	if _, e = s.CreateFirst(ctx, accounts.Setup{Code: key, Username: "second", Password: fixturePassword}); !errors.Is(e, accounts.ErrConflict) {
		t.Fatal("setup replay accepted", e)
	}
	if _, e = s.SetupKey(ctx); !errors.Is(e, accounts.ErrConflict) {
		t.Fatal("consumed setup key readable", e)
	}
	snap, e = s.Snapshot(ctx)
	if e != nil || !snap.Enabled || snap.SetupAvailable || len(snap.Users) != 1 {
		t.Fatal(snap, e)
	}
	b, _ := json.Marshal(snap)
	if strings.Contains(string(b), fixturePassword) || strings.Contains(string(b), key) || strings.Contains(string(b), "pbkdf2") || strings.Contains(string(b), u.Epoch) {
		t.Fatal("credential material in public view")
	}
	b, e = os.ReadFile(filepath.Join(state, accounts.FileName))
	if e != nil || strings.Contains(string(b), fixturePassword) {
		t.Fatal("plaintext password persisted", e)
	}
	i, _ := os.Stat(filepath.Join(state, accounts.FileName))
	if i.Mode().Perm() != 0600 {
		t.Fatal("unsafe store permissions")
	}
}
func TestAuthenticationChangesRevokeEpochAndProtectLastAdmin(t *testing.T) {
	s, owner, _ := first(t)
	ctx := context.Background()
	for _, name := range []string{"owner", "missing"} {
		if _, e := s.Authenticate(ctx, name, "wrong passphrase"); !errors.Is(e, accounts.ErrCredential) {
			t.Fatal("invalid credentials accepted", e)
		}
	}
	got, e := s.Authenticate(ctx, "owner", fixturePassword)
	if e != nil || got.ID != owner.ID {
		t.Fatal(got, e)
	}
	snap, _ := s.Snapshot(ctx)
	r := accounts.Request{Revision: snap.Revision, ID: owner.ID, Username: owner.Username, DisplayName: owner.DisplayName, Role: "viewer"}
	if _, e = s.Save(ctx, r); !errors.Is(e, accounts.ErrLastAdmin) {
		t.Fatal("last admin demoted", e)
	}
	if e = s.Remove(ctx, owner.ID, snap.Revision); !errors.Is(e, accounts.ErrLastAdmin) {
		t.Fatal("last admin deleted", e)
	}
	viewer, e := s.Save(ctx, accounts.Request{Revision: snap.Revision, Username: "reader", DisplayName: "Reader", Role: "viewer", Password: fixturePassword})
	if e != nil {
		t.Fatal(e)
	}
	snap, _ = s.Snapshot(ctx)
	changed, e := s.Save(ctx, accounts.Request{Revision: snap.Revision, ID: viewer.ID, Username: viewer.Username, DisplayName: "Updated", Role: "operator", Password: "new generated passphrase"})
	if e != nil || changed.Epoch == viewer.Epoch {
		t.Fatal("password/role did not revoke session epoch", changed, e)
	}
	if _, e = s.Authenticate(ctx, "reader", fixturePassword); !errors.Is(e, accounts.ErrCredential) {
		t.Fatal("old password accepted", e)
	}
	if _, e = s.Authenticate(ctx, "reader", "new generated passphrase"); e != nil {
		t.Fatal(e)
	}
	snap, _ = s.Snapshot(ctx)
	_, e = s.Save(ctx, accounts.Request{Revision: snap.Revision, ID: viewer.ID, Username: viewer.Username, DisplayName: "Updated", Role: "operator", Disabled: true})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Lookup(ctx, viewer.ID); !errors.Is(e, accounts.ErrCredential) {
		t.Fatal("disabled user lookup accepted", e)
	}
}
func TestIndependentStoresDetectStaleSaveAndRemovedFileFailsClosed(t *testing.T) {
	s, owner, state := first(t)
	other, e := Open(state)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	old, _ := other.Snapshot(ctx)
	snap, _ := s.Snapshot(ctx)
	_, e = s.Save(ctx, accounts.Request{Revision: snap.Revision, ID: owner.ID, Username: owner.Username, DisplayName: "New owner name", Role: "admin"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = other.Save(ctx, accounts.Request{Revision: old.Revision, ID: owner.ID, Username: owner.Username, DisplayName: "Stale owner", Role: "admin"}); !errors.Is(e, accounts.ErrConflict) {
		t.Fatal("stale independent store overwrite", e)
	}
	current, e := other.Lookup(ctx, owner.ID)
	if e != nil || current.DisplayName != "New owner name" {
		t.Fatal("external change not observed", current, e)
	}
	other.Close()
	if e = os.Remove(filepath.Join(state, accounts.FileName)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Snapshot(ctx); e == nil {
		t.Fatal("missing user store allowed anonymous fallback")
	}
	s.Close()
	if broken, e := Open(state); e == nil {
		broken.Close()
		t.Fatal("restart ignored enabled marker")
	}
}
func TestAccountBudgetsKDFAdmissionCancellationAndOfflineLease(t *testing.T) {
	s, _, state := first(t)
	ctx := context.Background()
	s.slots <- struct{}{}
	if _, e := s.Authenticate(ctx, "owner", fixturePassword); !errors.Is(e, accounts.ErrBusy) {
		t.Fatal("parallel derivation not bounded", e)
	}
	<-s.slots
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, e := s.Authenticate(cancelled, "owner", fixturePassword); !errors.Is(e, context.Canceled) {
		t.Fatal("cancelled verification admitted", e)
	}
	if lease, e := statelease.Acquire(state, true); !errors.Is(e, statelease.ErrBusy) {
		if lease != nil {
			lease.Close()
		}
		t.Fatal("offline restore raced account store", e)
	}
	s.mu.Lock()
	d := s.data
	d.Users = append([]record{}, d.Users...)
	for len(d.Users) < accounts.MaxUsers {
		u := d.Users[0]
		u.ID, _ = randomHex()
		u.Username = fmt.Sprintf("reader%d", len(d.Users))
		u.Role = "viewer"
		d.Users = append(d.Users, u)
	}
	e := s.publish(ctx, d)
	s.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	snap, _ := s.Snapshot(ctx)
	if _, e = s.Save(ctx, accounts.Request{Revision: snap.Revision, Username: "over-budget", Role: "viewer", Password: fixturePassword}); !errors.Is(e, accounts.ErrInvalid) {
		t.Fatal("user budget exceeded", e)
	}
	s.Close()
	lease, e := statelease.Acquire(state, true)
	if e != nil {
		t.Fatal(e)
	}
	lease.Close()
}
