// Package config loads and validates albumctl's TOML configuration.
//
// Every default in this package is deliberately conservative. The comments
// explaining each threshold live next to the default so that an operator
// reading the example file sees the reasoning, not just the number.
package config

import (
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration wraps time.Duration so TOML can express "30s" / "20m".
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("invalid duration %q", string(b))
	}
	if v < 0 {
		return fmt.Errorf("duration %q must not be negative", string(b))
	}
	d.Duration = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.Duration.String()), nil }

// D returns the underlying duration, tolerating the zero value.
func (d Duration) D() time.Duration { return d.Duration }

// Server configures the read-only web service.
type Server struct {
	Bind                   string   `toml:"bind"`
	AllowNonPrivateBind    bool     `toml:"allow_non_private_bind"`
	Token                  string   `toml:"token"`
	ReadTimeout            Duration `toml:"read_timeout"`
	ReadHeaderTimeout      Duration `toml:"read_header_timeout"`
	WriteTimeout           Duration `toml:"write_timeout"`
	IdleTimeout            Duration `toml:"idle_timeout"`
	ShutdownTimeout        Duration `toml:"shutdown_timeout"`
	MaxConcurrentRequests  int      `toml:"max_concurrent_requests"`
	MaxConcurrentStreams   int      `toml:"max_concurrent_streams"`
	PageDefault            int      `toml:"page_default"`
	PageMax                int      `toml:"page_max"`
	MaxResponseBytes       int64    `toml:"max_response_bytes"`
	StaticCacheSeconds     int      `toml:"static_cache_seconds"`
	ThumbPlaceholderSecs   int      `toml:"thumb_placeholder_cache_seconds"`
	ExposeResourceSnapshot bool     `toml:"expose_resource_snapshot"`
}

// Paths locates the writable state. Everything here must live on the system
// SSD, never on the media disks, so that index writes cannot disturb the array
// holding the originals and so that `doctor` can prove the separation.
type Paths struct {
	StateDir  string `toml:"state_dir"`
	CacheDir  string `toml:"cache_dir"`
	IndexFile string `toml:"index_file"`
	LogFile   string `toml:"log_file"`
	LockDir   string `toml:"lock_dir"`
	RunDir    string `toml:"run_dir"`
}

// Cache bounds thumbnail storage. Thumbnails are individual files inside a
// sharded tree, never blobs inside the database, so growth is inspectable and
// removable without touching the index.
type Cache struct {
	MaxBytes        int64 `toml:"max_bytes"`
	MaxFiles        int   `toml:"max_files"`
	ShardDepth      int   `toml:"shard_depth"`
	WarnAtPercent   int   `toml:"warn_at_percent"`
	OrphanGraceSecs int   `toml:"orphan_grace_seconds"`
}

// Guard holds the whole-machine resource policy. These numbers were chosen for
// a ~981 MiB machine that must keep SSH, Tailscale, Samba, FileBrowser and
// container workloads alive; they are starting points to be re-measured, not
// guarantees.
type Guard struct {
	// Whole-machine thresholds, compared against MemAvailable.
	StartMemAvailableKB int64 `toml:"start_mem_available_kb"`
	WarnMemAvailableKB  int64 `toml:"warn_mem_available_kb"`
	StopMemAvailableKB  int64 `toml:"stop_mem_available_kb"`

	// Sampling.
	SampleInterval Duration `toml:"sample_interval"`

	// StartHold is how long MemAvailable must stay inside the start budget
	// before a run may begin. It exists so that a momentary lull - somebody
	// else's page cache being reclaimed for a second - cannot admit a decoder.
	// Zero disables it; the shipped default is a few seconds.
	StartHold Duration `toml:"start_hold"`

	// Per-worker ceilings used by the supervisor's own RSS check.
	WorkerMemHighMB int64 `toml:"worker_mem_high_mb"`
	WorkerMemMaxMB  int64 `toml:"worker_mem_max_mb"`

	// Ceilings advertised for systemd / cgroup.
	ServerMemHighMB int64 `toml:"server_mem_high_mb"`
	ServerMemMaxMB  int64 `toml:"server_mem_max_mb"`

	// PSI gates (percent of some-avg10). Zero disables that gate.
	PSIMemorySomeAvg10Stop float64 `toml:"psi_memory_some_avg10_stop"`
	PSIIOSomeAvg10Stop     float64 `toml:"psi_io_some_avg10_stop"`

	// Swap policy. Swapping a weak ARM box hard is worse than skipping files.
	MaxSwapUsedKB int64 `toml:"max_swap_used_kb"`
	MinSwapFreeKB int64 `toml:"min_swap_free_kb"`

	// Isolation requirements.
	UseCgroup              bool     `toml:"use_cgroup"`
	RequireCgroupIsolation bool     `toml:"require_cgroup_isolation"`
	CgroupRoot             string   `toml:"cgroup_root"`
	UsePSI                 bool     `toml:"use_psi"`
	NiceLevel              int      `toml:"nice_level"`
	IoniceClass            string   `toml:"ionice_class"`
	CooldownAfterTrip      Duration `toml:"cooldown_after_trip"`
}

