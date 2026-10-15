package thumbs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/backupgate"
	"github.com/YoisakiKnd/nasctl/internal/cachepath"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/logx"
	"github.com/YoisakiKnd/nasctl/internal/resource"
	"github.com/YoisakiKnd/nasctl/internal/source"
	"github.com/YoisakiKnd/nasctl/internal/store"
	"github.com/YoisakiKnd/nasctl/internal/taskctl"
)

// RunOptions bound one thumbnail run.
type RunOptions struct {
	CollectionCode string
	Limit          int
	MaxRuntime     time.Duration
	// Force skips the post-trip cooldown but never the manual pause.
	Force bool
	// AllowNoCgroup permits a run without kernel-enforced worker isolation.
	// The design requires heavy tasks to be disabled by default when the
	// platform cannot enforce the limits, so this has to be an explicit act and
	// it is recorded in the task row.
	AllowNoCgroup bool
	// NoHandshake is used by tests that run a worker directly.
	NoHandshake bool
	OnProgress  func(string)
}

// FileRecord is the per-file diagnostic kept for the report and the task log.
type FileRecord struct {
	MediaID      int64
	RelPath      string
	Outcome      string
	Reason       string
	Detail       string
	MS           int64
	PeakRSSBytes int64
	CgroupPeak   int64
	MemAvailMin  int64
	ThumbBytes   int64
}

// Report is the outcome of a thumbnail run.
type Report struct {
	Collection           string
	TaskID               int64
	StopReason           string
	StopDetail           string
	Limit                int
	MaxRuntime           time.Duration
	Processed            int64
	Skipped              int64
	Failed               int64
	BytesWritten         int64
	CgroupIsolated       bool
	CgroupReason         string
	CgroupLayout         resource.CgroupLayout
	Capability           resource.Capability
	Stats                resource.Stats
	Files                []FileRecord
	Notes                []string
	SourceChecks         []source.Check
	MemAvailableBeforeKB int64
	MemAvailableAfterKB  int64
	// WorkerLiveSamples counts the samples at which a running worker was
	// actually measured. A zero here must not be read as "the decoder used no
	// memory": it means no sample landed while the worker was alive.
	WorkerLiveSamples int64
	// SampleInterval is the interval the supervisor sampled worker RSS at, so a
	// missing observation can be explained rather than just reported.
	SampleInterval time.Duration
	CacheBytes     int64
	CacheFiles     int64
	Started        time.Time
	Ended          time.Time
}

// Runner supervises bounded thumbnail work.
type Runner struct {
	Cfg   *config.Config
	Store *store.Store
	Log   *logx.Logger

	// Backup answers "is the existing backup busy?". It is nil when the
	// operator has configured no check, in which case the album task runs
	// without coordination and says so in its report.
	Backup *backupgate.Gate
}

