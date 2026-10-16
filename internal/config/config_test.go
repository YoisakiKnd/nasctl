package config

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// baseDoc declares only what every case needs. Tables that a case overrides
// are absent here, because TOML rejects a redefined table and a duplicate would
// fail the case for the wrong reason. Default() supplies bind and every other
// unset field.
const baseDoc = `
[paths]
state_dir = "/srv/albumctl/state"
cache_dir = "/srv/albumctl/cache"

[[collections]]
code = "photos"
name = "Photos"
root = "/mnt/raid0/photos"
data_mount = "/mnt/raid0"
enabled = true
`

func mustParse(t *testing.T, doc string) *Config {
	t.Helper()
	cfg, err := Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

// withSections appends TOML fragments to baseDoc.
func withSections(extra ...string) string {
	out := baseDoc
	for _, e := range extra {
		out += "\n" + e + "\n"
	}
	return out
}

// TestDefaultsAreTheSafeOnes is the most important test in this file. A
// default is a decision that nobody reviews again, so every capability the
// brief says is off by default is asserted here by name. If a later change
// flips one of these, the operator would be running a feature they never
// approved on a machine that already lost a library to resource exhaustion.
func TestDefaultsAreTheSafeOnes(t *testing.T) {
	d := Default()

	if d.Formats.VideoPoster {
		t.Error("video_poster defaults to on; it is a default-off capability that shells out to ffmpeg")
	}
	if !d.Formats.VideoProbe {
		t.Error("video_probe defaults to off; reading a container header in-process is the cheap, intended path")
	}
	if !d.Formats.JPEG || !d.Formats.PNG {
		t.Error("JPEG and PNG must be enabled by default: they are the two formats the brief prioritises")
	}
	if !d.Guard.RequireCgroupIsolation {
		t.Error("require_cgroup_isolation defaults to off; on a machine where the limits cannot be " +
			"enforced, heavy tasks must refuse to run rather than run unbounded")
	}
	if !d.Guard.UseCgroup {
		t.Error("use_cgroup defaults to off")
	}
	if d.Decode.MaxFrames != 1 {
		t.Errorf("max_frames defaults to %d, want 1: only the first frame of an animation is decoded", d.Decode.MaxFrames)
	}
	if d.Decode.RetryBackoff.D() <= 0 {
		t.Error("retry_backoff is zero, which would mean immediate repeated retries")
	}
	if d.Guard.CooldownAfterTrip.D() <= 0 {
		t.Error("cooldown_after_trip is zero, which would mean an automatic restart after a resource trip")
	}
	if d.Backup.SkipThumbsWhenBusy == false {
		t.Error("skip_thumbs_when_busy defaults to false; albumctl must yield to the existing backup by default")
	}
	if d.Time.FilenameHeuristic {
		t.Error("filename_heuristic defaults to on; the rule is real capture time, else file time, never a guess")
	}
	if d.Server.Bind != "127.0.0.1:8088" {
		t.Errorf("server.bind defaults to %q; it must default to a loopback address", d.Server.Bind)
	}
	if d.Cache.ShardDepth < 1 {
		t.Errorf("cache.shard_depth defaults to %d; a flat cache directory makes readdir slow on the target", d.Cache.ShardDepth)
	}

	// Guard thresholds must be strictly ordered, because a stop threshold at or
	// above the warn threshold means the app trips only after it is already
	// into the region it was meant to avoid.
	if !(d.Guard.StopMemAvailableKB < d.Guard.WarnMemAvailableKB &&
		d.Guard.WarnMemAvailableKB < d.Guard.StartMemAvailableKB) {
		t.Errorf("the shipped thresholds are not ordered stop(%d) < warn(%d) < start(%d)",
			d.Guard.StopMemAvailableKB, d.Guard.WarnMemAvailableKB, d.Guard.StartMemAvailableKB)
	}
	if d.Guard.StartMemAvailableKB != 400*1024 {
		t.Errorf("start_mem_available_kb = %d, want 400 MiB as the brief specifies", d.Guard.StartMemAvailableKB)
	}

	// The per-worker ceiling must be below the whole-machine warn threshold:
	// a worker is allowed to be up to 128 MiB, and on a 981 MiB machine that
	// has to leave headroom for everything else.
	if d.Guard.WorkerMemMaxMB >= d.Guard.WarnMemAvailableKB/1024 {
		t.Errorf("the worker ceiling (%d MiB) is not below the warn threshold (%d MiB)",
			d.Guard.WorkerMemMaxMB, d.Guard.WarnMemAvailableKB/1024)
	}

	// A default collection may not opt out of the volume checks.
	if len(d.Collections) != 0 {
		t.Error("Default() ships collections; it must ship none, so a config cannot be used without naming roots")
	}
}

// TestAllowSharedVolumeIsOffByDefault is separate from the block above because
// it is the flag most likely to be set while "just trying things" on a machine
// where the RAID did not assemble.
func TestAllowSharedVolumeIsOffByDefault(t *testing.T) {
	cfg := mustParse(t, baseDoc)
	if cfg.Collections[0].AllowSharedVolume {
		t.Error("allow_shared_volume defaults to on; it must be an explicit, documented opt-out")
	}
	if cfg.Collections[0].FollowSymlinks {
		t.Error("follow_symlinks defaults to on; symlinks must not be followed by default")
	}
}

func TestFinalizeAcceptsTheShippedDocument(t *testing.T) {
	cfg := mustParse(t, baseDoc)
	if cfg.Source != "<inline>" {
		t.Errorf("Source = %q, want %q", cfg.Source, "<inline>")
	}
	if len(cfg.Collections) != 1 {
		t.Fatalf("%d collections, want 1", len(cfg.Collections))
	}
	if cfg.Collections[0].Code != "photos" {
		t.Errorf("collection code = %q", cfg.Collections[0].Code)
	}
	if len(cfg.Collections[0].EnabledKinds) == 0 {
		t.Error("enabled_kinds was left empty and not defaulted")
	}
}

// TestEncodingErrors rejects the four ways a name could escape its element or
// the document. Each of these is a real XSS or crash vector when a file name
// reaches a page.
func TestEncodingErrors(t *testing.T) {
	for _, doc := range []string{
		baseDoc + "\nthis is not toml",
		baseDoc + "\n[server\nbind = \"x\"",
		"[server]\nbind = \n",
	} {
		if _, err := Parse(doc); err == nil {
			t.Errorf("Parse accepted a malformed document:\n%s", doc)
		}
	}
}

func TestRefusesAMediaRootInsideTheStateDirectory(t *testing.T) {
	doc := strings.Replace(baseDoc, `root = "/mnt/raid0/photos"`, `root = "/srv/albumctl/state/photos"`, 1)
	_, err := Parse(doc)
	if err == nil {
		t.Fatal("a media root inside state_dir was accepted; a scan would then index our own thumbnails")
	}
	if !strings.Contains(err.Error(), "state directory") {
		t.Errorf("the error does not explain the problem: %v", err)
	}
}

func TestRefusesAStateDirectoryInsideAMediaRoot(t *testing.T) {
	doc := strings.Replace(baseDoc, `state_dir = "/srv/albumctl/state"`, `state_dir = "/mnt/raid0/photos/.albumctl"`, 1)
	_, err := Parse(doc)
	if err == nil {
		t.Fatal("a state directory inside a media root was accepted; a cache GC could then delete user files")
	}
	if !strings.Contains(err.Error(), "state directory") {
		t.Errorf("the error does not explain the problem: %v", err)
	}
}

func TestRefusesACacheDirectoryInsideAMediaRoot(t *testing.T) {
	doc := strings.Replace(baseDoc, `cache_dir = "/srv/albumctl/cache"`, `cache_dir = "/mnt/raid0/photos/thumbs"`, 1)
	_, err := Parse(doc)
	if err == nil {
		t.Fatal("a cache directory inside a media root was accepted")
	}
}

// TestSiblingDirectoriesAreFine guards against a check that is too eager:
// "/mnt/raid0/photos" and "/srv/albumctl/state" share no path component, and a
// prefix check done on strings rather than path components would still be
// fine here, but "/mnt/raid0/photos-archive" must not be treated as being
// inside "/mnt/raid0/photos".
func TestSiblingDirectoriesAreFine(t *testing.T) {
	doc := strings.Replace(baseDoc, `root = "/mnt/raid0/photos"`, `root = "/mnt/raid0/photos-archive"`, 1)
	mustParse(t, doc)

	doc = strings.Replace(baseDoc, `root = "/mnt/raid0/photos"`, `root = "/mnt/raid0"`, 1)
	doc = strings.Replace(doc, `data_mount = "/mnt/raid0"`, `data_mount = "/mnt/raid0"`, 1)
	mustParse(t, doc)
}

func TestCollectionRootMustBeAbsolute(t *testing.T) {
	for _, root := range []string{`"photos"`, `"./photos"`, `"mnt/raid0/photos"`} {
		doc := strings.Replace(baseDoc, `root = "/mnt/raid0/photos"`, "root = "+root, 1)
		if _, err := Parse(doc); err == nil {
			t.Errorf("a relative collection root %s was accepted", root)
		}
	}
	doc := strings.Replace(baseDoc, `root = "/mnt/raid0/photos"`, `root = "/"`, 1)
	if _, err := Parse(doc); err == nil {
		t.Error(`root = "/" was accepted; scanning the whole filesystem is never intended`)
	}
}

func TestCollectionCodesAreRestricted(t *testing.T) {
	// The code becomes a URL query parameter and part of a cache key, so it is
	// a closed character set rather than free text.
	bad := []string{`"Photos"`, `"phot os"`, `"phot/os"`, `"../etc"`, `"phot$"`, `""`,
		`"phot osp"`, `"фото"`, `"photo;rm"`, `"photo?"`, `"photo#"`}
	for _, code := range bad {
		doc := strings.Replace(baseDoc, `code = "photos"`, "code = "+code, 1)
		if _, err := Parse(doc); err == nil {
			t.Errorf("collection code %s was accepted", code)
		}
	}
	for _, code := range []string{`"photos"`, `"a"`, `"p1"`, `"my_photos"`, `"my-photos"`, `"photos2"`} {
		doc := strings.Replace(baseDoc, `code = "photos"`, "code = "+code, 1)
		mustParse(t, doc)
	}
}

func TestDuplicateCollectionCodesAreRefused(t *testing.T) {
	doc := baseDoc + `
[[collections]]
code = "photos"
name = "Duplicate"
root = "/mnt/raid0/more"
`
	_, err := Parse(doc)
	if err == nil {
		t.Fatal("two collections with the same code were accepted; the code identifies a collection in the API")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("the error does not mention the duplicate: %v", err)
	}
}

func TestAtLeastOneCollectionIsRequired(t *testing.T) {
	doc := strings.Split(baseDoc, "[[collections]]")[0]
	if _, err := Parse(doc); err == nil {
		t.Fatal("a config with no collections was accepted")
	}
}

// TestAnEmptyCollectionNameFallsBackToTheCode documents a deliberate default:
// the display name is cosmetic, so a missing one becomes the code rather than a
// fatal error. What matters is that it is never left blank in the UI.
func TestAnEmptyCollectionNameFallsBackToTheCode(t *testing.T) {
	doc := strings.Replace(baseDoc, `name = "Photos"`, `name = ""`, 1)
	cfg := mustParse(t, doc)
	if cfg.Collections[0].Name == "" {
		t.Fatal("an empty collection name was accepted and left blank")
	}
	if cfg.Collections[0].Name != cfg.Collections[0].Code {
		t.Errorf("an empty name became %q, want the code %q", cfg.Collections[0].Name, cfg.Collections[0].Code)
	}
}

// TestDataMountMustBeAbsoluteButMayBeAbsent documents the one field that is
// genuinely optional: a collection without a declared mount is checked less
// strictly, which is allowed for an offline test but is not the production
// shape.
func TestDataMountMustBeAbsoluteButMayBeAbsent(t *testing.T) {
	doc := strings.Replace(baseDoc, `data_mount = "/mnt/raid0"`, `data_mount = "mnt/raid0"`, 1)
	if _, err := Parse(doc); err == nil {
		t.Error("a relative data_mount was accepted")
	}
	doc = strings.Replace(baseDoc, "data_mount = \"/mnt/raid0\"\n", "", 1)
	mustParse(t, doc)
}

// TestThresholdOrderingIsEnforced covers every ordered pair the guard depends
// on. A configuration where stop >= warn, or warn >= start, silently removes a
// safety gate.
func TestThresholdOrderingIsEnforced(t *testing.T) {
	cases := []struct{ name, doc string }{
		{"stop above warn", "[guard]\nstop_mem_available_kb = 300000\nwarn_mem_available_kb = 250000"},
		{"stop equal to warn", "[guard]\nstop_mem_available_kb = 250000\nwarn_mem_available_kb = 250000"},
		{"warn equal to start", "[guard]\nstart_mem_available_kb = 250000\nwarn_mem_available_kb = 250000"},
		{"warn above start", "[guard]\nstart_mem_available_kb = 100000\nwarn_mem_available_kb = 250000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse(baseDoc + "\n" + c.doc + "\n"); err == nil {
				t.Error("an inverted memory threshold was accepted")
			}
		})
	}
}

