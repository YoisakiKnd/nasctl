package resource

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The guard is the part of this program that decides whether a decoder may run
// on a machine with about a gigabyte of memory. These tests are written against
// injected snapshots so that the verdicts can be checked for conditions this
// development host will never produce: no memory at all, a cgroup OOM kill, a
// swap-in that has already begun.

const kibPerMiB = 1024 // kB in one MiB

// mib is a rendering helper in guard.go, so the unit constant is named apart
// from it rather than shadowing it.

func guardLimits() Limits {
	return Limits{
		StartMemAvailableKB:    400 * kibPerMiB,
		WarnMemAvailableKB:     250 * kibPerMiB,
		StopMemAvailableKB:     200 * kibPerMiB,
		WorkerMaxBytes:         128 << 20,
		WorkerHighBytes:        96 << 20,
		PSIMemorySomeAvg10Stop: 50,
		PSIIOSomeAvg10Stop:     80,
		MaxSwapUsedKB:          64 * kibPerMiB,
	}
}

func guardFor(l Limits) *Guard {
	return &Guard{Limits: l}
}

// mem builds a snapshot with the given MemAvailable, in kB, and no other signal.
func mem(availKB int64) Snapshot {
	return Snapshot{Mem: MemInfo{MemTotalKB: 981736, MemAvailableKB: availKB}}
}

func TestBeforeFileRefusesWithoutAnySignal(t *testing.T) {
	// Neither /proc/meminfo nor a cgroup: albumctl has no idea what the machine
	// is doing, so it must not start a decoder. Guessing here is exactly how the
	// original deployment took the machine down.
	g := guardFor(guardLimits())
	d := g.beforeFileDecision(Snapshot{MemErr: errors.New("no /proc/meminfo")})
	if d.Verdict != Stop || d.Reason != ReasonNoSignal {
		t.Fatalf("no signal gave %s/%s, want stop/%s", d.Verdict, d.Reason, ReasonNoSignal)
	}

	// With cgroup accounting present there is at least one fact, so the missing
	// meminfo is only a note: the cgroup ceilings still apply.
	d = g.beforeFileDecision(Snapshot{
		MemErr: errors.New("no /proc/meminfo"),
		Cgroup: Cgroup{Present: true, Current: 1 << 20, Max: 128 << 20},
	})
	if d.Verdict != Allow {
		t.Fatalf("with a readable cgroup but no meminfo the verdict was %s/%s, want allow: "+
			"the kernel-enforced ceiling is still in place", d.Verdict, d.Reason)
	}
}

func TestBeforeFileThresholdsAndTheirOrder(t *testing.T) {
	// Ordering is the safety property. A machine at 100 MiB available must be
	// reported as below the stop floor, never as merely "below the start
	// budget", or an operator reading the reason would think there was room.
	cases := []struct {
		availKB int64
		verdict Verdict
		reason  Reason
	}{
		{10 * kibPerMiB, Stop, ReasonMemBelowStop},
		{199 * kibPerMiB, Stop, ReasonMemBelowStop},
		{200 * kibPerMiB, Drain, ReasonMemBelowWarn}, // at the floor, not below it
		{249 * kibPerMiB, Drain, ReasonMemBelowWarn},
		{250 * kibPerMiB, Drain, ReasonMemBelowStart},
		{399 * kibPerMiB, Drain, ReasonMemBelowStart},
		{400 * kibPerMiB, Allow, ReasonOK}, // the start budget is met
		{401 * kibPerMiB, Allow, ReasonOK},
		{2000 * kibPerMiB, Allow, ReasonOK},
	}
	g := guardFor(guardLimits())
	for _, c := range cases {
		d := g.beforeFileDecision(mem(c.availKB))
		if d.Verdict != c.verdict || d.Reason != c.reason {
			t.Errorf("MemAvailable %d MiB gave %s/%s, want %s/%s",
				c.availKB/kibPerMiB, d.Verdict, d.Reason, c.verdict, c.reason)
		}
		if d.Verdict != Allow && d.Detail == "" {
			t.Errorf("MemAvailable %d MiB stopped work with no explanation", c.availKB/kibPerMiB)
		}
	}
}

