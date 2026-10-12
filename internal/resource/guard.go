package resource

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Verdict is the guard's answer to "may more decoding happen?".
type Verdict int

const (
	// Allow: resources are inside the start budget, start or continue a file.
	Allow Verdict = iota
	// Drain: let the file in flight finish, then stop. No new file starts.
	Drain
	// Stop: stop the file in flight now.
	Stop
)

func (v Verdict) String() string {
	switch v {
	case Allow:
		return "allow"
	case Drain:
		return "drain"
	case Stop:
		return "stop"
	}
	return "unknown"
}

// Reason is a stable machine-readable cause. Task records store these strings,
// so they must not be reworded casually.
type Reason string

const (
	ReasonOK              Reason = "ok"
	ReasonNoSignal        Reason = "no_resource_signal"
	ReasonMemBelowStart   Reason = "mem_below_start"
	ReasonMemBelowWarn    Reason = "mem_below_warn"
	ReasonMemBelowStop    Reason = "mem_below_stop"
	ReasonSwapUsed        Reason = "swap_in_use"
	ReasonWorkerRSS       Reason = "worker_rss_over_max"
	ReasonWorkerCgroup    Reason = "worker_cgroup_over_max"
	ReasonCgroupMaxEvents Reason = "cgroup_memory_max_events"
	ReasonCgroupOOMKill   Reason = "cgroup_oom_kill"
	ReasonPSIMemory       Reason = "psi_memory_pressure"
	ReasonPSIIO           Reason = "psi_io_pressure"
	ReasonPerFileTimeout  Reason = "per_file_timeout"
	ReasonWorkerExit      Reason = "worker_exit_nonzero"
	ReasonWorkerSignal    Reason = "worker_killed_by_signal"
	ReasonCacheFull       Reason = "cache_full"
	ReasonPaused          Reason = "paused"
	ReasonPressureTrip    Reason = "pressure_trip"
	ReasonLimitReached    Reason = "limit_reached"
	ReasonExhausted       Reason = "collection_exhausted"
	ReasonMaxRuntime      Reason = "max_runtime_reached"
	ReasonErrorStreak     Reason = "error_streak"
	ReasonLocked          Reason = "task_locked"
	ReasonSourceCheck     Reason = "source_check_failed"
	ReasonOperatorStop    Reason = "operator_stop"
	// ReasonBackupBusy is a cooperative yield, not a fault. The existing backup
	// takes priority; the album task stops and says so. It is deliberately not
	// a pressure trip, so it does not arm the cooldown that a resource
	// exhaustion does: nothing went wrong, and the task simply tries again the
	// next time the operator asks.
	ReasonBackupBusy     Reason = "backup_busy"
	ReasonNoBackupStatus Reason = "backup_status_unknown"
)

// Snapshot is one consistent set of observations.
type Snapshot struct {
	At        time.Time
	Mem       MemInfo
	MemErr    error
	PSIMem    PSI
	PSIIO     PSI
	Load      LoadAvg
	Cgroup    Cgroup
	CgroupErr error
	Worker    RSSSample
	WorkerErr error
	Notes     []string
}

// Sampler reads the machine's resource facts.
//
// Paths are injectable so the whole guard can be tested against fixture trees
// instead of the live machine — the policy logic is the risky part, not the
// parsing, and it must be tested against conditions this host never produces.
type Sampler struct {
	ProcRoot    string
	CgroupDir   string
	CgroupRoot  string
	MemInfoPath string
}

// NewSampler builds a sampler reading the live machine.
func NewSampler(procRoot, cgroupDir string) *Sampler {
	if procRoot == "" {
		procRoot = "/proc"
	}
	return &Sampler{
		ProcRoot:    procRoot,
		CgroupDir:   cgroupDir,
		MemInfoPath: filepath.Join(procRoot, "meminfo"),
	}
}

// errNoSampler is returned by a nil Sampler. It exists so that "nothing was
// measured" is never mistaken for "memory is fine" or for "memory is gone".
var errNoSampler = errors.New("no resource sampler was configured")

