package taskctl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The lock is the reason two thumbnail runs cannot overlap on a machine that has
// memory for exactly one. These tests pin the properties that keep it honest
// after a crash, which is the case that matters: pause, kill and restart are
// normal events on this box, not exceptions.

func TestAcquireIsExclusiveUntilReleased(t *testing.T) {
	dir := t.TempDir()

	first, err := Acquire(dir, "thumbs")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if want := filepath.Join(dir, "thumbs.lock"); first.Path() != want {
		t.Fatalf("Path = %q, want %q", first.Path(), want)
	}

	if _, err := Acquire(dir, "thumbs"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire returned %v, want ErrLocked: two overlapping thumbnail runs "+
			"would double the memory peak", err)
	}

	// A different task name is a different lock.
	other, err := Acquire(dir, "scan")
	if err != nil {
		t.Fatalf("Acquire(scan) while thumbs is held: %v", err)
	}
	if err := other.Release(); err != nil {
		t.Fatal(err)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := Acquire(dir, "thumbs")
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseIsSafeToCallTwiceAndOnNil(t *testing.T) {
	dir := t.TempDir()
	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Fatalf("Release on a nil lock: %v", err)
	}
	if nilLock.Path() != "" {
		t.Errorf("Path on a nil lock = %q, want empty", nilLock.Path())
	}
	l, err := Acquire(dir, "thumbs")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	// A deferred Release in the caller runs unconditionally, including after a
	// path that already released.
	if err := l.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}

func TestALockFileLeftByADeadProcessDoesNotBlockANewRun(t *testing.T) {
	// A SIGKILLed run never releases anything. The lock must be held by the
	// kernel, not by the file's existence, or the tool would need manual
	// cleanup on the one occasion it is most needed.
	dir := t.TempDir()
	path := filepath.Join(dir, "thumbs.lock")
	if err := os.WriteFile(path, []byte("pid=999999 at=2020-01-01T00:00:00Z argv=albumctl thumbs\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	l, err := Acquire(dir, "thumbs")
	if err != nil {
		t.Fatalf("a stale lock file from a dead process must not block the run: %v", err)
	}
	defer l.Release()

	// And the informational line must have been replaced by ours.
	line := Holder(path)
	if !strings.Contains(line, "pid=") {
		t.Fatalf("Holder = %q, want a pid line for the live holder", line)
	}
	if strings.Contains(line, "999999") {
		t.Errorf("Holder still reports the dead pid: %q", line)
	}
}

func TestHolderHidesADeadHolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thumbs.lock")

	// A pid that does not exist: status must say nobody holds it, not name a
	// corpse. This text is written by a process that was killed.
	if err := os.WriteFile(path, []byte("pid=2147483647 at=x argv=y\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := Holder(path); got != "" {
		t.Errorf("Holder = %q, want empty for a dead pid", got)
	}

	// Our own pid is alive, so the line is reported.
	self := "pid=" + itoa(os.Getpid()) + " at=now argv=albumctl\n"
	if err := os.WriteFile(path, []byte(self), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := Holder(path); got != strings.TrimSpace(self) {
		t.Errorf("Holder = %q, want %q", got, strings.TrimSpace(self))
	}

	// An unreadable path is not an error, it is simply "no holder".
	if got := Holder(filepath.Join(dir, "absent.lock")); got != "" {
		t.Errorf("Holder on a missing file = %q, want empty", got)
	}
}

func TestAcquireRejectsAnEmptyDirectory(t *testing.T) {
	if _, err := Acquire("", "thumbs"); err == nil {
		t.Fatal("Acquire with no lock directory must fail rather than take a lock somewhere " +
			"unpredictable")
	}
}

func TestParsePIDFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "worker.pid")
	if err := os.WriteFile(p, []byte("4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pid, err := ParsePIDFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 4242 {
		t.Fatalf("pid = %d, want 4242", pid)
	}
	if _, err := ParsePIDFile(filepath.Join(dir, "absent.pid")); err == nil {
		t.Error("a missing pid file must be an error, not pid 0")
	}
	if err := os.WriteFile(p, []byte("not a pid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePIDFile(p); err == nil {
		t.Error("a malformed pid file must be an error rather than a silent 0")
	}
}

func TestArgvDropsCredentialShapedArguments(t *testing.T) {
	// The holder line is written into the lock file and shown by status; it must
	// not become a place a token leaks into a backup.
	line := Argv()
	if strings.Contains(line, "token=") || strings.Contains(line, "password=") {
		t.Fatalf("Argv = %q, want credential-shaped arguments removed", line)
	}
	if len(line) > 512 {
		t.Errorf("Argv is %d bytes; it must be truncated", len(line))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
