package resource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write puts a fixture file in place, creating parents.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// procTree builds a fake /proc and returns its root.
func procTree(t *testing.T, meminfo, psiMem, psiIO, loadavg string) string {
	t.Helper()
	root := t.TempDir()
	if meminfo != "" {
		write(t, filepath.Join(root, "meminfo"), meminfo)
	}
	if psiMem != "" {
		write(t, filepath.Join(root, "pressure", "memory"), psiMem)
	}
	if psiIO != "" {
		write(t, filepath.Join(root, "pressure", "io"), psiIO)
	}
	if loadavg != "" {
		write(t, filepath.Join(root, "loadavg"), loadavg)
	}
	return root
}

// A real /proc/meminfo excerpt, including the keys albumctl ignores.
const goodMeminfo = `MemTotal:         981736 kB
MemFree:           41988 kB
MemAvailable:     106000 kB
Buffers:            1234 kB
Cached:           512000 kB
SwapCached:            8 kB
SwapTotal:        524284 kB
SwapFree:         524280 kB
Dirty:               100 kB
Writeback:             0 kB
AnonPages:        400000 kB
SReclaimable:      20000 kB
Shmem:              4000 kB
Slab:              30000 kB
Committed_AS:    1200000 kB
CommitLimit:     1015152 kB
VmallocTotal:   34359738367 kB
HugePages_Total:       0
Hugepagesize:       2048 kB
`