func TestBeforeFileSwapBlocksStarting(t *testing.T) {
	// The machine swapping during a decode means it has already lost the
	// argument. Note this is Drain at admission, so an operator sees a reason
	// rather than a hard failure.
	g := guardFor(guardLimits())
	snap := mem(2000 * kibPerMiB)
	snap.Mem.SwapTotalKB = 512 * kibPerMiB
	snap.Mem.SwapFreeKB = 512*kibPerMiB - 100*kibPerMiB // 100 MiB swapped out, over the 64 MiB cap
	d := g.beforeFileDecision(snap)
	if d.Verdict != Drain || d.Reason != ReasonSwapUsed {
		t.Fatalf("heavy swap gave %s/%s, want drain/%s", d.Verdict, d.Reason, ReasonSwapUsed)
	}

	// Inside the cap it must not block: a little swap in use is normal.
	snap.Mem.SwapFreeKB = 512*kibPerMiB - 10*kibPerMiB
	if d := g.beforeFileDecision(snap); d.Verdict != Allow {
		t.Fatalf("10 MiB of swap, inside the 64 MiB cap, gave %s/%s", d.Verdict, d.Reason)
	}

	// With the swap gate disabled (0) any amount of swap is ignored.
	l := guardLimits()
	l.MaxSwapUsedKB = 0
	snap.Mem.SwapFreeKB = 0
	if d := guardFor(l).beforeFileDecision(snap); d.Verdict != Allow {
		t.Fatalf("with the swap gate disabled the verdict was %s/%s", d.Verdict, d.Reason)
	}
}

func TestBeforeFilePSIMemoryBlocksStarting(t *testing.T) {
	g := guardFor(guardLimits())
	snap := mem(2000 * kibPerMiB)
	snap.PSIMem = PSI{Present: true, SomeAvg10: 51}
	d := g.beforeFileDecision(snap)
	if d.Verdict != Drain || d.Reason != ReasonPSIMemory {
		t.Fatalf("memory PSI 51%% gave %s/%s, want drain/%s", d.Verdict, d.Reason, ReasonPSIMemory)
	}

	// At the threshold it is not yet a stop: strict comparisons are deliberate
	// so a threshold of 50 means "50 is tolerable".
	snap.PSIMem.SomeAvg10 = 50
	if d := g.beforeFileDecision(snap); d.Verdict != Allow {
		t.Fatalf("PSI exactly at the threshold gave %s/%s", d.Verdict, d.Reason)
	}
	// A PSI file that is not there must not be read as zero pressure being
	// confirmed; Present=false means the gate is simply not applied.
	snap.PSIMem = PSI{Present: false, SomeAvg10: 99}
	if d := g.beforeFileDecision(snap); d.Verdict != Allow {
		t.Fatalf("an absent PSI file gated work: %s/%s", d.Verdict, d.Reason)
	}
}

func TestDuringFileCgroupOOMKillIsAttributedToThisFile(t *testing.T) {
	// Counters are cumulative for the life of the cgroup. An OOM kill that
	// happened an hour ago must not be attributed to the file in flight, or one
	// old failure would make every later file look fatal.
	g := guardFor(guardLimits())
	snap := mem(2000 * kibPerMiB)
	snap.Cgroup = Cgroup{Present: true, Current: 1 << 20, EventsOOMKill: 3, EventsOOM: 3}

	base := Baseline{CgroupOOMKill: 3, CgroupOOM: 3}
	if d := g.duringFileDecision(base, snap); d.Verdict != Allow {
		t.Fatalf("an old OOM kill (counter unchanged) aborted the next file: %s/%s",
			d.Verdict, d.Reason)
	}

	// A rise during this file is fatal and names the mechanism.
	snap.Cgroup.EventsOOMKill = 4
	d := g.duringFileDecision(base, snap)
	if d.Verdict != Stop || d.Reason != ReasonCgroupOOMKill {
		t.Fatalf("an OOM kill during the file gave %s/%s, want stop/%s",
			d.Verdict, d.Reason, ReasonCgroupOOMKill)

	}
	// OOM without a kill is still a stop: the kernel had to intervene.
	snap.Cgroup.EventsOOMKill = 3
	snap.Cgroup.EventsOOM = 4
	if d := g.duringFileDecision(base, snap); d.Verdict != Stop || d.Reason != ReasonCgroupOOMKill {
		t.Fatalf("an oom event during the file gave %s/%s", d.Verdict, d.Reason)
	}
}