// Sample collects one snapshot. Missing individual files produce Notes, not
// errors: a partially instrumented machine still has to be handled honestly.
func (s *Sampler) Sample(workerPID int) Snapshot {
	snap := Snapshot{At: time.Now()}
	if s == nil {
		// A snapshot from a sampler that does not exist must not look like a
		// clean reading of a machine with no memory: the guard would report
		// "below the stop floor" instead of "no signal at all". Making it an
		// error routes it to ReasonNoSignal, which is what an operator needs
		// to see.
		snap.MemErr = errNoSampler
		snap.Notes = append(snap.Notes, errNoSampler.Error())
		return snap
	}
	snap.Mem, snap.MemErr = ReadMemInfo(s.MemInfoPath)
	if snap.MemErr != nil {
		snap.Notes = append(snap.Notes, "meminfo: "+snap.MemErr.Error())
	}
	if p, err := ReadPSI(filepath.Join(s.ProcRoot, "pressure", "memory")); err == nil {
		snap.PSIMem = p
	} else {
		snap.Notes = append(snap.Notes, "psi memory: "+err.Error())
	}
	if p, err := ReadPSI(filepath.Join(s.ProcRoot, "pressure", "io")); err == nil {
		snap.PSIIO = p
	} else {
		snap.Notes = append(snap.Notes, "psi io: "+err.Error())
	}
	if l, err := ReadLoadAvg(filepath.Join(s.ProcRoot, "loadavg")); err == nil {
		snap.Load = l
	}
	if s.CgroupDir != "" {
		snap.Cgroup, snap.CgroupErr = ReadCgroup(s.CgroupDir)
		if snap.CgroupErr != nil {
			snap.Notes = append(snap.Notes, "cgroup: "+snap.CgroupErr.Error())
		}
	}
	if workerPID > 0 {
		snap.Worker, snap.WorkerErr = TreeRSS(s.ProcRoot, workerPID)
		if snap.WorkerErr != nil {
			snap.Notes = append(snap.Notes, "worker rss: "+snap.WorkerErr.Error())
		}
	}
	return snap
}

// SelfRSSBytes reports this process's resident set. Used to show that the web
// service is within its budget; VmHWM is reported separately because a spike
// that already happened is invisible in VmRSS.
func SelfRSSBytes(procRoot string) (rss, hwm int64, err error) {
	m, err := ReadProcMemory(procRoot, os.Getpid())
	if err != nil {
		return 0, 0, err
	}
	return m.VMRSSKB << 10, m.VMHWMB << 10, nil
}

// Limits is the policy the guard enforces.
type Limits struct {
	StartMemAvailableKB int64
	WarnMemAvailableKB  int64
	StopMemAvailableKB  int64

	WorkerMaxBytes  int64
	WorkerHighBytes int64

	PSIMemorySomeAvg10Stop float64
	PSIIOSomeAvg10Stop     float64

	MaxSwapUsedKB int64

	// StartHold is how long the whole-machine budget must hold before a run may
	// begin. BeforeFile gates every file; this gate is about the *start* of a
	// run, where a momentary lull would otherwise admit a decoder onto a machine
	// that is still under pressure.
	StartHold time.Duration
	// SampleInterval is how often StartHold re-checks. It is also the interval
	// the supervisor samples a running worker at.
	SampleInterval time.Duration
}

// Guard turns snapshots into verdicts.
type Guard struct {
	Limits  Limits
	Sampler *Sampler
}

// NewGuard builds a guard for the live machine.
func NewGuard(limits Limits, sampler *Sampler) *Guard {
	return &Guard{Limits: limits, Sampler: sampler}
}

// Decision is a verdict plus the evidence behind it.
type Decision struct {
	Verdict  Verdict
	Reason   Reason
	Detail   string
	Snapshot Snapshot
}

