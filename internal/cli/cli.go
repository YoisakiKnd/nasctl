// Package cli implements the albumctl command line.
//
// Every verb is deliberately small, bounded and reversible. There is no verb
// that deletes an original, none that renames one, none that starts an
// unbounded job, and none that can be triggered by the web interface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/buildinfo"
	"github.com/YoisakiKnd/nasctl/internal/cachepath"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
	"github.com/YoisakiKnd/nasctl/internal/logx"
	"github.com/YoisakiKnd/nasctl/internal/resource"
	"github.com/YoisakiKnd/nasctl/internal/scan"
	"github.com/YoisakiKnd/nasctl/internal/server"
	"github.com/YoisakiKnd/nasctl/internal/source"
	"github.com/YoisakiKnd/nasctl/internal/store"
	"github.com/YoisakiKnd/nasctl/internal/taskctl"
	"github.com/YoisakiKnd/nasctl/internal/thumbs"
	"github.com/YoisakiKnd/nasctl/internal/webstate"
)

// Usage is printed for `albumctl help`.
const Usage = `albumctl — a small, careful photo index for a small machine.

  albumctl status                        what is running, what resources remain,
                                         and how much work is waiting
  albumctl scan    --collection CODE [--limit N] [--dry-run]
                                         index a collection, with an explicit cap
  albumctl thumbs  --collection CODE [--limit N] --max-runtime DURATION
                                         generate thumbnails, one file at a time
  albumctl pause   [--reason TEXT]       stop heavy tasks, durably
  albumctl resume  [--force]             clear a pause or a pressure trip
  albumctl doctor                        read-only checks of config, mounts,
                                         permissions, space and resource limits
  albumctl cache-report                  what the thumbnail cache holds
  albumctl cache-gc --max-age D [--dry-run]
                                         remove stale thumbnails from albumctl's
                                         own cache directory only
  albumctl serve                         run the read-only web service
  albumctl version                       build information

Browsing never starts work. There is no command that scans or generates
anything without an explicit --limit and, for thumbs, --max-runtime.

Exit codes: 0 did the bounded work asked for; 1 the run failed; 2 the command
line was refused (for example an unbounded thumbs run); 3 stopped early and
cleanly (paused, a recorded pressure trip, the backup gate, a lost mount or a
held lock); 4 stopped by a resource threshold or a worker failure during the
run. A non-zero code from a bounded task means no more was attempted, never
that the machine was pushed harder.
`

// Env holds the seams the CLI needs. Keeping them injectable makes the whole
// command line testable without touching the real /proc or the real filesystem.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	Getenv func(string) string
	// ExePath is the binary to re-exec for worker processes.
	ExePath string
	// ProcRoot is normally "/proc".
	ProcRoot string
}

// Run executes one command line and returns a process exit code.
func Run(args []string, env Env) int {
	if env.Stdout == nil {
		env.Stdout = os.Stdout
	}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}
	if env.Stdin == nil {
		env.Stdin = os.Stdin
	}
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.ProcRoot == "" {
		env.ProcRoot = "/proc"
	}

	// --config may appear before or after the verb, so it is pulled out of the
	// whole command line first.
	cfgPath, rest := extractConfigPath(args, env)
	if len(rest) == 0 {
		fmt.Fprint(env.Stderr, Usage)
		return 2
	}

	switch rest[0] {
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, Usage)
		return 0
	case "version", "-v", "--version":
		fmt.Fprintln(env.Stdout, buildinfo.String())
		return 0
	}

	cmd, rest := rest[0], rest[1:]

	// The worker subcommand is internal and deliberately undocumented in the
	// help text: it is spawned by the supervisor, never by a person.
	if cmd == "exec-worker" {
		return runWorker(rest, env)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl: %v\n", err)
		return 1
	}

	ctx, cancel := signalContext()
	defer cancel()

	switch cmd {
	case "status":
		return cmdStatus(ctx, cfg, env, rest)
	case "scan":
		return cmdScan(ctx, cfg, env, rest)
	case "thumbs":
		return cmdThumbs(ctx, cfg, env, rest)
	case "pause":
		return cmdPause(ctx, cfg, env, rest)
	case "resume":
		return cmdResume(ctx, cfg, env, rest)
	case "doctor":
		return cmdDoctor(ctx, cfg, env, rest)
	case "cache-report", "cache":
		return cmdCacheReport(ctx, cfg, env)
	case "cache-gc":
		return cmdCacheGC(ctx, cfg, env, rest)
	case "serve", "server":
		return cmdServe(ctx, cfg, env)
	case "collections":
		return cmdCollections(ctx, cfg, env)
	default:
		fmt.Fprintf(env.Stderr, "albumctl: unknown command %q\n\n%s", cmd, Usage)
		return 2
	}
}

// signalContext cancels on SIGINT/SIGTERM.
//
// Cancellation is how every long verb stops: the loop notices at the next file
// boundary, finishes the file in flight, releases its lock and records the task
// as stopped. It never leaves a worker group running.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		select {
		case <-ch:
			fmt.Fprintln(os.Stderr, "\nalbumctl: stop requested; finishing the file in flight")
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func extractConfigPath(args []string, env Env) (string, []string) {
	out := make([]string, 0, len(args))
	path := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" || a == "-config":
			if i+1 < len(args) {
				path = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--config="):
			path = strings.TrimPrefix(a, "--config=")
		default:
			out = append(out, a)
		}
	}
	if path == "" {
		path = env.Getenv("ALBUMCTL_CONFIG")
	}
	if path == "" {
		for _, c := range []string{"/etc/albumctl/config.toml", "configs/albumctl.toml", "albumctl.toml"} {
			if _, err := os.Stat(c); err == nil {
				path = c
				break
			}
		}
	}
	return path, out
}

// ---- helpers ------------------------------------------------------------

func openStore(cfg *config.Config, readOnly bool) (*store.Store, error) {
	if err := fsutil.EnsureDir(cfg.Paths.StateDir); err != nil {
		return nil, err
	}
	return store.Open(cfg.Paths.IndexFile, store.Options{ReadOnly: readOnly})
}

func openLog(cfg *config.Config, quiet bool) *logx.Logger {
	if quiet {
		return logx.Discard()
	}
	l, err := logx.New(logx.Options{File: cfg.Paths.LogFile, Level: "info"})
	if err != nil {
		return logx.Discard()
	}
	return l
}

func newFlagSet(name string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	return fs
}

// ---- status -------------------------------------------------------------