// Decode bounds media decoding. The estimate is a pre-filter; process isolation
// is what actually protects the machine.
type Decode struct {
	MaxPixels      int64    `toml:"max_pixels"`
	MaxFileBytes   int64    `toml:"max_file_bytes"`
	MaxDecodeMemMB int64    `toml:"max_decode_mem_mb"`
	MaxFrames      int      `toml:"max_frames"`
	PerFileTimeout Duration `toml:"per_file_timeout"`
	GracePeriod    Duration `toml:"grace_period"`
	MaxAttempts    int      `toml:"max_attempts"`
	RetryBackoff   Duration `toml:"retry_backoff"`
	ThumbMaxEdge   int      `toml:"thumb_max_edge"`
	ThumbQuality   int      `toml:"thumb_quality"`
	// ThumbWorkers is how many thumbnail workers may run at once, sharing the
	// worker memory ceiling. 0 picks two on machines with four or more cores.
	ThumbWorkers    int `toml:"thumb_workers"`
	MaxScanDepth    int `toml:"max_scan_depth"`
	MaxErrorStreak  int `toml:"max_error_streak"`
	ProbeMaxMetaLen int `toml:"probe_max_metadata_bytes"`
	WorkerLogTailKB int `toml:"worker_log_tail_kb"`
}

// TimeCfg records the capture-time policy.
type TimeCfg struct {
	// FilenameHeuristic is off by default: the project rule is "real capture
	// time, otherwise file time, never a guess". Turning this on adds a
	// documented heuristic to the middle of that chain.
	FilenameHeuristic bool `toml:"filename_heuristic"`
}

// Formats is the per-format support matrix. Anything not enabled is probed for
// cheap facts only and shown as a placeholder tile.
type Formats struct {
	JPEG bool `toml:"jpeg"`
	PNG  bool `toml:"png"`
	GIF  bool `toml:"gif"`
	WebP bool `toml:"webp"`

	// VideoPoster is a separate, default-off capability.
	VideoPoster        bool     `toml:"video_poster"`
	FFmpegPath         string   `toml:"ffmpeg_path"`
	VideoPosterTimeout Duration `toml:"video_poster_timeout"`
	VideoPosterSeek    Duration `toml:"video_poster_seek"`

	// VideoProbe reads duration/creation time out of the container with a
	// bounded atom walk. No external tool, very little memory.
	VideoProbe bool `toml:"video_probe"`

	// UseVips hands WebP, TIFF, HEIC and AVIF thumbnails to libvips' `vips`
	// tool when it is installed: it streams large images and reads the
	// formats Go cannot. PNG stays built in, which streams with less memory. Without it nasctl uses its own decoders. VipsPath is
	// a name looked up in PATH or an absolute path.
	UseVips  bool   `toml:"use_vips"`
	VipsPath string `toml:"vips_path"`
	// VipsForJPEG also hands JPEG to libvips. Off by default: the built-in
	// decoder shrinks JPEG while decoding and skips libvips' start-up cost.
	VipsForJPEG bool `toml:"vips_for_jpeg"`
}