// Baseline records counters at the moment a file started so that deltas caused
// by that file can be attributed to it. Absolute counters are useless here: the
// machine may have OOM-killed something an hour ago.
type Baseline struct {
	At            time.Time
	CgroupOOM     int64
	CgroupOOMKill int64
	CgroupOOMGrp  int64
	CgroupMax     int64
	CgroupHigh    int64
	PSIMemTotal   int64
	PSIIOTotal    int64
}

// Baseline captures the counters that matter for delta attribution.
func (g *Guard) Baseline() Baseline {
	snap := g.Sampler.Sample(0)
	return Baseline{
		At:            snap.At,
		CgroupOOM:     snap.Cgroup.EventsOOM,
		CgroupOOMKill: snap.Cgroup.EventsOOMKill,
		CgroupOOMGrp:  snap.Cgroup.EventsOOMGroupKill,
		CgroupMax:     snap.Cgroup.EventsMax,
		CgroupHigh:    snap.Cgroup.EventsHigh,
		PSIMemTotal:   snap.PSIMem.SomeTotal,
		PSIIOTotal:    snap.PSIIO.SomeTotal,
	}
}

// BeforeFile decides whether a new file may be started.
//
// It returns Allow only when MemAvailable is at or above the start budget. This
// is checked before *and* after every file, never once per batch.
func (g *Guard) BeforeFile() Decision {
	snap := g.Sampler.Sample(0)
	return g.beforeFileDecision(snap)
}

func (g *Guard) beforeFileDecision(snap Snapshot) Decision {
	l := g.Limits
	d := Decision{Snapshot: snap}
	if snap.MemErr != nil && !snap.Cgroup.Present {
		d.Verdict = Stop
		d.Reason = ReasonNoSignal
		d.Detail = "neither /proc/meminfo nor a readable cgroup is available; " +
			"cannot verify that starting a decoder is safe"
		return d
	}
	if snap.MemErr == nil {
		avail := snap.Mem.MemAvailableKB
		switch {
		case avail < l.StopMemAvailableKB:
			return d.with(Stop, ReasonMemBelowStop, fmt.Sprintf(
				"MemAvailable %d kB is below the stop floor %d kB", avail, l.StopMemAvailableKB))
		case avail < l.WarnMemAvailableKB:
			return d.with(Drain, ReasonMemBelowWarn, fmt.Sprintf(
				"MemAvailable %d kB is below the warning floor %d kB; not starting new work",
				avail, l.WarnMemAvailableKB))
		case avail < l.StartMemAvailableKB:
			return d.with(Drain, ReasonMemBelowStart, fmt.Sprintf(
				"MemAvailable %d kB is below the start budget %d kB; not starting new work",
				avail, l.StartMemAvailableKB))
		}
	}
	if l.MaxSwapUsedKB > 0 && snap.Mem.SwapUsedKB() > l.MaxSwapUsedKB {
		return d.with(Drain, ReasonSwapUsed, fmt.Sprintf(
			"swap in use %d kB exceeds %d kB; a weak ARM box must not be pushed into swap",
			snap.Mem.SwapUsedKB(), l.MaxSwapUsedKB))
	}
	if l.PSIMemorySomeAvg10Stop > 0 && snap.PSIMem.Present &&
		snap.PSIMem.SomeAvg10 > l.PSIMemorySomeAvg10Stop {
		return d.with(Drain, ReasonPSIMemory, fmt.Sprintf(
			"memory PSI some avg10=%.2f exceeds %.2f; the machine is already stalling on memory",
			snap.PSIMem.SomeAvg10, l.PSIMemorySomeAvg10Stop))
	}
	return d.with(Allow, ReasonOK, "")
}

// AdmitStart decides whether a run may begin.
//
// BeforeFile answers "may another file start right now". This answers "is it
// safe to start a run at all", and it requires the budget to hold for
// Limits.StartHold rather than merely being true at one instant. A single sample
// can catch a machine between two allocations of somebody else's page cache and
// look calm when it is not.
//
// The hold is bounded by ctx so that Ctrl-C during the wait stops the run
// instead of starting it.
func (g *Guard) AdmitStart(ctx context.Context) Decision {
	return admitStart(ctx, g.Limits.StartHold, g.Limits.SampleInterval, g.BeforeFile, sleepCtx)
}