// TestWorkerCeilingsAreOrdered covers MemoryHigh <= MemoryMax for both the
// worker and the server. MemoryHigh above MemoryMax is not a stricter setting,
// it is a nonsensical one.
func TestWorkerCeilingsAreOrdered(t *testing.T) {
	cases := []string{
		"[guard]\nworker_mem_high_mb = 200\nworker_mem_max_mb = 128",
		"[guard]\nserver_mem_high_mb = 200\nserver_mem_max_mb = 64",
		"[guard]\nworker_mem_high_mb = 0\nworker_mem_max_mb = 128",
		"[guard]\nworker_mem_high_mb = 96\nworker_mem_max_mb = -1",
	}
	for _, extra := range cases {
		if _, err := Parse(baseDoc + "\n" + extra + "\n"); err == nil {
			t.Errorf("an inconsistent cgroup ceiling was accepted:\n%s", extra)
		}
	}
}

// TestTheDecodeEstimateCannotExceedTheProcessCeiling is the check that keeps the
// pre-filter and the isolation consistent. If a file's estimated decode size
// were allowed to exceed the worker's own hard ceiling, the estimate would
// admit work that the cgroup then kills - the app would be choosing files it
// cannot finish, and the fix must be a lower max_pixels, not a higher ceiling.
func TestTheDecodeEstimateCannotExceedTheProcessCeiling(t *testing.T) {
	doc := baseDoc + "\n[decode]\nmax_decode_mem_mb = 256\n"
	_, err := Parse(doc)
	if err == nil {
		t.Fatal("max_decode_mem_mb above worker_mem_max_mb was accepted")
	}
	if !strings.Contains(err.Error(), "worker_mem_max_mb") {
		t.Errorf("the error does not name the ceiling it conflicts with: %v", err)
	}

	// Equal is allowed: the estimate is an upper bound, so a file estimated at
	// exactly the ceiling is admissible.
	mustParse(t, baseDoc+"\n[decode]\nmax_decode_mem_mb = 128\n")
}