// Collection is one configured media directory.
type Collection struct {
	Code string `toml:"code"`
	Name string `toml:"name"`
	Root string `toml:"root"`
	// DataMount is the mountpoint the collection is expected to live under,
	// e.g. "/mnt/raid0". `doctor` and every task verify that the mount is
	// present and that Root really resides on it.
	DataMount      string `toml:"data_mount"`
	Enabled        bool   `toml:"enabled"`
	FollowSymlinks bool   `toml:"follow_symlinks"`
	// Private keeps the directory's photos and videos out of the ordinary
	// album and file browser. They stay where they are and open only after
	// the vault password is entered.
	Private bool `toml:"private"`
	// ExpectFSID pins the filesystem identity recorded on the first successful
	// check. Empty means "record it now"; a mismatch refuses to run.
	ExpectFSID string `toml:"expect_fsid"`
	// EnabledKinds limits which media kinds are indexed for this collection.
	EnabledKinds []string `toml:"enabled_kinds"`
	// Exclude are relative-glob patterns skipped during scan.
	Exclude []string `toml:"exclude"`

	// AllowSharedVolume relaxes two heuristic checks: that Root sits on a
	// different filesystem from albumctl's state directory, and that Root is not
	// on the same filesystem as "/".
	//
	// It defaults to false, and it should stay false on this machine. The
	// checks exist because a RAID0 array that failed to assemble leaves the
	// mount point as an ordinary empty directory on the system disk: a scan
	// would then report the whole library as missing, and cache writes would
	// land next to the operating system.
	//
	// The unambiguous check — that a declared data_mount really is a mount point
	// and really contains Root — is NOT affected by this flag and cannot be
	// turned off. Set this only for a genuinely single-volume setup or an
	// offline test, and expect `doctor` to say so.
	AllowSharedVolume bool `toml:"allow_shared_volume"`

	// Derived, filled by Finalize.
	ID int64 `toml:"-"`
}

// Config is the whole file.
type Config struct {
	// undecoded holds the dotted paths of keys present in the document that no
	// struct field claimed. They are carried here so that Finalize can refuse
	// them: a mistyped safety threshold would otherwise be silently ignored and
	// the operator would believe a floor was set that is not.
	undecoded []string

	Server      Server       `toml:"server"`
	Paths       Paths        `toml:"paths"`
	Cache       Cache        `toml:"cache"`
	Guard       Guard        `toml:"guard"`
	Decode      Decode       `toml:"decode"`
	Time        TimeCfg      `toml:"time"`
	Formats     Formats      `toml:"formats"`
	Backup      Backup       `toml:"backup"`
	Hub         Hub          `toml:"hub"`
	Collections []Collection `toml:"collections"`

	// Source records the file the config came from, for diagnostics.
	Source string `toml:"-"`
}

// Backup tells albumctl how to find out whether the existing backup is busy.
//
// The design requires that this be answered by an existing interface rather
// than by pattern-matching process names, so there is no "process name" option
// here at all: a renamed backup would be missed, and an unrelated process
// containing the same substring would stop albumctl for no reason. The command
// field is meant to point at the operator's own backupctl or an equivalent
// status script.
type Backup struct {
	// CheckCommand is run before each file. Exit code 0 - or any code listed in
	// BusyExitCodes - means the backup is busy.
	CheckCommand string `toml:"check_command"`

	// CheckArgs are passed to CheckCommand as separate arguments. There is no
	// shell, so no quoting rules apply and a path containing a space stays one
	// argument.
	CheckArgs []string `toml:"check_args"`

	// BusyExitCodes are the exit codes that mean "busy". The default is {0},
	// following the convention established by systemctl is-active: success
	// means the thing is running.
	BusyExitCodes []int `toml:"busy_exit_codes"`

	// CheckTimeout bounds one check. A status script that hangs must not hang
	// the album task.
	CheckTimeout Duration `toml:"check_timeout"`

	// SystemdUnits are consulted with `systemctl is-active` when no command is
	// configured. Useful when the backup is a systemd unit supplied by a
	// distribution package.
	SystemdUnits []string `toml:"systemd_units"`

	// SkipThumbsWhenBusy makes the thumbnail task yield while the backup runs.
	// It is the default and matches the design: albumctl is the lower priority
	// of the two, and it is the one that stops.
	SkipThumbsWhenBusy bool `toml:"skip_thumbs_when_busy"`

	// RequireKnown refuses to start heavy work when no check is configured at
	// all, instead of silently running with no coordination. Off by default so
	// a machine without any backup interface still works.
	RequireKnown bool `toml:"require_known"`
}

