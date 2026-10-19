package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/cachepath"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/exif"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
	"github.com/YoisakiKnd/nasctl/internal/logx"
	"github.com/YoisakiKnd/nasctl/internal/probe"
	"github.com/YoisakiKnd/nasctl/internal/source"
	"github.com/YoisakiKnd/nasctl/internal/store"
	"github.com/YoisakiKnd/nasctl/internal/taskctl"
	"github.com/YoisakiKnd/nasctl/internal/vips"
)

// BatchMoreMarker ends the output of a scan or thumbnail run that stopped at
// its limit with work left, so a supervisor running batches knows to go on.
const BatchMoreMarker = "nasctl-batch: more"

// Runner performs a bounded scan.
type Runner struct {
	Cfg   *config.Config
	Store *store.Store
	Log   *logx.Logger
}

// Options bound one scan.
type Options struct {
	CollectionCode string
	// Limit bounds how many files are indexed (new, changed or deferred) and how
	// many pending files are probed, each phase independently. Files the index
	// already holds unchanged do not count, so repeated runs with the same limit
	// work through a large library instead of stopping at its first files.
	Limit  int
	DryRun bool
	// MaxRuntime stops the run after this elapsed time. Zero means no limit.
	MaxRuntime time.Duration
	// OnProgress, when set, is called with a short line per processed file.
	OnProgress func(string)
}

// Result summarises a scan.
type Result struct {
	Collection     string
	DryRun         bool
	Walk           WalkStats
	Discovered     int64
	Inserted       int64
	Unchanged      int64
	Changed        int64
	Deferred       int64
	Probed         int64
	ProbeSkipped   int64
	ProbeFailed    int64
	ProbeAttempted int64
	MarkedMissing  int64
	Complete       bool
	StopReason     string
	// More reports that a limit, not the end of the work, stopped this run.
	More        bool
	SourceCheck source.Check
	Sample      []string
	Notes       []string
	Duration    time.Duration
}