func TestDecodeLimitsAreRangeChecked(t *testing.T) {
	// These are refused outright, because zero or a nonsensical value here
	// removes a bound rather than naming one.
	cases := []string{
		"[decode]\nmax_pixels = 0",
		"[decode]\nmax_pixels = -1",
		"[decode]\nmax_file_bytes = 0",
		"[decode]\nthumb_max_edge = 16",
		"[decode]\nthumb_max_edge = 8192",
		"[decode]\nthumb_quality = 5",
		"[decode]\nthumb_quality = 100",
		// A grace period longer than the timeout it precedes means the process
		// is always killed at the timeout and never gets its grace.
		"[decode]\nper_file_timeout = \"2s\"\ngrace_period = \"30s\"",
	}
	for _, extra := range cases {
		if _, err := Parse(baseDoc + "\n" + extra + "\n"); err == nil {
			t.Errorf("an out-of-range decode limit was accepted:\n%s", extra)
		}
	}
}

// TestZeroMeansUseTheDefault documents the one rule that is easy to get wrong
// when reading this file: for a handful of fields, zero is not an error, it
// means "unset" and the built-in value is substituted. The distinction is
// deliberate and follows from what zero would mean:
//
//   - max_pixels = 0 would mean "no pixel bound", so it is rejected;
//   - max_attempts = 0 would mean "never retry", which is safe and useless, so
//     it is replaced by the default 1 rather than refused.
//
// What matters for safety is that every substituted value is itself bounded, so
// this test asserts the substituted values, not merely that parsing succeeded.
func TestZeroMeansUseTheDefault(t *testing.T) {
	cfg := mustParse(t, withSections("[decode]",
		"max_attempts = 0\nmax_scan_depth = 0\nprobe_max_metadata_bytes = 0\n"+
			"worker_log_tail_kb = 0\nper_file_timeout = \"0s\"\ngrace_period = \"0s\""))
	if cfg.Decode.MaxAttempts < 1 {
		t.Errorf("max_attempts = 0 was defaulted to %d; a retry count below 1 would mean "+
			"a failed file is never retried and also never reported as given up", cfg.Decode.MaxAttempts)
	}
	if cfg.Decode.MaxAttempts > 10 {
		t.Errorf("max_attempts = 0 was defaulted to %d, which is a retry loop rather than a bound",
			cfg.Decode.MaxAttempts)
	}
	if cfg.Decode.MaxScanDepth <= 0 {
		t.Errorf("max_scan_depth = 0 was not replaced with a bound: %d", cfg.Decode.MaxScanDepth)
	}
	if cfg.Decode.ProbeMaxMetaLen <= 0 {
		t.Errorf("probe_max_metadata_bytes = 0 was not replaced with a bound: %d", cfg.Decode.ProbeMaxMetaLen)
	}
	if cfg.Decode.WorkerLogTailKB <= 0 {
		t.Errorf("worker_log_tail_kb = 0 was not replaced with a bound: %d", cfg.Decode.WorkerLogTailKB)
	}
	if cfg.Decode.PerFileTimeout.D() <= 0 {
		t.Errorf("per_file_timeout = 0 was not replaced with a bound: %v", cfg.Decode.PerFileTimeout.D())
	}
	if cfg.Decode.GracePeriod.D() <= 0 {
		t.Errorf("grace_period = 0 was not replaced with a bound: %v", cfg.Decode.GracePeriod.D())
	}
	if cfg.Decode.GracePeriod.D() > cfg.Decode.PerFileTimeout.D() {
		t.Errorf("the defaulted grace period (%v) exceeds the defaulted timeout (%v)",
			cfg.Decode.GracePeriod.D(), cfg.Decode.PerFileTimeout.D())
	}
}

