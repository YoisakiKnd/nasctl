// Package e2e drives the real albumctl binary as a separate process against a
// synthetic media library, and exercises the safety properties that phase C of
// the plan calls for.
//
// Everything here runs the shipped binary rather than calling library
// functions, because the properties under test are process-level: that the
// worker is a separate process, that a killed worker leaves nothing published,
// that pause survives a restart, that a request cannot make the server do heavy
// work. A test that called the functions in-process would not be able to show
// any of that.
//
// Nothing in this package touches production. It runs against a temporary
// directory tree and a loopback listener only.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/synth"
)

var (
	binPath string
	modRoot string
)

func TestMain(m *testing.M) {
	root, err := findModuleRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}
	modRoot = root

	dir, err := os.MkdirTemp("", "albumctl-e2e-bin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "albumctl")

	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/albumctl")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOPROXY=off", "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: building the binary failed: %v\n%s\n", err, stderr.String())
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

// ---- harness ------------------------------------------------------------

// rig is one isolated installation: a synthetic library, a config, a state
// directory and helpers to run the binary.
type rig struct {
	t        *testing.T
	Root     string
	Media    string
	State    string
	Cache    string
	Config   string
	Listen   string
	Library  *synth.Library
	EnvExtra []string

	// cacheMaxBytes and cacheMaxFiles are what [cache] max_bytes and max_files
	// are written as. They are fields so a test can set a ceiling small enough
	// to be reached: a ceiling of two gigabytes is correct and untestable.
	cacheMaxBytes int64
	cacheMaxFiles int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	r := &rig{
		t:      t,
		Root:   root,
		Media:  filepath.Join(root, "media"),
		State:  filepath.Join(root, "state"),
		Cache:  filepath.Join(root, "cache"),
		Config: filepath.Join(root, "config.toml"),
		Listen: freePort(t),

		cacheMaxBytes: 268435456,
		cacheMaxFiles: 5000,
	}
	if err := os.MkdirAll(r.Media, 0o755); err != nil {
		t.Fatal(err)
	}
	lib, err := synth.Write(r.Media)
	if err != nil {
		t.Fatal(err)
	}
	r.Library = lib
	r.writeConfig("")
	return r
}

// writeConfig writes config.toml, with extra appended verbatim.
func (r *rig) writeConfig(extra string) {
	r.t.Helper()
	cfg := fmt.Sprintf(`
[server]
bind = %q
shutdown_timeout = "5s"

[paths]
state_dir = %q
cache_dir = %q

[cache]
max_bytes = %d
max_files = %d
shard_depth = 3

[guard]
# These were written as start_min_avail_mb/resume_after for a while, which no
# struct field claimed, so the decoder dropped them and every test ran on the
# defaults while appearing to configure the thresholds. The names below are the
# real ones, and the loader now refuses a key it does not know, so this cannot
# silently drift again.
start_mem_available_kb = 409600
warn_mem_available_kb = 256000
stop_mem_available_kb = 204800
cooldown_after_trip = "15m"
# Disabled in the harness: the shipped default is 3s, which would add three
# seconds to every batch here and make the suite slow without testing anything
# the unit tests for AdmitStart do not already cover. The production default is
# exercised on the target machine, not here, and that is noted as unverified.
start_hold = "0s"

# The worker ceilings live in [guard], not [decode]: they are cgroup limits.
worker_mem_high_mb = 96
worker_mem_max_mb = 128

[decode]
max_pixels = 12000000
max_file_bytes = 67108864
max_decode_mem_mb = 96
thumb_max_edge = 512
thumb_quality = 82
per_file_timeout = "30s"

[formats]
jpeg = true
png = true
gif = true
webp = true
# There are no heic/raw/video_cover keys: those formats are not implemented at
# all, so there is nothing to switch off, and the loader now refuses a key that
# no field claims. Video posters are the one default-off capability that exists.
video_poster = false

[time]
# The harness asserts on the labelled file-time fallback, so the filename
# heuristic stays off, exactly as shipped.
filename_heuristic = false

[[collections]]
code = "smoke"
name = "Smoke"
root = %q
enabled = true
allow_shared_volume = true
%s`, r.Listen, r.State, r.Cache, r.cacheMaxBytes, r.cacheMaxFiles, r.Media, extra)
	if err := os.WriteFile(r.Config, []byte(cfg), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// run executes the binary and returns stdout, stderr and the exit code.
func (r *rig) run(args ...string) (string, string, int) {
	r.t.Helper()
	return r.runEnv(nil, args...)
}

func (r *rig) runEnv(env []string, args ...string) (string, string, int) {
	r.t.Helper()
	full := append([]string{"--config", r.Config}, args...)
	cmd := exec.Command(binPath, full...)
	cmd.Dir = r.Root
	cmd.Env = append(append(os.Environ(), r.EnvExtra...), env...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			r.t.Fatalf("running %v: %v", full, err)
		}
	}
	r.t.Logf("$ albumctl %s\n[exit %d]\n%s%s", strings.Join(args, " "), code, out.String(), errb.String())
	return out.String(), errb.String(), code
}

func (r *rig) mustRun(args ...string) string {
	r.t.Helper()
	out, errs, code := r.run(args...)
	if code != 0 {
		r.t.Fatalf("albumctl %s exited %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, out, errs)
	}
	return out
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// ---- web server ---------------------------------------------------------

// server is a running albumctl web service.
type server struct {
	t    *testing.T
	cmd  *exec.Cmd
	Base string
	out  *lockedBuffer
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startServer launches `serve` and waits for the status endpoint to answer.
func (r *rig) startServer() *server {
	r.t.Helper()
	cmd := exec.Command(binPath, "--config", r.Config, "serve")
	cmd.Dir = r.Root
	cmd.Env = append(os.Environ(), r.EnvExtra...)
	buf := &lockedBuffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		r.t.Fatalf("starting serve: %v", err)
	}
	s := &server{t: r.t, cmd: cmd, Base: "http://" + r.Listen, out: buf}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", s.Base+"/api/v1/status", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			r.t.Cleanup(s.stop)
			return s
		}
		time.Sleep(150 * time.Millisecond)
	}
	s.stop()
	r.t.Fatalf("the web service did not become ready; output:\n%s", buf.String())
	return nil
}

func (s *server) stop() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	// Ask for a graceful stop first, then insist.
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	s.cmd = nil
}