func cmdStatus(ctx context.Context, cfg *config.Config, env Env, args []string) int {
	fs := newFlagSet("status", env.Stderr)
	asJSON := fs.Bool("json", false, "emit machine readable output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(env.Stderr, "albumctl status: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *asJSON {
		return cmdStatusJSON(ctx, cfg, env)
	}
	st, err := openStore(cfg, true)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl: %v\n", err)
		return 1
	}
	defer st.Close()

	w := env.Stdout
	fmt.Fprintf(w, "%s\n", buildinfo.String())
	fmt.Fprintf(w, "config:      %s\n", orNone(cfg.Source))

	// Everything below is a bounded query against the index plus a couple of
	// /proc reads. This command returns promptly even while a worker is
	// decoding, because it never talks to the worker at all.
	ps, err := st.PauseState(ctx)
	if err != nil {
		fmt.Fprintf(w, "pause state: unreadable (%v)\n", err)
	}
	if ps.Paused {
		fmt.Fprintf(w, "paused:      yes since %s (%s)\n",
			time.Unix(ps.Since, 0).Format(time.RFC3339), orNone(ps.Reason))
	} else {
		fmt.Fprintf(w, "paused:      no\n")
	}

	pr, err := st.Pressure(ctx)
	if err == nil && pr.Tripped {
		fmt.Fprintf(w, "tripped:     %s at %s\n", orNone(pr.Reason),
			time.Unix(pr.At, 0).Format(time.RFC3339))
		fmt.Fprintf(w, "             %s\n", pr.Detail)
		fmt.Fprintf(w, "             %s\n", pr.Snapshot)
		fmt.Fprintf(w, "             automatic restart is disabled; `albumctl resume` clears this\n")
	}

	tasks, err := st.RunningTasks(ctx)
	if err != nil {
		fmt.Fprintf(w, "tasks:       unreadable (%v)\n", err)
	}
	if len(tasks) == 0 {
		fmt.Fprintf(w, "running:     nothing\n")
	} else {
		for _, t := range tasks {
			age := time.Since(time.Unix(t.HeartbeatAt, 0)).Round(time.Second)
			fmt.Fprintf(w, "running:     %s on %s since %s (pid %d, last heartbeat %s ago)\n",
				t.Kind, orNone(t.CollectionCode), time.Unix(t.StartedAt, 0).Format("15:04:05"),
				t.PID, age)
			fmt.Fprintf(w, "             processed %d, skipped %d, failed %d, wrote %s\n",
				t.Processed, t.Skipped, t.Failed, humanBytes(t.BytesWritten))
			if t.Argv != "" {
				fmt.Fprintf(w, "             argv: %s\n", t.Argv)
			}
		}
	}
	if last, err := st.LastTask(ctx, ""); err == nil && last.ID != 0 {
		fmt.Fprintf(w, "last run:    %s %s at %s — %s\n", last.Kind, last.State,
			time.Unix(last.EndedAt, 0).Format(time.RFC3339), orNone(last.Reason))
		if last.Detail != "" {
			fmt.Fprintf(w, "             %s\n", last.Detail)
		}
	}

	fmt.Fprintf(w, "\ncollections and pending work\n")
	cols, err := st.Collections(ctx)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl: %v\n", err)
		return 1
	}
	if len(cols) == 0 {
		fmt.Fprintf(w, "  none indexed yet; run `albumctl scan --collection CODE --limit N`\n")
	}
	for _, c := range cols {
		probePend, thumbPend, _ := st.CountPending(ctx, c.ID)
		mark := "ok"
		if !c.LastCheckOK {
			mark = "SOURCE MISSING"
		}
		fmt.Fprintf(w, "  %-16s %7d media  %6d to probe  %6d thumbnails due  [%s]\n",
			c.Code, c.MediaCount, probePend, thumbPend, mark)
		if !c.LastCheckOK && c.LastCheckNote != "" {
			fmt.Fprintf(w, "  %-16s %s\n", "", c.LastCheckNote)
		}
	}

	tt, err := st.Totals(ctx, 0)
	if err == nil {
		fmt.Fprintf(w, "\nindex\n")
		fmt.Fprintf(w, "  media %d (%d stills, %d moving, %d other), probed %d, unsupported %d\n",
			tt.Media, tt.Images, tt.Videos, tt.Other, tt.Probed, tt.Unsupported)
		fmt.Fprintf(w, "  thumbnails ok %d, due %d, failed %d\n", tt.ThumbOK, tt.ThumbDue, tt.ThumbFailed)
		fmt.Fprintf(w, "  no usable date %d, with coordinates %d, recorded but not present %d\n",
			tt.Undated, tt.WithGPS, tt.Missing)
	}
	if files, bytes, err := st.SumThumbBytes(ctx); err == nil {
		pct := 0.0
		if cfg.Cache.MaxBytes > 0 {
			pct = float64(bytes) / float64(cfg.Cache.MaxBytes) * 100
		}
		fmt.Fprintf(w, "  cache %s in %d files (%.1f%% of the %s ceiling, %d file ceiling)\n",
			humanBytes(bytes), files, pct, humanBytes(cfg.Cache.MaxBytes), cfg.Cache.MaxFiles)
	}

	// The machine itself. Three families of numbers, printed apart.
	fmt.Fprintf(w, "\nmachine\n")
	sampler := resource.NewSampler(env.ProcRoot, "")
	snap := sampler.Sample(0)
	if snap.MemErr == nil {
		fmt.Fprintf(w, "  MemAvailable %s of %s total; swap used %s\n",
			humanBytes(snap.Mem.MemAvailableKB<<10), humanBytes(snap.Mem.MemTotalKB<<10),
			humanBytes(snap.Mem.SwapUsedKB()<<10))
	} else {
		fmt.Fprintf(w, "  MemAvailable unreadable: %v\n", snap.MemErr)
	}
	if snap.PSIMem.Present {
		fmt.Fprintf(w, "  PSI memory some avg10 %.2f avg60 %.2f avg300 %.2f\n",
			snap.PSIMem.SomeAvg10, snap.PSIMem.SomeAvg60, snap.PSIMem.SomeAvg300)
	} else {
		fmt.Fprintf(w, "  PSI memory: not available on this kernel; the guard falls back to MemAvailable and RSS\n")
	}
	if rss, hwm, err := resource.SelfRSSBytes(env.ProcRoot); err == nil {
		fmt.Fprintf(w, "  this command's own RSS %s, VmHWM %s\n", humanBytes(rss), humanBytes(hwm))
	}

	// The web service gets its own lines. It is the process that stays resident,
	// so its footprint is the one that matters against the idle-RSS target, and
	// asking `ps` for it is exactly the kind of thing an operator should not have
	// to do. Reading the record is a file read and one /proc lookup: no request
	// to the service, so `status` still returns while a decode is running.
	fmt.Fprintf(w, "\nweb service\n")
	switch rec, err := webstate.Read(cfg.Paths.RunDir); {
	case err != nil:
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(w, "  record unreadable: %v\n", err)
			break
		}
		fmt.Fprintf(w, "  not running\n")
	case !webstate.Alive(env.ProcRoot, rec.PID):
		fmt.Fprintf(w, "  not running; pid %d (started %s) left a record behind, so it was "+
			"killed rather than stopped\n", rec.PID, rec.Started)
	default:
		line := fmt.Sprintf("  running on %s (pid %d, started %s)", rec.Bind, rec.PID, rec.Started)
		if tree, err := resource.TreeRSS(env.ProcRoot, rec.PID); err == nil && tree.RSSBytes > 0 {
			line += fmt.Sprintf(", RSS %s, VmHWM %s",
				humanBytes(tree.RSSBytes), humanBytes(tree.HWMBytes))
		} else {
			line += ", RSS unreadable"
		}
		fmt.Fprintf(w, "%s\n", line)
		if rec.Version != "" || rec.Schema > 0 {
			fmt.Fprintf(w, "  version %s, index schema %d, read-only: it holds no writable handle\n",
				rec.Version, rec.Schema)
		}
	}

	fmt.Fprintf(w, "  thresholds: start at %s, warn at %s, stop at %s\n",
		humanBytes(cfg.Guard.StartMemAvailableKB<<10),
		humanBytes(cfg.Guard.WarnMemAvailableKB<<10),
		humanBytes(cfg.Guard.StopMemAvailableKB<<10))

	lockDir := cfg.Paths.LockDir
	for _, name := range []string{"scan", "thumbs"} {
		p := filepath.Join(lockDir, name+".lock")
		if holder := taskctl.Holder(p); holder != "" {
			fmt.Fprintf(w, "  lock %-7s held by %s\n", name, holder)
		}
	}
	return 0
}