// Default returns the conservative starting configuration.
func Default() Config {
	return Config{
		Server: Server{
			Bind:                   "127.0.0.1:8088",
			ReadHeaderTimeout:      Duration{10 * time.Second},
			ReadTimeout:            Duration{30 * time.Second},
			WriteTimeout:           Duration{60 * time.Second},
			IdleTimeout:            Duration{60 * time.Second},
			ShutdownTimeout:        Duration{10 * time.Second},
			MaxConcurrentRequests:  8,
			MaxConcurrentStreams:   3,
			PageDefault:            60,
			PageMax:                200,
			MaxResponseBytes:       2 << 20,
			StaticCacheSeconds:     86400,
			ThumbPlaceholderSecs:   60,
			ExposeResourceSnapshot: true,
		},
		Paths: Paths{
			StateDir: "/var/lib/albumctl",
		},
		Cache: Cache{
			MaxBytes:        2 << 30, // 2 GiB
			MaxFiles:        40000,
			ShardDepth:      3,
			WarnAtPercent:   90,
			OrphanGraceSecs: 3600,
		},
		Guard: Guard{
			StartMemAvailableKB:    400 * 1024,
			WarnMemAvailableKB:     250 * 1024,
			StopMemAvailableKB:     200 * 1024,
			SampleInterval:         Duration{time.Second},
			StartHold:              Duration{3 * time.Second},
			WorkerMemHighMB:        96,
			WorkerMemMaxMB:         128,
			ServerMemHighMB:        48,
			ServerMemMaxMB:         64,
			PSIMemorySomeAvg10Stop: 50,
			PSIIOSomeAvg10Stop:     80,
			MaxSwapUsedKB:          64 * 1024,
			MinSwapFreeKB:          0,
			UseCgroup:              true,
			RequireCgroupIsolation: true,
			CgroupRoot:             "/sys/fs/cgroup",
			UsePSI:                 true,
			NiceLevel:              15,
			IoniceClass:            "idle",
			CooldownAfterTrip:      Duration{30 * time.Minute},
		},
		Decode: Decode{
			MaxPixels:       12_000_000,
			MaxFileBytes:    256 << 20,
			MaxDecodeMemMB:  96,
			MaxFrames:       1,
			PerFileTimeout:  Duration{60 * time.Second},
			GracePeriod:     Duration{5 * time.Second},
			MaxAttempts:     3,
			RetryBackoff:    Duration{30 * time.Minute},
			ThumbMaxEdge:    512,
			ThumbQuality:    82,
			MaxScanDepth:    64,
			MaxErrorStreak:  5,
			ProbeMaxMetaLen: 256 * 1024,
			WorkerLogTailKB: 8,
		},
		Backup: Backup{
			CheckTimeout:       Duration{5 * time.Second},
			BusyExitCodes:      []int{0},
			SkipThumbsWhenBusy: true,
		},
		Formats: Formats{
			JPEG:               true,
			PNG:                true,
			GIF:                true,
			WebP:               true,
			VideoPoster:        false,
			FFmpegPath:         "ffmpeg",
			UseVips:            true,
			VipsPath:           "vips",
			VideoPosterTimeout: Duration{30 * time.Second},
			VideoPosterSeek:    Duration{3 * time.Second},
			VideoProbe:         true,
		},
		// The hub section defaults to an empty console: no storage roots, no
		// integrations. That is the only safe thing to get by copying this file
		// without reading it, and it means a hub key left out cannot widen what
		// the console may touch.
		Hub: Hub{
			Files: DefaultFileLimits(),
			Anime: Anime{PollIntervalSeconds: 900, MaxItemsPerFeed: 200, QBittorrentURL: "http://127.0.0.1:8080"},
			// Agents defaults to empty: no agent has a credential, so none can
			// authenticate. An operator adds one deliberately.
			Agents: nil,
			Jobs: JobLimits{
				MaxOpenJobs:        32,
				MaxHistory:         5000,
				AuditRetentionDays: 90,
			},
		},
	}
}