func (s *server) get(path string) (*http.Response, []byte) {
	s.t.Helper()
	resp, err := http.Get(s.Base + path)
	if err != nil {
		s.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		s.t.Fatalf("reading %s: %v", path, err)
	}
	return resp, body
}

func (s *server) getJSON(path string, into any) int {
	s.t.Helper()
	resp, body := s.get(path)
	if resp.StatusCode == 200 {
		if err := json.Unmarshal(body, into); err != nil {
			s.t.Fatalf("GET %s did not return JSON: %v\n%s", path, err, truncate(string(body), 400))
		}
	}
	return resp.StatusCode
}

type statusDoc struct {
	Version     string `json:"version"`
	Schema      int    `json:"schema"`
	Paused      bool   `json:"paused"`
	PauseReason string `json:"pause_reason"`
	ReadOnly    bool   `json:"read_only"`
	// The field names and json tags here must match what the service actually
	// sends. Two tags in this struct were wrong at first ("resources" for
	// "resource", and a "file_name" listing field that does not exist), and
	// every assertion built on them silently compared against zero values.
	Totals struct {
		Media     int `json:"media"`
		Images    int `json:"images"`
		Videos    int `json:"videos"`
		Other     int `json:"other"`
		Probed    int `json:"probed"`
		ThumbOK   int `json:"thumb_ok"`
		ThumbDue  int `json:"thumb_pending"`
		Supported int `json:"unsupported"`
		WithGPS   int `json:"with_gps"`
		Undated   int `json:"undated"`
		Missing   int `json:"missing"`
	} `json:"totals"`
	Running *struct {
		Kind      string `json:"kind"`
		State     string `json:"state"`
		Processed int    `json:"processed"`
	} `json:"running"`
	Last *struct {
		Kind      string `json:"kind"`
		State     string `json:"state"`
		Reason    string `json:"reason"`
		Detail    string `json:"detail"`
		Processed int    `json:"processed"`
		Failed    int    `json:"failed"`
	} `json:"last"`
	Cache       cacheDoc `json:"cache"`
	Warned      []string `json:"warnings"`
	Collections []struct {
		Code       string `json:"code"`
		MediaCount int    `json:"media_count"`
		MountOK    bool   `json:"mount_ok"`
	} `json:"collections"`
	Resource struct {
		MemAvailableKB  int64 `json:"mem_available_kb"`
		MemTotalKB      int64 `json:"mem_total_kb"`
		PressureTripped bool  `json:"pressure_tripped"`
		ReadOnly        bool  `json:"read_only"`
		WebServiceRSS   int64 `json:"web_service_rss_bytes"`
	} `json:"resource"`
}