// Run executes the scan.
func (r *Runner) Run(ctx context.Context, opts Options) (*Result, error) {
	if err := r.Store.WaitForWriteIdle(ctx, 10*time.Second); err != nil {
		return nil, err
	}
	col, ok := r.Cfg.Collection(opts.CollectionCode)
	if !ok {
		return nil, fmt.Errorf("unknown collection %q", opts.CollectionCode)
	}
	if !col.Enabled {
		return nil, fmt.Errorf("collection %q is disabled in the configuration", col.Code)
	}

	lock, err := taskctl.Acquire(r.Cfg.Paths.LockDir, "scan")
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()

	// --- source verification before a single file is touched -------------
	colID, err := r.Store.UpsertCollection(ctx, col.Code, col.Name, col.Root, col.DataMount,
		col.Enabled, col.FollowSymlinks)
	if err != nil {
		return nil, err
	}
	pinned := col.ExpectFSID
	if pinned == "" {
		pinned = r.storedFSID(ctx, colID)
	}
	sc := source.Verify(col, r.Cfg.Paths.StateDir, pinned)
	res := &Result{Collection: col.Code, DryRun: opts.DryRun, SourceCheck: sc}
	if !sc.OK {
		if !opts.DryRun {
			_ = r.Store.RecordMountCheck(ctx, colID, false, sc.Fatal, sc.FSID, sc.RootDevice)
		}
		return res, errors.New(sc.Fatal)
	}
	if !opts.DryRun {
		if err := r.Store.RecordMountCheck(ctx, colID, true, strings.Join(sc.Notes, "; "), sc.FSID, sc.RootDevice); err != nil {
			return res, err
		}
	}

	if !opts.DryRun {
		if n, err := r.requeueForPolicy(ctx, col.Code, colID); err != nil {
			res.Notes = append(res.Notes, err.Error())
		} else if n > 0 {
			res.Notes = append(res.Notes, fmt.Sprintf("%d files skipped under an earlier decode policy were queued again", n))
		}
	}

	deadline := time.Now().Add(opts.MaxRuntime)
	expired := func() bool {
		return opts.MaxRuntime > 0 && time.Now().After(deadline)
	}

	started := time.Now()

	// The run's identity, stamped on every row this run sees so that
	// MarkMissing can ask "which rows did I not see?" exactly. A wall-clock
	// start second cannot answer that: run a scan, delete a file, run the scan
	// again within the same second, and the deleted file would still look seen.
	// last_seen is kept as a human-readable timestamp; this is what decides.
	runToken, err := r.Store.NextScanToken(ctx)
	if err != nil {
		return res, err
	}

	include := extensionFilter(r.Cfg, col)
	oneFS := col.DataMount != ""

	// --- phase 1: discovery ---------------------------------------------
	//
	// Complete starts true and is cleared by anything that ends the walk early.
	// It is the flag that authorises marking a row absent, so the safe default
	// has to be "not complete": a walk that was interrupted before it reached a
	// branch must never look like a tree in which files disappeared. The two
	// ways the walk itself can stop short - a limit and the deadline - clear it
	// below, and the walker's own cut counters (depth, other filesystem,
	// unreadable directories) are checked before it is relied on.
	res.Complete = true
	var discovered int64
	walk, err := WalkStream(col.Root, WalkOptions{
		MaxDepth:       r.Cfg.Decode.MaxScanDepth,
		Batch:          256,
		FollowSymlinks: col.FollowSymlinks,
		Exclude:        col.Exclude,
		OneFileSystem:  oneFS,
		RootDevice:     sc.RootDevice,
	}, include, func(e Entry) error {
		if ctx.Err() != nil {
			return StopWalk()
		}
		counted := discovered
		if !opts.DryRun {
			counted = res.Inserted + res.Changed + res.Deferred
		}
		if opts.Limit > 0 && counted >= int64(opts.Limit) {
			res.Complete = false
			res.StopReason = "limit_reached"
			res.More = true
			return StopWalk()
		}
		if expired() {
			res.Complete = false
			res.StopReason = "max_runtime_reached"
			return StopWalk()
		}
		discovered++
		if opts.DryRun {
			if len(res.Sample) < 50 {
				res.Sample = append(res.Sample, fmt.Sprintf("would index %s (%d bytes, mtime_ns=%d)",
					e.RelPath, e.Size, e.MtimeNS))
			}
			return nil
		}
		if err := r.record(ctx, colID, col.Code, runToken, e, res); err != nil {
			return err
		}
		if opts.OnProgress != nil && discovered%200 == 0 {
			opts.OnProgress(fmt.Sprintf("discovered %d files", discovered))
		}
		return nil
	})
	res.Walk = walk
	res.Discovered = discovered
	if ctx.Err() != nil {
		res.Complete = false
		res.StopReason = "cancelled"
	}
	res.Duration = time.Since(started)
	if err != nil && !IsStop(err) {
		return res, err
	}

	// --- phase 2: metadata probes ---------------------------------------
	if !opts.DryRun {
		pr := r.probePending(ctx, col, colID, opts, expired)
		res.Probed, res.ProbeSkipped, res.ProbeFailed, res.ProbeAttempted = pr.OK, pr.Skipped, pr.Failed, pr.Count
		if opts.Limit > 0 && pr.Count >= int64(opts.Limit) {
			res.More = true
		}
		if pr.Err != nil {
			res.Notes = append(res.Notes, pr.Err.Error())
		}
	}

	// --- phase 3: bookkeeping -------------------------------------------
	if !opts.DryRun {
		// A row may only be flagged absent if this pass really visited
		// everywhere the row could have been. "Somewhere" is the whole of the
		// collection's in-scope tree, so every way of not visiting all of it has
		// to be excluded, not just the two obvious ones:
		//
		//   - a limit or a deadline stopped the walk (res.Complete),
		//   - a directory could not be read,
		//   - the depth limit cut a branch,
		//   - a directory was skipped for being on another filesystem.
		//
		// The last two are the ones that matter most in practice. A collection
		// on a RAID0 array with a deep tree, or one that spans a second
		// filesystem, would otherwise have every file under the unseen branch
		// flagged as not present, which reads exactly like data loss when the
		// files are sitting right there. Rows are never deleted either way, but
		// a false "missing" is still a lie in the interface.
		switch {
		case walk.ReadErrors > 0:
			res.Notes = append(res.Notes,
				fmt.Sprintf("%d directories could not be read; no file was marked missing, because "+
					"absence proves nothing when part of the tree was not seen", walk.ReadErrors))
		case walk.DepthCut > 0:
			res.Notes = append(res.Notes,
				fmt.Sprintf("%d directories were past decode.max_scan_depth; no file was marked "+
					"missing, because nothing below them was visited", walk.DepthCut))
		case walk.OtherFS > 0:
			res.Notes = append(res.Notes,
				fmt.Sprintf("%d directories were on another filesystem and were not descended; no "+
					"file was marked missing", walk.OtherFS))
		case !res.Complete:
			res.Notes = append(res.Notes,
				"the walk stopped before the whole collection was covered, so no file was marked "+
					"missing")
		default:
			n, merr := r.Store.MarkMissing(ctx, colID, runToken)
			if merr != nil {
				res.Notes = append(res.Notes, merr.Error())
			}
			res.MarkedMissing = n
		}
		if err := r.Store.RefreshMediaCount(ctx, colID); err != nil {
			res.Notes = append(res.Notes, err.Error())
		}
		if err := r.Store.SetLastScan(ctx, colID, store.UnixNow()); err != nil {
			res.Notes = append(res.Notes, err.Error())
		}
	}
	res.Duration = time.Since(started)
	res.Notes = append(res.Notes, walk.Notes...)
	return res, nil
}