// admitStart is AdmitStart with its clock injected, so the policy can be tested
// without waiting in real time.
//
// sample returns the current verdict; wait sleeps for the given duration and
// reports whether it completed (false means the context was cancelled).
func admitStart(ctx context.Context, hold, interval time.Duration,
	sample func() Decision, wait func(context.Context, time.Duration) bool) Decision {

	first := sample()
	// Nothing to hold: either the machine is already outside the budget, in
	// which case waiting cannot help, or no hold was asked for.
	if first.Verdict != Allow || hold <= 0 {
		return first
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}

	var held time.Duration
	for held < hold {
		step := interval
		if remaining := hold - held; remaining < step {
			step = remaining
		}
		if !wait(ctx, step) {
			d := first
			d.Verdict = Stop
			d.Reason = ReasonOperatorStop
			d.Detail = fmt.Sprintf(
				"the start-of-run hold (%s) was interrupted before it completed", hold)
			return d
		}
		held += step

		d := sample()
		if d.Verdict != Allow {
			// Keep the reason the guard produced - the exit code and the stored
			// task reason both come from it - but say plainly that the budget
			// failed to hold, which is a different fact from a momentary dip.
			d.Detail = fmt.Sprintf("%s (the start budget must hold for %s before a run "+
				"begins; after %s it did not)", d.Detail, hold, held)
			return d
		}
		first = d
	}
	return first
}

// sleepCtx sleeps for d, or returns false early if ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// DuringFile decides whether the running file may continue.
//
// The counters are compared against base so that an OOM kill from an earlier
// run does not abort an unrelated file.
func (g *Guard) DuringFile(base Baseline, workerPID int) Decision {
	snap := g.Sampler.Sample(workerPID)
	return g.duringFileDecision(base, snap)
}