// cmdStatusJSON emits the same facts as cmdStatus in a machine readable form.
//
// The text report remains the primary output: it explains what the numbers mean
// and says so when something is unknown. This document exists so a scheduled
// check on the target machine does not have to parse prose. Unknown or unreadable
// values are null or omitted rather than invented.
func cmdStatusJSON(ctx context.Context, cfg *config.Config, env Env) int {
	type collJSON struct {
		Code       string `json:"code"`
		Name       string `json:"name"`
		Enabled    bool   `json:"enabled"`
		Root       string `json:"root"`
		Media      int64  `json:"media"`
		ToProbe    int64  `json:"to_probe"`
		ThumbsDue  int64  `json:"thumbnails_due"`
		SourceOK   bool   `json:"source_ok"`
		LastScanAt int64  `json:"last_scan_at,omitempty"`
	}
	type taskJSON struct {
		Kind       string `json:"kind"`
		Collection string `json:"collection,omitempty"`
		PID        int64  `json:"pid"`
		StartedAt  int64  `json:"started_at"`
		Heartbeat  int64  `json:"heartbeat_at"`
		Processed  int64  `json:"processed"`
		Skipped    int64  `json:"skipped"`
		Failed     int64  `json:"failed"`
		Bytes      int64  `json:"bytes_written"`
	}
	type webJSON struct {
		State    string `json:"state"` // running | not_running | stale_record | unknown
		PID      int64  `json:"pid,omitempty"`
		Bind     string `json:"bind,omitempty"`
		Started  string `json:"started,omitempty"`
		Version  string `json:"version,omitempty"`
		Schema   int    `json:"schema,omitempty"`
		RSSBytes int64  `json:"rss_bytes,omitempty"`
		HWMBytes int64  `json:"hwm_bytes,omitempty"`
		Note     string `json:"note,omitempty"`
	}

	doc := struct {
		Version     string     `json:"version"`
		Target      string     `json:"target"`
		Config      string     `json:"config,omitempty"`
		Paused      bool       `json:"paused"`
		PausedAt    int64      `json:"paused_since,omitempty"`
		PausedWhy   string     `json:"paused_reason,omitempty"`
		Tripped     bool       `json:"tripped"`
		TripWhy     string     `json:"trip_reason,omitempty"`
		TripAt      int64      `json:"trip_at,omitempty"`
		TripNote    string     `json:"trip_detail,omitempty"`
		Running     []taskJSON `json:"running"`
		Last        *taskJSON  `json:"last,omitempty"`
		LastState   string     `json:"last_state,omitempty"`
		LastWhy     string     `json:"last_reason,omitempty"`
		Collections []collJSON `json:"collections"`
		Totals      *struct {
			Media       int64 `json:"media"`
			Images      int64 `json:"images"`
			Videos      int64 `json:"videos"`
			Other       int64 `json:"other"`
			Probed      int64 `json:"probed"`
			ThumbOK     int64 `json:"thumb_ok"`
			ThumbDue    int64 `json:"thumb_due"`
			ThumbFailed int64 `json:"thumb_failed"`
			Unsupported int64 `json:"unsupported"`
			WithGPS     int64 `json:"with_gps"`
			Undated     int64 `json:"undated"`
			Missing     int64 `json:"missing"`
		} `json:"totals,omitempty"`
		Cache *struct {
			Bytes   int64   `json:"bytes"`
			Files   int64   `json:"files"`
			MaxByte int64   `json:"max_bytes"`
			MaxFile int64   `json:"max_files"`
			UsedPct float64 `json:"used_pct"`
		} `json:"cache,omitempty"`
		Machine struct {
			MemAvailableKB int64   `json:"mem_available_kb"`
			MemTotalKB     int64   `json:"mem_total_kb"`
			SwapUsedKB     int64   `json:"swap_used_kb"`
			SwapTotalKB    int64   `json:"swap_total_kb"`
			PSIMemSome     float64 `json:"psi_memory_some_avg10"`
			PSIMemPresent  bool    `json:"psi_memory_present"`
			Readable       bool    `json:"meminfo_readable"`
			Err            string  `json:"error,omitempty"`
		} `json:"machine"`
		Web struct {
			webJSON
		} `json:"web"`
		Thresholds struct {
			StartKB int64 `json:"start_mem_available_kb"`
			WarnKB  int64 `json:"warn_mem_available_kb"`
			StopKB  int64 `json:"stop_mem_available_kb"`
		} `json:"thresholds"`
		Locks map[string]string `json:"locks"`
	}{
		Version:     buildinfo.Version,
		Target:      buildinfo.Target,
		Config:      cfg.Source,
		Locks:       map[string]string{},
		Running:     []taskJSON{},
		Collections: []collJSON{},
	}

	// Exit code mirrors the text form: a broken config or an unreadable index is
	// a failure, everything else is reported inside the document.
	code := 0
	if st, err := openStore(cfg, true); err != nil {
		doc.Web.Note = ""
		fmt.Fprintf(env.Stderr, "albumctl status --json: %v\n", err)
		code = 1
	} else {
		defer st.Close()
		if ps, err := st.PauseState(ctx); err == nil {
			doc.Paused, doc.PausedAt, doc.PausedWhy = ps.Paused, ps.Since, ps.Reason
		}
		if pr, err := st.Pressure(ctx); err == nil && pr.Tripped {
			doc.Tripped, doc.TripWhy, doc.TripAt, doc.TripNote = true, pr.Reason, pr.At, pr.Detail
		}
		if tasks, err := st.RunningTasks(ctx); err == nil {
			for _, t := range tasks {
				doc.Running = append(doc.Running, taskJSON{
					Kind: t.Kind, Collection: t.CollectionCode, PID: t.PID,
					StartedAt: t.StartedAt, Heartbeat: t.HeartbeatAt,
					Processed: t.Processed, Skipped: t.Skipped, Failed: t.Failed,
					Bytes: t.BytesWritten,
				})
			}
		}
		if last, err := st.LastTask(ctx, ""); err == nil && last.ID != 0 {
			doc.Last = &taskJSON{
				Kind: last.Kind, Collection: last.CollectionCode, PID: last.PID,
				StartedAt: last.StartedAt, Heartbeat: last.EndedAt,
				Processed: last.Processed, Skipped: last.Skipped, Failed: last.Failed,
				Bytes: last.BytesWritten,
			}
			doc.LastState, doc.LastWhy = last.State, last.Reason
		}
		if cols, err := st.Collections(ctx); err == nil {
			for _, c := range cols {
				p, d, _ := st.CountPending(ctx, c.ID)
				doc.Collections = append(doc.Collections, collJSON{
					Code: c.Code, Name: c.Name, Enabled: c.Enabled, Root: c.Root,
					Media: c.MediaCount, ToProbe: p, ThumbsDue: d,
					SourceOK: c.LastCheckOK, LastScanAt: c.LastScanAt,
				})
			}
		}
		if tt, err := st.Totals(ctx, 0); err == nil {
			doc.Totals = &struct {
				Media       int64 `json:"media"`
				Images      int64 `json:"images"`
				Videos      int64 `json:"videos"`
				Other       int64 `json:"other"`
				Probed      int64 `json:"probed"`
				ThumbOK     int64 `json:"thumb_ok"`
				ThumbDue    int64 `json:"thumb_due"`
				ThumbFailed int64 `json:"thumb_failed"`
				Unsupported int64 `json:"unsupported"`
				WithGPS     int64 `json:"with_gps"`
				Undated     int64 `json:"undated"`
				Missing     int64 `json:"missing"`
			}{tt.Media, tt.Images, tt.Videos, tt.Other, tt.Probed, tt.ThumbOK,
				tt.ThumbDue, tt.ThumbFailed, tt.Unsupported, tt.WithGPS, tt.Undated, tt.Missing}
		}
		if files, bytes, err := st.SumThumbBytes(ctx); err == nil {
			pct := 0.0
			if cfg.Cache.MaxBytes > 0 {
				pct = float64(bytes) / float64(cfg.Cache.MaxBytes) * 100
			}
			doc.Cache = &struct {
				Bytes   int64   `json:"bytes"`
				Files   int64   `json:"files"`
				MaxByte int64   `json:"max_bytes"`
				MaxFile int64   `json:"max_files"`
				UsedPct float64 `json:"used_pct"`
			}{bytes, files, cfg.Cache.MaxBytes, int64(cfg.Cache.MaxFiles), pct}
		}
	}
	// The configured collections are reported even before the first scan, so an
	// empty index still describes what is supposed to be there.
	if len(doc.Collections) == 0 {
		for _, c := range cfg.Collections {
			doc.Collections = append(doc.Collections, collJSON{
				Code: c.Code, Name: c.Name, Enabled: c.Enabled, Root: c.Root,
				Media: 0, ToProbe: 0, ThumbsDue: 0, SourceOK: false,
			})
		}
	}

	sampler := resource.NewSampler(env.ProcRoot, "")
	snap := sampler.Sample(0)
	if snap.MemErr == nil {
		doc.Machine.Readable = true
		doc.Machine.MemAvailableKB = snap.Mem.MemAvailableKB
		doc.Machine.MemTotalKB = snap.Mem.MemTotalKB
		doc.Machine.SwapUsedKB = snap.Mem.SwapUsedKB()
		doc.Machine.SwapTotalKB = snap.Mem.SwapTotalKB
	} else {
		doc.Machine.Err = snap.MemErr.Error()
	}
	doc.Machine.PSIMemPresent = snap.PSIMem.Present
	doc.Machine.PSIMemSome = snap.PSIMem.SomeAvg10

	switch rec, err := webstate.Read(cfg.Paths.RunDir); {
	case err != nil && errors.Is(err, os.ErrNotExist):
		doc.Web.State = "not_running"
	case err != nil:
		doc.Web.State, doc.Web.Note = "unknown", err.Error()
	case !webstate.Alive(env.ProcRoot, rec.PID):
		doc.Web.State, doc.Web.PID, doc.Web.Bind, doc.Web.Started = "stale_record", int64(rec.PID), rec.Bind, rec.Started
	default:
		doc.Web.State, doc.Web.PID, doc.Web.Bind, doc.Web.Started = "running", int64(rec.PID), rec.Bind, rec.Started
		doc.Web.Version, doc.Web.Schema = rec.Version, rec.Schema
		if tree, err := resource.TreeRSS(env.ProcRoot, rec.PID); err == nil {
			doc.Web.RSSBytes, doc.Web.HWMBytes = tree.RSSBytes, tree.HWMBytes
		}
	}

	doc.Thresholds.StartKB = cfg.Guard.StartMemAvailableKB
	doc.Thresholds.WarnKB = cfg.Guard.WarnMemAvailableKB
	doc.Thresholds.StopKB = cfg.Guard.StopMemAvailableKB
	for _, name := range []string{"scan", "thumbs"} {
		if holder := taskctl.Holder(filepath.Join(cfg.Paths.LockDir, name+".lock")); holder != "" {
			doc.Locks[name] = holder
		}
	}

	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		fmt.Fprintf(env.Stderr, "albumctl status --json: %v\n", err)
		return 1
	}
	return code
}