func (r *Runner) storedFSID(ctx context.Context, colID int64) string {
	cols, err := r.Store.Collections(ctx)
	if err != nil {
		return ""
	}
	for _, c := range cols {
		if c.ID == colID {
			return c.ExpectFSID
		}
	}
	return ""
}

// index records one discovered file.
func (r *Runner) index(ctx context.Context, colID int64, code string, runToken int64, e Entry) (store.UpsertResult, error) {
	// Re-stat immediately before writing. A file that is being copied in, or a
	// video that is still being recorded, would otherwise be indexed with a
	// size that never matches again and a thumbnail generated from a partial
	// frame. A changed file is deferred, not published.
	fi, err := os.Lstat(e.AbsPath)
	if err != nil {
		return store.UpsertResult{}, err
	}
	if !fi.Mode().IsRegular() {
		return store.UpsertResult{}, fmt.Errorf("not a regular file")
	}
	if fi.Size() != e.Size || fi.ModTime().UnixNano() != e.MtimeNS {
		return store.UpsertResult{}, fsutil.ErrUnstable
	}
	kind := probe.ExtensionIndex()[e.Ext()]
	return r.Store.UpsertMedia(ctx, store.MediaScan{
		CollectionID: colID,
		RelPath:      e.RelPath,
		RelDir:       e.RelDir,
		BaseName:     e.Name,
		Ext:          e.Ext(),
		Size:         fi.Size(),
		MtimeNS:      fi.ModTime().UnixNano(),
		Kind:         string(kind),
		MIME:         probe.MIMEForExt(e.Ext()),
		At:           store.UnixNow(),
		RunToken:     runToken,
	})
}

// record writes one walked entry into the index and accounts for the outcome.
//
// The deferral branch is not just bookkeeping. A file the walk saw but could not
// re-index — it is still being copied in, it stopped being a regular file, or
// reading it failed — is stamped as *seen* for this run. Without that, the very
// walk that declined to touch the file would go on to flag it absent, hiding a
// picture that is merely being written and taking its original out of service
// until the next scan. "Missing" has to mean "absent from the directory", not
// "not refreshed"; the row's size, mtime and thumbnail state stay untouched, so
// nothing partial is published either way.
func (r *Runner) record(ctx context.Context, colID int64, code string, runToken int64, e Entry, res *Result) error {
	outcome, derr := r.index(ctx, colID, code, runToken, e)
	if derr != nil {
		if _, terr := r.Store.TouchSeen(ctx, colID, e.RelPath, runToken); terr != nil {
			res.Notes = append(res.Notes,
				fmt.Sprintf("%s: could not record it as seen: %v", e.RelPath, terr))
		}
		res.Notes = append(res.Notes, fmt.Sprintf("%s: %v", e.RelPath, derr))
		res.Deferred++
		return nil
	}
	switch {
	case outcome.Created:
		res.Inserted++
	case outcome.Changed:
		res.Changed++
	default:
		res.Unchanged++
	}
	if outcome.Changed && outcome.PreviousThumbState == store.ThumbOK {
		// The content changed, so the cached thumbnail is now a picture of
		// something else. The index has already been invalidated; remove the
		// file, never the original.
		r.deleteThumb(colID, code, e.RelPath)
	}
	return nil
}