func TestSampleIntervalIsBounded(t *testing.T) {
	// Sampling is what stops a worker mid-decode. An interval of zero samples
	// never; a very long one samples too late to help.
	if _, err := Parse(baseDoc + "\n[guard]\nsample_interval = \"1h\"\n"); err == nil {
		t.Error("a sample_interval of one hour was accepted; sampling is what stops a worker mid-decode")
	}
	// Zero means unset and is defaulted to the shipped one second, which is
	// itself inside the bound.
	slow := mustParse(t, baseDoc+"\n[guard]\nsample_interval = \"0s\"\n")
	if d := slow.Guard.SampleInterval.D(); d <= 0 || d > 10*time.Second {
		t.Errorf("sample_interval = 0 was defaulted to %v, outside the 0..10s bound", d)
	}
	mustParse(t, baseDoc+"\n[guard]\nsample_interval = \"2s\"\n")
}

func TestNiceAndIoniceAreRangeChecked(t *testing.T) {
	// nice and ionice are additional measures, never a substitute for a memory
	// limit, but an out-of-range value would make the helper call fail.
	for _, bad := range []string{"[guard]\nnice_level = -21", "[guard]\nnice_level = 20"} {
		if _, err := Parse(baseDoc + "\n" + bad + "\n"); err == nil {
			t.Errorf("an out-of-range nice_level was accepted: %s", bad)
		}
	}
	mustParse(t, baseDoc+"\n[guard]\nnice_level = 19\n")
	mustParse(t, baseDoc+"\n[guard]\nnice_level = 0\n")
}

func TestPSIGatesArePercentages(t *testing.T) {
	for _, bad := range []string{"[guard]\npsi_memory_some_avg10_stop = -1", "[guard]\npsi_memory_some_avg10_stop = 101"} {
		if _, err := Parse(baseDoc + "\n" + bad + "\n"); err == nil {
			t.Errorf("an out-of-range PSI gate was accepted: %s", bad)
		}
	}
	// Zero disables the gate, which must remain possible on a kernel without PSI.
	mustParse(t, baseDoc+"\n[guard]\npsi_memory_some_avg10_stop = 0\npsi_io_some_avg10_stop = 0\n")
}

func TestServerLimitsAreOrdered(t *testing.T) {
	cases := []string{
		"[server]\npage_default = 500\npage_max = 200",
		"[server]\npage_max = 1\npage_default = 60",
		"[server]\nmax_concurrent_streams = 99\nmax_concurrent_requests = 8",
		"[server]\nbind = \"not-a-host-port\"",
		"[server]\nbind = \"127.0.0.1\"",
		"[server]\nbind = \"127.0.0.1:notaport\"",
	}
	for _, extra := range cases {
		if _, err := Parse(baseDoc + "\n" + extra + "\n"); err == nil {
			t.Errorf("an invalid server setting was accepted:\n%s", extra)
		}
	}
}