// Load reads path, applies defaults for absent keys and validates the result.
func Load(path string) (*Config, error) {
	cfg := Default()
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.undecoded = undecodedKeys(md)
	abs, err := filepath.Abs(path)
	if err == nil {
		cfg.Source = abs
	} else {
		cfg.Source = path
	}
	if err := cfg.Finalize(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &cfg, nil
}

// Parse is Load for an in-memory document, used by tests.
func Parse(doc string) (*Config, error) {
	cfg := Default()
	md, err := toml.Decode(doc, &cfg)
	if err != nil {
		return nil, err
	}
	cfg.undecoded = undecodedKeys(md)
	cfg.Source = "<inline>"
	if err := cfg.Finalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var codeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// undecodedKeys returns the dotted paths of keys the decoder could not place.
// toml.MetaData reports them deepest-last but not sorted; the caller sorts.
func undecodedKeys(md toml.MetaData) []string {
	keys := md.Undecoded()
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.String())
	}
	return out
}

// Finalize normalizes paths, fills derived values and validates the whole file.
// Configured reports whether any backup-busy check exists.
func (b Backup) Configured() bool {
	return strings.TrimSpace(b.CheckCommand) != "" || len(b.SystemdUnits) > 0
}

func (c *Config) Finalize() error {
	var problems []string
	fail := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }

	// ---- backup ----
	if c.Backup.CheckTimeout.Duration <= 0 {
		c.Backup.CheckTimeout = Duration{5 * time.Second}
	}
	if c.Backup.CheckTimeout.Duration > time.Minute {
		fail("backup.check_timeout (%s) must not exceed 1m: a status check that takes longer "+
			"than that is itself a sign the machine is loaded", c.Backup.CheckTimeout)
	}
	if c.Backup.CheckCommand != "" {
		// The command is run with exec.CommandContext, which does not invoke a
		// shell. That has two consequences the check below enforces for every
		// form of the value, including an absolute path:
		//
		//   - a shell metacharacter is not syntax, it is part of the program
		//     name, so the check would fail at run time with a puzzling ENOENT;
		//   - embedded whitespace likewise makes the whole string the program
		//     name, so arguments must be passed through check_args.
		//
		// Checking only the bare-name case let "/usr/bin/backupctl; rm -rf /"
		// through, which is the shape an operator reaches for when they think a
		// shell is involved. It is rejected here rather than silently failing
		// on the first file of a batch.
		if m := strings.ContainsAny(c.Backup.CheckCommand, "|&;<>$`(){}*?!~\n\r\t"); m {
			fail("backup.check_command %q contains shell metacharacters; it is executed "+
				"directly with no shell, so pass the program and put its arguments in "+
				"backup.check_args", c.Backup.CheckCommand)
		} else if strings.ContainsAny(c.Backup.CheckCommand, " ") {
			fail("backup.check_command %q contains spaces; the whole string would be taken "+
				"as the program name, so pass the program alone and put its arguments in "+
				"backup.check_args", c.Backup.CheckCommand)
		}
	}
	for i, code := range c.Backup.BusyExitCodes {
		if code < 0 || code > 255 {
			fail("backup.busy_exit_codes[%d] = %d is not a valid process exit code", i, code)
		}
	}
	for i, u := range c.Backup.SystemdUnits {
		if strings.TrimSpace(u) == "" {
			fail("backup.systemd_units[%d] is empty", i)
		}
	}
	if c.Backup.RequireKnown && !c.Backup.Configured() {
		fail("backup.require_known is set but neither backup.check_command nor " +
			"backup.systemd_units is configured, so heavy tasks could never start")
	}

	// ---- paths ----
	if strings.TrimSpace(c.Paths.StateDir) == "" {
		fail("paths.state_dir must be set")
	} else if !filepath.IsAbs(c.Paths.StateDir) {
		fail("paths.state_dir must be an absolute path, got %q", c.Paths.StateDir)
	} else {
		c.Paths.StateDir = filepath.Clean(c.Paths.StateDir)
	}
	if c.Paths.CacheDir == "" {
		c.Paths.CacheDir = filepath.Join(c.Paths.StateDir, "cache")
	} else if !filepath.IsAbs(c.Paths.CacheDir) {
		fail("paths.cache_dir must be an absolute path, got %q", c.Paths.CacheDir)
	}
	c.Paths.CacheDir = filepath.Clean(c.Paths.CacheDir)
	if c.Paths.IndexFile == "" {
		c.Paths.IndexFile = filepath.Join(c.Paths.StateDir, "index.sqlite3")
	}
	c.Paths.IndexFile = filepath.Clean(c.Paths.IndexFile)
	if c.Paths.LockDir == "" {
		c.Paths.LockDir = filepath.Join(c.Paths.StateDir, "locks")
	}
	c.Paths.LockDir = filepath.Clean(c.Paths.LockDir)
	if c.Paths.RunDir == "" {
		c.Paths.RunDir = c.Paths.StateDir
	}
	c.Paths.RunDir = filepath.Clean(c.Paths.RunDir)

	// ---- server ----
	if c.Server.Bind == "" {
		c.Server.Bind = "127.0.0.1:8088"
	}
	if _, port, err := net.SplitHostPort(c.Server.Bind); err != nil {
		fail("server.bind %q is not host:port: %v", c.Server.Bind, err)
	} else if _, err := net.LookupPort("tcp", port); err != nil {
		// SplitHostPort accepts any text after the colon, so "127.0.0.1:abc"
		// passes it and only fails when the listener is created. Catching it
		// here means doctor and the CLI report it as a configuration problem.
		fail("server.bind %q has an unusable port %q: %v", c.Server.Bind, port, err)
	}
	if c.Server.PageDefault <= 0 {
		c.Server.PageDefault = 60
	}
	if c.Server.PageMax <= 0 {
		c.Server.PageMax = 200
	}
	if c.Server.PageDefault > c.Server.PageMax {
		fail("server.page_default (%d) exceeds server.page_max (%d)", c.Server.PageDefault, c.Server.PageMax)
	}
	if c.Server.MaxConcurrentRequests <= 0 {
		c.Server.MaxConcurrentRequests = 8
	}
	if c.Server.MaxConcurrentStreams <= 0 {
		c.Server.MaxConcurrentStreams = 3
	}
	if c.Server.MaxConcurrentStreams > c.Server.MaxConcurrentRequests {
		fail("server.max_concurrent_streams must not exceed server.max_concurrent_requests")
	}
	if c.Server.MaxResponseBytes <= 0 {
		c.Server.MaxResponseBytes = 2 << 20
	}

	// ---- cache ----
	if c.Cache.MaxBytes <= 0 {
		fail("cache.max_bytes must be positive")
	}
	if c.Cache.MaxFiles <= 0 {
		fail("cache.max_files must be positive")
	}
	if c.Cache.ShardDepth < 1 || c.Cache.ShardDepth > 4 {
		fail("cache.shard_depth must be between 1 and 4, got %d", c.Cache.ShardDepth)
	}

	// ---- guard ----
	if c.Guard.StopMemAvailableKB >= c.Guard.WarnMemAvailableKB ||
		c.Guard.WarnMemAvailableKB >= c.Guard.StartMemAvailableKB {
		fail("guard thresholds must satisfy stop < warn < start, but "+
			"guard.stop_mem_available_kb=%d, guard.warn_mem_available_kb=%d, "+
			"guard.start_mem_available_kb=%d",
			c.Guard.StopMemAvailableKB, c.Guard.WarnMemAvailableKB, c.Guard.StartMemAvailableKB)
	}
	if c.Guard.WorkerMemHighMB <= 0 || c.Guard.WorkerMemMaxMB <= 0 {
		fail("guard worker memory ceilings must be positive")
	}
	if c.Guard.WorkerMemHighMB > c.Guard.WorkerMemMaxMB {
		fail("guard.worker_mem_high_mb must not exceed guard.worker_mem_max_mb")
	}
	if c.Guard.ServerMemHighMB > c.Guard.ServerMemMaxMB {
		fail("guard.server_mem_high_mb must not exceed guard.server_mem_max_mb")
	}
	if c.Guard.SampleInterval.D() <= 0 {
		c.Guard.SampleInterval = Duration{time.Second}
	}
	if c.Guard.StartHold.D() < 0 {
		fail("guard.start_hold must not be negative, got %s", c.Guard.StartHold.D())
	}
	if c.Guard.StartHold.D() > time.Minute {
		fail("guard.start_hold must be at most 1m, got %s: it delays the start of every "+
			"run, and a long hold is not a substitute for a correct budget",
			c.Guard.StartHold.D())
	}
	if c.Guard.SampleInterval.D() > 10*time.Second {
		fail("guard.sample_interval must be <= 10s; slower sampling cannot stop a runaway decoder in time")
	}
	if c.Guard.CgroupRoot == "" {
		c.Guard.CgroupRoot = "/sys/fs/cgroup"
	}
	switch c.Guard.IoniceClass {
	case "", "none", "idle", "best-effort", "realtime":
	default:
		fail("guard.ionice_class must be one of none|idle|best-effort|realtime, got %q", c.Guard.IoniceClass)
	}
	if c.Guard.NiceLevel < -20 || c.Guard.NiceLevel > 19 {
		fail("guard.nice_level must be within -20..19, got %d", c.Guard.NiceLevel)
	}
	if c.Guard.PSIMemorySomeAvg10Stop < 0 || c.Guard.PSIMemorySomeAvg10Stop > 100 {
		fail("guard.psi_memory_some_avg10_stop must be 0..100")
	}
	if c.Guard.PSIIOSomeAvg10Stop < 0 || c.Guard.PSIIOSomeAvg10Stop > 100 {
		fail("guard.psi_io_some_avg10_stop must be 0..100")
	}

	// ---- decode ----
	if c.Decode.MaxPixels <= 0 {
		fail("decode.max_pixels must be positive")
	}
	if c.Decode.MaxFileBytes <= 0 {
		fail("decode.max_file_bytes must be positive")
	}
	if c.Decode.MaxDecodeMemMB <= 0 {
		fail("decode.max_decode_mem_mb must be positive")
	}
	if c.Decode.MaxDecodeMemMB > c.Guard.WorkerMemMaxMB {
		fail("decode.max_decode_mem_mb (%d) must not exceed guard.worker_mem_max_mb (%d): "+
			"an estimate larger than the hard ceiling can never be satisfied",
			c.Decode.MaxDecodeMemMB, c.Guard.WorkerMemMaxMB)
	}
	if c.Decode.ThumbMaxEdge < 64 || c.Decode.ThumbMaxEdge > 2048 {
		fail("decode.thumb_max_edge must be within 64..2048, got %d", c.Decode.ThumbMaxEdge)
	}
	if c.Decode.ThumbWorkers < 0 || c.Decode.ThumbWorkers > 4 {
		fail("decode.thumb_workers must be within 0..4, got %d", c.Decode.ThumbWorkers)
	}
	if c.Decode.ThumbQuality < 40 || c.Decode.ThumbQuality > 95 {
		fail("decode.thumb_quality must be within 40..95, got %d", c.Decode.ThumbQuality)
	}
	if c.Decode.MaxAttempts < 1 {
		c.Decode.MaxAttempts = 1
	}
	if c.Decode.GracePeriod.D() <= 0 {
		c.Decode.GracePeriod = Duration{5 * time.Second}
	}
	if c.Decode.PerFileTimeout.D() <= 0 {
		c.Decode.PerFileTimeout = Duration{60 * time.Second}
	}
	if c.Decode.GracePeriod.D() > c.Decode.PerFileTimeout.D() {
		fail("decode.grace_period must not exceed decode.per_file_timeout")
	}
	if c.Decode.MaxScanDepth <= 0 {
		c.Decode.MaxScanDepth = 64
	}
	if c.Decode.ProbeMaxMetaLen <= 0 {
		c.Decode.ProbeMaxMetaLen = 256 * 1024
	}
	if c.Decode.WorkerLogTailKB <= 0 {
		c.Decode.WorkerLogTailKB = 8
	}

	if c.Formats.FFmpegPath == "" {
		c.Formats.FFmpegPath = "ffmpeg"
	}
	if c.Formats.VipsPath == "" {
		c.Formats.VipsPath = "vips"
	}
	if c.Formats.VideoPosterTimeout.D() <= 0 {
		c.Formats.VideoPosterTimeout = Duration{30 * time.Second}
	}

	// ---- collections ----
	if len(c.Collections) == 0 && !c.Hub.Anime.Enabled && len(c.Hub.Storage) == 0 && !c.Hub.Setup {
		fail("at least one collection, storage root, or enabled native anime module is required")
	}
	seen := map[string]bool{}
	for i := range c.Collections {
		col := &c.Collections[i]
		if !codeRe.MatchString(col.Code) {
			fail("collections[%d].code %q must match %s", i, col.Code, codeRe)
		}
		if seen[col.Code] {
			fail("collections[%d].code %q is a duplicate", i, col.Code)
		}
		seen[col.Code] = true
		if col.Name == "" {
			col.Name = col.Code
		}
		if col.Root == "" || !filepath.IsAbs(col.Root) {
			fail("collections[%d].root must be an absolute path, got %q", i, col.Root)
		} else {
			col.Root = filepath.Clean(col.Root)
		}
		if col.DataMount != "" && !filepath.IsAbs(col.DataMount) {
			fail("collections[%d].data_mount must be an absolute path, got %q", i, col.DataMount)
		} else if col.DataMount != "" {
			col.DataMount = filepath.Clean(col.DataMount)
		}
		if col.Root == "/" {
			fail("collections[%d].root must not be the filesystem root", i)
		}
		for _, k := range col.EnabledKinds {
			switch k {
			case "image", "video", "other":
			default:
				fail("collections[%d].enabled_kinds contains unknown kind %q", i, k)
			}
		}
		if len(col.EnabledKinds) == 0 {
			col.EnabledKinds = []string{"image", "video", "other"}
		}

		// Safety: the index/cache must never live inside a media tree, and a
		// media tree must never live inside the state directory. Either way an
		// accidental scan could index our own thumbnails, or a cache GC could
		// delete user files.
		for _, reserved := range []string{c.Paths.StateDir, c.Paths.CacheDir} {
			if reserved == "" || col.Root == "" {
				continue
			}
			if pathContains(col.Root, reserved) {
				fail("collections[%d].root %q is inside the application state directory %q; "+
					"the media tree must be separate from index and cache storage", i, col.Root, reserved)
			}
			if pathContains(reserved, col.Root) {
				fail("application state directory %q is inside collections[%d].root %q; "+
					"index and cache writes must never land in a media tree", reserved, i, col.Root)
			}
		}
	}

	// A private directory hides its files only if no ordinary directory also
	// indexes them, so the two may not overlap in either direction.
	for i, a := range c.Collections {
		for _, b := range c.Collections[i+1:] {
			if a.Private != b.Private && (pathContains(a.Root, b.Root) || pathContains(b.Root, a.Root)) {
				fail("collections %q and %q overlap, but only one of them is private", a.Code, b.Code)
			}
		}
	}

	// ---- hub ----
	c.finalizeHub(fail)
	c.finalizeAlbum(fail)

	// ---- unknown keys ----
	// Refusing an unrecognised key is deliberate. In this file most numbers are
	// ceilings and floors; silently ignoring a misspelled one would leave the
	// operator believing a limit is in force when it is not.
	for _, k := range c.undecoded {
		fail("unknown key %q: no such option; check the spelling against the "+
			"example configuration", k)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// pathContains reports whether child is inner or a descendant of outer.
func pathContains(outer, inner string) bool {
	if outer == "" || inner == "" {
		return false
	}
	rel, err := filepath.Rel(outer, inner)
	if err != nil {
		return false
	}
	rel = filepath.Clean(rel)
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Collection returns the collection with the given code.
func (c *Config) Collection(code string) (Collection, bool) {
	for _, col := range c.Collections {
		if col.Code == code {
			return col, true
		}
	}
	return Collection{}, false
}

// EnabledCollections returns the collections that may be processed.
func (c *Config) EnabledCollections() []Collection {
	out := make([]Collection, 0, len(c.Collections))
	for _, col := range c.Collections {
		if col.Enabled {
			out = append(out, col)
		}
	}
	return out
}

// MaxDecodeBytes converts the decode memory budget to bytes.
func (c *Config) MaxDecodeBytes() int64 { return c.Decode.MaxDecodeMemMB << 20 }

// ThumbIsSupported reports whether the format matrix allows thumbnail decoding
// for an extension (lower-case, without dot).
func (c *Config) ThumbIsSupported(ext string) bool {
	switch ext {
	case "jpg", "jpeg":
		return c.Formats.JPEG
	case "png":
		return c.Formats.PNG
	case "gif":
		return c.Formats.GIF
	case "webp":
		return c.Formats.WebP
	default:
		return false
	}
}