func (e Entry) Ext() string {
	i := strings.LastIndexByte(e.Name, '.')
	if i < 0 || i == len(e.Name)-1 {
		return ""
	}
	return strings.ToLower(e.Name[i+1:])
}

func (r *Runner) deleteThumb(colID int64, code, relPath string) {
	key := cachepath.Key(code, relPath)
	path, err := cachepath.Path(r.Cfg.Paths.CacheDir, key, r.Cfg.Decode.ThumbMaxEdge, r.Cfg.Cache.ShardDepth)
	if err != nil {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		r.Log.Warn("could not remove a stale thumbnail", "path", path, "err", err.Error())
	}
}

func extensionFilter(cfg *config.Config, col config.Collection) func(string) bool {
	allowed := map[string]bool{}
	for _, k := range col.EnabledKinds {
		switch k {
		case "image":
			for _, e := range probe.ImageExts() {
				allowed[e] = true
			}
		case "video":
			for _, e := range probe.VideoExts() {
				allowed[e] = true
			}
		case "other":
			for _, e := range probe.OtherMediaExts() {
				allowed[e] = true
			}
		}
	}
	return func(ext string) bool { return allowed[ext] }
}

// probeTally is the outcome of the metadata phase.
type probeTally struct {
	OK      int64
	Skipped int64
	Failed  int64
	Count   int64
	Err     error
}

// budget is the decode budget probes are checked against.
func (r *Runner) budget() probe.Budget {
	return probe.Budget{
		MaxPixels:      r.Cfg.Decode.MaxPixels,
		MaxFileBytes:   r.Cfg.Decode.MaxFileBytes,
		MaxDecodeBytes: r.Cfg.MaxDecodeBytes(),
		MaxFrames:      r.Cfg.Decode.MaxFrames,
		ThumbMaxEdge:   r.Cfg.Decode.ThumbMaxEdge,
		Vips:           vips.Find(r.Cfg.Formats.UseVips, r.Cfg.Formats.VipsPath) != "",
		VipsJPEG:       r.Cfg.Formats.VipsForJPEG,
	}
}

// requeueForPolicy queues files skipped under a different decode policy, once
// per policy change: a looser budget, scaled JPEG decoding or a newly
// installed libvips can make them thumbnailable. Files that stay over the
// budget are not probed again on every scan.
func (r *Runner) requeueForPolicy(ctx context.Context, code string, colID int64) (int64, error) {
	b := r.budget()
	// 3: PNG streams and progressive JPEG keeps only the coefficients it
	// reads, so files once over budget or stopped at the worker ceiling fit.
	policy := fmt.Sprintf("3|%d|%d|%d|%d|%t|%t", b.MaxPixels, b.MaxFileBytes, b.MaxDecodeBytes, b.ThumbMaxEdge, b.Vips, b.Vips && b.VipsJPEG)
	key := "decode_policy:" + code
	if current, ok, err := r.Store.GetState(ctx, key); err != nil || ok && current == policy {
		return 0, err
	}
	var formats []string
	if b.Vips {
		for ext := range vips.ExtraExtensions {
			formats = append(formats, ext)
		}
		sort.Strings(formats)
	}
	n, err := r.Store.RequeueSkipped(ctx, colID, formats)
	if err != nil {
		return n, err
	}
	return n, r.Store.SetState(ctx, key, policy)
}