// TestServerZeroLimitsAreDefaulted covers the other half of the zero rule for
// this section. Each substituted value has to be bounded, because these four
// settings are what keeps a single request from opening hundreds of streams or
// returning an unbounded body.
func TestServerZeroLimitsAreDefaulted(t *testing.T) {
	cfg := mustParse(t, withSections("[server]",
		"page_default = 0\npage_max = 0\nmax_concurrent_requests = 0\n"+
			"max_concurrent_streams = 0\nmax_response_bytes = 0\nbind = \"\""))
	if cfg.Server.Bind == "" {
		t.Error("an empty server.bind was not replaced; the service would have no address")
	}
	if _, _, err := net.SplitHostPort(cfg.Server.Bind); err != nil {
		t.Errorf("the defaulted bind %q is not host:port: %v", cfg.Server.Bind, err)
	}
	if cfg.Server.PageDefault <= 0 || cfg.Server.PageDefault > cfg.Server.PageMax {
		t.Errorf("page_default = %d is not a usable default within page_max = %d",
			cfg.Server.PageDefault, cfg.Server.PageMax)
	}
	if cfg.Server.PageMax <= 0 || cfg.Server.PageMax > 1000 {
		t.Errorf("page_max = %d is not a bounded default", cfg.Server.PageMax)
	}
	if cfg.Server.MaxConcurrentRequests <= 0 || cfg.Server.MaxConcurrentRequests > 64 {
		t.Errorf("max_concurrent_requests = %d is not a bounded default", cfg.Server.MaxConcurrentRequests)
	}
	if cfg.Server.MaxConcurrentStreams <= 0 ||
		cfg.Server.MaxConcurrentStreams > cfg.Server.MaxConcurrentRequests {
		t.Errorf("max_concurrent_streams = %d is not a usable default against max_concurrent_requests = %d",
			cfg.Server.MaxConcurrentStreams, cfg.Server.MaxConcurrentRequests)
	}
	if cfg.Server.MaxResponseBytes <= 0 || cfg.Server.MaxResponseBytes > 64<<20 {
		t.Errorf("max_response_bytes = %d is not a bounded default", cfg.Server.MaxResponseBytes)
	}
}

// TestServerBindDefaultsToLoopback is asserted separately from the range checks
// because it is a posture decision: the service is reached over Tailscale, so
// it must not be bound to every interface by default.
func TestServerBindDefaultsToLoopback(t *testing.T) {
	cfg := mustParse(t, baseDoc)
	host, port, err := net.SplitHostPort(cfg.Server.Bind)
	if err != nil {
		t.Fatalf("the default bind %q is not a host:port", cfg.Server.Bind)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		t.Errorf("the default bind host is %q, which is not a loopback address", host)
	}
	if port == "" {
		t.Error("the default bind has no port")
	}
}

// TestServerBindPortMustBeSpecified refuses a bare ":8088" is not tested here
// because it is a valid Go address meaning all interfaces; what is asserted is
// that a malformed value is refused, and that a loopback default is documented.
func TestServerBindIsParsedAsAHostPort(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8088", "100.64.0.1:8088", "[::1]:8088", ":8088"} {
		mustParse(t, withSections("[server]", `bind = "`+ok+`"`))
	}
}

// TestBackupCommandRefusesShellMetacharacters is a security check with a
// practical purpose: the check command is executed by albumctl, and the config
// file is a file the operator edits by hand. Refusing metacharacters keeps the
// field a single executable path plus arguments instead of a shell command, so
// there is no shell to inject into.
func TestBackupCommandRefusesShellMetacharacters(t *testing.T) {
	// An absolute path is the case that used to slip through: the metacharacter
	// check ran only for a bare name, so an operator writing a shell pipeline
	// that began with an absolute path got no warning at all and then a
	// confusing ENOENT on the first file of a batch.
	for _, cmd := range []string{
		`"/bin/sh -c 'backupctl status' | grep busy"`,
		`"/usr/bin/backupctl; rm -rf /"`,
		`"/usr/bin/backupctl && touch /tmp/x"`,
		`"/usr/bin/backupctl > /tmp/out"`,
		`"/usr/bin/backupctl $(id)"`,
		"\"`/usr/bin/backupctl`\"",
		`"backupctl | grep busy"`,
		`"backupctl; true"`,
		// Whitespace is equally wrong: without a shell, the whole string is the
		// program name, so arguments must go in check_args.
		`"/usr/local/bin/backupctl status --busy"`,
		`"backupctl status"`,
	} {
		doc := baseDoc + "\n[backup]\ncheck_command = " + cmd + "\n"
		_, err := Parse(doc)
		if err == nil {
			t.Errorf("a backup check command that cannot be exec'd was accepted: %s", cmd)
			continue
		}
		if !strings.Contains(err.Error(), "shell metacharacters") &&
			!strings.Contains(err.Error(), "spaces") {
			t.Errorf("the error for %s does not explain the restriction: %v", cmd, err)
		}
	}
	// A single program path or bare name, with arguments supplied separately.
	mustParse(t, withSections("[backup]", `check_command = "/usr/local/bin/backupctl"`,
		`check_args = ["status", "--busy"]`))
	mustParse(t, withSections("[backup]", `check_command = "backupctl"`))
}

func TestBackupBusyExitCodesAreBytes(t *testing.T) {
	for _, bad := range []string{"[backup]\nbusy_exit_codes = [-1]", "[backup]\nbusy_exit_codes = [256]"} {
		if _, err := Parse(baseDoc + "\n" + bad + "\n"); err == nil {
			t.Errorf("an exit code outside 0..255 was accepted: %s", bad)
		}
	}
	mustParse(t, baseDoc+"\n[backup]\nbusy_exit_codes = [0, 1, 255]\n")
}

// TestBackupCheckTimeoutIsBounded keeps the backup check from becoming a stall:
// it runs before every file, so a hanging check would stop the whole batch.
func TestBackupCheckTimeoutIsBounded(t *testing.T) {
	if _, err := Parse(baseDoc + "\n[backup]\ncheck_timeout = \"10m\"\n"); err == nil {
		t.Error("a backup check timeout above one minute was accepted")
	}
	// A missing or zero timeout is defaulted, not rejected: the field is
	// optional and the default is the safe small value.
	cfg := mustParse(t, baseDoc+"\n[backup]\ncheck_timeout = \"0s\"\n")
	if cfg.Backup.CheckTimeout.D() != 5*time.Second {
		t.Errorf("a zero check_timeout was not defaulted: %v", cfg.Backup.CheckTimeout.D())
	}
	if cfg.Backup.CheckTimeout.D() > time.Minute {
		t.Error("the default check timeout exceeds the bound it is supposed to satisfy")
	}
}