type cacheDoc struct {
	Files    int     `json:"files"`
	Bytes    int64   `json:"bytes"`
	MaxBytes int64   `json:"max_bytes"`
	UsedPct  float64 `json:"used_pct"`
}

type mediaPage struct {
	Items []struct {
		ID int64 `json:"id"`
		// The listing sends the base name as "name"; there is no
		// "file_name" field. Declaring the wrong tag here made every
		// name-based assertion compare against an empty string.
		FileName   string `json:"name"`
		RelDir     string `json:"rel_dir"`
		Kind       string `json:"kind"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		ThumbState string `json:"thumb_state"`
		TakenAt    string `json:"taken_at"`
		Fallback   bool   `json:"taken_is_fallback"`
		Missing    bool   `json:"missing"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
	Total      int    `json:"total"`
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---- helpers ------------------------------------------------------------

// waitFor polls until cond is true or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

// indexAll scans everything the synthetic library contains.
func (r *rig) indexAll() {
	r.t.Helper()
	r.mustRun("scan", "--collection", "smoke", "--limit", "5000")
}

// thumbnailAll generates thumbnails for everything that is due.
func (r *rig) thumbnailAll() string {
	r.t.Helper()
	return r.mustRun("thumbs", "--collection", "smoke", "--limit", "500", "--max-runtime", "5m", "--allow-no-cgroup")
}

// ---- HTTP conveniences --------------------------------------------------

// api is a thin client over the read-only API, so structured assertions do not
// depend on the text layout of the CLI reports.
type api struct {
	t *testing.T
	s *server
}

func (r *rig) api(s *server) *api { return &api{t: r.t, s: s} }

func (a *api) status() statusDoc {
	a.t.Helper()
	var doc statusDoc
	if code := a.s.getJSON("/api/v1/status", &doc); code != 200 {
		a.t.Fatalf("GET /api/v1/status returned %d", code)
	}
	return doc
}

func (a *api) mediaPage(query string) mediaPage {
	a.t.Helper()
	var page mediaPage
	if code := a.s.getJSON("/api/v1/media?"+query, &page); code != 200 {
		a.t.Fatalf("GET /api/v1/media?%s returned %d", query, code)
	}
	return page
}

// allMedia pages through the whole collection using the API's own cursor
// protocol, which is the same path the browser takes.
func (a *api) allMedia(collection string) []mediaPage {
	a.t.Helper()
	var pages []mediaPage
	cursor := ""
	for i := 0; i < 200; i++ {
		q := "collection=" + collection + "&limit=25"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		p := a.mediaPage(q)
		pages = append(pages, p)
		if p.NextCursor == "" {
			return pages
		}
		if p.NextCursor == cursor {
			a.t.Fatalf("the cursor protocol repeated the same cursor %q; paging would never terminate", cursor)
		}
		cursor = p.NextCursor
	}
	a.t.Fatal("paging did not terminate within 200 pages")
	return nil
}

func lastNonEmpty(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// indexedNames returns the base names the scanner is expected to index, derived
// from the fixture inventory rather than from the index itself. Comparing a
// search result against the same index that produced it would prove nothing,
// so the expectation is built from what was written to the media tree.
func (r *rig) indexedNames(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, f := range r.Library.Files {
		base := filepath.Base(f.Rel)
		if strings.HasPrefix(base, ".") {
			continue // dot-prefixed names are skipped by the walker
		}
		if base != strings.TrimPrefix(base, ".") {
			continue
		}
		switch strings.ToLower(filepath.Ext(base)) {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".tif", ".tiff",
			".heic", ".heif", ".cr2", ".mp4", ".mov", ".avi", ".mkv":
			out = append(out, base)
		}
	}
	if len(out) == 0 {
		t.Fatal("the fixture inventory yielded no indexable names")
	}
	return out
}

// countCacheFiles walks the cache directory tree. A cache directory that does
// not exist yet counts as empty: that is the correct reading before any worker
// has ever run, and it lets a test assert that a request created nothing.
func (r *rig) countCacheFiles() (int, int64) {
	r.t.Helper()
	var files int
	var size int64
	err := filepath.WalkDir(r.Cache, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				// Nothing has been cached yet, which is the state before the
				// first worker run. It is not an error to be empty.
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		files++
		size += fi.Size()
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return files, size
}

var _ = strconv.Itoa