// Run performs one bounded thumbnail run.
func (r *Runner) Run(ctx context.Context, opts RunOptions) (*Report, error) {
	rep := &Report{
		Collection:     opts.CollectionCode,
		Limit:          opts.Limit,
		MaxRuntime:     opts.MaxRuntime,
		SampleInterval: r.Cfg.Guard.SampleInterval.D(),
		Started:        time.Now(),
	}

	col, ok := r.Cfg.Collection(opts.CollectionCode)
	if !ok {
		return rep, fmt.Errorf("unknown collection %q", opts.CollectionCode)
	}
	if !col.Enabled {
		return rep, fmt.Errorf("collection %q is disabled in the configuration", col.Code)
	}

	if err := r.Store.WaitForWriteIdle(ctx, 15*time.Second); err != nil {
		return rep, err
	}
	lock, err := taskctl.Acquire(r.Cfg.Paths.LockDir, "thumbs")
	if err != nil {
		return rep, err
	}
	defer func() { _ = lock.Release() }()

	// --- the pause / pressure gate. Checked before anything else. --------
	gate, err := r.Store.CheckRunGate(ctx, r.Cfg.Guard.CooldownAfterTrip.D(), opts.Force)
	if err != nil {
		return rep, err
	}
	if gate.Paused {
		rep.StopReason = string(resource.ReasonPaused)
		rep.StopDetail = fmt.Sprintf("albumctl is paused (%s). Run `albumctl resume` to allow heavy tasks.",
			orDefault(gate.PauseReason, "no reason recorded"))
		return rep, nil
	}
	if !gate.CanStart() {
		rep.StopReason = string(resource.ReasonPressureTrip)
		rep.StopDetail = fmt.Sprintf(
			"a previous run was stopped by resource pressure at %s (%s). "+
				"Automatic restart is disabled by design; run `albumctl resume` to clear the trip "+
				"after checking the machine, or wait out the %s cooldown.",
			time.Unix(gate.Pressure.At, 0).Format(time.RFC3339),
			orDefault(gate.Pressure.Reason, "reason not recorded"),
			r.Cfg.Guard.CooldownAfterTrip.D())
		return rep, nil
	}

	// --- source verification --------------------------------------------
	var colID int64
	for _, c := range r.Cfg.EnabledCollections() {
		if opts.CollectionCode != "" && c.Code != col.Code {
			continue
		}
		sc := source.Verify(c, r.Cfg.Paths.StateDir, "")
		rep.SourceChecks = append(rep.SourceChecks, sc)
		if !sc.OK {
			rep.StopReason = string(resource.ReasonSourceCheck)
			rep.StopDetail = sc.Fatal
			return rep, errors.New(sc.Fatal)
		}
		id, uerr := r.Store.UpsertCollection(ctx, c.Code, c.Name, c.Root, c.DataMount, c.Enabled, c.FollowSymlinks)
		if uerr != nil {
			return rep, uerr
		}
		if c.Code == col.Code {
			colID = id
			rep.SourceChecks[len(rep.SourceChecks)-1].OK = true
			_ = r.Store.RecordMountCheck(ctx, id, true, strings.Join(sc.Notes, "; "), sc.FSID, sc.RootDevice)
		}
	}
	if colID == 0 {
		return rep, fmt.Errorf("collection %q is not in the enabled set", col.Code)
	}

	// --- resource capability gate ---------------------------------------
	sampler := resource.NewSampler("/proc", "")
	cm, cerr := resource.NewCgroupManager(r.Cfg.Guard.CgroupRoot, "/proc")
	if cerr != nil {
		r.Log.Warn("cgroup manager could not be initialised", "err", cerr.Error())
	}
	cap := resource.ProbeCapability(sampler, cm)
	rep.Capability = cap
	// Observation is read-only; a newly created manager cannot already be
	// isolated. Require usable signals here, then establish and verify the
	// actual cgroup before admitting any decoder below.
	if problems := cap.CapabilityProblems(false); len(problems) > 0 {
		rep.StopReason = "isolation_unavailable"
		rep.StopDetail = strings.Join(problems, "; ")
		return rep, errors.New(rep.StopDetail)
	}

	layout := resource.CgroupLayout{
		WorkerMemHighBytes: r.Cfg.Guard.WorkerMemHighMB << 20,
		WorkerMemMaxBytes:  r.Cfg.Guard.WorkerMemMaxMB << 20,
		WorkerSwapMax:      0,
		OOMGroup:           true,
	}
	rep.CgroupLayout = layout
	if r.Cfg.Guard.UseCgroup && cm != nil {
		if err := cm.Prepare(layout); err != nil {
			rep.CgroupReason = cm.Reason
			if r.Cfg.Guard.RequireCgroupIsolation && !opts.AllowNoCgroup {
				rep.StopReason = "isolation_unavailable"
				rep.StopDetail = "cgroup isolation is required but could not be established: " + cm.Reason
				return rep, errors.New(rep.StopDetail)
			}
			r.Log.Warn("running without cgroup isolation; the RSS guard and the systemd unit limit are the only ceilings",
				"reason", cm.Reason)
		} else {
			rep.CgroupIsolated = true
			rep.CgroupReason = cm.Reason
			defer cm.Cleanup()
		}
	} else {
		rep.CgroupReason = "cgroup isolation disabled by configuration"
	}
	if cm != nil && cm.Active {
		sampler = resource.NewSampler("/proc", cm.WorkDir)
	}
	cap = resource.ProbeCapability(sampler, cm)
	rep.Capability = cap
	if problems := cap.CapabilityProblems(r.Cfg.Guard.RequireCgroupIsolation && !opts.AllowNoCgroup); len(problems) > 0 {
		rep.StopReason = "isolation_unavailable"
		rep.StopDetail = strings.Join(problems, "; ")
		return rep, errors.New(rep.StopDetail)
	}

	if isolated := cm != nil && cm.Active; isolated {
		if eff, err := cm.EffectiveWorkerLimitBytes(); err == nil {
			rep.CgroupReason += fmt.Sprintf("; effective kernel ceiling %.0f MiB", float64(eff)/(1<<20))
		}
	}

	guard := resource.NewGuard(resource.Limits{
		StartHold:              r.Cfg.Guard.StartHold.D(),
		SampleInterval:         r.Cfg.Guard.SampleInterval.D(),
		StartMemAvailableKB:    r.Cfg.Guard.StartMemAvailableKB,
		WarnMemAvailableKB:     r.Cfg.Guard.WarnMemAvailableKB,
		StopMemAvailableKB:     r.Cfg.Guard.StopMemAvailableKB,
		WorkerMaxBytes:         r.Cfg.Guard.WorkerMemMaxMB << 20,
		WorkerHighBytes:        r.Cfg.Guard.WorkerMemHighMB << 20,
		PSIMemorySomeAvg10Stop: r.Cfg.Guard.PSIMemorySomeAvg10Stop,
		PSIIOSomeAvg10Stop:     r.Cfg.Guard.PSIIOSomeAvg10Stop,
		MaxSwapUsedKB:          r.Cfg.Guard.MaxSwapUsedKB,
	}, sampler)

	pre := sampler.Sample(0)
	rep.MemAvailableBeforeKB = pre.Mem.MemAvailableKB
	rep.Stats.Observe(pre)

	// Report and cache accounting are seeded once, then maintained in memory so
	// that each iteration does not run an aggregate over the whole table.
	files, bytes, err := r.Store.SumThumbBytes(ctx)
	if err != nil {
		return rep, err
	}
	rep.CacheFiles, rep.CacheBytes = files, bytes

	taskID, err := r.Store.StartTask(ctx, store.Task{
		Kind:           store.TaskThumbs,
		CollectionCode: col.Code,
		PID:            int64(os.Getpid()),
		LimitN:         int64(opts.Limit),
		MaxRuntimeS:    int64(opts.MaxRuntime.Seconds()),
		CgroupIsolated: rep.CgroupIsolated,
		Argv:           taskctl.Argv(),
	})
	if err != nil {
		return rep, err
	}
	rep.TaskID = taskID

	// --- the run ---------------------------------------------------------
	stopReason, stopDetail := r.loop(ctx, opts, col, colID, guard, sampler, cm, rep, taskID, &bytes, &files)

	// The in-loop counters are authoritative for this run; the values read from
	// the index before it started are stale by definition.
	rep.CacheBytes, rep.CacheFiles = bytes, files

	rep.StopReason = stopReason
	rep.StopDetail = stopDetail
	if stopDetail != "" {
		rep.Notes = append(rep.Notes, stopDetail)
	}

	post := sampler.Sample(0)
	rep.MemAvailableAfterKB = post.Mem.MemAvailableKB
	rep.Stats.Observe(post)
	rep.Ended = time.Now()

	if rss, hwm, err := resource.SelfRSSBytes("/proc"); err == nil {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"supervisor RSS %.1f MiB, supervisor VmHWM %.1f MiB",
			float64(rss)/(1<<20), float64(hwm)/(1<<20)))
	}

	// A resource stop becomes a persistent trip so the next run cannot start
	// itself. This is the mechanism behind "no immediate automatic restart".
	if isPressureReason(stopReason) {
		snapshot := fmt.Sprintf(
			"MemAvailable %d kB, swap used %d kB, worker RSS peak %d B, cgroup peak %d B, PSI memory some avg10 %.2f",
			rep.Stats.MinMemAvailableKB, rep.Stats.MaxSwapUsedKB, rep.Stats.MaxWorkerRSSBytes,
			rep.Stats.MaxCgroupPeak, rep.Stats.MaxPSIMemSomeAvg10)
		if terr := r.Store.TripPressure(ctx, stopReason, stopDetail, snapshot); terr != nil {
			rep.Notes = append(rep.Notes, "could not record the pressure trip: "+terr.Error())
		}
		rep.Notes = append(rep.Notes,
			"a resource-pressure trip was recorded; heavy tasks will not start again until `albumctl resume`")
	}

	task := store.Task{
		Processed:    rep.Processed,
		Skipped:      rep.Skipped,
		Failed:       rep.Failed,
		BytesWritten: rep.BytesWritten,
	}
	state := store.TaskCompleted
	switch stopReason {
	case string(resource.ReasonLimitReached), string(resource.ReasonExhausted):
		state = store.TaskCompleted
	case string(resource.ReasonPaused), string(resource.ReasonPressureTrip):
		state = store.TaskStopped
	default:
		state = store.TaskStopped
	}
	if err := r.Store.FinishTask(ctx, taskID, state, stopReason, stopDetail, task, rep.ResourceReport()); err != nil {
		rep.Notes = append(rep.Notes, err.Error())
	}
	return rep, nil
}