func (g *Guard) duringFileDecision(base Baseline, snap Snapshot) Decision {
	l := g.Limits
	d := Decision{Snapshot: snap}

	// Kernel-enforced limits first: these are facts, not estimates.
	if snap.Cgroup.Present {
		if snap.Cgroup.EventsOOMKill > base.CgroupOOMKill {
			return d.with(Stop, ReasonCgroupOOMKill, fmt.Sprintf(
				"the worker cgroup was OOM-killed (oom_kill %d -> %d); memory.max=%d MiB was exceeded",
				base.CgroupOOMKill, snap.Cgroup.EventsOOMKill, l.WorkerMaxBytes>>20))
		}
		if snap.Cgroup.EventsOOM > base.CgroupOOM {
			return d.with(Stop, ReasonCgroupOOMKill, fmt.Sprintf(
				"cgroup oom events rose (%d -> %d) during this file",
				base.CgroupOOM, snap.Cgroup.EventsOOM))
		}
		if snap.Cgroup.EventsMax > base.CgroupMax {
			return d.with(Stop, ReasonCgroupMaxEvents, fmt.Sprintf(
				"allocations hit memory.max %d MiB (max events %d -> %d)",
				l.WorkerMaxBytes>>20, base.CgroupMax, snap.Cgroup.EventsMax))
		}
		if l.WorkerMaxBytes > 0 && snap.Cgroup.Current > l.WorkerMaxBytes {
			return d.with(Stop, ReasonWorkerCgroup, fmt.Sprintf(
				"worker cgroup current %.1f MiB exceeds %d MiB",
				float64(snap.Cgroup.Current)/(1<<20), l.WorkerMaxBytes>>20))
		}
	}
	if l.WorkerMaxBytes > 0 && snap.WorkerErr == nil && snap.Worker.RSSBytes > l.WorkerMaxBytes {
		return d.with(Stop, ReasonWorkerRSS, fmt.Sprintf(
			"worker tree RSS %.1f MiB (%d processes) exceeds the %d MiB ceiling",
			float64(snap.Worker.RSSBytes)/(1<<20), len(snap.Worker.PIDs), l.WorkerMaxBytes>>20))
	}
	if snap.MemErr == nil {
		avail := snap.Mem.MemAvailableKB
		if avail < l.StopMemAvailableKB {
			return d.with(Stop, ReasonMemBelowStop, fmt.Sprintf(
				"MemAvailable fell to %d kB, below the stop floor %d kB", avail, l.StopMemAvailableKB))
		}
		if l.MaxSwapUsedKB > 0 && snap.Mem.SwapUsedKB() > l.MaxSwapUsedKB {
			return d.with(Stop, ReasonSwapUsed, fmt.Sprintf(
				"swap in use %d kB exceeds %d kB during decode", snap.Mem.SwapUsedKB(), l.MaxSwapUsedKB))
		}
	}
	if l.PSIMemorySomeAvg10Stop > 0 && snap.PSIMem.Present &&
		snap.PSIMem.SomeAvg10 > l.PSIMemorySomeAvg10Stop {
		return d.with(Stop, ReasonPSIMemory, fmt.Sprintf(
			"memory PSI some avg10=%.2f exceeds %.2f during decode",
			snap.PSIMem.SomeAvg10, l.PSIMemorySomeAvg10Stop))
	}
	if l.PSIIOSomeAvg10Stop > 0 && snap.PSIIO.Present &&
		snap.PSIIO.SomeAvg10 > l.PSIIOSomeAvg10Stop {
		return d.with(Drain, ReasonPSIIO, fmt.Sprintf(
			"I/O PSI some avg10=%.2f exceeds %.2f; finishing this file and stopping",
			snap.PSIIO.SomeAvg10, l.PSIIOSomeAvg10Stop))
	}
	if snap.MemErr == nil && snap.Mem.MemAvailableKB < l.WarnMemAvailableKB {
		return d.with(Drain, ReasonMemBelowWarn, fmt.Sprintf(
			"MemAvailable %d kB is below the warning floor %d kB",
			snap.Mem.MemAvailableKB, l.WarnMemAvailableKB))
	}
	return d.with(Allow, ReasonOK, "")
}

func (d Decision) with(v Verdict, r Reason, detail string) Decision {
	d.Verdict = v
	d.Reason = r
	d.Detail = detail
	return d
}

// Capability reports which control and observation mechanisms actually work
// here. `doctor` prints it; heavy tasks refuse to start when a required one is
// missing.
type Capability struct {
	MemInfo          bool
	PSIMemory        bool
	PSIIO            bool
	ProcFS           bool
	CgroupAccounting bool
	CgroupIsolation  bool
	CgroupReason     string
	Notes            []string
}

// ProbeCapability inspects the environment without changing it.
func ProbeCapability(sampler *Sampler, cm *CgroupManager) Capability {
	var c Capability
	snap := sampler.Sample(0)
	c.MemInfo = snap.MemErr == nil
	c.PSIMemory = snap.PSIMem.Present
	c.PSIIO = snap.PSIIO.Present
	if _, err := os.Stat(filepath.Join(sampler.ProcRoot, "meminfo")); err == nil {
		c.ProcFS = true
	}
	c.CgroupAccounting = snap.Cgroup.Present
	if cm != nil {
		c.CgroupIsolation = cm.Active
		c.CgroupReason = cm.Reason
	}
	if !c.MemInfo {
		c.Notes = append(c.Notes, "/proc/meminfo is unreadable: whole-machine gating is impossible")
	}
	if !c.CgroupAccounting {
		c.Notes = append(c.Notes, "no cgroup memory accounting: worker cost cannot be attributed")
	}
	if !c.PSIMemory {
		c.Notes = append(c.Notes, "memory PSI unavailable: stall detection falls back to MemAvailable alone")
	}
	if !c.PSIIO {
		c.Notes = append(c.Notes, "I/O PSI unavailable: disk-stall detection is not available")
	}
	return c
}

