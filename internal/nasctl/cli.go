// Package nasctl is the console's command-line interface.
//
// It is a thin adapter over the same job layer the HTTP API uses, not a second
// implementation of the rules. Every command that does something goes through
// jobs.Manager, so a permission check, a mode gate, an approval and an audit row
// behave identically whether the call came from a terminal or a browser. The
// alternative - a CLI with its own shortcuts - is how a console ends up with two
// policies and one of them undocumented.
package nasctl

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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/buildinfo"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/hubops"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
	"github.com/YoisakiKnd/nasctl/internal/resource"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
	"github.com/YoisakiKnd/nasctl/internal/suzu"
)

// Env carries what the commands need.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Now is injectable so that output is reproducible in tests.
	Now func() time.Time
}

// Main runs the CLI and returns a process exit code.
func Main(argv []string, env Env) int {
	if env.Stdin == nil {
		env.Stdin = os.Stdin
	}
	if env.Stdout == nil {
		env.Stdout = os.Stdout
	}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}
	if env.Now == nil {
		env.Now = time.Now
	}

	// The global flags are extracted from the whole command line rather than
	// parsed by a subcommand's flag set, so that `nasctl --config X status` and
	// `nasctl status --config X` both work. An operator should not have to
	// remember which side of the verb the flag goes on.
	args, globals, err := extractGlobals(argv)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 2
	}
	if len(args) == 0 {
		usage(env.Stdout)
		return 2
	}

	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "help", "-h", "--help":
		usage(env.Stdout)
		return 0
	case "version":
		fmt.Fprintf(env.Stdout, "nasctl %s (schema %d)\n", buildinfo.Version, buildinfo.Schema)
		return 0
	case "init":
		ctx, stop := signalContext()
		defer stop()
		return cmdInit(ctx, env, globals, rest)
	}

	cfg, err := loadCommandConfig(globals, cmd, rest)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}

	ctx, cancel := signalContext()
	defer cancel()
	// Catalogs and configuration share this lifetime lease even when --hub-db
	// points elsewhere. Offline restore cannot race a running NAS console.
	offlineRepair := len(rest) > 0 && (cmd == "users" && rest[0] == "recover" || cmd == "settings" && rest[0] == "reset")
	if cmd != "state" && !offlineRepair {
		if cmd == "serve" {
			if err := os.MkdirAll(cfg.Paths.StateDir, 0750); err != nil {
				fmt.Fprintln(env.Stderr, "nasctl:", err)
				return 1
			}
		}
		if _, err := os.Stat(cfg.Paths.StateDir); err == nil {
			lease, err := statelease.Acquire(cfg.Paths.StateDir, false)
			if err != nil {
				fmt.Fprintln(env.Stderr, "nasctl:", err)
				return 1
			}
			defer lease.Close()
			// Re-read under the lifetime lease: a completed offline restore may
			// have changed the overlay between the initial parse and acquisition.
			statePath := cfg.Paths.StateDir
			cfg, err = loadCommandConfig(globals, cmd, rest)
			if err != nil || cfg.Paths.StateDir != statePath {
				fmt.Fprintln(env.Stderr, "nasctl: configuration changed while acquiring state lease; retry after reviewing it")
				return 1
			}
		} else if !os.IsNotExist(err) {
			fmt.Fprintln(env.Stderr, "nasctl:", err)
			return 1
		}
	}

	switch cmd {
	case "status":
		return cmdStatus(ctx, env, cfg, globals)
	case "doctor":
		return cmdDoctor(ctx, env, cfg, globals)
	case "serve":
		return cmdServe(ctx, env, cfg, globals, rest)
	case "ops":
		return cmdOps(env, cfg, globals)
	case "audit":
		return cmdAudit(ctx, env, cfg, globals)
	case "files":
		return cmdFiles(ctx, env, cfg, globals, rest)
	case "state":
		return cmdState(ctx, env, cfg, globals, rest)
	case "users":
		return cmdUsers(ctx, env, cfg, globals, rest)
	case "settings":
		return cmdSettings(ctx, env, cfg, globals, rest)
	case "mode":
		return cmdMode(ctx, env, cfg, globals, rest)
	case "approvals":
		return cmdApprovals(ctx, env, cfg, globals, rest)
	case "media":
		return cmdMedia(ctx, env, cfg, globals, rest)
	case "jobs":
		return cmdJobs(ctx, env, cfg, globals, rest)
	default:
		fmt.Fprintf(env.Stderr, "nasctl: unknown command %q\n\n", cmd)
		usage(env.Stderr)
		return 2
	}
}

// globals holds the flags that apply to every command.
type globals struct {
	configPath string
	jsonOut    bool
	hubDB      string
	asAgent    string
}

// extractGlobals pulls the global flags out of the argument list.
func extractGlobals(argv []string) ([]string, globals, error) {
	var g globals
	var out []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		next := func(name string) (string, error) {
			if i+1 >= len(argv) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return argv[i], nil
		}
		switch {
		case a == "--config" || a == "-c":
			v, err := next(a)
			if err != nil {
				return nil, g, err
			}
			g.configPath = v
		case strings.HasPrefix(a, "--config="):
			g.configPath = strings.TrimPrefix(a, "--config=")
		case a == "--json":
			g.jsonOut = true
		case a == "--hub-db":
			v, err := next(a)
			if err != nil {
				return nil, g, err
			}
			g.hubDB = v
		case strings.HasPrefix(a, "--hub-db="):
			g.hubDB = strings.TrimPrefix(a, "--hub-db=")
		case a == "--agent":
			v, err := next(a)
			if err != nil {
				return nil, g, err
			}
			g.asAgent = v
		case strings.HasPrefix(a, "--agent="):
			g.asAgent = strings.TrimPrefix(a, "--agent=")
		default:
			out = append(out, a)
		}
	}
	return out, g, nil
}