// loop is the bounded per-file cycle.
func (r *Runner) loop(ctx context.Context, opts RunOptions, col config.Collection, colID int64, guard *resource.Guard,
	sampler *resource.Sampler, cm *resource.CgroupManager, rep *Report, taskID int64,
	cacheBytes, cacheFiles *int64) (stopReason, stopDetail string) {

	deadline := time.Now().Add(opts.MaxRuntime)
	expired := func() bool { return opts.MaxRuntime > 0 && time.Now().After(deadline) }

	// The sustained-start hold is spent before the first file, so a runtime
	// budget shorter than the hold cannot pay for a run at all. Saying so is
	// better than overshooting the budget the operator asked for.
	if hold := r.Cfg.Guard.StartHold.D(); hold > 0 && opts.MaxRuntime > 0 &&
		opts.MaxRuntime <= hold {
		return string(resource.ReasonMaxRuntime), fmt.Sprintf(
			"the %s runtime budget is not longer than the %s start-of-run hold; "+
				"a run that cannot begin was not started", opts.MaxRuntime, hold)
	}

	binary, err := taskctl.SelfExecPath()
	if err != nil {
		return "internal_error", "cannot locate the albumctl binary to spawn workers: " + err.Error()
	}
	stderrPath := filepath.Join(r.Cfg.Paths.StateDir, "worker.log")

	var processed int64
	// firstIteration is tracked separately from `processed` so that a future
	// path that skips a file without counting it cannot make the sustained-start
	// hold run again for the next file.
	firstIteration := true
	for {
		if ctx.Err() != nil {
			return string(resource.ReasonOperatorStop), "interrupted by the operator"
		}
		if opts.Limit > 0 && processed >= int64(opts.Limit) {
			return string(resource.ReasonLimitReached), fmt.Sprintf("processed %d files, the requested limit", processed)
		}
		if expired() {
			return string(resource.ReasonMaxRuntime), fmt.Sprintf("the %s runtime budget elapsed after %d files",
				opts.MaxRuntime, processed)
		}

		// The gate is re-evaluated before every file, not once per batch. The
		// first file of a run additionally has to clear the sustained-start
		// hold, so a momentary lull cannot begin a batch.
		var before resource.Decision
		if firstIteration {
			before = guard.AdmitStart(ctx)
		} else {
			before = guard.BeforeFile()
		}
		rep.Stats.Observe(before.Snapshot)
		if before.Verdict != resource.Allow {
			return string(before.Reason), before.Detail
		}

		pauseCheck, _ := r.Store.PauseState(ctx)
		if pauseCheck.Paused {
			return string(resource.ReasonPaused), "albumctl was paused while this run was in progress"
		}

		// The existing backup outranks the album. albumctl is the one that
		// yields, and it yields by stopping - never by touching the backup.
		if r.Cfg.Backup.SkipThumbsWhenBusy {
			switch v := r.BackupVerdict(ctx); {
			case v.Busy:
				return string(resource.ReasonBackupBusy), v.Detail
			case !v.Known && r.Cfg.Backup.RequireKnown:
				return string(resource.ReasonNoBackupStatus),
					"backup.require_known is set but the backup could not be asked: " + v.Detail
			case !v.Known:
				rep.Notes = append(rep.Notes,
					"the backup was not consulted: "+v.Detail+
						"; albumctl ran without coordinating with it")
			}
		}

		refs, qerr := r.Store.NextThumb(ctx, colID, r.Cfg.Decode.MaxAttempts, store.UnixNow(), 1)
		if qerr != nil {
			return "store_error", qerr.Error()
		}
		if len(refs) == 0 {
			return string(resource.ReasonExhausted),
				"no pending thumbnails remain for this collection within the retry budget"
		}
		ref := refs[0]

		out, err := cachepath.Path(r.Cfg.Paths.CacheDir, cachepath.Key(ref.CollectionCode, ref.RelPath),
			r.Cfg.Decode.ThumbMaxEdge, r.Cfg.Cache.ShardDepth)
		if err != nil {
			return "internal_error", err.Error()
		}

		// Cache ceiling is checked before the work, not after. When the cache is
		// full the run stops and says so; it never silently keeps growing.
		est := r.estimatedThumbBytes()
		if r.Cfg.Cache.MaxBytes > 0 && *cacheBytes+est > r.Cfg.Cache.MaxBytes {
			return string(resource.ReasonCacheFull), fmt.Sprintf(
				"the cache holds %s of a %s ceiling and adding more would exceed it; "+
					"run `albumctl cache-report` and prune deliberately with `albumctl cache-gc`",
				humanBytes(*cacheBytes), humanBytes(r.Cfg.Cache.MaxBytes))
		}
		if r.Cfg.Cache.MaxFiles > 0 && *cacheFiles >= int64(r.Cfg.Cache.MaxFiles) {
			return string(resource.ReasonCacheFull), fmt.Sprintf(
				"the cache holds %d files, at its configured ceiling of %d", *cacheFiles, r.Cfg.Cache.MaxFiles)
		}

		rec := r.runOne(ctx, opts, col, guard, sampler, cm, binary, stderrPath, ref, out, taskID, rep)
		firstIteration = false
		processed++
		rep.Processed++
		if rec != nil {
			switch rec.Outcome {
			case StateOK:
				rep.BytesWritten += rec.ThumbBytes
				*cacheBytes += rec.ThumbBytes
				*cacheFiles++
			case StateSkipped:
				rep.Skipped++
			default:
				rep.Failed++
			}
			if len(rep.Files) < 200 {
				rep.Files = append(rep.Files, *rec)
			}
		}
		if err := r.Store.Heartbeat(ctx, taskID, rep.Processed, rep.Skipped, rep.Failed, 0, rep.BytesWritten); err != nil {
			rep.Notes = append(rep.Notes, err.Error())
		}
		if opts.OnProgress != nil && processed%10 == 0 {
			opts.OnProgress(fmt.Sprintf("processed %d files: %d ok, %d skipped, %d failed",
				processed, rep.Processed-int64(rep.Skipped)-int64(rep.Failed), rep.Skipped, rep.Failed))
		}

		// The design's stop-loss rule: a burst of failures means something
		// systemic, so the run stops instead of grinding through the library.
		if r.Cfg.Decode.MaxErrorStreak > 0 && rep.Failed >= int64(r.Cfg.Decode.MaxErrorStreak) &&
			rep.Processed-rep.Skipped == rep.Failed {
			return string(resource.ReasonErrorStreak), fmt.Sprintf(
				"the first %d files all failed; stopping rather than working through the library", rep.Failed)
		}
	}
}