func TestReadMemInfoParsesTheKeysAlbumctlUses(t *testing.T) {
	root := procTree(t, goodMeminfo, "", "", "")
	m, err := ReadMemInfo(filepath.Join(root, "meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  int64
		want int64
	}{
		{"MemTotalKB", m.MemTotalKB, 981736},
		{"MemFreeKB", m.MemFreeKB, 41988},
		{"MemAvailableKB", m.MemAvailableKB, 106000},
		{"CachedKB", m.CachedKB, 512000},
		{"SReclaimableKB", m.SReclaimableKB, 20000},
		{"SwapTotalKB", m.SwapTotalKB, 524284},
		{"SwapFreeKB", m.SwapFreeKB, 524280},
		{"CommittedASKB", m.CommittedASKB, 1200000},
		{"CommitLimitKB", m.CommitLimitKB, 1015152},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	// The anecdote from the incident report: 106 MiB available. If this ever
	// parsed wrong the guard would be reading a different machine.
	if m.MemAvailableKB != 106000 {
		t.Errorf("the 106 MiB figure from the incident parsed as %d kB", m.MemAvailableKB)
	}
}

func TestReadMemInfoSurvivesGarbage(t *testing.T) {
	// Malformed lines must be skipped, not fatal: this file is read on every
	// sample and a single odd line must not blind the guard.
	junk := `this line has no colon
MemTotal:         981736 kB
MemAvailable:     not-a-number kB
MemFree:           41988 kB
MemFree:           41988
:
: 123 kB
MemTotal:         981736 kB
SwapTotal:            -5 kB
SwapFree:             -7 kB
MemAvailable:     106000 kB
Buffers       1234 kB
`
	root := procTree(t, junk, "", "", "")
	m, err := ReadMemInfo(filepath.Join(root, "meminfo"))
	if err != nil {
		t.Fatalf("garbage lines should be skipped, got error: %v", err)
	}
	if m.MemTotalKB != 981736 {
		t.Errorf("MemTotal = %d, want 981736", m.MemTotalKB)
	}
	if m.MemAvailableKB != 106000 {
		t.Errorf("the later good MemAvailable should win, got %d", m.MemAvailableKB)
	}
	if m.MemFreeKB != 41988 {
		t.Errorf("a value given twice without a unit should still parse, got %d", m.MemFreeKB)
	}
	// A negative swap total is nonsense; the used figure must not go negative.
	if got := m.SwapUsedKB(); got != 0 {
		t.Errorf("SwapUsedKB with nonsense input = %d, want 0", got)
	}
}

func TestReadMemInfoWithoutMemTotalIsAnError(t *testing.T) {
	// Without MemTotal this is not a /proc/meminfo, and silently returning
	// zeroes would make the guard read "no memory at all" as fact.
	root := procTree(t, "MemAvailable: 106000 kB\n", "", "", "")
	if _, err := ReadMemInfo(filepath.Join(root, "meminfo")); err == nil {
		t.Fatal("a meminfo without MemTotal was accepted")
	}
	if _, err := ReadMemInfo(filepath.Join(root, "meminfo-nonexistent")); err == nil {
		t.Fatal("a missing meminfo was accepted")
	}
}

func TestReadMemInfoWithoutMemAvailableReadsAsPressure(t *testing.T) {
	// MemAvailable arrived in Linux 3.14 and is the whole basis of the guard's
	// thresholds. On a kernel without it the field stays zero, so every verdict
	// becomes "stop". That is the safe direction, and this test pins it so a
	// later "optimisation" cannot quietly turn a missing signal into free
	// memory.
	root := procTree(t, "MemTotal: 981736 kB\nMemFree: 900000 kB\n", "", "", "")
	m, err := ReadMemInfo(filepath.Join(root, "meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	if m.MemAvailableKB != 0 {
		t.Fatalf("MemAvailable = %d, want 0 when the key is absent", m.MemAvailableKB)
	}
	g := guardFor(guardLimits())
	d := g.beforeFileDecision(Snapshot{Mem: m})
	if d.Verdict != Stop || d.Reason != ReasonMemBelowStop {
		t.Fatalf("a kernel without MemAvailable produced %s/%s, want stop/%s: a missing "+
			"signal must not read as headroom", d.Verdict, d.Reason, ReasonMemBelowStop)
	}
}

func TestSwapUsedIsDerivedAndClamped(t *testing.T) {
	cases := []struct {
		name       string
		total, fr  int64
		wantUsedKB int64
	}{
		{"no swap configured", 0, 0, 0},
		{"nothing swapped out", 524284, 524284, 0},
		{"a little swapped out", 524284, 524280, 4},
		{"all of it swapped out", 524284, 0, 524284},
		{"inconsistent, free above total", 1000, 2000, 0},
		{"negative total", -1, -1, 0},
	}
	for _, c := range cases {
		got := MemInfo{SwapTotalKB: c.total, SwapFreeKB: c.fr}.SwapUsedKB()
		if got != c.wantUsedKB {
			t.Errorf("%s: SwapUsedKB(%d,%d) = %d, want %d",
				c.name, c.total, c.fr, got, c.wantUsedKB)
		}
	}
}

func TestReadPSIParsesBothLines(t *testing.T) {
	// The format is "some avg10=0.00 avg60=0.00 avg300=0.00 total=123".
	psi := `some avg10=12.34 avg60=5.00 avg300=1.00 total=987654
full avg10=3.21 avg60=2.00 avg300=0.50 total=123456
`
	root := procTree(t, goodMeminfo, psi, psi, "0.10 0.20 0.30 1/200 12345")
	p, err := ReadPSI(filepath.Join(root, "pressure", "memory"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Present {
		t.Error("Present = false for a file that exists")
	}
	if p.SomeAvg10 != 12.34 || p.SomeAvg60 != 5 || p.SomeAvg300 != 1 || p.SomeTotal != 987654 {
		t.Errorf("some line parsed as %+v", p)
	}
	if p.FullAvg10 != 3.21 || p.FullTotal != 123456 {
		t.Errorf("full line parsed as %+v", p)
	}
}

func TestReadPSIAbsentIsNotAnError(t *testing.T) {
	// PSI is optional: a kernel built without CONFIG_PSI must not look like a
	// broken machine, but it must also not look like zero pressure.
	root := procTree(t, goodMeminfo, "", "", "")
	p, err := ReadPSI(filepath.Join(root, "pressure", "memory"))
	if err != nil {
		t.Fatalf("a missing PSI file is not an error, got %v", err)
	}
	if p.Present || p.SomeAvg10 != 0 {
		t.Errorf("a missing PSI file reported as %+v", p)
	}
}

func TestReadPSISurvivesGarbage(t *testing.T) {
	psi := `garbage line
some avg10=x avg60=1.5 avg300= avg999=4 total=notanumber
some avg10=9.5 total=42
full
`
	root := procTree(t, goodMeminfo, psi, "", "")
	p, err := ReadPSI(filepath.Join(root, "pressure", "memory"))
	if err != nil {
		t.Fatal(err)
	}
	// Each field is parsed independently and the last value for that field
	// wins: the kernel writes one "some" line per file, so a single bad field
	// must not discard the others.
	if p.SomeAvg10 != 9.5 || p.SomeTotal != 42 || p.SomeAvg60 != 1.5 {
		t.Errorf("garbage-tolerant parse gave %+v, want avg10=9.5 avg60=1.5 total=42", p)
	}
}

func TestReadLoadAvg(t *testing.T) {
	root := procTree(t, goodMeminfo, "", "", "0.10 0.20 0.30 1/200 12345\n")
	l, err := ReadLoadAvg(filepath.Join(root, "loadavg"))
	if err != nil {
		t.Fatal(err)
	}
	if l.One != 0.10 || l.Five != 0.20 || l.Fifteen != 0.30 {
		t.Errorf("loadavg parsed as %+v", l)
	}
	write(t, filepath.Join(root, "loadavg"), "short\n")
	if _, err := ReadLoadAvg(filepath.Join(root, "loadavg")); err == nil {
		t.Error("a truncated loadavg was accepted")
	}
}

// cgroupDir builds a fake cgroup v2 directory.
func cgroupDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		write(t, filepath.Join(dir, name), content)
	}
	return dir
}

func TestReadCgroupParsesTheMemoryController(t *testing.T) {
	dir := cgroupDir(t, map[string]string{
		"memory.current":      "73400320\n",
		"memory.peak":         "96468992\n",
		"memory.swap.current": "0\n",
		"memory.high":         "100663296\n",
		"memory.max":          "134217728\n",
		"memory.low":          "0\n",
		"memory.events": `low 0
high 3
max 2
oom 1
oom_kill 1
oom_group_kill 0
`,
		"memory.stat": `anon 41943040
file 20971520
kernel_stack 131072
slab 1048576
sock 0
shmem 4096
pgscan 500
pgsteal 400
pgscan_direct 20
pgsteal_direct 10
workingset_refault 7
`,
		"memory.pressure": "some avg10=1.50 avg60=0.50 avg300=0.10 total=500000\n",
	})
	c, err := ReadCgroup(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Present {
		t.Fatal("Present = false for an existing cgroup directory")
	}
	if c.Current != 73400320 || c.Peak != 96468992 {
		t.Errorf("current/peak = %d/%d", c.Current, c.Peak)
	}
	if c.High != 100663296 || c.Max != 134217728 || !c.LimitsKnown {
		t.Errorf("limits = high %d max %d known=%v", c.High, c.Max, c.LimitsKnown)
	}
	// These counters are what the guard subtracts its baseline from, so they
	// must be read exactly.
	if c.EventsOOMKill != 1 || c.EventsOOM != 1 || c.EventsMax != 2 || c.EventsHigh != 3 {
		t.Errorf("events parsed as oom_kill=%d oom=%d max=%d high=%d",
			c.EventsOOMKill, c.EventsOOM, c.EventsMax, c.EventsHigh)
	}
	if c.Anon != 41943040 || c.File != 20971520 || c.PgscanDirect != 20 || c.Refault != 7 {
		t.Errorf("stat parsed as anon=%d file=%d pgscan_direct=%d refault=%d",
			c.Anon, c.File, c.PgscanDirect, c.Refault)
	}
	if !c.Pressure.Present || c.Pressure.SomeAvg10 != 1.5 {
		t.Errorf("per-cgroup pressure parsed as %+v", c.Pressure)
	}
	if got := c.LimitDescription(); !strings.Contains(got, "memory.current") {
		t.Errorf("LimitDescription = %q", got)
	}
}

func TestReadCgroupTreatsMaxAsUnset(t *testing.T) {
	// "max" is the kernel's spelling of "no limit". Reading it as 0 would say
	// the worker may use nothing; reading it as a number would make the guard
	// think a limit is enforced when none is.
	dir := cgroupDir(t, map[string]string{
		"memory.current": "1024\n",
		"memory.high":    "max\n",
		"memory.max":     "max\n",
	})
	c, err := ReadCgroup(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Max != cgroupUnset || c.High != cgroupUnset {
		t.Fatalf("max parsed as high=%d max=%d, want the unset sentinel %d",
			c.High, c.Max, cgroupUnset)
	}
	if !c.LimitsKnown {
		t.Error("LimitsKnown = false: the files were present, they just say unlimited")
	}
	if d := c.LimitDescription(); !strings.Contains(d, "max") {
		t.Errorf("LimitDescription hides that the limit is unset: %q", d)
	}
}

func TestReadCgroupAbsentIsNotAnError(t *testing.T) {
	// A plain shell, a container without delegation: albumctl must run and
	// report honestly that accounting is missing, not refuse to start.
	c, err := ReadCgroup(filepath.Join(t.TempDir(), "no-such-cgroup"))
	if err != nil {
		t.Fatalf("an absent cgroup directory is not an error, got %v", err)
	}
	if c.Present {
		t.Error("Present = true for a directory that does not exist")
	}
	if got := c.LimitDescription(); got != "cgroup accounting unavailable" {
		t.Errorf("LimitDescription = %q", got)
	}
	// A file where a directory is expected is a real configuration error.
	f := filepath.Join(t.TempDir(), "file")
	write(t, f, "x")
	if _, err := ReadCgroup(f); err == nil {
		t.Error("a regular file was accepted as a cgroup directory")
	}
}

func TestSamplerNotesWhatItCouldNotRead(t *testing.T) {
	// The sampler must never fail; it must record what was missing so a report
	// can say which signal was absent instead of implying it was zero.
	root := procTree(t, goodMeminfo, "", "", "")
	s := NewSampler(root, "")
	snap := s.Sample(0)
	if snap.MemErr != nil {
		t.Fatalf("meminfo should have been read: %v", snap.MemErr)
	}
	if snap.Mem.MemAvailableKB != 106000 {
		t.Errorf("sampled MemAvailable = %d", snap.Mem.MemAvailableKB)
	}
	// A missing PSI file is not an error and not a note; what carries that fact
	// is Present=false, which is what stops the guard treating "no PSI" as
	// "zero pressure measured".
	if snap.PSIMem.Present {
		t.Error("Present = true for a PSI file that is not there")
	}
	if snap.Cgroup.Present {
		t.Error("a cgroup was reported present though none was configured")
	}
	if len(snap.Notes) != 0 {
		t.Errorf("notes = %q, but nothing failed to read here", snap.Notes)
	}
	// A nil sampler is legal and means "no observations at all".
	var nilS *Sampler
	if snap := nilS.Sample(0); snap.MemErr == nil {
		t.Error("a nil sampler produced a snapshot claiming memory was read")
	}
}