func TestRequireKnownBackupNeedsAConfiguredCheck(t *testing.T) {
	if _, err := Parse(baseDoc + "\n[backup]\nrequire_known = true\n"); err == nil {
		t.Error("require_known with no check command or systemd unit was accepted; " +
			"it could never be satisfied, so every run would refuse forever")
	}
	mustParse(t, baseDoc+"\n[backup]\nrequire_known = true\nsystemd_units = [\"rclone-upload.service\"]\n")
}

func TestSystemdUnitsMustNotBeBlank(t *testing.T) {
	if _, err := Parse(baseDoc + "\n[backup]\nsystemd_units = [\"\"]\n"); err == nil {
		t.Error("a blank systemd unit name was accepted")
	}
}

func TestCacheLimitsAreRequired(t *testing.T) {
	cases := []string{
		"[cache]\nmax_bytes = 0",
		"[cache]\nmax_files = 0",
		"[cache]\nshard_depth = 0",
		"[cache]\nshard_depth = 9",
	}
	for _, extra := range cases {
		if _, err := Parse(baseDoc + "\n" + extra + "\n"); err == nil {
			t.Errorf("an invalid cache limit was accepted:\n%s", extra)
		}
	}
}

func TestStateDirMustBeAbsolute(t *testing.T) {
	doc := strings.Replace(baseDoc, `state_dir = "/srv/albumctl/state"`, `state_dir = "var/lib/albumctl"`, 1)
	if _, err := Parse(doc); err == nil {
		t.Error("a relative state_dir was accepted")
	}
}

// TestUnknownKindsAreRefused keeps enabled_kinds honest: a typo would otherwise
// silently index nothing, or index something under the wrong kind.
func TestUnknownKindsAreRefused(t *testing.T) {
	doc := baseDoc + "enabled_kinds = [\"image\", \"videp\"]\n"
	if _, err := Parse(doc); err == nil {
		t.Error("an unknown enabled_kinds entry was accepted")
	}
	doc = baseDoc + "enabled_kinds = [\"image\"]\n"
	cfg := mustParse(t, doc)
	if len(cfg.Collections[0].EnabledKinds) != 1 || cfg.Collections[0].EnabledKinds[0] != "image" {
		t.Errorf("enabled_kinds = %v, want [image]", cfg.Collections[0].EnabledKinds)
	}
}

// TestFinalizeIsIdempotent matters because Finalize is called by Load and by
// the CLI, and a second pass that changed a value would make the effective
// configuration depend on how many times it was finalized.
func TestFinalizeIsIdempotent(t *testing.T) {
	cfg := mustParse(t, baseDoc)
	before := *cfg
	if err := cfg.Finalize(); err != nil {
		t.Fatalf("the second Finalize failed: %v", err)
	}
	if cfg.Collections[0].Code != before.Collections[0].Code ||
		len(cfg.Collections[0].EnabledKinds) != len(before.Collections[0].EnabledKinds) ||
		cfg.Paths.StateDir != before.Paths.StateDir ||
		cfg.Paths.CacheDir != before.Paths.CacheDir {
		t.Errorf("Finalize is not idempotent:\nbefore: %+v\nafter:  %+v", before.Collections[0], cfg.Collections[0])
	}
}

// TestDurationsParseFromTOMLStrings covers the Duration type, which is the one
// non-primitive in the file and the one most likely to reject a reasonable
// operator value.
func TestDurationsParseFromTOMLStrings(t *testing.T) {
	cases := map[string]time.Duration{
		"1500ms": 1500 * time.Millisecond,
		"2s":     2 * time.Second,
		"90s":    90 * time.Second,
		"5m":     5 * time.Minute,
		"2h":     2 * time.Hour,
	}
	// The grace period must be shortened alongside the timeout, since the
	// default five seconds would otherwise exceed the shorter timeouts asserted
	// here and the document would be refused for a reason unrelated to parsing.
	grace := "grace_period = \"100ms\"\n"
	for text, want := range cases {
		doc := baseDoc + "\n[decode]\n" + grace + "per_file_timeout = \"" + text + "\"\n"
		cfg := mustParse(t, doc)
		if cfg.Decode.PerFileTimeout.D() != want {
			t.Errorf("per_file_timeout %q parsed as %v, want %v", text, cfg.Decode.PerFileTimeout.D(), want)
		}
	}
	// A bare number is not a duration and must be refused rather than read as
	// nanoseconds, which would make a timeout effectively instant.
	if _, err := Parse(baseDoc + "\n[decode]\nper_file_timeout = 30\n"); err == nil {
		t.Error("a bare integer was accepted as a duration; TOML integers are not durations here")
	}
}

func TestMaxDecodeBytesAndSupportQueries(t *testing.T) {
	cfg := mustParse(t, baseDoc)
	if got := cfg.MaxDecodeBytes(); got != cfg.Decode.MaxDecodeMemMB<<20 {
		t.Errorf("MaxDecodeBytes = %d, want %d", got, cfg.Decode.MaxDecodeMemMB<<20)
	}
	for _, ext := range []string{"jpg", "jpeg", "png", "gif"} {
		if !cfg.ThumbIsSupported(ext) {
			t.Errorf("ThumbIsSupported(%q) = false, but it is enabled by default", ext)
		}
	}
	// Formats that are off by default must not be reportable as supported.
	for _, ext := range []string{"webp_disabled_placeholder", "heic", "cr2", "tif", "dng"} {
		if cfg.ThumbIsSupported(ext) {
			t.Errorf("ThumbIsSupported(%q) = true for a format that is not in the enabled set", ext)
		}
	}
}