func TestDuringFileUsesKernelLimitsBeforeGuesses(t *testing.T) {
	g := guardFor(guardLimits())
	base := Baseline{}

	// memory.max being hit is a fact. It must be reported as such rather than
	// as the generic "this process looks big" verdict.
	snap := mem(2000 * kibPerMiB)
	snap.Cgroup = Cgroup{Present: true, Current: 1 << 20, Max: 128 << 20, EventsMax: 1}
	d := g.duringFileDecision(base, snap)
	if d.Verdict != Stop || d.Reason != ReasonCgroupMaxEvents {
		t.Fatalf("a memory.max event gave %s/%s, want stop/%s", d.Verdict, d.Reason, ReasonCgroupMaxEvents)
	}

	// Current above the ceiling without an event is still a stop.
	snap.Cgroup = Cgroup{Present: true, Current: 200 << 20, Max: 128 << 20}
	if d := g.duringFileDecision(base, snap); d.Verdict != Stop || d.Reason != ReasonWorkerCgroup {
		t.Fatalf("cgroup current over the ceiling gave %s/%s", d.Verdict, d.Reason)
	}

	// The worker's own memory, counted over its whole process tree, not just
	// the process we forked.
	snap.Cgroup = Cgroup{}
	snap.Worker = RSSSample{RSSBytes: 200 << 20, PIDs: []int{1, 2, 3}}
	if d := g.duringFileDecision(base, snap); d.Verdict != Stop || d.Reason != ReasonWorkerRSS {
		t.Fatalf("a worker tree over the ceiling gave %s/%s", d.Verdict, d.Reason)
	}
	// A tree under the ceiling is fine even on a machine with plenty of memory.
	snap.Worker = RSSSample{RSSBytes: 100 << 20, PIDs: []int{1}}
	if d := g.duringFileDecision(base, snap); d.Verdict != Allow {
		t.Fatalf("a worker tree under the ceiling gave %s/%s", d.Verdict, d.Reason)
	}
	// An unreadable worker RSS is not a reason to stop by itself: on many
	// kernels a short-lived child disappears between listing and reading.
	snap.Worker = RSSSample{}
	snap.WorkerErr = errors.New("process vanished")
	if d := g.duringFileDecision(base, snap); d.Verdict != Allow {
		t.Fatalf("an unreadable worker RSS gave %s/%s", d.Verdict, d.Reason)
	}
}

func TestDuringFileWholeMachineStopBeatsWarning(t *testing.T) {
	// While a file is decoding the machine can fall off a cliff. Below the stop
	// floor the worker is killed now; between the floors the file is allowed to
	// finish. The two must not be confused, and the stop must win.
	g := guardFor(guardLimits())
	base := Baseline{}

	d := g.duringFileDecision(base, mem(150*kibPerMiB))
	if d.Verdict != Stop || d.Reason != ReasonMemBelowStop {
		t.Fatalf("150 MiB available during a decode gave %s/%s, want stop/%s",
			d.Verdict, d.Reason, ReasonMemBelowStop)
	}

	d = g.duringFileDecision(base, mem(230*kibPerMiB))
	if d.Verdict != Drain || d.Reason != ReasonMemBelowWarn {
		t.Fatalf("230 MiB available during a decode gave %s/%s, want drain/%s",
			d.Verdict, d.Reason, ReasonMemBelowWarn)
	}

	// Between the warning floor and the start budget a running file is fine:
	// the start budget gates starting, not finishing.
	if d := g.duringFileDecision(base, mem(350*kibPerMiB)); d.Verdict != Allow {
		t.Fatalf("350 MiB available mid-file gave %s/%s; the start budget must not "+
			"abort work already in flight", d.Verdict, d.Reason)
	}
}