// probePending reads headers for files awaiting metadata.
//
// One row is fetched at a time and each iteration re-reads the pending set, so a
// concurrent writer (or another task) can never make this loop hold a stale
// list. The loop is bounded by --limit and by the run's deadline.
func (r *Runner) probePending(ctx context.Context, col config.Collection, colID int64, opts Options, expired func() bool) probeTally {
	var t probeTally
	lim := exif.Limits{
		MaxScanBytes:   r.Cfg.Decode.ProbeMaxMetaLen + 256*1024,
		MaxMetaBytes:   r.Cfg.Decode.ProbeMaxMetaLen,
		MaxIFDEntries:  512,
		MaxIFDs:        8,
		MaxStringBytes: 512,
		MaxKeys:        24,
	}
	budget := r.budget()
	limit := opts.Limit
	if limit <= 0 {
		limit = 1 << 30
	}
	for t.Count < int64(limit) {
		if ctx.Err() != nil || expired() {
			return t
		}
		refs, qerr := r.Store.NextProbe(ctx, colID, r.Cfg.Decode.MaxAttempts, store.UnixNow(), 1)
		if qerr != nil {
			t.Err = qerr
			return t
		}
		if len(refs) == 0 {
			return t
		}
		ref := refs[0]
		t.Count++
		u, perr := r.probeOne(col, ref, lim, budget)
		u.At = store.UnixNow()
		switch {
		case perr != nil:
			// A file that cannot even be opened is recorded as a failure with a
			// reason and a bounded retry budget. It never blocks the rest of
			// the library and it is never marked ok.
			t.Failed++
			u = store.ProbeUpdate{
				ID:            ref.ID,
				Kind:          ref.Kind,
				State:         store.ProbeFailed,
				Reason:        "probe_error",
				Note:          perr.Error(),
				ThumbState:    store.ThumbSkipped,
				ThumbReason:   "probe_failed",
				NextAttemptAt: store.UnixNow() + int64(r.Cfg.Decode.RetryBackoff.D().Seconds()),
				At:            store.UnixNow(),
			}
		case u.State == store.ProbeSkipped:
			t.Skipped++
		default:
			t.OK++
		}
		if serr := r.Store.SetProbe(ctx, u); serr != nil {
			t.Err = serr
			return t
		}
		if opts.OnProgress != nil && t.Count%100 == 0 {
			opts.OnProgress(fmt.Sprintf("probed %d files", t.Count))
		}
	}
	return t
}

// probeOne reads the header of one file and turns it into a store update.
func (r *Runner) probeOne(col config.Collection, ref store.MediaRef, lim exif.Limits, budget probe.Budget) (store.ProbeUpdate, error) {
	abs, err := safeAbs(col, ref)
	if err != nil {
		return store.ProbeUpdate{}, err
	}
	pr, err := probe.Probe(abs, ref.Ext, budget, probe.Limits{
		ScanBytes:       lim.MaxScanBytes,
		MaxFileBytes:    budget.MaxFileBytes,
		MaxPixels:       budget.MaxPixels,
		MaxDecodeBytes:  budget.MaxDecodeBytes,
		MaxFrames:       budget.MaxFrames,
		ProbeMaxMetaLen: lim.MaxMetaBytes,
		ThumbMaxEdge:    budget.ThumbMaxEdge,
		Exif:            lim,
	})
	if err != nil {
		return store.ProbeUpdate{ID: ref.ID}, err
	}

	fi, err := os.Lstat(abs)
	if err != nil {
		return store.ProbeUpdate{ID: ref.ID}, err
	}
	capture := ResolveCapture(pr, ref.BaseName, fi.ModTime(), r.Cfg.Time.FilenameHeuristic)

	u := store.ProbeUpdate{
		ID:          ref.ID,
		Kind:        string(pr.Kind),
		MIME:        pr.MIME,
		Format:      pr.Format,
		Width:       pr.Width,
		Height:      pr.Height,
		Pixels:      pr.Pixels,
		Animated:    pr.Animated,
		Frames:      pr.Frames,
		DurationMS:  pr.DurationMS,
		TakenAt:     capture.Unix,
		TakenSrc:    capture.Src,
		TakenTZ:     capture.TZ,
		TZOffsetMin: capture.OffsetMin,
		MetaExtra:   encodeExtra(pr, capture),
		State:       store.ProbeOK,
		ThumbState:  store.ThumbPending,
	}
	if pr.Tags != nil {
		u.Make = pr.Tags.Make
		u.Model = pr.Tags.Model
		u.Lens = pr.Tags.LensModel
		u.Orientation = pr.Tags.Orientation
		if pr.Tags.GPS != nil {
			lat, lon := pr.Tags.GPS.Lat, pr.Tags.GPS.Lon
			u.Latitude, u.Longitude = &lat, &lon
			if pr.Tags.GPS.AltitudeM != nil {
				u.AltitudeM = pr.Tags.GPS.AltitudeM
			}
		}
	}

	switch {
	case pr.UnsupportedReason != "":
		u.State = store.ProbeSkipped
		u.Reason = pr.UnsupportedReason
		u.ThumbState = store.ThumbSkipped
		u.ThumbReason = pr.UnsupportedReason
		u.Note = "recognised as media but this build does not decode it; " +
			"the original file is still listed and can be opened"
	case pr.SkippedReason != "":
		u.State = store.ProbeSkipped
		u.Reason = pr.SkippedReason
		u.ThumbState = store.ThumbSkipped
		u.ThumbReason = pr.SkippedReason
		u.Note = "skipped before decoding because it exceeds a configured budget: " + pr.SkippedReason
	}
	if len(pr.Notes) > 0 {
		if u.Note != "" {
			u.Note += "; "
		}
		u.Note += strings.Join(dedupe(pr.Notes), "; ")
	}
	if len(u.Note) > 900 {
		u.Note = u.Note[:900]
	}
	return u, nil
}