// ---- scan ---------------------------------------------------------------

func cmdScan(ctx context.Context, cfg *config.Config, env Env, args []string) int {
	fs := newFlagSet("scan", env.Stderr)
	collection := fs.String("collection", "", "collection code to scan")
	limit := fs.Int("limit", 0, "maximum number of files to touch (0 means no cap; use a cap)")
	dryRun := fs.Bool("dry-run", false, "report what would happen without writing anything")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *collection == "" {
		fmt.Fprintln(env.Stderr, "albumctl scan: --collection is required")
		return 2
	}
	if !*dryRun && *limit <= 0 {
		fmt.Fprintf(env.Stderr,
			"albumctl scan: refusing to run without --limit.\n"+
				"An uncapped first run over a 5000 file library is exactly the load this tool exists to avoid.\n"+
				"Start with --limit 50, check `albumctl status`, then raise it.\n")
		return 2
	}

	log := openLog(cfg, false)
	defer log.Close()

	st, err := openStore(cfg, false)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl: %v\n", err)
		return 1
	}
	defer st.Close()

	r := &scan.Runner{Cfg: cfg, Store: st, Log: log}
	res, err := r.Run(ctx, scan.Options{
		CollectionCode: *collection,
		Limit:          *limit,
		DryRun:         *dryRun,
		OnProgress: func(msg string) {
			fmt.Fprintf(env.Stderr, "  %s\n", msg)
		},
	})
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl scan: %v\n", err)
		return 1
	}
	fmt.Fprint(env.Stdout, res.HumanReport())
	if res.StopReason != "" && res.StopReason != "limit_reached" && res.StopReason != "exhausted" {
		return 1
	}
	return 0
}

// ---- thumbs -------------------------------------------------------------

func cmdThumbs(ctx context.Context, cfg *config.Config, env Env, args []string) int {
	fs := newFlagSet("thumbs", env.Stderr)
	collection := fs.String("collection", "", "collection code to process")
	limit := fs.Int("limit", 0, "maximum number of files to process")
	maxRuntime := fs.Duration("max-runtime", 0, "wall clock budget, e.g. 10m")
	force := fs.Bool("force", false, "ignore the post-trip cooldown (never overrides a pause)")
	allowNoCgroup := fs.Bool("allow-no-cgroup", false,
		"allow running without kernel-enforced worker isolation (weaker; recorded in the task row)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *collection == "" {
		fmt.Fprintln(env.Stderr, "albumctl thumbs: --collection is required")
		return 2
	}
	if *limit <= 0 || *maxRuntime <= 0 {
		fmt.Fprintf(env.Stderr,
			"albumctl thumbs: --limit and --max-runtime are both required.\n"+
				"A thumbnail run is a bounded task; an unbounded one is what took the machine down before.\n"+
				"Start with --limit 10 --max-runtime 10m and watch `albumctl status`.\n")
		return 2
	}

	log := openLog(cfg, false)
	defer log.Close()

	st, err := openStore(cfg, false)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl: %v\n", err)
		return 1
	}
	defer st.Close()

	r := &thumbs.Runner{Cfg: cfg, Store: st, Log: log}
	rep, err := r.Run(ctx, thumbs.RunOptions{
		CollectionCode: *collection,
		Limit:          *limit,
		MaxRuntime:     *maxRuntime,
		Force:          *force,
		AllowNoCgroup:  *allowNoCgroup,
		OnProgress: func(msg string) {
			fmt.Fprintf(env.Stderr, "  %s\n", msg)
		},
	})
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl thumbs: %v\n", err)
		if rep.TaskID != 0 {
			fmt.Fprint(env.Stdout, rep.ResourceReport())
		}
		return 1
	}
	fmt.Fprint(env.Stdout, rep.ResourceReport())
	return thumbsExitCode(rep.StopReason)
}