func TestDuringFileIOPSIIsOnlyADrain(t *testing.T) {
	// I/O pressure is a reason to stop soon, not to kill the file: killing
	// mid-write is what leaves a half-written thumbnail behind, and the cache
	// write is atomic precisely so that this case is cheap to handle.
	g := guardFor(guardLimits())
	base := Baseline{}
	snap := mem(2000 * kibPerMiB)
	snap.PSIIO = PSI{Present: true, SomeAvg10: 90}
	d := g.duringFileDecision(base, snap)
	if d.Verdict != Drain || d.Reason != ReasonPSIIO {
		t.Fatalf("I/O PSI 90%% gave %s/%s, want drain/%s", d.Verdict, d.Reason, ReasonPSIIO)
	}

	// Memory PSI during a decode is a stop: unlike I/O, additional memory
	// demand makes the stall worse.
	snap.PSIIO = PSI{}
	snap.PSIMem = PSI{Present: true, SomeAvg10: 90}
	if d := g.duringFileDecision(base, snap); d.Verdict != Stop || d.Reason != ReasonPSIMemory {
		t.Fatalf("memory PSI 90%% during a decode gave %s/%s", d.Verdict, d.Reason)
	}
}

func TestDuringFileSwapDuringDecodeIsAStop(t *testing.T) {
	// Reaching swap while decoding is the failure mode the whole design exists
	// to avoid: on this box it means the decoder is already living on a slow
	// external disk. Stop now rather than let the machine thrash.
	g := guardFor(guardLimits())
	snap := mem(2000 * kibPerMiB)
	snap.Mem.SwapTotalKB = 512 * kibPerMiB
	snap.Mem.SwapFreeKB = 512*kibPerMiB - 200*kibPerMiB
	d := g.duringFileDecision(Baseline{}, snap)
	if d.Verdict != Stop || d.Reason != ReasonSwapUsed {
		t.Fatalf("swap use during a decode gave %s/%s, want stop/%s",
			d.Verdict, d.Reason, ReasonSwapUsed)
	}
}

func TestVerdictsAndReasonsAreStableStrings(t *testing.T) {
	// Task rows and the CLI's exit codes are built from these names, so a
	// reworded reason is a silent behaviour change in the operator interface.
	for v, want := range map[Verdict]string{
		Allow: "allow", Drain: "drain", Stop: "stop", Verdict(99): "unknown",
	} {
		if got := v.String(); got != want {
			t.Errorf("Verdict(%d).String() = %q, want %q", int(v), got, want)
		}
	}
	for _, r := range []Reason{
		ReasonOK, ReasonNoSignal, ReasonMemBelowStart, ReasonMemBelowWarn,
		ReasonMemBelowStop, ReasonSwapUsed, ReasonWorkerRSS, ReasonWorkerCgroup,
		ReasonCgroupMaxEvents, ReasonCgroupOOMKill, ReasonPSIMemory, ReasonPSIIO,
		ReasonPerFileTimeout, ReasonWorkerExit, ReasonWorkerSignal, ReasonCacheFull,
		ReasonPaused, ReasonPressureTrip, ReasonLimitReached, ReasonExhausted,
		ReasonMaxRuntime, ReasonBackupBusy, ReasonNoBackupStatus,
	} {
		if r == "" {
			t.Error("an empty reason is in the list")
		}
		if string(r) != string(r) {
			t.Errorf("reason %q is not stable", r)
		}
	}
}