// loadConfig loads and validates the configuration.
func loadConfig(g globals) (*config.Config, error) {
	path := g.configPath
	if path == "" {
		path = "/etc/albumctl/config.toml"
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("cannot read the configuration at %s: %w (use --config to "+
			"point at another file)", path, err)
	}
	return config.LoadNAS(path)
}

func loadCommandConfig(g globals, command string, args []string) (*config.Config, error) {
	maintenance := len(args) > 0 && (command == "state" && args[0] == "recover" || command == "users" && (args[0] == "password" || args[0] == "setup-code" || args[0] == "recover") || command == "settings" && args[0] == "reset")
	if !maintenance {
		return loadConfig(g)
	}
	path := g.configPath
	if path == "" {
		path = "/etc/albumctl/config.toml"
	}
	return config.Load(path)
}

// openStore opens the console's database.
func openStore(cfg *config.Config, g globals) (*hubstore.Store, error) {
	path := g.hubDB
	if path == "" {
		path = filepath.Join(cfg.Paths.StateDir, "hub.db")
	}
	return hubstore.Open(path, hubstore.Limits{
		MaxOpenJobs: cfg.Hub.Jobs.MaxOpenJobs,
		MaxHistory:  cfg.Hub.Jobs.MaxHistory,
		AuditDays:   cfg.Hub.Jobs.AuditRetentionDays,
	})
}

// newManager wires a job manager with the guard, as the console would.
func newManager(cfg *config.Config, st *hubstore.Store, g globals) (*jobs.Manager, error) {
	reg, err := operations.New(hubops.Declarations()...)
	if err != nil {
		return nil, err
	}
	sampler := resource.NewSampler("/proc", "")
	guard := resource.NewGuard(resource.Limits{
		StartMemAvailableKB:    cfg.Guard.StartMemAvailableKB,
		WarnMemAvailableKB:     cfg.Guard.WarnMemAvailableKB,
		StopMemAvailableKB:     cfg.Guard.StopMemAvailableKB,
		WorkerMaxBytes:         int64(cfg.Guard.WorkerMemMaxMB) << 20,
		WorkerHighBytes:        int64(cfg.Guard.WorkerMemHighMB) << 20,
		PSIMemorySomeAvg10Stop: cfg.Guard.PSIMemorySomeAvg10Stop,
		PSIIOSomeAvg10Stop:     cfg.Guard.PSIIOSomeAvg10Stop,
		MaxSwapUsedKB:          cfg.Guard.MaxSwapUsedKB,
		StartHold:              cfg.Guard.StartHold.D(),
		SampleInterval:         cfg.Guard.SampleInterval.D(),
	}, sampler)
	// The native console can be developed on macOS. Linux-specific resource
	// sampling remains mandatory for albumctl decoders, which are separate.
	if runtime.GOOS != "linux" {
		guard = nil
	}

	return jobs.New(jobs.Config{
		Registry: reg,
		Store:    st,
		Guard:    guard,
		Now:      time.Now,
	})
}

// principalFor returns the calling principal: a local operator, or an agent when
// one is named.
//
// The distinction is not cosmetic. A local operator acts on their own authority;
// an agent is subject to the approval rules and may not start heavy work unless
// the operator has enabled it.
func principalFor(g globals) domain.Principal {
	if strings.TrimSpace(g.asAgent) != "" {
		return domain.Principal{Kind: domain.PrincipalAI, Name: sanitise(g.asAgent)}
	}
	return domain.Principal{Kind: domain.PrincipalLocal, Name: os.Getenv("USER")}
}

func sanitise(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// signalContext cancels on SIGINT or SIGTERM so a long command stops cleanly.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// emit writes either JSON or a human rendering.
func emit(w io.Writer, asJSON bool, v any, human func(io.Writer)) {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		return
	}
	human(w)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `nasctl - the OneCloud Hub console

Usage:
  nasctl [global flags] COMMAND [arguments]

Commands:
  status              Machine, mode, jobs and storage in one view
  doctor              Read-only check of configuration, storage and resources
  serve               Run the web console (foreground)
  init                Create a new private NAS config; never overwrite
  users list|setup-code|create|set|password|remove|recover
                      Local account administration; passwords via stdin
  settings reset      Offline repair of saved settings; --confirm required
  files list          List a directory in a storage root
  files stat          Show one entry's metadata
  files search        Bounded search for a name
  files mkdir|rename|upload|copy|move|trash|restore|purge|batch
                      Operator writes; --confirm required (use -h for flags)
  state backup|verify|restore|recover
                      Private offline service state; see docs/STATE-BACKUPS.md
  ops                 Print the operation table
  jobs list           List job history
  jobs show ID        Show one job and its event history
  mode                Show or change the operating mode
  approvals           List or revoke approvals
  media               Report the IWantWatchAnime integration
  audit               Print recent audited actions
  version             Print the version
  help                Print this text

Global flags:
  --config PATH       Configuration file (default /etc/albumctl/config.toml)
  --hub-db PATH       Override the console database location
  --json              Machine-readable output
  --agent NAME        Act as the named AI principal instead of the local operator

Notes:
  This console does not scan, decode or generate thumbnails. Those are bounded
  tasks of the album service, run as separate commands with their own limits.
  Console file writes require an opt-in writable root and exact operator consent.
  File limits and batch/CLI workflows are documented in docs/FILES.md.
`)
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

// statusDoc is the machine-readable status.
type statusDoc struct {
	Version       string             `json:"version"`
	Now           int64              `json:"now"`
	Mode          domain.ModeState   `json:"mode"`
	Modes         []domain.ModeState `json:"modes"`
	Warnings      []string           `json:"warnings"`
	Jobs          map[string]int     `json:"job_counts"`
	OpenJobs      int                `json:"open_jobs"`
	Attention     []hubstore.Job     `json:"attention,omitempty"`
	Resources     *resourceDoc       `json:"resources,omitempty"`
	ResourceError string             `json:"resource_error,omitempty"`
	Roots         []files.RootInfo   `json:"roots"`
	ReadOnly      bool               `json:"read_only"`
	AllowAIHeavy  bool               `json:"allow_ai_heavy"`
}

