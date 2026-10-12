package webstate

import (
	"os"
	"path/filepath"
	"testing"
)

// The record file is how `albumctl status` answers "is the web service up, and
// what is it costing?" without talking to the service. Two things have to hold:
// a stale record must be distinguishable from a live one, and a reader must
// never see a half-written file.

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := Record{PID: 4321, Bind: "127.0.0.1:8088", Started: "2026-10-11T12:00:00Z", Version: "0.1.0", Schema: 2, ReadOnly: true}
	if err := Write(dir, rec); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != rec {
		t.Fatalf("read back %+v, want %+v", got, rec)
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); !os.IsNotExist(err) {
		t.Fatalf("after Remove, Read returned %v; want a not-exist error", err)
	}
	// Removing an absent record is not an error: shutdown runs it unconditionally.
	if err := Remove(dir); err != nil {
		t.Fatalf("Remove on an absent record: %v", err)
	}
}

func TestWriteReplacesAtomicallyAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	first := Record{PID: 1, Bind: "127.0.0.1:1", Started: "a"}
	second := Record{PID: 2, Bind: "127.0.0.1:2", Started: "b"}
	if err := Write(dir, first); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, second); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == first || got.PID != 2 {
		t.Fatalf("after a second Write the record is %+v, want the second one", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("run directory holds %v; a replaced record must leave no temporary file behind", names)
	}
}

func TestReadRejectsAHalfWrittenRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte(`{"pid": 42, "bind": "127.0`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("a truncated record must be reported as an error, not silently treated as empty")
	}
}

func TestAliveIsFalseForAPidThatIsNotTheService(t *testing.T) {
	// The current process is alive but is not the web service: /proc/<pid>/comm
	// does not say "albumctl". Reporting it as the service would make `status`
	// claim the service is running when it is only this command looking at
	// itself.
	if Alive(ProcRoot(), os.Getpid()) {
		t.Errorf("Alive reports pid %d (this test binary) as the albumctl web service", os.Getpid())
	}
	// A pid that certainly does not exist.
	if Alive(ProcRoot(), 1<<30) {
		t.Error("Alive reports an impossible pid as alive")
	}
}

func TestStartedAtParsesTheRecordedTime(t *testing.T) {
	rec := Record{Started: "2026-10-11T12:00:00Z"}
	got := rec.StartedAt()
	if got.IsZero() {
		t.Fatal("StartedAt returned the zero time for a well-formed stamp")
	}
	if got.UTC().Format("2006-01-02T15:04:05Z") != rec.Started {
		t.Fatalf("StartedAt = %s, want %s", got.UTC(), rec.Started)
	}
	// An unparseable stamp must not look like a real start time.
	if !(Record{Started: "not a time"}).StartedAt().IsZero() {
		t.Error("a malformed stamp must yield the zero time rather than a plausible-looking one")
	}
}

func TestProcRootCanBeRedirected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ALBUMCTL_PROC_ROOT", dir)
	if got := ProcRoot(); got != filepath.Clean(dir) {
		t.Fatalf("ProcRoot = %q, want %q", got, dir)
	}
}