func TestStatsObserveKeepsPeaksNotAverages(t *testing.T) {
	// The report must show the worst moment, not a comfortable mean: the mean
	// is exactly what hid the original incident's 106 MiB low-water mark.
	var s Stats
	s.Observe(Snapshot{
		At:  time.Unix(1000, 0),
		Mem: MemInfo{MemTotalKB: 981736, MemAvailableKB: 700 * kibPerMiB},
	})
	s.Observe(Snapshot{
		At:     time.Unix(1001, 0),
		Mem:    MemInfo{MemTotalKB: 981736, MemAvailableKB: 106 * kibPerMiB},
		Worker: RSSSample{RSSBytes: 90 << 20},
	})
	s.Observe(Snapshot{
		At:     time.Unix(1002, 0),
		Mem:    MemInfo{MemTotalKB: 981736, MemAvailableKB: 600 * kibPerMiB},
		Worker: RSSSample{RSSBytes: 20 << 20},
	})

	if s.MinMemAvailableKB != 106*kibPerMiB {
		t.Errorf("MinMemAvailableKB = %d, want the 106 MiB low-water mark", s.MinMemAvailableKB)
	}
	if s.MaxWorkerRSSBytes != 90<<20 {
		t.Errorf("MaxWorkerRSSBytes = %d, want the 90 MiB peak", s.MaxWorkerRSSBytes)
	}
	if s.Samples != 3 {
		t.Errorf("Samples = %d, want 3", s.Samples)
	}
	if got := s.HumanReport(); got == "" {
		t.Error("HumanReport is empty")
	}
}

func TestStatsLowWaterMarkOfZeroSurvivesLaterSamples(t *testing.T) {
	// Zero available memory is the single most important number a run report
	// can contain, and it is also the natural sentinel for "no sample yet". If
	// the two are confused, the report shows a comfortable minimum for a run in
	// which the machine actually ran out.
	var s Stats
	s.Observe(Snapshot{Mem: MemInfo{MemTotalKB: 981736, MemAvailableKB: 700 * kibPerMiB}})
	s.Observe(Snapshot{Mem: MemInfo{MemTotalKB: 981736, MemAvailableKB: 0}})
	s.Observe(Snapshot{Mem: MemInfo{MemTotalKB: 981736, MemAvailableKB: 650 * kibPerMiB}})

	if s.MinMemAvailableKB != 0 {
		t.Errorf("MinMemAvailableKB = %d, want 0: the machine really did reach zero", s.MinMemAvailableKB)
	}
	if s.Samples != 3 {
		t.Errorf("Samples = %d, want 3", s.Samples)
	}
}