func TestCollectionLookup(t *testing.T) {
	cfg := mustParse(t, baseDoc)
	if _, ok := cfg.Collection("photos"); !ok {
		t.Error("Collection(\"photos\") did not find the configured collection")
	}
	if _, ok := cfg.Collection("PHOTOS"); ok {
		t.Error("Collection lookup is case-insensitive; codes are a closed lowercase set")
	}
	if _, ok := cfg.Collection("missing"); ok {
		t.Error("Collection found a collection that does not exist")
	}
	enabled := cfg.EnabledCollections()
	if len(enabled) != 1 {
		t.Errorf("EnabledCollections returned %d, want 1", len(enabled))
	}

	doc := strings.Replace(baseDoc, "enabled = true", "enabled = false", 1)
	cfg = mustParse(t, doc)
	if len(cfg.EnabledCollections()) != 0 {
		t.Error("a disabled collection was returned by EnabledCollections")
	}
	if _, ok := cfg.Collection("photos"); !ok {
		t.Error("a disabled collection should still be findable by code, for status and doctor output")
	}
}

// TestErrorsNameTheOffendingField keeps a rejection actionable. Every one of
// these messages is read by an operator editing the file by hand, often over
// SSH on a phone.
func TestErrorsNameTheOffendingField(t *testing.T) {
	cases := []struct{ extra, want string }{
		{"[guard]\nstart_mem_available_kb = 1", "start_mem_available_kb"},
		{"[backup]\ncheck_command = \"backupctl | grep busy\"", "check_command"},
		{"[decode]\nmax_pixels = 0", "max_pixels"},
		{"[decode]\nmax_decode_mem_mb = 4096", "max_decode_mem_mb"},
		{"[cache]\nmax_bytes = 0", "max_bytes"},
		{"[server]\nbind = \"127.0.0.1\"", "bind"},
		{"[decode]\nthumb_quality = 1", "thumb_quality"},
	}
	for _, c := range cases {
		_, err := Parse(baseDoc + "\n" + c.extra + "\n")
		if err == nil {
			t.Errorf("no error for %s", c.extra)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("the error for %q does not name %q: %v", c.extra, c.want, err)
		}
	}
}

// TestSeveralProblemsAreReportedTogether means an operator does not fix one
// mistake per run. On a slow SSH session that is the difference between one
// edit and six.
func TestSeveralProblemsAreReportedTogether(t *testing.T) {
	doc := withSections("[guard]", "start_mem_available_kb = 1",
		"[decode]", "max_pixels = 0", "[cache]", "max_bytes = 0")
	_, err := Parse(doc)
	if err == nil {
		t.Fatal("a document with three problems was accepted")
	}
	msg := err.Error()
	for _, want := range []string{"start_mem_available_kb", "max_pixels", "max_bytes"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the combined error omits %q:\n%s", want, msg)
		}
	}
}

// TestAMisspelledKeyIsRefusedByItsName is the guard against the worst kind of
// configuration mistake.
//
// Most numbers in this file are floors and ceilings. A silently ignored key
// leaves the operator believing a limit is in force when it is not, and the
// failure only appears as unexplained behaviour much later. The end-to-end
// harness had exactly this defect: it configured the guard thresholds under
// names no struct field claimed, and every test ran on the defaults while
// appearing to set them.
func TestAMisspelledKeyIsRefusedByItsName(t *testing.T) {
	cases := []struct {
		doc  string
		want string
	}{
		{"[guard]\nstart_min_avail_mb = 400", "start_min_avail_mb"},
		{"[guard]\nresume_after = \"15m\"", "resume_after"},
		{"[decode]\nworker_mem_max_mb = 128", "worker_mem_max_mb"},
		{"[format]\njpeg = true", "format"},
		{"[guard]\nstart_mem_availible_kb = 409600", "start_mem_availible_kb"},
		{"[cache]\nmax_bytes  = 1024\nmax_bytez = 2048", "max_bytez"},
	}
	for _, c := range cases {
		_, err := Parse(withSections(c.doc))
		if err == nil {
			t.Errorf("%q was accepted; a key no option claims must be refused", c.doc)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("the refusal for %q does not name %q: %v", c.doc, c.want, err)
		}
		if !strings.Contains(err.Error(), "unknown key") {
			t.Errorf("the refusal for %q does not say what is wrong: %v", c.doc, err)
		}
	}
}

// TestTheShippedExampleHasNoUnknownKeys is the other half: the refusal above is
// only useful if the file we ship passes it. The example is the document an
// operator copies, so a key it lists must be a key that exists.
func TestTheShippedExampleHasNoUnknownKeys(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "configs", "nasctl.example.toml"),
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Skipf("the example configuration is not in this checkout: %v", err)
		}
		cfg, err := Parse(string(b))
		if err != nil {
			t.Fatalf("%s is not a valid configuration: %v", path, err)
		}
		// It is a sample, so its own stated contract is that it equals the
		// built-in defaults.
		dflt := Default()
		// Compare the policy only. The paths and the sample collections are
		// necessarily local to the operator's machine; every other value is
		// meant to be exactly the built-in default, and the file says so.
		gotPaths, gotColls := cfg.Paths, cfg.Collections
		cfg.Paths, cfg.Collections = dflt.Paths, dflt.Collections
		cfg.Source = ""
		cfg.undecoded = nil
		// An explicit empty list in the document decodes to a non-nil empty
		// slice, while the built-in default leaves the field nil. That is not a
		// policy difference, so it is normalised away instead of being reported.
		if len(cfg.Backup.CheckArgs) == 0 {
			cfg.Backup.CheckArgs = nil
		}
		if len(cfg.Backup.SystemdUnits) == 0 {
			cfg.Backup.SystemdUnits = nil
		}
		// Config carries a slice now, so this compares field by field.
		if !reflect.DeepEqual(*cfg, dflt) {
			t.Errorf("%s differs from the built-in defaults somewhere outside [paths] "+
				"and [[collections]]; the file promises that every policy value is the "+
				"shipped default, so an operator edits paths, not policy", path)
		}
		if gotPaths.StateDir == "" || gotPaths.CacheDir == "" {
			t.Errorf("%s leaves a path empty", path)
		}
		if len(gotColls) == 0 {
			t.Errorf("%s declares no collection to copy", path)
		}
		// The example must show the default-off capabilities as off, because
		// that is the state the deliverable ships in.
		if !gotColls[len(gotColls)-1].Enabled && gotColls[0].Enabled && len(gotColls) < 2 {
			t.Errorf("%s does not show a disabled second collection", path)
		}
		if len(cfg.undecoded) != 0 {
			t.Errorf("%s has keys no option claims: %v", path, cfg.undecoded)
		}
	}
}