type resourceDoc struct {
	MemAvailableKB int64   `json:"mem_available_kb"`
	MemTotalKB     int64   `json:"mem_total_kb"`
	SwapUsedKB     int64   `json:"swap_used_kb"`
	Verdict        string  `json:"verdict"`
	Reason         string  `json:"reason,omitempty"`
	Detail         string  `json:"detail,omitempty"`
	PSIMemAvg10    float64 `json:"psi_memory_some_avg10,omitempty"`
	PSIIOAvg10     float64 `json:"psi_io_some_avg10,omitempty"`
}

func cmdStatus(ctx context.Context, env Env, cfg *config.Config, g globals) int {
	st, err := openStore(cfg, g)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	defer st.Close()
	jm, err := newManager(cfg, st, g)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}

	doc := statusDoc{
		Version: buildinfo.Version, Now: env.Now().Unix(),
		Warnings: []string{}, Jobs: map[string]int{}, Roots: []files.RootInfo{},
		ReadOnly: false, AllowAIHeavy: jm.AllowAIHeavy(),
	}
	modes, err := st.Modes(ctx)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	doc.Modes = modes.States
	doc.Mode = modes.Effective(env.Now())
	if !doc.Mode.AllowsHeavyWork() {
		doc.Warnings = append(doc.Warnings, fmt.Sprintf("heavy work is stopped by mode %q (%s)",
			doc.Mode.Mode, doc.Mode.Source))
	}
	counts, err := st.CountByState(ctx)
	if err == nil {
		doc.Jobs = counts
	}
	doc.OpenJobs, _ = st.CountOpen(ctx)

	// The roots are opened through the files manager so that a missing mount is
	// reported as unavailable rather than as an empty directory.
	if fm, err := files.New(cfg.Hub.Storage, files.ConfigLimits(cfg.Hub.Files)); err == nil {
		doc.Roots = fm.Roots()
		_ = fm.Close()
	}
	for _, r := range doc.Roots {
		if !r.Available {
			doc.Warnings = append(doc.Warnings, fmt.Sprintf("storage %q is unavailable: %s",
				r.ID, r.Note))
		}
	}

	if guard := jm.Guard(); guard != nil {
		snap := guard.Sampler.Sample(0)
		if snap.MemErr != nil {
			doc.ResourceError = "the machine's memory could not be read: " + snap.MemErr.Error()
		} else {
			d := guard.BeforeFile()
			doc.Resources = &resourceDoc{
				MemAvailableKB: snap.Mem.MemAvailableKB,
				MemTotalKB:     snap.Mem.MemTotalKB,
				SwapUsedKB:     snap.Mem.SwapTotalKB - snap.Mem.SwapFreeKB,
				Verdict:        d.Verdict.String(),
				Reason:         string(d.Reason),
				Detail:         d.Detail,
				PSIMemAvg10:    snap.PSIMem.SomeAvg10,
				PSIIOAvg10:     snap.PSIIO.SomeAvg10,
			}
		}
	}

	// Jobs that need attention, so that a failed or unknown job is not something
	// an operator has to go looking for.
	open, _ := st.Jobs(ctx, hubstore.JobFilter{Open: true, Limit: 100})
	for _, j := range open {
		if j.State.NeedsAttention() {
			doc.Attention = append(doc.Attention, j)
			doc.Warnings = append(doc.Warnings, fmt.Sprintf("job %s (%s) is %s", j.ID, j.Op, j.State))
		}
	}

	emit(env.Stdout, g.jsonOut, doc, func(w io.Writer) {
		fmt.Fprintf(w, "nasctl %s\n", doc.Version)
		fmt.Fprintf(w, "mode:      %s (%s)\n", doc.Mode.Mode, orUnknown(doc.Mode.Source))
		if doc.Mode.Reason != "" {
			fmt.Fprintf(w, "           %s\n", doc.Mode.Reason)
		}
		if !doc.Mode.ExpiresAt.IsZero() {
			fmt.Fprintf(w, "           expires %s\n", doc.Mode.ExpiresAt.Format(time.RFC3339))
		}
		if doc.Resources != nil {
			r := doc.Resources
			fmt.Fprintf(w, "memory:    %s available of %s, swap used %s\n",
				mib(r.MemAvailableKB), mib(r.MemTotalKB), mib(r.SwapUsedKB))
			fmt.Fprintf(w, "guard:     %s", r.Verdict)
			if r.Reason != "" {
				fmt.Fprintf(w, " (%s)", r.Reason)
			}
			fmt.Fprintln(w)
			if r.Detail != "" {
				fmt.Fprintf(w, "           %s\n", r.Detail)
			}
		} else {
			fmt.Fprintf(w, "memory:    unknown (%s)\n", doc.ResourceError)
		}
		fmt.Fprintf(w, "jobs:      %d open\n", doc.OpenJobs)
		for _, k := range sortedKeys(doc.Jobs) {
			fmt.Fprintf(w, "           %-12s %d\n", k, doc.Jobs[k])
		}
		if len(doc.Roots) == 0 {
			fmt.Fprintf(w, "storage:   none configured\n")
		}
		for _, r := range doc.Roots {
			state := "available"
			if !r.Available {
				state = "UNAVAILABLE"
			}
			fmt.Fprintf(w, "storage:   %-10s %-12s %s\n", r.ID, state, r.Path)
			if r.Note != "" {
				fmt.Fprintf(w, "           %s\n", r.Note)
			}
		}
		if len(doc.Warnings) > 0 {
			fmt.Fprintf(w, "\nneeds attention:\n")
			for _, warn := range doc.Warnings {
				fmt.Fprintf(w, "  - %s\n", warn)
			}
		}
	})
	return 0
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown source"
	}
	return s
}