// CapabilityProblems lists the reasons heavy tasks must not run. An empty slice
// means the required mechanisms are present.
func (c Capability) CapabilityProblems(requireCgroupIsolation bool) []string {
	var out []string
	if !c.MemInfo && !c.CgroupAccounting {
		out = append(out, "no usable resource signal: /proc/meminfo and the cgroup memory "+
			"controller are both unavailable, so nothing can stop a decoder that runs away")
	}
	if requireCgroupIsolation && !c.CgroupIsolation {
		reason := c.CgroupReason
		if reason == "" {
			reason = "not established"
		}
		out = append(out, "cgroup isolation is required by configuration but unavailable: "+reason+
			"; run the service with systemd Delegate=yes or set guard.require_cgroup_isolation=false "+
			"to accept RSS-only supervision")
	}
	return out
}

// Stats accumulates a resource report.
type Stats struct {
	Samples int
	// MinMemAvailableKB is the low-water mark. memMinSeen distinguishes a real
	// zero from "no readable sample yet": without it, a machine that truly ran
	// out of available memory would report whatever later, healthier figure
	// arrived first.
	MinMemAvailableKB  int64
	memMinSeen         bool
	MaxSwapUsedKB      int64
	MaxWorkerRSSBytes  int64
	MaxWorkerHWMBytes  int64
	MaxCgroupCurrent   int64
	MaxCgroupPeak      int64
	MaxPSIMemSomeAvg10 float64
	MaxPSIIOSomeAvg10  float64
	SumWorkerRSSBytes  int64
	CgroupAvailable    bool
	Notes              []string
}

// Observe folds one snapshot into the report.
func (s *Stats) Observe(snap Snapshot) {
	s.Samples++
	// MemTotalKB > 0 is what distinguishes "a machine was measured" from "a
	// zero-valued Snapshot was passed in". Without it, an empty snapshot would
	// contribute a low-water mark of zero available memory.
	if snap.MemErr == nil && snap.Mem.MemTotalKB > 0 {
		if !s.memMinSeen || snap.Mem.MemAvailableKB < s.MinMemAvailableKB {
			s.MinMemAvailableKB = snap.Mem.MemAvailableKB
			s.memMinSeen = true
		}
		if u := snap.Mem.SwapUsedKB(); u > s.MaxSwapUsedKB {
			s.MaxSwapUsedKB = u
		}
	}
	if snap.WorkerErr == nil {
		if snap.Worker.RSSBytes > s.MaxWorkerRSSBytes {
			s.MaxWorkerRSSBytes = snap.Worker.RSSBytes
		}
		if snap.Worker.HWMBytes > s.MaxWorkerHWMBytes {
			s.MaxWorkerHWMBytes = snap.Worker.HWMBytes
		}
		s.SumWorkerRSSBytes += snap.Worker.RSSBytes
	}
	if snap.Cgroup.Present {
		s.CgroupAvailable = true
		if snap.Cgroup.Current > s.MaxCgroupCurrent {
			s.MaxCgroupCurrent = snap.Cgroup.Current
		}
		if snap.Cgroup.Peak > s.MaxCgroupPeak {
			s.MaxCgroupPeak = snap.Cgroup.Peak
		}
	}
	if snap.PSIMem.SomeAvg10 > s.MaxPSIMemSomeAvg10 {
		s.MaxPSIMemSomeAvg10 = snap.PSIMem.SomeAvg10
	}
	if snap.PSIIO.SomeAvg10 > s.MaxPSIIOSomeAvg10 {
		s.MaxPSIIOSomeAvg10 = snap.PSIIO.SomeAvg10
	}
	s.Notes = append(s.Notes, snap.Notes...)
}