// BackupVerdict asks the backup gate, tolerating a nil gate.
func (r *Runner) BackupVerdict(ctx context.Context) backupgate.Verdict {
	if r.Backup == nil {
		return backupgate.Verdict{Known: false, Detail: "no backup check is configured"}
	}
	return r.Backup.Check(ctx)
}

// runOne spawns, supervises and reaps a single worker.
func (r *Runner) runOne(ctx context.Context, opts RunOptions, col config.Collection, guard *resource.Guard,
	sampler *resource.Sampler, cm *resource.CgroupManager, binary, stderrPath string,
	ref store.MediaRef, outPath string, taskID int64, rep *Report) *FileRecord {

	start := time.Now()
	rec := &FileRecord{MediaID: ref.ID, RelPath: ref.RelPath}

	args := []string{"exec-worker",
		"--root", col.Root,
		"--rel", ref.RelPath,
		"--out", outPath,
		"--expect-size", itoa(ref.Size),
		"--expect-mtime", itoa(ref.MtimeNS),
		"--max-edge", itoa(int64(r.Cfg.Decode.ThumbMaxEdge)),
		"--quality", itoa(int64(r.Cfg.Decode.ThumbQuality)),
		"--max-pixels", itoa(r.Cfg.Decode.MaxPixels),
		"--max-file-bytes", itoa(r.Cfg.Decode.MaxFileBytes),
		"--max-decode-mb", itoa(r.Cfg.Decode.MaxDecodeMemMB),
		"--max-frames", itoa(int64(r.Cfg.Decode.MaxFrames)),
		"--formats", r.formatNames(),
	}
	cgroupDir := ""
	if cm != nil && cm.Active {
		cgroupDir = cm.WorkDir
		args = append(args, "--cgroup-dir", cgroupDir)
	}
	if !opts.NoHandshake {
		args = append(args, "--handshake", "--max-runtime", r.Cfg.Decode.PerFileTimeout.D().String())
	} else {
		args = append(args, "--max-runtime", r.Cfg.Decode.PerFileTimeout.D().String())
	}

	proc, err := taskctl.SpawnWorker(binary, args, stderrPath)
	if err != nil {
		rec.Outcome = StateFailed
		rec.Reason = "spawn_failed"
		rec.Detail = err.Error()
		rec.MS = time.Since(start).Milliseconds()
		r.record(ctx, taskID, rec, store.ThumbUpdate{
			ID: ref.ID, State: store.ThumbFailed, Reason: rec.Reason,
			NextAttemptAt: store.UnixNow() + int64(r.Cfg.Decode.RetryBackoff.D().Seconds()),
			At:            store.UnixNow(),
		})
		return rec
	}
	defer proc.Close()

	if cm != nil && cm.Active {
		if aerr := cm.AttachWorker(proc.PID); aerr != nil {
			r.Log.Error("could not place the worker in its cgroup; aborting this file rather than decoding unguarded",
				"err", aerr.Error(), "media", ref.ID)
			_, _, _ = resource.TerminateGroup(proc.PGID, r.Cfg.Decode.GracePeriod.D(), 25*time.Millisecond)
			_ = proc.Cmd.Wait()
			rec.Outcome = StateFailed
			rec.Reason = "cgroup_attach_failed"
			rec.Detail = aerr.Error()
			r.record(ctx, taskID, rec, store.ThumbUpdate{
				ID: ref.ID, State: store.ThumbFailed, Reason: rec.Reason,
				NextAttemptAt: store.UnixNow() + int64(r.Cfg.Decode.RetryBackoff.D().Seconds()),
				At:            store.UnixNow(),
			})
			return rec
		}
	}

	type event struct {
		ev  WorkerEvent
		err error
	}
	events := make(chan event, 2)
	br := bufio.NewReaderSize(proc.Stdout, 64<<10)
	go func() {
		for i := 0; i < 2; i++ {
			_ = proc.Stdout.SetReadDeadline(time.Now().Add(r.Cfg.Decode.PerFileTimeout.D() + 10*time.Second))
			ev, err := ReadEvent(br)
			events <- event{ev: ev, err: err}
			if err != nil {
				return
			}
		}
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- proc.Cmd.Wait() }()

	// --- readiness handshake ---------------------------------------------
	var ready WorkerEvent
	select {
	case e := <-events:
		if e.err != nil {
			rec.Outcome = StateFailed
			rec.Reason = "worker_no_handshake"
			rec.Detail = e.err.Error()
			r.finishKilled(ctx, taskID, rec, ref, proc, waitCh)
			return rec
		}
		ready = e.ev
	case <-time.After(10 * time.Second):
		rec.Outcome = StateFailed
		rec.Reason = "worker_handshake_timeout"
		rec.Detail = "the worker did not report readiness within 10s"
		r.finishKilled(ctx, taskID, rec, ref, proc, waitCh)
		return rec
	case <-ctx.Done():
		rec.Outcome = StateFailed
		rec.Reason = "operator_stop"
		r.finishKilled(ctx, taskID, rec, ref, proc, waitCh)
		return rec
	}
	if ready.Event != EventReady {
		rec.Outcome = StateFailed
		rec.Reason = "worker_protocol_error"
		rec.Detail = "expected a ready event, got " + ready.Event
		r.finishKilled(ctx, taskID, rec, ref, proc, waitCh)
		return rec
	}
	if cm != nil && cm.Active && ready.CgroupKnown && ready.CgroupMaxBytes <= 0 {
		// The worker is in a cgroup with no memory.max. Refusing here is the
		// whole point of the handshake.
		rec.Outcome = StateFailed
		rec.Reason = "worker_limit_not_effective"
		rec.Detail = fmt.Sprintf("the worker reported cgroup %s with no memory.max (%d); not starting the decode",
			ready.CgroupDir, ready.CgroupMaxBytes)
		r.finishKilled(ctx, taskID, rec, ref, proc, waitCh)
		return rec
	}

	if _, err := proc.Stdin.Write([]byte("g")); err != nil {
		rec.Outcome = StateFailed
		rec.Reason = "worker_handshake_write_failed"
		rec.Detail = err.Error()
		r.finishKilled(ctx, taskID, rec, ref, proc, waitCh)
		return rec
	}
	_ = proc.Stdin.Close()

	// --- supervised execution --------------------------------------------
	base := guard.Baseline()
	interval := r.Cfg.Guard.SampleInterval.D()
	if interval <= 0 {
		interval = time.Second
	}
	fileDeadline := time.Now().Add(r.Cfg.Decode.PerFileTimeout.D())

	var (
		killReason string
		killDetail string
		peakCgroup int64
		minAvail   int64
	)
	// sampleOnce reads one snapshot and folds it into every accumulator.
	sampleOnce := func() resource.Decision {
		dec := guard.DuringFile(base, proc.PID)
		rep.Stats.Observe(dec.Snapshot)
		if dec.Snapshot.Cgroup.Peak > peakCgroup {
			peakCgroup = dec.Snapshot.Cgroup.Peak
		}
		if dec.Snapshot.MemErr == nil &&
			(minAvail == 0 || dec.Snapshot.Mem.MemAvailableKB < minAvail) {
			minAvail = dec.Snapshot.Mem.MemAvailableKB
		}
		// Count only readings that saw a worker holding memory. A read that
		// succeeds but returns zero means the sample landed before the worker
		// had allocated or after it had exited, which is not an observation of
		// the decoder and must not be reported as one.
		if dec.Snapshot.WorkerErr == nil && dec.Snapshot.Worker.RSSBytes > 0 {
			rep.WorkerLiveSamples++
			if dec.Snapshot.Worker.RSSBytes > rec.PeakRSSBytes {
				rec.PeakRSSBytes = dec.Snapshot.Worker.RSSBytes
			}
		}
		return dec
	}

	// One sample immediately after the go-ahead. Most images here finish in
	// tens of milliseconds, so a timer-only loop would measure a short file zero
	// times and could never notice a decode that spiked early. The worker's own
	// VmHWM, reported in the result event, fills in the peak this sampling can
	// miss.
	if dec := sampleOnce(); dec.Verdict == resource.Stop {
		killReason, killDetail = string(dec.Reason), dec.Detail
	}

poll:
	for killReason == "" {
		select {
		case <-waitCh:
			// The worker finished on its own; its result event is already
			// queued by the reader goroutine.
			break poll
		case <-time.After(interval):
		}
		dec := sampleOnce()
		if dec.Verdict == resource.Stop {
			killReason, killDetail = string(dec.Reason), dec.Detail
			break poll
		}
		if time.Now().After(fileDeadline) {
			killReason = string(resource.ReasonPerFileTimeout)
			killDetail = fmt.Sprintf("the file exceeded the %s per-file deadline", r.Cfg.Decode.PerFileTimeout.D())
			break poll
		}
	}

	// A final sample after the worker has gone: it captures the cgroup peak the
	// file left behind and the machine's state right after the decode, which is
	// what the next BeforeFile decision will act on.
	sampleOnce()

	rec.CgroupPeak = peakCgroup
	rec.MemAvailMin = minAvail

	if killReason != "" {
		terminated, escalated, terr := resource.TerminateGroup(proc.PGID, r.Cfg.Decode.GracePeriod.D(), 25*time.Millisecond)
		<-waitCh
		rec.Outcome = StateFailed
		rec.Reason = killReason
		rec.Detail = killDetail
		if terr != nil {
			rec.Detail += "; terminate error: " + terr.Error()
		} else if escalated {
			rec.Detail += "; the worker group ignored SIGTERM and was SIGKILLed after the grace period"
		} else if !terminated {
			rec.Detail += "; WARNING: the worker process group did not exit"
		}
		rec.MS = time.Since(start).Milliseconds()
		// A resource kill is deferred, not retried in a loop.
		r.record(ctx, taskID, rec, store.ThumbUpdate{
			ID: ref.ID, State: store.ThumbFailed, Reason: rec.Reason,
			NextAttemptAt: store.UnixNow() + int64(r.Cfg.Decode.RetryBackoff.D().Seconds()),
			At:            store.UnixNow(),
		})
		return rec
	}

	// Read the result event, which the reader goroutine already has queued.
	var result WorkerEvent
	select {
	case e := <-events:
		if e.err != nil {
			rec.Outcome = StateFailed
			rec.Reason = "worker_result_unreadable"
			rec.Detail = e.err.Error()
		} else {
			result = e.ev
		}
	case <-time.After(5 * time.Second):
		rec.Outcome = StateFailed
		rec.Reason = "worker_result_timeout"
		rec.Detail = "the worker exited without reporting a result"
	}

	rec.MS = time.Since(start).Milliseconds()
	if result.Outcome != nil {
		rec.Outcome = result.Outcome.State
		if rec.Reason == "" {
			rec.Reason = result.Outcome.Reason
		}
		if rec.Detail == "" {
			rec.Detail = result.Outcome.Detail
		}
		if result.Outcome.PeakRSSBytes > rec.PeakRSSBytes {
			rec.PeakRSSBytes = result.Outcome.PeakRSSBytes
		}
		rec.ThumbBytes = result.Outcome.Bytes
	}
	if rec.Outcome == "" {
		rec.Outcome = StateFailed
		rec.Reason = "worker_exit_nonzero"
		rec.Detail = "the worker exited without a parsable result; see worker.log"
	}

	var st store.ThumbUpdate
	switch rec.Outcome {
	case StateOK:
		st = store.ThumbUpdate{ID: ref.ID, State: store.ThumbOK, Bytes: rec.ThumbBytes, At: store.UnixNow()}
	case StateSkipped:
		// Skipped is terminal for this configuration. It is not retried, because
		// retrying would mean either ignoring the budget or repeatedly hitting
		// the same wall.
		st = store.ThumbUpdate{ID: ref.ID, State: store.ThumbSkipped, Reason: rec.Reason, At: store.UnixNow()}
	default:
		st = store.ThumbUpdate{ID: ref.ID, State: store.ThumbFailed, Reason: rec.Reason,
			NextAttemptAt: store.UnixNow() + int64(r.Cfg.Decode.RetryBackoff.D().Seconds()), At: store.UnixNow()}
	}
	r.record(ctx, taskID, rec, st)
	return rec
}