// Exit codes for a bounded task. They exist so a supervising script does not
// have to parse the report to tell "the batch finished" from "the batch stopped
// early", which is exactly the distinction that matters when the reason for
// stopping is a resource threshold.
//
//	0  the bounded amount of work asked for was done (or there was none left)
//	1  the run failed: a configuration, index or filesystem error
//	3  the run stopped early for a reason that is not a failure of the media:
//	   a pause, a recorded resource-pressure trip, the backup gate, a lost
//	   source mount, or a lost lock
//	4  a stop caused by a resource threshold during the run, or by the
//	   per-file timeout / worker failure limit
const (
	ExitThumbsStopped  = 3
	ExitThumbsResource = 4
)

func thumbsExitCode(reason string) int {
	switch reason {
	case string(resource.ReasonLimitReached), string(resource.ReasonExhausted),
		string(resource.ReasonMaxRuntime):
		return 0
	case string(resource.ReasonPaused), string(resource.ReasonPressureTrip),
		string(resource.ReasonBackupBusy), string(resource.ReasonNoBackupStatus),
		string(resource.ReasonSourceCheck), string(resource.ReasonLocked),
		string(resource.ReasonOperatorStop), "isolation_unavailable":
		return ExitThumbsStopped
	case string(resource.ReasonMemBelowStart), string(resource.ReasonMemBelowWarn),
		string(resource.ReasonMemBelowStop), string(resource.ReasonSwapUsed),
		string(resource.ReasonWorkerRSS), string(resource.ReasonWorkerCgroup),
		string(resource.ReasonCgroupMaxEvents), string(resource.ReasonCgroupOOMKill),
		string(resource.ReasonPSIMemory), string(resource.ReasonPSIIO),
		string(resource.ReasonPerFileTimeout), string(resource.ReasonWorkerExit),
		string(resource.ReasonWorkerSignal), string(resource.ReasonCacheFull),
		string(resource.ReasonErrorStreak), string(resource.ReasonNoSignal):
		return ExitThumbsResource
	}
	// An unrecognised reason is reported as a stop rather than as success: a
	// new reason added without being classified here should not look like a
	// clean finish.
	return ExitThumbsStopped
}

// ---- pause / resume -----------------------------------------------------

func cmdPause(ctx context.Context, cfg *config.Config, env Env, args []string) int {
	fs := newFlagSet("pause", env.Stderr)
	reason := fs.String("reason", "", "why the pause was set")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st, err := openStore(cfg, false)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl: %v\n", err)
		return 1
	}
	defer st.Close()

	r := orNone(*reason)
	if *reason == "" {
		r = "set by albumctl pause"
	}
	if err := st.Pause(ctx, r, "cli"); err != nil {
		fmt.Fprintf(env.Stderr, "albumctl pause: %v\n", err)
		return 1
	}
	// The pause is written to the index, so it survives a reboot, a crash and a
	// `systemctl restart`. Any task already running sees it at its next file
	// boundary and stops there.
	fmt.Fprintf(env.Stdout, "Paused. Heavy tasks will not start, and any run in progress stops at the next file boundary.\n")
	fmt.Fprintf(env.Stdout, "Reason recorded: %s\n", r)
	fmt.Fprintf(env.Stdout, "Browsing is unaffected. Clear it with `albumctl resume`.\n")
	return 0
}

func cmdResume(ctx context.Context, cfg *config.Config, env Env, args []string) int {
	fs := newFlagSet("resume", env.Stderr)
	force := fs.Bool("force", false, "clear a pressure trip even if the cooldown has not elapsed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st, err := openStore(cfg, false)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl: %v\n", err)
		return 1
	}
	defer st.Close()

	ps, _ := st.PauseState(ctx)
	pr, _ := st.Pressure(ctx)

	if err := st.Resume(ctx); err != nil {
		fmt.Fprintf(env.Stderr, "albumctl resume: %v\n", err)
		return 1
	}
	if ps.Paused {
		fmt.Fprintf(env.Stdout, "Pause cleared (it was: %s).\n", orNone(ps.Reason))
	} else {
		fmt.Fprintf(env.Stdout, "No pause was set.\n")
	}
	if pr.Tripped {
		if !*force {
			left := time.Until(time.Unix(pr.At, 0).Add(cfg.Guard.CooldownAfterTrip.D()))
			if left > 0 {
				fmt.Fprintf(env.Stdout,
					"A pressure trip from %s is still inside its %s cooldown (%s left).\n",
					time.Unix(pr.At, 0).Format(time.RFC3339), cfg.Guard.CooldownAfterTrip.D(), left.Round(time.Second))
				fmt.Fprintf(env.Stdout, "It was: %s\n", pr.Detail)
				fmt.Fprintf(env.Stdout,
					"Check the machine first. Then run `albumctl resume --force` to clear it early.\n")
				return 0
			}
		}
		if err := st.ClearPressure(ctx); err != nil {
			fmt.Fprintf(env.Stderr, "albumctl resume: %v\n", err)
			return 1
		}
		fmt.Fprintf(env.Stdout, "Pressure trip cleared (%s).\n", orNone(pr.Reason))
	}
	fmt.Fprintf(env.Stdout, "Resuming does not start anything. Run a scan or a thumbnail batch explicitly when you are ready.\n")
	return 0
}

// ---- doctor -------------------------------------------------------------

