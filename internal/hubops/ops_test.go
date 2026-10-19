package hubops

import (
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

// TestEveryDeclarationIsValid exercises the registry's own validator over the
// real declaration list. A malformed declaration is a programming error, and this
// is where it is caught rather than at the moment a caller relies on it.
func TestEveryDeclarationIsValid(t *testing.T) {
	if _, err := operations.New(Declarations()...); err != nil {
		t.Fatalf("the declaration list does not validate: %v", err)
	}
}

// TestReadOperationsNeedNoApproval: if a read required an approval, an agent
// could not read anything, and the "default read-only for agents" policy would be
// unusable in practice - which is how read-only policies get replaced by
// something weaker.
func TestReadOperationsNeedNoApproval(t *testing.T) {
	for _, op := range Declarations() {
		if op.Class != domain.RiskRead {
			continue
		}
		if op.Approval != domain.ApprovalNone {
			t.Errorf("%s is read-only but requires %q approval", op.ID, op.Approval)
		}
		if !op.ReadOnly {
			t.Errorf("%s is classed read but does not set read_only", op.ID)
		}
	}
}

// TestMutatingOperationsAllNeedApproval: every operation that changes something
// must be gated. A mutating operation with no approval rule would be a hole in
// the model, and it would be invisible in a review that looked only at the
// operations an operator happens to care about.
func TestMutatingOperationsAllNeedApproval(t *testing.T) {
	for _, op := range Declarations() {
		if op.Class == domain.RiskRead {
			continue
		}
		if op.Approval == domain.ApprovalNone {
			t.Errorf("%s has class %q but requires no approval", op.ID, op.Class)
		}
		if op.ReadOnly {
			t.Errorf("%s claims to be read-only but is classed %q", op.ID, op.Class)
		}
	}
}

// TestHeavyOperationsAreAsyncAndBounded: anything that could run long must be a
// job with a bound, not something a request waits for. A synchronous heavy
// operation is how a console becomes unresponsive.
func TestHeavyOperationsAreAsyncAndBounded(t *testing.T) {
	for _, op := range Declarations() {
		if op.Class != domain.RiskBounded {
			continue
		}
		switch op.ID {
		case "album.pause", "album.resume", "jobs.cancel",
			"anime.download.pause", "anime.download.resume", "anime.download.recheck",
			"mode.set_watching", "mode.set_maintenance", "mode.clear":
			// These are quick state changes with an explicit count or time
			// bound of their own; downloader controls acknowledge one tagged
			// action and never wait for qBittorrent's file hashing or download.
			continue
		}
		if !op.Async {
			t.Errorf("%s is bounded heavy work but not async; a caller would wait on it", op.ID)
		}
		if op.Timeout <= 0 {
			t.Errorf("%s has no timeout", op.ID)
		}
	}
}

// TestEveryBoundedBatchRequiresAnExplicitLimit: a scan or thumbnail run without
// a limit is the exact shape of the incident this project exists to avoid.
func TestEveryBoundedBatchRequiresAnExplicitLimit(t *testing.T) {
	for _, id := range []string{"album.scan", "album.thumbs"} {
		op := findOp(t, id)
		var found bool
		for _, p := range op.Params {
			if p.Name == "limit" {
				found = true
				if !p.Required {
					t.Errorf("%s has an optional limit; a run without one would be unbounded", id)
				}
			}
		}
		if !found {
			t.Errorf("%s has no limit parameter at all", id)
		}
	}
}

// TestStorageOperationsNameTheirRoot: every file operation must be addressed by
// storage ID, never by a bare path. An operation that takes only a path invites
// a caller to supply one, which is the traversal the files module exists to
// prevent.
func TestStorageOperationsNameTheirRoot(t *testing.T) {
	for _, op := range Declarations() {
		if len(op.Scope) == 0 || op.Scope[0] != "storage" {
			continue
		}
		if op.ID == "files.roots" {
			// Listing the roots is the operation that tells a caller which IDs
			// exist, so it is the one storage operation that cannot name one.
			continue
		}
		if op.ID == "files.batch" {
			// A batch binds all per-item root IDs and destination via its complete
			// normalized selection digest. No machine path is accepted instead.
			for _, name := range []string{"selection", "action", "count", "limit", "max_bytes"} {
				found := false
				for _, p := range op.Params {
					if p.Name == name && p.Required {
						found = true
					}
				}
				if !found {
					t.Errorf("batch missing required %s", name)
				}
			}
			continue
		}
		var hasRoot bool
		for _, p := range op.Params {
			if p.Name == "root" {
				hasRoot = true
				if !p.Required {
					t.Errorf("%s has an optional root, so a call could omit it", op.ID)
				}
			}
		}
		if !hasRoot {
			t.Errorf("%s acts on storage but takes no root parameter", op.ID)
		}
	}
}

// TestMutatingFileOperationsDeclareTheirMutex: two writers in the same tree must
// not run at once, and the declaration is where that is stated.
func TestMutatingFileOperationsDeclareTheirMutex(t *testing.T) {
	for _, op := range Declarations() {
		if len(op.Scope) == 0 || op.Scope[0] != "storage" {
			continue
		}
		if op.Class != domain.RiskMutating {
			continue
		}
		switch op.ID {
		case "files.mkdir", "files.rename", "files.trash", "files.restore", "files.edit":
			// Single-entry operations with a short timeout; serialisation comes
			// from the store, not from a class lock.
			continue
		}
		if op.Mutex == operations.MutexNone {
			t.Errorf("%s changes files but declares no mutex class", op.ID)
		}
	}
}

// External backup services remain inspection-only; native local copies are
// explicit, bounded tasks and never expose command, unit or PID controls.
func TestExternalBackupStatusRemainsReadOnlyAndNativeBackupIsBounded(t *testing.T) {
	for _, op := range Declarations() {
		if len(op.Scope) == 0 || op.Scope[0] != "backup" {
			continue
		}
		if op.ID == "backup.status" || op.ID == "backup.plan" {
			if op.Class != domain.RiskRead {
				t.Errorf("external status %s became mutating", op.ID)
			}
			continue
		}
		if op.Mutex != operations.MutexBackup || op.Approval != domain.ApprovalPerCall || op.Timeout > 2*time.Hour {
			t.Errorf("native backup operation lacks bounded admission: %+v", op)
		}
		for _, p := range op.Params {
			if p.Name == "command" || p.Name == "unit" || p.Name == "pid" {
				t.Errorf("native backup exposes external-service control: %s", op.ID)
			}
		}
	}
}

// TestNoOperationOffersAShell: the console must have no way to run a command.
// This is a declaration-level test because the prohibition is about what the
// console offers, and a future handler must not be able to add one quietly.
func TestNoOperationOffersAShell(t *testing.T) {
	forbidden := []string{"shell", "exec", "sudo", "command", "run_script", "eval", "plugin"}
	for _, op := range Declarations() {
		for _, f := range forbidden {
			if containsWord(op.ID, f) {
				t.Errorf("%s looks like it offers command execution (%q); the console must "+
					"have no such operation", op.ID, f)
			}
		}
	}
}

func containsWord(s, w string) bool {
	for i := 0; i+len(w) <= len(s); i++ {
		if s[i:i+len(w)] == w {
			return true
		}
	}
	return false
}

func findOp(t *testing.T, id string) operations.Operation {
	t.Helper()
	for _, op := range Declarations() {
		if op.ID == id {
			return op
		}
	}
	t.Fatalf("operation %s is not declared", id)
	return operations.Operation{}
}

// TestDeclaredButUnimplementedOperationsAreVisible: the console declares work it
// cannot yet do so that a caller sees "not implemented" rather than "unknown".
// This test pins that the declarations exist, so removing one silently would be
// a change in the published permission table.
func TestDeclaredButUnimplementedOperationsAreVisible(t *testing.T) {
	expected := []string{
		"files.upload", "files.mkdir", "files.rename", "files.copy",
		"files.trash", "files.restore", "schedule.enable", "schedule.disable",
	}
	reg, err := operations.New(Declarations()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range expected {
		if _, ok := reg.Lookup(id); !ok {
			t.Errorf("%s is not declared; a caller would get 'unknown operation' instead of "+
				"'not implemented in this build'", id)
		}
	}
}