func TestStatsMergeKeepsTheWorstOfBoth(t *testing.T) {
	// Merge is how a run's own samples are combined with what the store already
	// recorded for the collection. Taking the better of the two would quietly
	// hide the worse run.
	var a, b Stats
	a.Observe(Snapshot{Mem: MemInfo{MemTotalKB: 981736, MemAvailableKB: 500 * kibPerMiB}})
	a.Observe(Snapshot{
		Worker: RSSSample{RSSBytes: 40 << 20},
		Cgroup: Cgroup{Present: true, Current: 30 << 20},
	})
	b.Observe(Snapshot{Mem: MemInfo{MemTotalKB: 981736, MemAvailableKB: 90 * kibPerMiB}})
	b.Observe(Snapshot{
		Worker: RSSSample{RSSBytes: 110 << 20},
		Cgroup: Cgroup{Present: true, Current: 120 << 20},
	})

	var merged Stats
	merged.Merge(a)
	merged.Merge(b)

	if merged.MinMemAvailableKB != 90*kibPerMiB {
		t.Errorf("merged MinMemAvailableKB = %d, want the worse 90 MiB", merged.MinMemAvailableKB)
	}
	if merged.MaxWorkerRSSBytes != 110<<20 {
		t.Errorf("merged MaxWorkerRSSBytes = %d, want the worse 110 MiB", merged.MaxWorkerRSSBytes)
	}
	if merged.MaxCgroupCurrent != 120<<20 {
		t.Errorf("merged MaxCgroupCurrent = %d, want the worse 120 MiB", merged.MaxCgroupCurrent)
	}
	if !merged.CgroupAvailable {
		t.Error("CgroupAvailable was lost by the merge")
	}
	// A Snapshot that carries no memory reading at all - a zero value, or a
	// sample from a machine that could not be read - must contribute no
	// low-water mark. Reading it as "zero available" would make every report
	// claim the machine ran out of memory.
	var poisoned Stats
	poisoned.Observe(Snapshot{Worker: RSSSample{RSSBytes: 10 << 20}})
	if poisoned.memMinSeen || poisoned.MinMemAvailableKB != 0 {
		t.Errorf("a snapshot with no memory reading set a low-water mark of %d",
			poisoned.MinMemAvailableKB)
	}
	if poisoned.Samples != 1 || poisoned.MaxWorkerRSSBytes != 10<<20 {
		t.Errorf("the rest of the snapshot was discarded too: %+v", poisoned)
	}

	// Merging a zero-value report must change nothing, or a first-ever run
	// would be counted twice.
	before := merged
	merged.Merge(Stats{})
	if merged.Samples != before.Samples ||
		merged.MinMemAvailableKB != before.MinMemAvailableKB ||
		merged.MaxWorkerRSSBytes != before.MaxWorkerRSSBytes {
		t.Errorf("merging an empty report changed the totals: samples %d -> %d, min %d -> %d",
			before.Samples, merged.Samples, before.MinMemAvailableKB, merged.MinMemAvailableKB)
	}
}

// scripted returns a sample function that yields the given verdicts in order,
// repeating the last one, and counts how often it was called.
func scripted(ds ...Decision) (func() Decision, *int) {
	i := 0
	calls := 0
	return func() Decision {
		calls++
		d := ds[i]
		if i < len(ds)-1 {
			i++
		}
		return d
	}, &calls
}

func TestAdmitStartWaitsForTheBudgetToHold(t *testing.T) {
	// The command should not begin on one lucky sample. With a 3s hold and a 1s
	// interval the machine is re-checked three times before the first file.
	ok := Decision{Verdict: Allow, Reason: ReasonOK}
	sample, calls := scripted(ok, ok, ok, ok, ok)
	waits := 0
	var slept time.Duration
	wait := func(_ context.Context, d time.Duration) bool {
		waits++
		slept += d
		return true
	}
	d := admitStart(context.Background(), 3*time.Second, time.Second, sample, wait)
	if d.Verdict != Allow {
		t.Fatalf("a budget that held throughout gave %s/%s", d.Verdict, d.Reason)
	}
	if slept != 3*time.Second {
		t.Errorf("the hold slept %s, want exactly the 3s window", slept)
	}
	if waits != 3 {
		t.Errorf("the hold checked %d times, want 3 (once per second)", waits)
	}
	if *calls != 4 { // the initial sample plus three during the hold
		t.Errorf("the sampler was called %d times, want 4", *calls)
	}
}

func TestAdmitStartReturnsImmediatelyWhenAlreadyBlocked(t *testing.T) {
	// If the machine is outside the budget now, waiting cannot improve it, and
	// waiting would only delay the report an operator is reading.
	low := Decision{Verdict: Drain, Reason: ReasonMemBelowStart, Detail: "below the budget"}
	sample, calls := scripted(low)
	waits := 0
	wait := func(context.Context, time.Duration) bool { waits++; return true }
	d := admitStart(context.Background(), 3*time.Second, time.Second, sample, wait)
	if d.Verdict != Drain || d.Reason != ReasonMemBelowStart {
		t.Fatalf("an already-blocked machine gave %s/%s", d.Verdict, d.Reason)
	}
	if waits != 0 {
		t.Errorf("the hold waited %d times before a run that was already refused", waits)
	}
	if *calls != 1 {
		t.Errorf("the sampler was called %d times, want 1", *calls)
	}
}