func cmdDoctor(ctx context.Context, cfg *config.Config, env Env, args []string) int {
	fs := newFlagSet("doctor", env.Stderr)
	resetFSID := fs.String("reset-fsid", "", "re-pin the filesystem identity of a collection")
	asJSON := fs.Bool("json", false, "emit machine readable output")
	probeWrites := fs.Bool("probe-writes", false,
		"actually create and delete a file inside each media root to test writability "+
			"(off by default: the media tree is meant to be read-only and an interrupted "+
			"probe could leave a stray file in the photo library)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	w := env.Stdout
	problems := 0
	notes := 0

	// entries collects the same findings the text report prints, so the JSON
	// form cannot drift from the prose: both come from these calls.
	type diagEntry struct {
		Level   string `json:"level"` // ok | problem | note
		Message string `json:"message"`
	}
	var entries []diagEntry
	emit := func(level, msg string) {
		entries = append(entries, diagEntry{Level: level, Message: msg})
		if !*asJSON {
			switch level {
			case "ok":
				fmt.Fprintf(w, "[ok  ] %s\n", msg)
			case "problem":
				fmt.Fprintf(w, "[FAIL] %s\n", msg)
			default:
				fmt.Fprintf(w, "[note] %s\n", msg)
			}
		}
	}
	report := func(ok bool, format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		if ok {
			emit("ok", msg)
			return
		}
		problems++
		emit("problem", msg)
	}
	warn := func(format string, a ...any) {
		notes++
		emit("note", fmt.Sprintf(format, a...))
	}

	if !*asJSON {
		fmt.Fprintf(w, "%s\nconfig: %s\n\n", buildinfo.String(), orNone(cfg.Source))
	}

	// 1. State directory and its writability.
	if err := fsutil.EnsureDir(cfg.Paths.StateDir); err != nil {
		report(false, "state directory %s: %v", cfg.Paths.StateDir, err)
	} else if err := probeWritable(cfg.Paths.StateDir); err != nil {
		report(false, "state directory %s is not writable: %v", cfg.Paths.StateDir, err)
	} else {
		report(true, "state directory %s is writable", cfg.Paths.StateDir)
	}
	if err := fsutil.EnsureDir(cfg.Paths.CacheDir); err != nil {
		report(false, "cache directory %s: %v", cfg.Paths.CacheDir, err)
	} else {
		report(true, "cache directory %s is writable", cfg.Paths.CacheDir)
	}

	// 2. The index itself.
	st, err := openStore(cfg, false)
	if err != nil {
		report(false, "index %s cannot be opened: %v", cfg.Paths.IndexFile, err)
	} else {
		report(true, "index %s opened, schema %d", cfg.Paths.IndexFile, store.SchemaVersion)
		if ss, err := st.SchemaState(ctx); err == nil {
			if ss.OK {
				report(true, "index schema matches this binary (%d)", ss.Found)
			} else {
				report(false, "index schema mismatch: binary expects %d, file has %d (%s)",
					store.SchemaVersion, ss.Found, ss.Note)
			}
		}
		if stale, err := st.SweepStaleTasks(ctx, env.ProcRoot, 2*time.Minute); err == nil && stale > 0 {
			warn("marked %d stale task row(s) as interrupted (their process is gone)", stale)
		}
		st.Close()
	}

	// 3. Free space where the state lives. A full system disk is a machine-wide
	// outage, not just an albumctl problem.
	if free, total, err := fsutil.FreeBytes(cfg.Paths.StateDir); err == nil {
		pct := float64(free) / float64(total) * 100
		if pct < 5 {
			report(false, "state filesystem has only %s free of %s (%.1f%%)",
				humanBytes(int64(free)), humanBytes(int64(total)), pct)
		} else {
			report(true, "state filesystem has %s free of %s (%.1f%%)",
				humanBytes(int64(free)), humanBytes(int64(total)), pct)
		}
	} else {
		report(false, "cannot read free space for %s: %v", cfg.Paths.StateDir, err)
	}
	if files, bytes, err := fsutil.DirUsage(cfg.Paths.CacheDir); err == nil {
		report(true, "thumbnail cache holds %s in %d files", humanBytes(bytes), files)
		if cfg.Cache.MaxBytes > 0 && bytes > cfg.Cache.MaxBytes {
			report(false, "thumbnail cache (%s) has exceeded its configured ceiling (%s)",
				humanBytes(bytes), humanBytes(cfg.Cache.MaxBytes))
		}
	}

	// 4. Collections: mount identity, permissions, and the system-disk trap.
	for _, c := range cfg.Collections {
		tag := fmt.Sprintf("collection %-14s", c.Code)
		if !c.Enabled {
			report(true, "%s is disabled in the config", tag)
			continue
		}
		if *resetFSID == c.Code {
			if st, err := openStore(cfg, false); err == nil {
				if col, ok := findCollection(ctx, st, c.Code); ok {
					if err := st.PinCollectionFSID(ctx, col.ID, ""); err != nil {
						report(false, "%s could not clear the pinned filesystem id: %v", tag, err)
					} else {
						report(true, "%s pinned filesystem id cleared; the next run will record the current one", tag)
					}
				}
				st.Close()
			}
		}
		pinned := c.ExpectFSID
		if st, err := openStore(cfg, true); err == nil {
			if col, ok := findCollection(ctx, st, c.Code); ok && col.ExpectFSID != "" {
				pinned = col.ExpectFSID
			}
			st.Close()
		}
		sc := source.Verify(c, cfg.Paths.StateDir, pinned)
		if sc.OK {
			report(true, "%s root %s verified (device %d, fsid %s)", tag, c.Root, sc.RootDevice, orNone(sc.FSID))
			for _, n := range sc.Notes {
				warn("%s %s", tag, n)
			}
		} else {
			report(false, "%s %s", tag, sc.Fatal)
		}
		if err := probeReadable(c.Root); err != nil {
			report(false, "%s root is not readable: %v", tag, err)
		} else {
			report(true, "%s root is readable", tag)
		}
		// Whether the media tree is writable is a property of the mount and the
		// permissions on it, not of albumctl. It is checked without writing by
		// default, because the design promises that the photo tree is never
		// written to: a probe that creates and deletes a file is still a write,
		// and a crash between the two steps would leave an orphan in the
		// library. --probe-writes asks for the invasive version explicitly.
		if *probeWrites {
			if err := probeNotWritable(c.Root); err != nil {
				warn("%s root is writable by this user (%v); the design expects the media tree to be read-only",
					tag, err)
			} else {
				report(true, "%s root is not writable, as the design requires (verified by writing)", tag)
			}
		} else {
			writable, why := mayWrite(c.Root)
			if writable {
				warn("%s root appears writable by this user (%s); the design expects the media tree "+
					"to be read-only. This was judged from permissions, not by writing. "+
					"Re-run with --probe-writes to confirm.", tag, why)
			} else {
				report(true, "%s root looks read-only (%s); judged from permissions, not verified by writing", tag, why)
			}
		}
	}

	// 5. Resource limits: can this platform actually enforce a ceiling?
	sampler := resource.NewSampler(env.ProcRoot, "")
	cm, cerr := resource.NewCgroupManager(cfg.Guard.CgroupRoot, env.ProcRoot)
	var cap resource.Capability
	if cerr != nil {
		report(false, "cgroup manager could not be initialised: %v", cerr)
		cap = resource.ProbeCapability(sampler, nil)
	} else {
		cap = resource.ProbeCapability(sampler, cm)
	}
	if cap.CgroupIsolation {
		report(true, "cgroup v2 isolation is available: %s", orNone(cap.CgroupReason))
		if already, err := cm.EffectiveWorkerLimitBytes(); err == nil && already > 0 {
			report(true, "an enclosing unit already imposes a memory ceiling of %s on albumctl",
				humanBytes(already))
		} else if err == nil {
			warn("no memory ceiling is set on albumctl's own cgroup; the systemd unit is expected to set MemoryMax")
		}
	} else {
		report(false, "cgroup v2 isolation is not available: %s", orNone(cap.CgroupReason))
		if cfg.Guard.RequireCgroupIsolation {
			report(false, "guard.require_cgroup_isolation is true, so heavy tasks will refuse to run")
		} else {
			warn("guard.require_cgroup_isolation is false: worker decoding is supervised by RSS polling " +
				"only, which is weaker than a kernel-enforced ceiling. This is a deliberate, recorded choice")
		}
	}
	if cap.MemInfo {
		report(true, "MemAvailable is readable from %s/meminfo", env.ProcRoot)
	} else {
		report(false, "MemAvailable is not readable; the guard cannot make a start decision")
	}
	if cap.PSIMemory {
		report(true, "memory PSI is available")
	} else {
		warn("memory PSI is not available; pressure spikes are detected from MemAvailable and RSS only")
	}
	if cap.PSIIO {
		report(true, "I/O PSI is available")
	} else {
		warn("I/O PSI is not available; an I/O stall will not be detected directly")
	}
	for _, n := range cap.Notes {
		warn("%s", n)
	}
	if problems := cap.CapabilityProblems(cfg.Guard.RequireCgroupIsolation); len(problems) > 0 {
		for _, p := range problems {
			report(false, "%s", p)
		}
	}
	if !*asJSON {
		fmt.Fprintf(w, "\nconfigured thresholds: start %s, warn %s, stop %s (MemAvailable)\n",
			humanBytes(cfg.Guard.StartMemAvailableKB<<10),
			humanBytes(cfg.Guard.WarnMemAvailableKB<<10),
			humanBytes(cfg.Guard.StopMemAvailableKB<<10))
		fmt.Fprintf(w, "worker ceiling: MemoryHigh %s, MemoryMax %s; per-file timeout %s, grace %s\n",
			humanBytes(cfg.Guard.WorkerMemHighMB<<20), humanBytes(cfg.Guard.WorkerMemMaxMB<<20),
			cfg.Decode.PerFileTimeout.D(), cfg.Decode.GracePeriod.D())
	}

	// 6. The bind address, because a wrong one is a silent exposure.
	if err := server.CheckBind(cfg); err != nil {
		report(false, "web bind address: %v", err)
	} else {
		report(true, "web bind address %s is loopback, private or Tailscale", cfg.Server.Bind)
	}
	if cfg.Server.Token == "" {
		warn("no web token is configured; access control rests entirely on the bind address and the " +
			"network in front of it. A private address is not a promise that every device on it is yours")
	}

	if *asJSON {
		doc := struct {
			Version      string `json:"version"`
			Target       string `json:"target"`
			Config       string `json:"config,omitempty"`
			Bind         string `json:"bind"`
			Problems     int    `json:"problems"`
			Notes        int    `json:"notes"`
			ProbeWrites  bool   `json:"probe_writes"`
			ThresholdsKB struct {
				Start int64 `json:"start_mem_available_kb"`
				Warn  int64 `json:"warn_mem_available_kb"`
				Stop  int64 `json:"stop_mem_available_kb"`
			} `json:"thresholds"`
			Findings []diagEntry `json:"findings"`
		}{
			Version: buildinfo.Version, Target: buildinfo.Target, Config: cfg.Source,
			Bind: cfg.Server.Bind, Problems: problems, Notes: notes,
			ProbeWrites: *probeWrites, Findings: entries,
		}
		doc.ThresholdsKB.Start = cfg.Guard.StartMemAvailableKB
		doc.ThresholdsKB.Warn = cfg.Guard.WarnMemAvailableKB
		doc.ThresholdsKB.Stop = cfg.Guard.StopMemAvailableKB
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(doc); err != nil {
			fmt.Fprintf(env.Stderr, "albumctl doctor --json: %v\n", err)
			return 1
		}
	} else {
		fmt.Fprintf(w, "\n%s", strings.Repeat("-", 60))
		fmt.Fprintf(w, "\n%d problem(s), %d note(s).\n", problems, notes)
	}
	if problems > 0 {
		return 1
	}
	return 0
}

func findCollection(ctx context.Context, st *store.Store, code string) (store.Collection, bool) {
	cols, err := st.Collections(ctx)
	if err != nil {
		return store.Collection{}, false
	}
	for _, c := range cols {
		if c.Code == code {
			return c, true
		}
	}
	return store.Collection{}, false
}

// ---- cache --------------------------------------------------------------

func cmdCacheReport(ctx context.Context, cfg *config.Config, env Env) int {
	w := env.Stdout
	files, bytes, err := fsutil.DirUsage(cfg.Paths.CacheDir)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl cache-report: %v\n", err)
		return 1
	}
	st, err := openStore(cfg, true)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl: %v\n", err)
		return 1
	}
	defer st.Close()
	ok, _, err := st.SumThumbBytes(ctx)
	if err != nil {
		ok = -1
	}
	tt, _ := st.Totals(ctx, 0)

	pct := 0.0
	if cfg.Cache.MaxBytes > 0 {
		pct = float64(bytes) / float64(cfg.Cache.MaxBytes) * 100
	}
	fmt.Fprintf(w, "cache directory:  %s\n", cfg.Paths.CacheDir)
	fmt.Fprintf(w, "on disk:          %s in %d files (%.1f%% of the %s ceiling)\n",
		humanBytes(bytes), files, pct, humanBytes(cfg.Cache.MaxBytes))
	fmt.Fprintf(w, "file ceiling:     %d files (%.1f%% used)\n", cfg.Cache.MaxFiles,
		pctOf(float64(files), float64(cfg.Cache.MaxFiles)))
	fmt.Fprintf(w, "index says ok:    %d thumbnails\n", maxI64(ok, 0))
	fmt.Fprintf(w, "index totals:     %d media, %d ok, %d due, %d failed, %d unsupported\n",
		tt.Media, tt.ThumbOK, tt.ThumbDue, tt.ThumbFailed, tt.Unsupported)
	if ok >= 0 && ok != int64(files) {
		fmt.Fprintf(w, "note: the index records %d thumbnails and the directory holds %d files.\n", ok, files)
		fmt.Fprintf(w, "      A difference is expected if temp files or orphans are present. Nothing is removed\n")
		fmt.Fprintf(w, "      automatically; inspect before removing anything.\n")
	}
	avg := int64(0)
	if files > 0 {
		avg = bytes / int64(files)
	}
	fmt.Fprintf(w, "average size:     %s\n", humanBytes(avg))
	fmt.Fprintf(w, "\nThis command reports only. Pruning is a separate, explicit act: `albumctl cache-gc`.\n")
	return 0
}