// finishKilled terminates a worker that never got going.
func (r *Runner) finishKilled(ctx context.Context, taskID int64, rec *FileRecord, ref store.MediaRef,
	proc *taskctl.Process, waitCh chan error) {
	_, _, _ = resource.TerminateGroup(proc.PGID, r.Cfg.Decode.GracePeriod.D(), 25*time.Millisecond)
	select {
	case <-waitCh:
	case <-time.After(3 * time.Second):
	}
	rec.MS = 0
	r.record(ctx, taskID, rec, store.ThumbUpdate{
		ID: ref.ID, State: store.ThumbFailed, Reason: rec.Reason,
		NextAttemptAt: store.UnixNow() + int64(r.Cfg.Decode.RetryBackoff.D().Seconds()),
		At:            store.UnixNow(),
	})
}

// record writes the index update and the bounded task-log row.
func (r *Runner) record(ctx context.Context, taskID int64, rec *FileRecord, st store.ThumbUpdate) {
	if err := r.Store.SetThumb(ctx, st); err != nil {
		r.Log.Error("could not record a thumbnail result", "media", rec.MediaID, "err", err.Error())
	}
	detail := rec.Detail
	if rec.Reason != "" && detail != "" {
		detail = rec.Reason + ": " + detail
	} else if detail == "" {
		detail = rec.Reason
	}
	if err := r.Store.AddTaskItem(ctx, store.TaskItem{
		TaskID:      taskID,
		MediaID:     &rec.MediaID,
		RelPath:     rec.RelPath,
		Action:      store.TaskThumbs,
		Outcome:     rec.Outcome,
		Reason:      rec.Reason,
		Detail:      detail,
		MS:          rec.MS,
		PeakRSS:     rec.PeakRSSBytes,
		CgroupPeak:  rec.CgroupPeak,
		MemAvailMin: rec.MemAvailMin,
		At:          store.UnixNow(),
	}); err != nil {
		r.Log.Warn("could not append a task item", "err", err.Error())
	}
}