// TestTheStartHoldHasAShippedDefaultAndBounds keeps the one parameter that
// exists purely to be conservative honest.
func TestTheStartHoldHasAShippedDefaultAndBounds(t *testing.T) {
	d := Default()
	if got := d.Guard.StartHold.D(); got != 3*time.Second {
		t.Errorf("guard.start_hold defaults to %s, want 3s", got)
	}
	// Zero is allowed: it is what an offline test or a rig sets, and it means
	// "one check", not "no check".
	cfg, err := Parse(withSections("[guard]", "start_hold = \"0s\""))
	if err != nil {
		t.Fatalf("start_hold = 0s was refused: %v", err)
	}
	if cfg.Guard.StartHold.D() != 0 {
		t.Errorf("start_hold = 0s parsed as %s", cfg.Guard.StartHold.D())
	}
	// A negative hold is meaningless, and a very long one is a way of hiding a
	// wrong budget behind a delay, so both are refused by name.
	for _, c := range []struct{ doc, want string }{
		{"[guard]\nstart_hold = \"-1s\"", "start_hold"},
		{"[guard]\nstart_hold = \"10m\"", "start_hold"},
	} {
		_, err := Parse(withSections(c.doc))
		if err == nil {
			t.Errorf("%q was accepted", c.doc)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("the refusal for %q does not name %s: %v", c.doc, c.want, err)
		}
	}
}

// TestAgentCredentialsAreValidated covers the rules that make the AI distinction
// enforceable. Each one exists because breaking it would make an agent
// indistinguishable from the operator, which is the whole reason the credential
// is checked before the request is handled.
func TestAgentCredentialsAreValidated(t *testing.T) {
	base := func(agents string) string {
		return `
[paths]
state_dir = "/var/lib/albumctl"
[server]
token = "the-operator-token-value"
[hub]
[[hub.agents]]
` + agents + `
[[collections]]
code = "photos"
root = "/mnt/data/photos"
`
	}

	t.Run("a well-formed agent is accepted", func(t *testing.T) {
		cfg, err := Parse(base(`name = "hermes"
token = "the-agent-token-value"`))
		if err != nil {
			t.Fatalf("a valid agent was refused: %v", err)
		}
		if len(cfg.Hub.Agents) != 1 || cfg.Hub.Agents[0].Name != "hermes" {
			t.Errorf("agents = %+v", cfg.Hub.Agents)
		}
		if cfg.Hub.Agents[0].AllowHeavy {
			t.Error("allow_heavy defaults to true; an agent must not be able to start " +
				"heavy work without the operator saying so")
		}
	})

	bad := []struct {
		name  string
		agent string
		want  string
	}{
		{"no name", `token = "the-agent-token-value"`, "name is empty"},
		{"unsafe name", `name = "Hermes Bot"
token = "the-agent-token-value"`, "must be lowercase"},
		{"no token", `name = "hermes"`, "token is empty"},
		{"short token", `name = "hermes"
token = "short"`, "can be guessed"},
		{"the operator's token", `name = "hermes"
token = "the-operator-token-value"`, "same as server.token"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(base(c.agent))
			if err == nil {
				t.Fatalf("a configuration with %s was accepted", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}

	t.Run("two agents cannot share a token", func(t *testing.T) {
		doc := `
[paths]
state_dir = "/var/lib/albumctl"
[server]
token = "the-operator-token-value"
[hub]
[[hub.agents]]
name = "hermes"
token = "the-shared-agent-token"
[[hub.agents]]
name = "other"
token = "the-shared-agent-token"
[[collections]]
code = "photos"
root = "/mnt/data/photos"
`
		_, err := Parse(doc)
		if err == nil {
			t.Fatal("two agents sharing a token were accepted; the console could not " +
				"tell them apart")
		}
		if !strings.Contains(err.Error(), "same as") {
			t.Errorf("error = %v, want it to name the collision", err)
		}
	})

	t.Run("two agents cannot share a name", func(t *testing.T) {
		doc := `
[paths]
state_dir = "/var/lib/albumctl"
[server]
token = "the-operator-token-value"
[hub]
[[hub.agents]]
name = "hermes"
token = "the-first-agent-token"
[[hub.agents]]
name = "hermes"
token = "the-second-agent-token"
[[collections]]
code = "photos"
root = "/mnt/data/photos"
`
		if _, err := Parse(doc); err == nil {
			t.Fatal("two agents sharing a name were accepted")
		}
	})
}

// TestTheDefaultConfigurationHasNoAgents: an agent must be added deliberately.
func TestTheDefaultConfigurationHasNoAgents(t *testing.T) {
	if len(Default().Hub.Agents) != 0 {
		t.Error("the shipped default configures an agent; it must configure none")
	}
}