func cmdCacheGC(ctx context.Context, cfg *config.Config, env Env, args []string) int {
	fs := newFlagSet("cache-gc", env.Stderr)
	maxAge := fs.Duration("max-age", 90*24*time.Hour,
		"remove cache files not referenced by the index and older than this")
	dryRun := fs.Bool("dry-run", false, "list what would be removed without removing it")
	limit := fs.Int("limit", 5000, "maximum number of files to remove in one pass")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	w := env.Stdout

	// The cache directory is resolved from configuration and then checked to
	// look like albumctl's own cache tree. Nothing outside it is ever touched,
	// and there is no code path here that can reach a media directory.
	root := filepath.Clean(cfg.Paths.CacheDir)
	if _, err := os.Stat(root); err != nil {
		fmt.Fprintf(env.Stderr, "albumctl cache-gc: %s is not readable: %v\n", root, err)
		return 1
	}
	if cfg.Paths.StateDir == "" || !fsutil.IsWithin(cfg.Paths.StateDir, root) {
		fmt.Fprintf(env.Stderr,
			"albumctl cache-gc: refusing to prune %s because it is not inside the state directory %s\n",
			root, cfg.Paths.StateDir)
		return 1
	}
	for _, c := range cfg.Collections {
		if fsutil.IsWithin(c.Root, root) {
			fmt.Fprintf(env.Stderr,
				"albumctl cache-gc: refusing to prune %s because it is inside media collection %s\n", root, c.Code)
			return 1
		}
	}

	lock, err := taskctl.Acquire(cfg.Paths.LockDir, "thumbs")
	if err != nil {
		fmt.Fprintf(env.Stderr,
			"albumctl cache-gc: refusing to run while a thumbnail task holds its lock (%v)\n", err)
		return 1
	}
	defer lock.Release()

	st, err := openStore(cfg, true)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl: %v\n", err)
		return 1
	}
	defer st.Close()

	referenced, err := st.AllThumbPaths(ctx, cfg.Paths.CacheDir, cfg.Decode.ThumbMaxEdge, cfg.Cache.ShardDepth)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl cache-gc: cannot read the index: %v\n", err)
		return 1
	}
	refSet := make(map[string]struct{}, len(referenced))
	for _, p := range referenced {
		refSet[p] = struct{}{}
	}

	cutoff := time.Now().Add(-*maxAge)
	removed, kept, freed := 0, 0, int64(0)
	var err2 error
	err2 = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || ctx.Err() != nil || removed >= *limit {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		if cachepath.IsCacheArtifact(fi.Name()) {
			if _, ok := refSet[p]; ok {
				kept++
				return nil
			}
		} else if !fsutil.IsTempArtifact(fi.Name()) {
			// Something in the cache directory that albumctl does not
			// recognise as its own. Leave it alone and say so.
			kept++
			return nil
		}
		if fi.ModTime().After(cutoff) {
			kept++
			return nil
		}
		if *dryRun {
			fmt.Fprintf(w, "would remove %s (%s, %s)\n", p, humanBytes(fi.Size()),
				fi.ModTime().Format(time.RFC3339))
			removed++
			freed += fi.Size()
			return nil
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		removed++
		freed += fi.Size()
		return nil
	})
	if err2 != nil {
		fmt.Fprintf(env.Stderr, "albumctl cache-gc: %v\n", err2)
		return 1
	}
	verb := "removed"
	if *dryRun {
		verb = "would remove"
	}
	fmt.Fprintf(w, "%s %d file(s), %s; kept %d.\n", verb, removed, humanBytes(freed), kept)
	fmt.Fprintf(w, "Only files inside the albumctl cache directory were considered. No media file was touched.\n")
	return 0
}