// extraEntry is the bounded metadata payload shown in the detail panel.
type extraEntry struct {
	Capture     string   `json:"capture_note,omitempty"`
	Container   string   `json:"container_brand,omitempty"`
	ContainerTZ string   `json:"container_time,omitempty"`
	Tags        []kvPair `json:"tags,omitempty"`
	ProbeNotes  []string `json:"probe_notes,omitempty"`
}

type kvPair struct {
	K string `json:"k"`
	V string `json:"v"`
}

// maxExtraBytes caps the stored metadata blob so that a file with a fabricated
// metadata payload cannot bloat the index.
const maxExtraBytes = 4096

func encodeExtra(pr *probe.Result, capture CaptureResolution) string {
	e := extraEntry{
		Capture:     capture.Note,
		Container:   pr.ContainerBrand,
		ContainerTZ: pr.ContainerTime,
		ProbeNotes:  dedupe(pr.Notes),
	}
	if pr.Tags != nil {
		for _, kv := range pr.Tags.Extra {
			e.Tags = append(e.Tags, kvPair{K: kv.Key, V: kv.Value})
		}
	}
	b, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	if len(b) > maxExtraBytes {
		e.Tags = e.Tags[:min(len(e.Tags), 8)]
		e.ProbeNotes = e.ProbeNotes[:min(len(e.ProbeNotes), 4)]
		b, err = json.Marshal(e)
		if err != nil {
			return ""
		}
		if len(b) > maxExtraBytes {
			return ""
		}
	}
	return string(b)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= 8 {
			break
		}
	}
	sort.Strings(out)
	return out
}

// safeAbs resolves a stored relative path against its collection root and
// proves the result is inside it.
func safeAbs(col config.Collection, ref store.MediaRef) (string, error) {
	joined, err := fsutil.SafeRelJoin(col.Root, ref.RelPath)
	if err != nil {
		return "", err
	}
	real, err := fsutil.ResolveWithin(col.Root, joined)
	if err != nil {
		return "", err
	}
	return real, nil
}

// Describe renders a scan result for a terminal.
func (r *Result) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "collection:      %s\n", r.Collection)
	fmt.Fprintf(&b, "dry run:         %v\n", r.DryRun)
	fmt.Fprintf(&b, "complete walk:   %v (%s)\n", r.Complete, r.StopReason)
	fmt.Fprintf(&b, "directories:     %d\n", r.Walk.Dirs)
	fmt.Fprintf(&b, "files seen:      %d\n", r.Walk.FilesSeen)
	fmt.Fprintf(&b, "media candidates:%d\n", r.Walk.FilesKept)
	fmt.Fprintf(&b, "new:             %d\n", r.Inserted)
	fmt.Fprintf(&b, "changed:         %d\n", r.Changed)
	fmt.Fprintf(&b, "unchanged:       %d\n", r.Unchanged)
	fmt.Fprintf(&b, "deferred:        %d\n", r.Deferred)
	fmt.Fprintf(&b, "probed ok:       %d\n", r.Probed)
	fmt.Fprintf(&b, "probe skipped:   %d\n", r.ProbeSkipped)
	fmt.Fprintf(&b, "probe failed:    %d\n", r.ProbeFailed)
	fmt.Fprintf(&b, "marked missing:  %d\n", r.MarkedMissing)
	fmt.Fprintf(&b, "symlinks seen:   %d\n", r.Walk.Symlinks)
	fmt.Fprintf(&b, "excluded:        %d\n", r.Walk.Excluded)
	fmt.Fprintf(&b, "hidden skipped:  %d\n", r.Walk.Hidden)
	fmt.Fprintf(&b, "other filesystem:%d\n", r.Walk.OtherFS)
	fmt.Fprintf(&b, "read errors:     %d\n", r.Walk.ReadErrors)
	fmt.Fprintf(&b, "duration:        %s\n", r.Duration.Round(time.Millisecond))
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	for _, s := range r.Sample {
		fmt.Fprintf(&b, "plan: %s\n", s)
	}
	return b.String()
}