// Merge folds another report into this one.
func (s *Stats) Merge(o Stats) {
	if o.Samples == 0 {
		return
	}
	s.Samples += o.Samples
	s.SumWorkerRSSBytes += o.SumWorkerRSSBytes
	s.CgroupAvailable = s.CgroupAvailable || o.CgroupAvailable
	if o.memMinSeen && (!s.memMinSeen || o.MinMemAvailableKB < s.MinMemAvailableKB) {
		s.MinMemAvailableKB = o.MinMemAvailableKB
		s.memMinSeen = true
	}
	if o.MaxSwapUsedKB > s.MaxSwapUsedKB {
		s.MaxSwapUsedKB = o.MaxSwapUsedKB
	}
	if o.MaxWorkerRSSBytes > s.MaxWorkerRSSBytes {
		s.MaxWorkerRSSBytes = o.MaxWorkerRSSBytes
	}
	if o.MaxWorkerHWMBytes > s.MaxWorkerHWMBytes {
		s.MaxWorkerHWMBytes = o.MaxWorkerHWMBytes
	}
	if o.MaxCgroupCurrent > s.MaxCgroupCurrent {
		s.MaxCgroupCurrent = o.MaxCgroupCurrent
	}
	if o.MaxCgroupPeak > s.MaxCgroupPeak {
		s.MaxCgroupPeak = o.MaxCgroupPeak
	}
	if o.MaxPSIMemSomeAvg10 > s.MaxPSIMemSomeAvg10 {
		s.MaxPSIMemSomeAvg10 = o.MaxPSIMemSomeAvg10
	}
	if o.MaxPSIIOSomeAvg10 > s.MaxPSIIOSomeAvg10 {
		s.MaxPSIIOSomeAvg10 = o.MaxPSIIOSomeAvg10
	}
	s.Notes = append(s.Notes, o.Notes...)
}

// HumanReport renders the report for a terminal.
//
// The three numbers are deliberately kept apart: process tree RSS, cgroup
// accounting, and whole-machine MemAvailable answer different questions and
// must never be added together into a single "usage" figure.
func (s Stats) HumanReport() string {
	var b strings.Builder
	fmt.Fprintf(&b, "samples:              %d\n", s.Samples)
	fmt.Fprintf(&b, "MemAvailable min:     %s (%d kB)\n", kib(float64(s.MinMemAvailableKB)), s.MinMemAvailableKB)
	fmt.Fprintf(&b, "swap used max:        %s (%d kB)\n", kib(float64(s.MaxSwapUsedKB)), s.MaxSwapUsedKB)
	fmt.Fprintf(&b, "worker tree RSS peak: %s (%d bytes)\n", mib(float64(s.MaxWorkerRSSBytes)), s.MaxWorkerRSSBytes)
	fmt.Fprintf(&b, "worker tree HWM sum:  %s (sum of per-process VmHWM, an upper bound)\n",
		mib(float64(s.MaxWorkerHWMBytes)))
	if s.Samples > 0 {
		fmt.Fprintf(&b, "worker RSS mean:      %s\n", mib(float64(s.SumWorkerRSSBytes)/float64(s.Samples)))
	}
	if s.CgroupAvailable {
		fmt.Fprintf(&b, "cgroup current peak:  %s\n", mib(float64(s.MaxCgroupCurrent)))
		fmt.Fprintf(&b, "cgroup memory.peak:   %s\n", mib(float64(s.MaxCgroupPeak)))
	} else {
		fmt.Fprintf(&b, "cgroup current peak:  unavailable\n")
	}
	fmt.Fprintf(&b, "PSI memory some max:  %.2f%%\n", s.MaxPSIMemSomeAvg10)
	fmt.Fprintf(&b, "PSI io some max:      %.2f%%\n", s.MaxPSIIOSomeAvg10)
	return b.String()
}

func kib(v float64) string { return fmt.Sprintf("%.1f MiB", v/1024) }
func mib(v float64) string { return fmt.Sprintf("%.1f MiB", v/(1<<20)) }