func mib(kb int64) string {
	return fmt.Sprintf("%.1f MiB", float64(kb)/1024)
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

// doctorDoc is the machine-readable doctor report.
type doctorDoc struct {
	OK       bool     `json:"ok"`
	Problems []string `json:"problems"`
	Notes    []string `json:"notes"`
	Checked  []string `json:"checked"`
}

func cmdDoctor(ctx context.Context, env Env, cfg *config.Config, g globals) int {
	var problems, notes, checked []string

	checked = append(checked, "configuration parsed and validated")
	checked = append(checked, fmt.Sprintf("state_dir %s", cfg.Paths.StateDir))

	// Storage roots: existence, directory-ness, and identity.
	fm, err := files.New(cfg.Hub.Storage, files.ConfigLimits(cfg.Hub.Files))
	if err != nil {
		problems = append(problems, "the storage roots could not be opened: "+err.Error())
	} else {
		roots := fm.Roots()
		checked = append(checked, fmt.Sprintf("%d storage root(s)", len(roots)))
		if len(roots) == 0 {
			notes = append(notes, "no [[hub.storage]] roots are configured, so the file "+
				"manager has nothing to browse")
		}
		for _, r := range roots {
			if !r.Available {
				problems = append(problems, fmt.Sprintf("storage %q: %s", r.ID, r.Note))
				continue
			}
			// A root that is inside the state or cache directory would let a
			// browsable path reach the console's own files.
			for _, reserved := range []string{cfg.Paths.StateDir, cfg.Paths.CacheDir} {
				if reserved != "" && pathWithin(r.Path, reserved) {
					problems = append(problems, fmt.Sprintf("storage %q (%s) is inside the "+
						"console's own state directory %s", r.ID, r.Path, reserved))
				}
			}
			checked = append(checked, fmt.Sprintf("storage %q at %s", r.ID, r.Path))
		}
		_ = fm.Close()
	}

	// The state directory must be writable, since the console records jobs there.
	if err := checkWritable(cfg.Paths.StateDir); err != nil {
		problems = append(problems, "state_dir: "+err.Error())
	} else {
		checked = append(checked, "state_dir is writable")
	}

	// The database.
	st, err := openStore(cfg, g)
	if err != nil {
		problems = append(problems, "the console database could not be opened: "+err.Error())
	} else {
		if out, err := st.IntegrityCheck(ctx); err != nil {
			problems = append(problems, "the console database failed its integrity check: "+err.Error())
		} else if out != "ok" {
			problems = append(problems, "the console database reports: "+out)
		} else {
			checked = append(checked, fmt.Sprintf("console database %s", st.Path()))
		}
		st.Close()
	}

	// suzu, when configured.
	if cfg.Hub.Suzu.DBPath != "" {
		sc, err := suzu.Open(suzu.Config{
			DBPath: cfg.Hub.Suzu.DBPath, APIBase: cfg.Hub.Suzu.APIBase,
			Token: cfg.Hub.Suzu.Token, LibraryRoot: cfg.Hub.Suzu.LibraryRoot,
		})
		if err != nil {
			problems = append(problems, "the IWantWatchAnime integration: "+err.Error())
		} else {
			checked = append(checked, "IWantWatchAnime database opened read-only")
			if cfg.Hub.Suzu.LibraryRoot == "" {
				notes = append(notes, "hub.suzu.library_root is empty, so episode playback is "+
					"disabled; the console will not guess where files are")
			}
			sc.Close()
		}
	} else {
		notes = append(notes, "the IWantWatchAnime integration is not configured")
	}

	// Resources: whether the machine's condition can be read at all, and whether
	// heavy work could be admitted.
	sampler := resource.NewSampler("/proc", "")
	snap := sampler.Sample(0)
	if snap.MemErr != nil {
		problems = append(problems, "the machine's memory cannot be read: "+snap.MemErr.Error()+
			"; heavy work must be treated as disabled")
	} else {
		checked = append(checked, fmt.Sprintf("memory readable: %s available of %s",
			mib(snap.Mem.MemAvailableKB), mib(snap.Mem.MemTotalKB)))
		if snap.Mem.MemAvailableKB < cfg.Guard.WarnMemAvailableKB {
			notes = append(notes, fmt.Sprintf("available memory (%s) is below the warning "+
				"threshold (%s)", mib(snap.Mem.MemAvailableKB), mib(cfg.Guard.WarnMemAvailableKB)))
		}
	}

	// cgroup accounting: without it, a worker's memory cannot be bounded, and the
	// project's own rule is that heavy work is disabled when the limit cannot be
	// enforced.
	cap := resource.ProbeCapability(sampler, nil)
	if probs := cap.CapabilityProblems(cfg.Guard.RequireCgroupIsolation); len(probs) > 0 {
		for _, p := range probs {
			if cfg.Guard.RequireCgroupIsolation {
				problems = append(problems, p)
			} else {
				notes = append(notes, p)
			}
		}
	} else {
		checked = append(checked, "cgroup memory accounting is available")
	}

	// The bind address, which is this console's access control.
	if err := checkBind(cfg); err != nil {
		problems = append(problems, err.Error())
	} else {
		checked = append(checked, fmt.Sprintf("bind %s", cfg.Server.Bind))
	}

	doc := doctorDoc{
		OK: len(problems) == 0, Problems: emptyIfNil(problems),
		Notes: emptyIfNil(notes), Checked: checked,
	}
	emit(env.Stdout, g.jsonOut, doc, func(w io.Writer) {
		for _, c := range checked {
			fmt.Fprintf(w, "  ok    %s\n", c)
		}
		for _, n := range notes {
			fmt.Fprintf(w, "  note  %s\n", n)
		}
		for _, p := range problems {
			fmt.Fprintf(w, "  FAIL  %s\n", p)
		}
		if len(problems) == 0 {
			fmt.Fprintf(w, "\nno problems found; %d check(s) passed\n", len(checked))
		} else {
			fmt.Fprintf(w, "\n%d problem(s) found\n", len(problems))
		}
	})
	if len(problems) > 0 {
		return 1
	}
	return 0
}

// checkBind validates the console's bind address.
//
// It duplicates a small amount of the web layer's rule rather than importing it,
// because importing the web layer would drag its dependencies into the CLI for
// one string check. The rule itself is the same and is tested on both sides.
func checkBind(cfg *config.Config) error {
	bind := cfg.Hub.Bind
	if bind == "" {
		bind = cfg.Server.Bind
	}
	if bind == "" {
		return errors.New("no bind address is configured for the console")
	}
	if !cfg.Server.AllowNonPrivateBind {
		host := bind
		if i := strings.LastIndex(bind, ":"); i >= 0 {
			host = bind[:i]
		}
		host = strings.Trim(host, "[]")
		if host == "" || host == "0.0.0.0" || host == "::" {
			return fmt.Errorf("the console would bind to every interface (%s); it has no "+
				"per-user authentication, so set a Tailscale or loopback address", bind)
		}
	}
	return nil
}

// checkWritable reports whether a directory can be written to, without creating
// anything in it.
func checkWritable(dir string) error {
	if dir == "" {
		return errors.New("no directory is configured")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%w (create it, or point paths.state_dir elsewhere)", err)
	}
	if !fi.IsDir() {
		return errors.New("the path exists but is not a directory")
	}
	// Probing by writing would modify the operator's machine during a read-only
	// check. The permission bits are the honest, side-effect-free answer; they
	// are not a guarantee, and the report says what was checked.
	if fi.Mode().Perm()&0o200 == 0 {
		return fmt.Errorf("the directory is not writable by its owner (mode %s)", fi.Mode().Perm())
	}
	return nil
}

// pathWithin reports whether inner is inside outer.
func pathWithin(inner, outer string) bool {
	a, err1 := filepath.Abs(inner)
	b, err2 := filepath.Abs(outer)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(b, a)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---------------------------------------------------------------------------
// ops
// ---------------------------------------------------------------------------

func cmdOps(env Env, cfg *config.Config, g globals) int {
	reg, err := operations.New(hubops.Declarations()...)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	type opDoc struct {
		ID       string   `json:"id"`
		Summary  string   `json:"summary"`
		Scope    []string `json:"scope"`
		Class    string   `json:"class"`
		Approval string   `json:"approval"`
		Timeout  string   `json:"timeout"`
		Async    bool     `json:"async"`
		Mutex    string   `json:"mutex,omitempty"`
		ReadOnly bool     `json:"read_only"`
	}
	var docs []opDoc
	for _, op := range reg.All() {
		docs = append(docs, opDoc{
			ID: op.ID, Summary: op.Summary, Scope: op.Scope, Class: string(op.Class),
			Approval: string(op.Approval), Timeout: op.Timeout.String(),
			Async: op.Async, Mutex: string(op.Mutex), ReadOnly: op.ReadOnly,
		})
	}
	emit(env.Stdout, g.jsonOut, map[string]any{"operations": docs}, func(w io.Writer) {
		fmt.Fprintf(w, "%-24s %-11s %-15s %-6s %s\n", "OPERATION", "CLASS", "APPROVAL", "ASYNC", "SUMMARY")
		for _, d := range docs {
			async := "no"
			if d.Async {
				async = "yes"
			}
			fmt.Fprintf(w, "%-24s %-11s %-15s %-6s %s\n", d.ID, d.Class, d.Approval, async, d.Summary)
		}
		fmt.Fprintf(w, "\n%d operations. Read-only operations need no approval; everything "+
			"else is gated by the job layer.\n", len(docs))
	})
	return 0
}

// ---------------------------------------------------------------------------
// audit
// ---------------------------------------------------------------------------

func cmdAudit(ctx context.Context, env Env, cfg *config.Config, g globals) int {
	st, err := openStore(cfg, g)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	defer st.Close()
	rows, err := st.Audit(ctx, 100, 0)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	emit(env.Stdout, g.jsonOut, map[string]any{"entries": rows}, func(w io.Writer) {
		if len(rows) == 0 {
			fmt.Fprintln(w, "nothing has been recorded yet")
			return
		}
		for _, e := range rows {
			fmt.Fprintf(w, "%s  %-18s %-16s %-10s %s\n",
				e.At.Format("2006-01-02 15:04:05"), e.Principal, e.Action, e.Outcome, e.Note)
		}
	})
	return 0
}

// ---------------------------------------------------------------------------
// files
// ---------------------------------------------------------------------------

func cmdFiles(ctx context.Context, env Env, cfg *config.Config, g globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "nasctl files: expected roots, list, stat, search, mkdir, rename, upload, copy, move, trash, restore, purge or batch")
		return 2
	}
	fm, err := files.New(cfg.Hub.Storage, files.ConfigLimits(cfg.Hub.Files))
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	defer fm.Close()

	sub := args[0]
	fs := flag.NewFlagSet("files "+sub, flag.ContinueOnError)
	switch sub {
	case "mkdir", "rename", "upload", "copy", "move", "trash", "restore", "purge", "batch":
		return cmdFileWrite(ctx, env, cfg, g, fm, sub, args[1:])
	}
	fs.SetOutput(env.Stderr)
	root := fs.String("root", "", "storage root ID")
	pathFlag := fs.String("path", "/", "root-relative path")
	sortFlag := fs.String("sort", "name", "sort key: name, size, time, kind")
	orderFlag := fs.String("order", "asc", "sort direction: asc or desc")
	page := fs.Int("page", 1, "page number")
	size := fs.Int("page-size", 50, "entries per page")
	query := fs.String("q", "", "search text")
	depth := fs.Int("depth", 2, "search depth")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	switch sub {
	case "roots":
		emit(env.Stdout, g.jsonOut, map[string]any{"roots": fm.Roots()}, func(w io.Writer) {
			for _, r := range fm.Roots() {
				state := "available"
				if !r.Available {
					state = "UNAVAILABLE"
				}
				fmt.Fprintf(w, "%-10s %-12s %s\n", r.ID, state, r.Path)
				if r.Note != "" {
					fmt.Fprintf(w, "           %s\n", r.Note)
				}
			}
		})
		return 0
	case "list":
		if *root == "" {
			fmt.Fprintln(env.Stderr, "nasctl files list: --root is required")
			return 2
		}
		l, err := fm.List(*root, files.ListOptions{
			Path: *pathFlag, Page: *page, PageSize: *size,
			Sort: files.ParseSort(*sortFlag), Order: files.ParseOrder(*orderFlag),
		})
		if err != nil {
			fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
			return 1
		}
		emit(env.Stdout, g.jsonOut, l, func(w io.Writer) {
			for _, e := range l.Entries {
				name := e.Name
				if e.IsDir {
					name += "/"
				}
				if e.Symlink {
					name += " ->"
				}
				fmt.Fprintf(w, "%-10s %12s  %s  %s\n",
					e.Kind, humanSize(e.Size), e.ModTime.Format("2006-01-02 15:04"), name)
			}
			fmt.Fprintf(w, "\n%d entries, page %d of %d", l.Total, l.Page, max(1, l.Pages()))
			if l.Truncated {
				fmt.Fprintf(w, " (truncated: this directory holds more entries than the console examines)")
			}
			fmt.Fprintln(w)
		})
		return 0
	case "stat":
		if *root == "" || *pathFlag == "" {
			fmt.Fprintln(env.Stderr, "nasctl files stat: --root and --path are required")
			return 2
		}
		e, err := fm.Stat(*root, *pathFlag)
		if err != nil {
			fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
			return 1
		}
		emit(env.Stdout, g.jsonOut, e, func(w io.Writer) {
			fmt.Fprintf(w, "name     %s\n", e.Name)
			fmt.Fprintf(w, "path     %s\n", e.Path)
			fmt.Fprintf(w, "kind     %s\n", e.Kind)
			fmt.Fprintf(w, "size     %s (%d bytes)\n", humanSize(e.Size), e.Size)
			fmt.Fprintf(w, "modified %s\n", e.ModTime.Format(time.RFC3339))
			fmt.Fprintf(w, "mode     %s\n", e.Mode)
			if e.Symlink {
				fmt.Fprintf(w, "note     a symbolic link; following it is refused unless it stays inside the root\n")
			}
		})
		return 0
	case "search":
		if *root == "" || strings.TrimSpace(*query) == "" {
			fmt.Fprintln(env.Stderr, "nasctl files search: --root and -q are required")
			return 2
		}
		res, err := fm.Search(*root, files.SearchOptions{
			Path: *pathFlag, Query: *query, MaxDepth: *depth,
		})
		if err != nil {
			fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
			return 1
		}
		emit(env.Stdout, g.jsonOut, res, func(w io.Writer) {
			for _, e := range res.Entries {
				fmt.Fprintf(w, "%-10s %12s  %s\n", e.Kind, humanSize(e.Size), e.Path)
			}
			fmt.Fprintf(w, "\n%d result(s), %d examined in %dms", len(res.Entries), res.Examined, res.ElapsedMS)
			if res.Truncated {
				fmt.Fprintf(w, " (stopped at the %s)", res.Reason)
			}
			fmt.Fprintln(w)
		})
		return 0
	default:
		fmt.Fprintf(env.Stderr, "nasctl files: unknown subcommand %q\n", sub)
		return 2
	}
}

func humanSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	i := -1
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// ---------------------------------------------------------------------------
// mode
// ---------------------------------------------------------------------------

func cmdMode(ctx context.Context, env Env, cfg *config.Config, g globals, args []string) int {
	st, err := openStore(cfg, g)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	defer st.Close()
	jm, err := newManager(cfg, st, g)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	p := principalFor(g)

	if len(args) == 0 || args[0] == "show" {
		modes, err := st.Modes(ctx)
		if err != nil {
			fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
			return 1
		}
		emit(env.Stdout, g.jsonOut, map[string]any{
			"states": modes.States, "effective": modes.Effective(env.Now()),
		}, func(w io.Writer) {
			eff := modes.Effective(env.Now())
			fmt.Fprintf(w, "effective: %s (%s)\n", eff.Mode, orUnknown(eff.Source))
			if eff.Reason != "" {
				fmt.Fprintf(w, "reason:    %s\n", eff.Reason)
			}
			if len(modes.States) == 0 {
				fmt.Fprintln(w, "stored:    none")
			}
			for _, s := range modes.States {
				exp := "no expiry"
				if !s.ExpiresAt.IsZero() {
					exp = "expires " + s.ExpiresAt.Format(time.RFC3339)
				}
				fmt.Fprintf(w, "stored:    %-14s %s, %s\n", s.Mode, s.Source, exp)
			}
		})
		return 0
	}

	switch args[0] {
	case "watch":
		dur := "2h"
		if len(args) > 1 {
			dur = args[1]
		}
		return applyMode(ctx, env, jm, p, domain.ModeWatching, dur, "watching something", g)
	case "maintenance":
		dur := "1h"
		if len(args) > 1 {
			dur = args[1]
		}
		return applyMode(ctx, env, jm, p, domain.ModeMaintenance, dur, "maintenance window", g)
	case "pause":
		reason := "paused from the command line"
		if len(args) > 1 {
			reason = strings.Join(args[1:], " ")
		}
		if err := jm.StartMode(ctx, domain.ModeState{
			Mode: domain.ModeManualPause, Reason: reason,
		}, p); err != nil {
			fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
			return 1
		}
		fmt.Fprintln(env.Stdout, "paused. This stops this console's heavy work; it does not "+
			"stop the album service's own commands, which you should also not start.")
		return 0
	case "clear":
		if len(args) < 2 {
			fmt.Fprintln(env.Stderr, "nasctl mode clear: name a mode (watching, maintenance, manual_pause)")
			return 2
		}
		mode := domain.Mode(args[1])
		if err := jm.ClearMode(ctx, mode, p); err != nil {
			fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
			return 1
		}
		fmt.Fprintf(env.Stdout, "cleared %s\n", mode)
		return 0
	default:
		fmt.Fprintf(env.Stderr, "nasctl mode: unknown subcommand %q\n", args[0])
		return 2
	}
}