func (r *Runner) estimatedThumbBytes() int64 {
	e := int64(r.Cfg.Decode.ThumbMaxEdge)
	// Rough but generous: a 512 px JPEG at q82 is well under 200 KiB, and 4
	// bytes per output pixel is a safe upper bound for the encoded size.
	return e * e * 4
}

func (r *Runner) formatNames() string {
	return Formats{
		JPEG: r.Cfg.Formats.JPEG,
		PNG:  r.Cfg.Formats.PNG,
		GIF:  r.Cfg.Formats.GIF,
		WebP: r.Cfg.Formats.WebP,
	}.FormatNames()
}

// isPressureReason reports whether a stop reason was caused by machine
// resources. Only these reasons create a persistent trip.
func isPressureReason(reason string) bool {
	switch resource.Reason(reason) {
	case resource.ReasonMemBelowStop,
		resource.ReasonMemBelowWarn,
		resource.ReasonSwapUsed,
		resource.ReasonWorkerRSS,
		resource.ReasonWorkerCgroup,
		resource.ReasonCgroupMaxEvents,
		resource.ReasonCgroupOOMKill,
		resource.ReasonPSIMemory,
		resource.ReasonPSIIO,
		resource.ReasonNoSignal:
		return true
	}
	return false
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	i := -1
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// ResourceReport renders the measurement section of a run.
//
// The three families are printed separately on purpose. Adding "worker RSS" to
// "MemAvailable" would produce a number that means nothing, and the design
// explicitly forbids presenting cache, free memory and process RSS as a single
// budget figure.
func (rep *Report) ResourceReport() string {
	var b strings.Builder
	fmt.Fprintf(&b, "albumctl thumbs report\n")
	fmt.Fprintf(&b, "collection:            %s\n", rep.Collection)
	fmt.Fprintf(&b, "task id:               %d\n", rep.TaskID)
	fmt.Fprintf(&b, "started:               %s\n", rep.Started.Format(time.RFC3339))
	fmt.Fprintf(&b, "ended:                 %s\n", rep.Ended.Format(time.RFC3339))
	fmt.Fprintf(&b, "duration:              %s\n", rep.Ended.Sub(rep.Started).Round(time.Millisecond))
	fmt.Fprintf(&b, "processed:             %d\n", rep.Processed)
	fmt.Fprintf(&b, "skipped:               %d\n", rep.Skipped)
	fmt.Fprintf(&b, "failed:                %d\n", rep.Failed)
	fmt.Fprintf(&b, "thumbnail bytes:       %s\n", humanBytes(rep.BytesWritten))
	fmt.Fprintf(&b, "cache after run:       %s in %d files\n", humanBytes(rep.CacheBytes), rep.CacheFiles)
	fmt.Fprintf(&b, "stop reason:           %s\n", rep.StopReason)
	if rep.StopDetail != "" {
		fmt.Fprintf(&b, "stop detail:           %s\n", rep.StopDetail)
	}
	b.WriteString("\n-- isolation -------------------------------------------------\n")
	fmt.Fprintf(&b, "cgroup isolation:      %v\n", rep.CgroupIsolated)
	fmt.Fprintf(&b, "cgroup reason:         %s\n", rep.CgroupReason)
	fmt.Fprintf(&b, "worker memory max:     %s\n", humanBytes(rep.CgroupLayout.WorkerMemMaxBytes))
	fmt.Fprintf(&b, "worker memory high:    %s\n", humanBytes(rep.CgroupLayout.WorkerMemHighBytes))
	fmt.Fprintf(&b, "capability:            meminfo=%v psi_memory=%v psi_io=%v cgroup_accounting=%v\n",
		rep.Capability.MemInfo, rep.Capability.PSIMemory, rep.Capability.PSIIO, rep.Capability.CgroupAccounting)
	for _, n := range rep.Capability.Notes {
		fmt.Fprintf(&b, "capability note:       %s\n", n)
	}
	b.WriteString("\n-- whole machine --------------------------------------------\n")
	fmt.Fprintf(&b, "MemAvailable before:   %.1f MiB\n", float64(rep.MemAvailableBeforeKB)/1024)
	fmt.Fprintf(&b, "MemAvailable min:      %.1f MiB\n", float64(rep.Stats.MinMemAvailableKB)/1024)
	fmt.Fprintf(&b, "MemAvailable after:    %.1f MiB\n", float64(rep.MemAvailableAfterKB)/1024)
	fmt.Fprintf(&b, "swap used max:         %.1f MiB\n", float64(rep.Stats.MaxSwapUsedKB)/1024)
	fmt.Fprintf(&b, "PSI memory some max:   %.2f%%\n", rep.Stats.MaxPSIMemSomeAvg10)
	fmt.Fprintf(&b, "PSI io some max:       %.2f%%\n", rep.Stats.MaxPSIIOSomeAvg10)
	b.WriteString("\n-- worker process tree --------------------------------------\n")
	if rep.WorkerLiveSamples > 0 {
		fmt.Fprintf(&b, "worker RSS peak:       %s (highest of %d live samples; a fast file can "+
			"finish between samples)\n", humanBytes(rep.Stats.MaxWorkerRSSBytes), rep.WorkerLiveSamples)
	} else {
		fmt.Fprintf(&b, "worker RSS peak:       not sampled: no sample landed while a worker "+
			"was both running and holding memory, so this run has no independent "+
			"observation of decoder RSS. Files that finish inside the %s sample "+
			"interval cannot be caught this way. The per-file VmHWM below is the "+
			"worker's own report about itself.\n", rep.SampleInterval)
	}
	// Reported per file rather than as a mean over all samples, because the
	// supervisor's sample count includes whole-machine snapshots taken between
	// files, and averaging those in would understate the decoder.
	var worstFile int64
	for _, f := range rep.Files {
		if f.PeakRSSBytes > worstFile {
			worstFile = f.PeakRSSBytes
		}
	}
	fmt.Fprintf(&b, "worker RSS max/file:   %s (from the worker's own VmHWM)\n", humanBytes(worstFile))
	b.WriteString("\n-- worker cgroup --------------------------------------------\n")
	if rep.Stats.CgroupAvailable {
		fmt.Fprintf(&b, "cgroup current peak:   %s\n", humanBytes(rep.Stats.MaxCgroupCurrent))
		fmt.Fprintf(&b, "cgroup memory.peak:    %s\n", humanBytes(rep.Stats.MaxCgroupPeak))
	} else {
		fmt.Fprintf(&b, "cgroup accounting:     unavailable on this host\n")
	}
	fmt.Fprintf(&b, "samples taken:         %d\n", rep.Stats.Samples)
	if len(rep.Files) > 0 {
		b.WriteString("\n-- per file (bounded sample) --------------------------------\n")
		for _, f := range rep.Files {
			fmt.Fprintf(&b, "%-60s %-8s %6dms rss=%-9s cgroup=%-9s memavail_min=%s\n",
				truncate(f.RelPath, 60), f.Outcome, f.MS,
				humanBytes(f.PeakRSSBytes), humanBytes(f.CgroupPeak), humanBytes(f.MemAvailMin<<10))
		}
	}
	if len(rep.Notes) > 0 {
		b.WriteString("\n-- notes ----------------------------------------------------\n")
		for _, n := range rep.Notes {
			fmt.Fprintf(&b, "%s\n", n)
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-(n-3):]
}