// HumanReport renders a scan result for a terminal.
//
// The numbers are the honest ones: what was discovered, what was written, what
// was deferred, and — most importantly — whether the walk finished. A partial
// walk never marks anything missing, and saying so plainly is part of the
// contract.
func (res *Result) HumanReport() string {
	var b strings.Builder
	fmt.Fprintf(&b, "collection:   %s\n", res.Collection)
	if res.DryRun {
		fmt.Fprintf(&b, "mode:         dry run, nothing was written\n")
	}
	fmt.Fprintf(&b, "duration:     %s\n", res.Duration.Round(time.Millisecond))
	fmt.Fprintf(&b, "source:       %s\n", strings.TrimSpace(res.SourceCheck.Describe()))
	b.WriteString("\n-- filesystem walk ------------------------------------------\n")
	fmt.Fprintf(&b, "directories:  %d visited, %d excluded by name, %d past the depth limit, %d on other filesystems\n",
		res.Walk.Dirs, res.Walk.Excluded, res.Walk.DepthCut, res.Walk.OtherFS)
	// Every file the walk saw is accounted for below, and the parts add up to
	// the whole. A bare "seen N, kept M" leaves an operator unable to tell
	// whether the difference is sidecars and text files or two thousand
	// photographs that a bug dropped.
	fmt.Fprintf(&b, "files seen:   %d (%d kept, %d not a media extension, %d excluded by name)\n",
		res.Walk.FilesSeen, res.Walk.FilesKept, res.Walk.NotMedia, res.Walk.FilesByName)
	fmt.Fprintf(&b, "              %d symlink(s), %d hidden name(s) skipped, %d unreadable directorie(s)\n",
		res.Walk.Symlinks, res.Walk.Hidden, res.Walk.ReadErrors)
	if res.Walk.Hidden > 0 {
		fmt.Fprintf(&b, "              %d dot-prefixed name(s) were skipped; macOS writes an\n"+
			"              AppleDouble sidecar named \"._photo.jpg\" beside each photo on a\n"+
			"              network share, and those are not pictures\n", res.Walk.Hidden)
	}
	fmt.Fprintf(&b, "entries:      %d indexed, %d unchanged, %d changed, %d deferred\n",
		res.Inserted, res.Unchanged, res.Changed, res.Deferred)
	b.WriteString("\n-- metadata --------------------------------------------------\n")
	fmt.Fprintf(&b, "probed:       %d of %d attempted\n", res.Probed, res.ProbeAttempted)
	fmt.Fprintf(&b, "skipped:      %d (over budget or unsupported)\n", res.ProbeSkipped)
	fmt.Fprintf(&b, "failed:       %d\n", res.ProbeFailed)
	if res.Complete {
		fmt.Fprintf(&b, "marked gone:  %d (a complete walk can do this)\n", res.MarkedMissing)
	} else {
		fmt.Fprintf(&b, "marked gone:  none — the walk did not complete, so absence proves nothing\n")
	}
	fmt.Fprintf(&b, "stop reason:  %s\n", orDash(res.StopReason))
	if len(res.Sample) > 0 {
		b.WriteString("\n-- sample ---------------------------------------------------\n")
		for _, s := range res.Sample {
			fmt.Fprintf(&b, "%s\n", s)
		}
	}
	if len(res.Notes) > 0 {
		b.WriteString("\n-- notes ----------------------------------------------------\n")
		for _, n := range res.Notes {
			fmt.Fprintf(&b, "%s\n", n)
		}
	}
	b.WriteString("\nNothing was moved, renamed, deleted or written back to. The media tree was only read.\n")
	return b.String()
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}