func applyMode(ctx context.Context, env Env, jm *jobs.Manager, p domain.Principal,
	mode domain.Mode, dur, reason string, g globals) int {
	d, err := time.ParseDuration(dur)
	if err != nil || d <= 0 {
		fmt.Fprintf(env.Stderr, "nasctl: %q is not a positive duration such as 2h\n", dur)
		return 2
	}
	if d > 24*time.Hour {
		fmt.Fprintf(env.Stderr, "nasctl: a %s window may not exceed 24h\n", mode)
		return 2
	}
	if err := jm.StartMode(ctx, domain.ModeState{
		Mode: mode, Reason: reason, ExpiresAt: env.Now().Add(d),
	}, p); err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "%s until %s\n", mode, env.Now().Add(d).Format(time.RFC3339))
	return 0
}

// ---------------------------------------------------------------------------
// approvals
// ---------------------------------------------------------------------------

func cmdApprovals(ctx context.Context, env Env, cfg *config.Config, g globals, args []string) int {
	st, err := openStore(cfg, g)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	defer st.Close()
	jm, err := newManager(cfg, st, g)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}

	if len(args) > 0 && args[0] == "revoke" {
		if len(args) < 2 {
			fmt.Fprintln(env.Stderr, "nasctl approvals revoke: an approval ID is required")
			return 2
		}
		if err := jm.Revoke(ctx, args[1], principalFor(g)); err != nil {
			fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
			return 1
		}
		fmt.Fprintf(env.Stdout, "revoked %s\n", args[1])
		return 0
	}

	if len(args) > 0 && args[0] == "grant" {
		fs := flag.NewFlagSet("approvals grant", flag.ContinueOnError)
		fs.SetOutput(env.Stderr)
		opID := fs.String("op", "", "operation ID")
		forWhom := fs.String("for", "", "principal, e.g. ai:hermes")
		target := fs.String("target", "", "the target the approval covers")
		params := fs.String("params", "", "the exact parameter string the approval covers")
		dur := fs.String("duration", "1h", "how long the approval lasts")
		oneShot := fs.Bool("one-shot", true, "consume the approval on first use")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *opID == "" || *forWhom == "" {
			fmt.Fprintln(env.Stderr, "nasctl approvals grant: --op and --for are required")
			return 2
		}
		d, err := time.ParseDuration(*dur)
		if err != nil || d <= 0 {
			fmt.Fprintf(env.Stderr, "nasctl: %q is not a positive duration\n", *dur)
			return 2
		}
		kind := domain.PrincipalAI
		name := *forWhom
		if i := strings.Index(*forWhom, ":"); i > 0 {
			kind = domain.PrincipalKind((*forWhom)[:i])
			name = (*forWhom)[i+1:]
		}
		a, err := jm.Grant(ctx, jobs.GrantRequest{
			OpID: *opID, For: domain.Principal{Kind: kind, Name: name},
			Target: *target, Params: *params,
			GrantedBy: principalFor(g), ExpiresAt: env.Now().Add(d), OneShot: *oneShot,
		})
		if err != nil {
			fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
			return 1
		}
		emit(env.Stdout, g.jsonOut, a, func(w io.Writer) {
			fmt.Fprintf(w, "granted %s\n", a.ID)
			fmt.Fprintf(w, "  operation  %s\n", a.Op)
			fmt.Fprintf(w, "  for        %s\n", a.Principal)
			fmt.Fprintf(w, "  target     %s\n", a.Target)
			fmt.Fprintf(w, "  params     %s\n", a.Params)
			fmt.Fprintf(w, "  expires    %s\n", a.ExpiresAt.Format(time.RFC3339))
			fmt.Fprintf(w, "  granted by %s\n", a.GrantedBy)
		})
		return 0
	}

	rows, err := st.Approvals(ctx, 100)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	emit(env.Stdout, g.jsonOut, map[string]any{"approvals": rows}, func(w io.Writer) {
		if len(rows) == 0 {
			fmt.Fprintln(w, "no approvals are outstanding")
			return
		}
		for _, a := range rows {
			state := "valid"
			if !a.UsedAt.IsZero() {
				state = "used"
			} else if env.Now().After(a.ExpiresAt) {
				state = "expired"
			}
			fmt.Fprintf(w, "%-14s %-8s %-20s %-18s %s\n", a.ID, state, a.Op, a.Principal, a.Target)
		}
	})
	return 0
}