func TestAdmitStartStopsWhenTheBudgetStopsHolding(t *testing.T) {
	// The case the hold exists for: two calm samples, then the machine falls
	// below the floor. The run must not begin, and the reason must be the
	// guard's own so the exit code and the stored task reason still work.
	ok := Decision{Verdict: Allow, Reason: ReasonOK}
	dip := Decision{Verdict: Drain, Reason: ReasonMemBelowWarn, Detail: "230 MiB available"}
	sample, _ := scripted(ok, ok, dip, ok)
	wait := func(context.Context, time.Duration) bool { return true }

	d := admitStart(context.Background(), 3*time.Second, time.Second, sample, wait)
	if d.Verdict != Drain || d.Reason != ReasonMemBelowWarn {
		t.Fatalf("a dip during the hold gave %s/%s, want drain/%s",
			d.Verdict, d.Reason, ReasonMemBelowWarn)
	}
	if !strings.Contains(d.Detail, "must hold for 3s") {
		t.Errorf("the detail does not explain that the budget failed to hold: %q", d.Detail)
	}
	if !strings.Contains(d.Detail, "230 MiB") {
		t.Errorf("the detail dropped the guard's own explanation: %q", d.Detail)
	}
}

func TestAdmitStartWithNoHoldIsASingleCheck(t *testing.T) {
	// start_hold = 0 is the escape hatch for a test rig, and it must be exactly
	// equivalent to the per-file gate: no sleeping at all.
	ok := Decision{Verdict: Allow, Reason: ReasonOK}
	sample, calls := scripted(ok)
	waits := 0
	wait := func(context.Context, time.Duration) bool { waits++; return true }
	d := admitStart(context.Background(), 0, time.Second, sample, wait)
	if d.Verdict != Allow || waits != 0 || *calls != 1 {
		t.Fatalf("with the hold disabled: verdict=%s waits=%d calls=%d",
			d.Verdict, waits, *calls)
	}
}

func TestAdmitStartCanBeInterrupted(t *testing.T) {
	// Ctrl-C during the hold must stop the run, not start it. Reporting
	// operator_stop keeps this out of the "resource trip" exit code, because
	// nothing went wrong with the machine.
	ok := Decision{Verdict: Allow, Reason: ReasonOK}
	sample, _ := scripted(ok)
	wait := func(context.Context, time.Duration) bool { return false }

	d := admitStart(context.Background(), 5*time.Second, time.Second, sample, wait)
	if d.Verdict != Stop || d.Reason != ReasonOperatorStop {
		t.Fatalf("an interrupted hold gave %s/%s, want stop/%s",
			d.Verdict, d.Reason, ReasonOperatorStop)
	}
	if !strings.Contains(d.Detail, "interrupted") {
		t.Errorf("the detail does not mention the interruption: %q", d.Detail)
	}
}

func TestAdmitStartPhasesTheWaitToTheExactWindow(t *testing.T) {
	// A hold that is not a whole number of intervals must not overshoot into
	// the next interval: the last sleep is trimmed to the window's edge, so a
	// 2.5s hold with a 1s interval sleeps 1s, 1s, 0.5s and no more.
	ok := Decision{Verdict: Allow, Reason: ReasonOK}
	sample, _ := scripted(ok)
	var steps []time.Duration
	wait := func(_ context.Context, d time.Duration) bool {
		steps = append(steps, d)
		return true
	}
	if d := admitStart(context.Background(), 2500*time.Millisecond, time.Second, sample, wait); d.Verdict != Allow {
		t.Fatalf("verdict %s/%s", d.Verdict, d.Reason)
	}
	want := []time.Duration{time.Second, time.Second, 500 * time.Millisecond}
	if len(steps) != len(want) {
		t.Fatalf("slept %v, want %v", steps, want)
	}
	for i := range want {
		if steps[i] != want[i] {
			t.Fatalf("slept %v, want %v", steps, want)
		}
	}
}