// ---- serve --------------------------------------------------------------

func cmdServe(ctx context.Context, cfg *config.Config, env Env) int {
	if err := server.CheckBind(cfg); err != nil {
		fmt.Fprintf(env.Stderr, "albumctl serve: %v\n", err)
		return 1
	}
	log := openLog(cfg, false)
	defer log.Close()

	// The web service opens the index read-only. It is not a policy, it is a
	// property of the handle: SQLite is opened with query_only, so even a bug in
	// a handler cannot write.
	st, err := openStore(cfg, true)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl serve: %v\n", err)
		return 1
	}
	defer st.Close()

	srv, err := server.New(cfg, st, log)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl serve: %v\n", err)
		return 1
	}
	if err := server.Serve(ctx, cfg, srv, env.Stderr); err != nil {
		fmt.Fprintf(env.Stderr, "albumctl serve: %v\n", err)
		return 1
	}
	return 0
}

func cmdCollections(ctx context.Context, cfg *config.Config, env Env) int {
	st, err := openStore(cfg, true)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl: %v\n", err)
		return 1
	}
	defer st.Close()
	cols, err := st.Collections(ctx)
	if err != nil {
		fmt.Fprintf(env.Stderr, "albumctl: %v\n", err)
		return 1
	}
	for _, c := range cols {
		fmt.Fprintf(env.Stdout, "%-16s %-28s %8d media  last scan %s\n",
			c.Code, c.Name, c.MediaCount,
			orZero(c.LastScanAt))
	}
	return 0
}

// ---- exec-worker --------------------------------------------------------

func runWorker(args []string, env Env) int {
	fs := newFlagSet("exec-worker", env.Stderr)
	root := fs.String("root", "", "collection root")
	rel := fs.String("rel", "", "path relative to the root")
	out := fs.String("out", "", "thumbnail destination")
	expectSize := fs.Int64("expect-size", 0, "expected file size")
	expectMtime := fs.Int64("expect-mtime", 0, "expected modification time in nanoseconds")
	maxEdge := fs.Int("max-edge", 512, "")
	quality := fs.Int("quality", 82, "")
	maxPixels := fs.Int64("max-pixels", 0, "")
	maxFileBytes := fs.Int64("max-file-bytes", 0, "")
	maxDecodeMB := fs.Int64("max-decode-mb", 0, "")
	maxFrames := fs.Int("max-frames", 1, "")
	formats := fs.String("formats", "jpeg,png,gif,webp", "")
	cgroupDir := fs.String("cgroup-dir", "", "the cgroup the supervisor placed this worker in")
	handshake := fs.Bool("handshake", false, "wait for the supervisor's go-ahead before decoding")
	maxRuntime := fs.Duration("max-runtime", 0, "internal deadline")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	code, err := thumbs.RunWorker(thumbs.WorkerArgs{
		Root:          *root,
		Rel:           *rel,
		Out:           *out,
		ExpectSize:    *expectSize,
		ExpectMtimeNS: *expectMtime,
		CgroupDir:     *cgroupDir,
		Handshake:     *handshake,
		MaxRuntime:    *maxRuntime,
		Options: thumbs.Options{
			MaxEdge:        *maxEdge,
			Quality:        *quality,
			MaxPixels:      *maxPixels,
			MaxFileBytes:   *maxFileBytes,
			MaxDecodeBytes: *maxDecodeMB << 20,
			MaxFrames:      *maxFrames,
			Formats:        thumbs.ParseFormats(*formats),
		},
	}, env.Stdin, env.Stdout)
	if err != nil {
		fmt.Fprintf(env.Stderr, "exec-worker: %v\n", err)
		return 5
	}
	return code
}

// ---- small utilities ----------------------------------------------------

func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".albumctl-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

func probeReadable(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	_, err = f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		f.Close()
		return err
	}
	return f.Close()
}

// mayWrite reports whether this process could plausibly create a file in dir,
// judged without touching it.
//
// The judgement is deliberately approximate and the caller must say so. It
// answers the question the operator actually asks - "is this photo directory
// protected from me?" - while keeping the promise that albumctl never writes
// into the media tree, even for a moment.
func mayWrite(dir string) (bool, string) {
	fi, err := os.Stat(dir)
	if err != nil {
		return false, "cannot stat the directory: " + err.Error()
	}
	mode := fi.Mode().Perm()
	if fi.Mode()&os.ModeSymlink != 0 {
		return false, "the path is a symbolic link"
	}
	// A mount whose superblock is read-only is the strongest signal available
	// without writing.
	if ro, ok := mountReadOnly(dir); ok && ro {
		return false, "the filesystem is mounted read-only, or the directory sits on one that is"
	}
	if mode&0o222 == 0 {
		return false, fmt.Sprintf("no write bit is set on the directory (mode %04o)", mode)
	}
	uid := os.Geteuid()
	if uid == 0 {
		return true, "running as root, which may write almost anywhere"
	}
	groups, _ := os.Getgroups()
	if int(fi.Sys().(*syscall.Stat_t).Uid) == uid {
		return mode&0o200 != 0, "judged from the owner's write bit"
	}
	if int(fi.Sys().(*syscall.Stat_t).Gid) == uid || containsInt(groups, int(fi.Sys().(*syscall.Stat_t).Gid)) {
		return mode&0o020 != 0, "judged from the group's write bit"
	}
	return mode&0o002 != 0, "judged from the other-users' write bit"
}

// mountReadOnly reports whether the filesystem containing dir is mounted
// read-only, by finding the longest mount point prefix in /proc/self/mountinfo.
func mountReadOnly(dir string) (bool, bool) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false, false
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, false
	}
	best := ""
	bestRO := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		// mountinfo: id parent major:minor root mountpoint options ...
		mp := unescapeMount(fields[4])
		opts := strings.Split(fields[5], ",")
		if !fsutil.IsWithin(mp, real) {
			continue
		}
		if len(mp) > len(best) {
			best = mp
			bestRO = false
			for _, o := range opts {
				if o == "ro" {
					bestRO = true
				}
			}
		}
	}
	if best == "" {
		return false, false
	}
	return bestRO, true
}

// unescapeMount reverses the octal escaping the kernel uses in mountinfo.
func unescapeMount(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// probeNotWritable reports an error when the media tree IS writable by this
// user. It is a note rather than a failure: the design expects read-only, but a
// read-only mount is the operator's job, not albumctl's.
//
// Only called when the operator passes --probe-writes, because it writes into
// the media tree.
func probeNotWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".albumctl-write-probe-*")
	if err != nil {
		return nil // not writable: the expected outcome
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return fmt.Errorf("write probe succeeded at %s", name)
}

func humanBytes(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	v := float64(n)
	i := -1
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	s := fmt.Sprintf("%.1f %s", v, units[i])
	if neg {
		return "-" + s
	}
	return s
}

func pctOf(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	return a / b * 100
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none recorded)"
	}
	return s
}

func orZero(ts int64) string {
	if ts == 0 {
		return "never"
	}
	return time.Unix(ts, 0).Format(time.RFC3339)
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

var _ = sort.Strings