// ---------------------------------------------------------------------------
// media
// ---------------------------------------------------------------------------

func cmdMedia(ctx context.Context, env Env, cfg *config.Config, g globals, args []string) int {
	sc, err := suzu.Open(suzu.Config{
		DBPath: cfg.Hub.Suzu.DBPath, APIBase: cfg.Hub.Suzu.APIBase,
		Token: cfg.Hub.Suzu.Token, LibraryRoot: cfg.Hub.Suzu.LibraryRoot,
	})
	if err != nil {
		if errors.Is(err, suzu.ErrNotConfigured) {
			fmt.Fprintln(env.Stdout, "the IWantWatchAnime integration is not configured. Set "+
				"hub.suzu.db_path to enable it.")
			return 0
		}
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	defer sc.Close()

	st := sc.Status(ctx)
	if len(args) == 0 || args[0] == "status" {
		emit(env.Stdout, g.jsonOut, st, func(w io.Writer) {
			fmt.Fprintf(w, "database   %s\n", st.DBPath)
			if st.APIBase != "" {
				fmt.Fprintf(w, "api        %s\n", st.APIBase)
			}
			fmt.Fprintf(w, "tables     %s\n", strings.Join(st.Tables, ", "))
			fmt.Fprintf(w, "library    %s\n", countOrUnknown(st.LibraryCount))
			fmt.Fprintf(w, "subs       %s\n", countOrUnknown(st.SubscriptionCount))
			fmt.Fprintf(w, "tasks      %s\n", countOrUnknown(st.TaskCount))
			fmt.Fprintf(w, "\n%s\n", st.Note)
		})
		return 0
	}
	if args[0] == "library" {
		items, total, err := sc.Library(ctx, "", 50, 0)
		if err != nil {
			fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
			return 1
		}
		emit(env.Stdout, g.jsonOut, map[string]any{"items": items, "total": total}, func(w io.Writer) {
			for _, it := range items {
				mark := "  "
				if !it.Playable {
					mark = "! "
				}
				fmt.Fprintf(w, "%s%-50s %12s  %s\n", mark, truncateStr(it.Title, 50),
					humanSize(it.Size), it.Path)
				if it.Note != "" {
					fmt.Fprintf(w, "  %s\n", it.Note)
				}
			}
			fmt.Fprintf(w, "\n%d item(s)\n", total)
		})
		return 0
	}
	fmt.Fprintf(env.Stderr, "nasctl media: unknown subcommand %q\n", args[0])
	return 2
}

func countOrUnknown(n *int) string {
	if n == nil {
		return "unknown"
	}
	return strconv.Itoa(*n)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// ---------------------------------------------------------------------------
// jobs
// ---------------------------------------------------------------------------

func cmdJobs(ctx context.Context, env Env, cfg *config.Config, g globals, args []string) int {
	st, err := openStore(cfg, g)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	defer st.Close()

	if len(args) > 0 && args[0] == "show" {
		if len(args) < 2 {
			fmt.Fprintln(env.Stderr, "nasctl jobs show: a job ID is required")
			return 2
		}
		j, err := st.Job(ctx, args[1])
		if err != nil {
			fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
			return 1
		}
		events, err := st.JobEvents(ctx, args[1], 200)
		if err != nil {
			fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
			return 1
		}
		emit(env.Stdout, g.jsonOut, map[string]any{"job": j, "events": events}, func(w io.Writer) {
			fmt.Fprintf(w, "id         %s\n", j.ID)
			fmt.Fprintf(w, "operation  %s\n", j.Op)
			fmt.Fprintf(w, "state      %s\n", j.State)
			fmt.Fprintf(w, "by         %s\n", j.Principal)
			if j.Target != "" {
				fmt.Fprintf(w, "target     %s\n", j.Target)
			}
			if j.Params != "" {
				fmt.Fprintf(w, "params     %s\n", j.Params)
			}
			fmt.Fprintf(w, "created    %s\n", j.CreatedAt.Format(time.RFC3339))
			if !j.StartedAt.IsZero() {
				fmt.Fprintf(w, "started    %s\n", j.StartedAt.Format(time.RFC3339))
			}
			if !j.EndedAt.IsZero() {
				fmt.Fprintf(w, "ended      %s\n", j.EndedAt.Format(time.RFC3339))
			}
			if j.Total > 0 {
				fmt.Fprintf(w, "progress   %d / %d %s\n", j.Progress, j.Total, j.Unit)
			}
			if j.StopReason != "" {
				fmt.Fprintf(w, "stopped    %s\n", j.StopReason)
			}
			if j.Err != "" {
				fmt.Fprintf(w, "error      %s\n", j.Err)
			}
			fmt.Fprintf(w, "verified   %v\n", j.Verified)
			fmt.Fprintf(w, "\nhistory:\n")
			for _, e := range events {
				fmt.Fprintf(w, "  %s  %-12s -> %-12s %s\n",
					e.At.Format("15:04:05"), orDash(e.From), orDash(e.To), e.Note)
			}
		})
		return 0
	}

	fs := flag.NewFlagSet("jobs list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	openOnly := fs.Bool("open", false, "only jobs that still hold a slot")
	state := fs.String("state", "", "filter by state")
	limit := fs.Int("limit", 50, "rows to show")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	f := hubstore.JobFilter{Limit: *limit, Open: *openOnly}
	if *state != "" {
		if !domain.JobState(*state).Valid() {
			fmt.Fprintf(env.Stderr, "nasctl: %q is not a job state\n", *state)
			return 2
		}
		f.State = domain.JobState(*state)
	}
	rows, err := st.Jobs(ctx, f)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	emit(env.Stdout, g.jsonOut, map[string]any{"jobs": rows}, func(w io.Writer) {
		if len(rows) == 0 {
			fmt.Fprintln(w, "no jobs match")
			return
		}
		fmt.Fprintf(w, "%-18s %-20s %-12s %-16s %s\n", "ID", "OPERATION", "STATE", "BY", "WHEN")
		for _, j := range rows {
			fmt.Fprintf(w, "%-18s %-20s %-12s %-16s %s\n", j.ID, j.Op, j.State, j.Principal,
				j.CreatedAt.Format("2006-01-02 15:04"))
		}
	})
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
