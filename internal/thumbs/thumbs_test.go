package thumbs

import (
	"bytes"
	"encoding/json"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/synth"
)

func testOptions() Options {
	return Options{
		MaxEdge:        512,
		Quality:        82,
		MaxPixels:      12_000_000,
		MaxFileBytes:   64 << 20,
		MaxDecodeBytes: 256 << 20,
		MaxFrames:      1,
		Formats:        DefaultFormats(),
	}
}

func decodeJPEG(t *testing.T, path string) image.Config {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		t.Fatalf("%s is not a decodable JPEG: %v", path, err)
	}
	return cfg
}

// TestGenerateProducesABoundedJPEG walks every thumbnailable file the
// generator knows about and asserts the one property the whole cache design
// rests on: the output is a real JPEG whose longest edge is at most MaxEdge,
// and whose decoded pixel count is therefore bounded no matter how large the
// original is.
func TestGenerateProducesABoundedJPEG(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	opt := testOptions()

	cases := []string{
		"photos/2021/trip/IMG_0100.jpg",
		"photos/2019/paris/IMG_0007.jpg",
		"photos/2018/IMG_0001.jpg",
		"photos/2020/big/DSC_0001.jpg",
		"photos/2022/Art/logo.png",
		"edge/tiny.gif",
		"edge/tall.png",
		"edge/anim.gif",
	}
	for _, rel := range cases {
		rec, ok := lib.Find(rel)
		if !ok {
			t.Fatalf("%s missing from the synthetic library", rel)
		}
		dst := filepath.Join(out, strings.ReplaceAll(rel, "/", "_")+".jpg")
		src := filepath.Join(lib.Root, filepath.FromSlash(rel))
		fi, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}

		got := Generate(src, dst, fi.Size(), fi.ModTime().UnixNano(), opt)
		if got.State != StateOK {
			t.Errorf("%s: state %q reason %q detail %q", rel, got.State, got.Reason, got.Detail)
			continue
		}
		cfg := decodeJPEG(t, dst)
		longest := cfg.Width
		if cfg.Height > longest {
			longest = cfg.Height
		}
		if longest > opt.MaxEdge {
			t.Errorf("%s: thumbnail is %dx%d, longer edge %d exceeds the %d limit",
				rel, cfg.Width, cfg.Height, longest, opt.MaxEdge)
		}
		if cfg.Width < 1 || cfg.Height < 1 {
			t.Errorf("%s: thumbnail is %dx%d", rel, cfg.Width, cfg.Height)
		}
		// A 1x1 source must not be blown up to 512x512.
		if rec.Width <= opt.MaxEdge && cfg.Width > rec.Width {
			t.Errorf("%s: source is %d wide, thumbnail is %d wide; a thumbnail must never "+
				"be larger than its source", rel, rec.Width, cfg.Width)
		}
		// Aspect ratio is preserved within rounding, after the photo is turned
		// upright: IMG_0007 carries orientation 6, a quarter turn.
		srcAR := float64(rec.Width) / float64(rec.Height)
		if rel == "photos/2019/paris/IMG_0007.jpg" {
			srcAR = 1 / srcAR
		}
		outAR := float64(cfg.Width) / float64(cfg.Height)
		if diff := srcAR - outAR; diff > 0.02 || diff < -0.02 {
			t.Errorf("%s: aspect ratio changed from %.3f to %.3f", rel, srcAR, outAR)
		}
		fi2, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if fi2.Size() != got.Bytes {
			t.Errorf("%s: reported %d bytes, file is %d", rel, got.Bytes, fi2.Size())
		}
	}
}

// TestGenerateRefusesAPixelBombBeforeDecoding is the pre-filter, and it is the
// reason the pixel budget exists at all. A 45 byte PNG can declare 3.6
// gigapixels; a decoder that trusts the header allocates the better part of a
// gigabyte of RGBA before it reads a single byte of image data. On a 981 MiB
// machine that is the whole device.
func TestGenerateRefusesAPixelBombBeforeDecoding(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	opt := testOptions()

	for _, rel := range []string{"edge/bomb.png", "edge/bomb2.png"} {
		src := filepath.Join(lib.Root, filepath.FromSlash(rel))
		fi, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(out, "bomb.jpg")
		got := Generate(src, dst, fi.Size(), fi.ModTime().UnixNano(), opt)

		if got.State == StateOK {
			t.Fatalf("%s was decoded and produced a %d byte thumbnail; it must be refused from the header",
				rel, got.Bytes)
		}
		if got.State != StateSkipped {
			t.Errorf("%s: state %q, want %q (a budget refusal is a skip, not a failure, "+
				"so it does not consume the retry budget)", rel, got.State, StateSkipped)
		}
		if !strings.Contains(got.Reason, "pixel") {
			t.Errorf("%s: reason %q does not name the pixel budget", rel, got.Reason)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("%s: a skipped file produced a cache entry at %s; it must produce nothing", rel, dst)
		}
		// The header is cheap to read and worth reporting: the UI shows the
		// real dimensions next to a placeholder.
		if got.SourceWidth != 60000 && got.SourceWidth != 40000 {
			t.Errorf("%s: source width reported as %d; the declared dimensions should be "+
				"visible even though the file is never decoded", rel, got.SourceWidth)
		}
	}
}

func TestGenerateRefusesUnsupportedFormat(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	opt := testOptions()
	for _, rel := range []string{"unsupported/photo.heic", "unsupported/raw.cr2", "unsupported/scan.tiff"} {
		src := filepath.Join(lib.Root, filepath.FromSlash(rel))
		fi, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		got := Generate(src, filepath.Join(out, "x.jpg"), fi.Size(), fi.ModTime().UnixNano(), opt)
		if got.State != StateSkipped {
			t.Errorf("%s: state %q, want skipped", rel, got.State)
		}
	}
}

func TestGenerateRefusesDisabledFormat(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	opt := testOptions()
	opt.Formats = Formats{JPEG: true} // PNG deliberately off
	src := filepath.Join(lib.Root, "photos/2022/Art/logo.png")
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	got := Generate(src, filepath.Join(out, "x.jpg"), fi.Size(), fi.ModTime().UnixNano(), opt)
	if got.State != StateSkipped {
		t.Errorf("a disabled format produced state %q, want skipped", got.State)
	}
	if _, err := os.Stat(filepath.Join(out, "x.jpg")); !os.IsNotExist(err) {
		t.Error("a disabled format still wrote a cache entry")
	}
}

// TestGenerateLeavesNoPartialFileOnDecoderFailure covers the corrupted input
// case end to end: whatever the decoder does, the cache directory must not
// contain a file that looks like a thumbnail.
func TestGenerateLeavesNoPartialFileOnDecoderFailure(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	opt := testOptions()

	for _, rel := range []string{"edge/broken.jpg", "edge/empty.jpg", "edge/exif_overflow.jpg", "video/broken.mp4"} {
		src := filepath.Join(lib.Root, filepath.FromSlash(rel))
		fi, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(out, "out.jpg")
		got := Generate(src, dst, fi.Size(), fi.ModTime().UnixNano(), opt)
		if got.State == StateOK {
			t.Errorf("%s: reported success for a file that cannot be decoded", rel)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("%s: left a cache entry behind", rel)
		}
		entries, _ := os.ReadDir(out)
		for _, e := range entries {
			t.Errorf("%s: left %q in the cache directory", rel, e.Name())
		}
	}
}

// TestGenerateDefersAFileReplacedDuringProcessing is the "file changed under
// us" case. Publishing a thumbnail of a different picture than the index row
// describes would be worse than showing a placeholder, so the mismatch must be
// detected and the file deferred.
func TestGenerateDefersAFileReplacedDuringProcessing(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	opt := testOptions()
	rel := "photos/2021/trip/IMG_0100.jpg"
	src := filepath.Join(lib.Root, filepath.FromSlash(rel))
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	// The index recorded a different size than what is on disk now.
	dst := filepath.Join(out, "out.jpg")
	got := Generate(src, dst, fi.Size()+1, fi.ModTime().UnixNano(), opt)
	if got.State == StateOK {
		t.Fatal("a size mismatch between the index row and the file produced a thumbnail")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("a deferred file still produced a cache entry")
	}

	// And a different mtime with the correct size is caught too.
	got = Generate(src, dst, fi.Size(), fi.ModTime().UnixNano()+int64(time.Second), opt)
	if got.State == StateOK {
		t.Fatal("an mtime mismatch between the index row and the file produced a thumbnail")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("a deferred file still produced a cache entry")
	}
}

func TestGenerateRefusesOversizeFile(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	opt := testOptions()
	opt.MaxFileBytes = 1024
	rel := "photos/2020/big/DSC_0001.jpg"
	src := filepath.Join(lib.Root, filepath.FromSlash(rel))
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	got := Generate(src, filepath.Join(out, "x.jpg"), fi.Size(), fi.ModTime().UnixNano(), opt)
	if got.State != StateSkipped {
		t.Errorf("an oversize file produced state %q, want skipped", got.State)
	}
}

func TestGenerateFailsOnMissingSource(t *testing.T) {
	out := t.TempDir()
	got := Generate(filepath.Join(t.TempDir(), "nope.jpg"), filepath.Join(out, "x.jpg"), 10, 10, testOptions())
	if got.State != StateFailed && got.State != StateSkipped {
		t.Errorf("a missing source produced state %q", got.State)
	}
}

// TestGenerateSurvivesHostileEXIF proves the decoder path does not depend on
// the EXIF parser: metadata that lies about its own extent must not affect
// whether a valid image can be thumbnailed.
func TestGenerateSurvivesHostileEXIF(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	opt := testOptions()
	for i, rel := range []string{"edge/exif_overflow.jpg", "edge/exif_many_entries.jpg", "edge/exif_loop.jpg"} {
		src := filepath.Join(lib.Root, filepath.FromSlash(rel))
		fi, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(out, "hostile.jpg")
		done := make(chan Outcome, 1)
		go func() { done <- Generate(src, dst, fi.Size(), fi.ModTime().UnixNano(), opt) }()
		select {
		case got := <-done:
			if got.State == StateOK {
				cfg := decodeJPEG(t, dst)
				if cfg.Width > opt.MaxEdge || cfg.Height > opt.MaxEdge {
					t.Errorf("%s: thumbnail %dx%d exceeds the edge limit", rel, cfg.Width, cfg.Height)
				}
			}
			_ = i
		case <-time.After(20 * time.Second):
			t.Fatalf("%s: Generate did not return within 20s; hostile metadata must not "+
				"be able to make the worker loop", rel)
		}
	}
}

// TestGenerateIsDeterministic matters for cache stability: the same input must
// produce byte-identical output, otherwise every run would rewrite every file
// and the cache would never converge.
func TestGenerateIsDeterministic(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	opt := testOptions()
	rel := "photos/2019/paris/IMG_0007.jpg"
	src := filepath.Join(lib.Root, filepath.FromSlash(rel))
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	var first []byte
	for i := 0; i < 3; i++ {
		dst := filepath.Join(out, "run.jpg")
		got := Generate(src, dst, fi.Size(), fi.ModTime().UnixNano(), opt)
		if got.State != StateOK {
			t.Fatalf("run %d: %s (%s)", i, got.State, got.Reason)
		}
		data, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			primary := primaryFrameBytes(t, dst)
			first = primary
			continue
		}
		if !bytes.Equal(first, primaryFrameBytes(t, dst)) {
			t.Errorf("run %d produced different bytes for identical input", i)
		}
		_ = data
	}
}

// primaryFrameBytes returns the JPEG payload of a file, ignoring nothing: the
// encoder is deterministic so the whole file should match.
func primaryFrameBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- worker protocol ----------------------------------------------------

// TestWorkerHandshakeBeforeDecode is the protocol's whole purpose: the parent
// must be able to move the worker into its memory cgroup before any decoding
// happens. If the worker decoded first, the memory limit would not be in force
// during the only moment it matters.
func TestWorkerHandshakeBeforeDecode(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rel := "photos/2021/trip/IMG_0100.jpg"
	fi, err := os.Stat(filepath.Join(lib.Root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out.jpg")

	// The parent writes a line only after it has placed the worker in a cgroup.
	// Emulate that by immediately granting permission.
	stdin := strings.NewReader("go\n")
	var stdout bytes.Buffer

	args := WorkerArgs{
		Root:          lib.Root,
		Rel:           rel,
		Out:           dst,
		ExpectSize:    fi.Size(),
		ExpectMtimeNS: fi.ModTime().UnixNano(),
		Options:       testOptions(),
		Handshake:     true,
	}
	code, err := RunWorker(args, stdin, &stdout)
	if err != nil {
		t.Fatalf("RunWorker: %v", err)
	}

	events := decodeEvents(t, stdout.Bytes())
	if len(events) == 0 {
		t.Fatal("the worker emitted no events")
	}
	if events[0].Event != EventReady {
		t.Fatalf("the first event is %q, want %q: the worker must announce readiness "+
			"and wait for permission before doing any work", events[0].Event, EventReady)
	}
	var result *WorkerEvent
	for i := range events {
		if events[i].Event == EventResult {
			result = &events[i]
		}
	}
	if result == nil {
		t.Fatal("the worker never emitted a result event")
	}
	if result.Outcome == nil || result.Outcome.State != StateOK {
		t.Fatalf("outcome is %+v, want ok", result.Outcome)
	}
	if code != ExitOK {
		t.Errorf("exit code %d, want %d", code, ExitOK)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("the worker reported success but wrote no file: %v", err)
	}
}

// TestWorkerWithoutHandshakeDoesNotWait is the counterpart: the admin-invoked
// `exec-worker` path has no supervisor, so it must run immediately rather than
// blocking forever on a handshake that will never arrive.
func TestWorkerWithoutHandshakeDoesNotWait(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rel := "photos/2021/trip/IMG_0100.jpg"
	fi, err := os.Stat(filepath.Join(lib.Root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out.jpg")
	args := WorkerArgs{
		Root:          lib.Root,
		Rel:           rel,
		Out:           dst,
		ExpectSize:    fi.Size(),
		ExpectMtimeNS: fi.ModTime().UnixNano(),
		Options:       testOptions(),
		Handshake:     false,
	}
	done := make(chan struct{})
	var code int
	var werr error
	go func() {
		code, werr = RunWorker(args, strings.NewReader(""), &bytes.Buffer{})
		close(done)
	}()
	select {
	case <-done:
		if werr != nil {
			t.Fatalf("RunWorker: %v", werr)
		}
		if code != ExitOK {
			t.Errorf("exit code %d, want %d", code, ExitOK)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the worker blocked waiting for a handshake that was not requested")
	}
}

// TestWorkerRefusesToEscapeItsRoot is the defence-in-depth check on the worker
// side. Even if the parent passed a hostile relative path, the worker must
// refuse to open it.
func TestWorkerRefusesToEscapeItsRoot(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	args := WorkerArgs{
		Root:          lib.Root,
		Rel:           "../../etc/passwd",
		Out:           filepath.Join(t.TempDir(), "out.jpg"),
		ExpectSize:    1,
		ExpectMtimeNS: 1,
		Options:       testOptions(),
	}
	code, _ := RunWorker(args, strings.NewReader(""), &bytes.Buffer{})
	if code == ExitOK {
		t.Fatal("the worker accepted a relative path that escapes its root")
	}
	for _, rel := range []string{"/etc/passwd", "a/../../etc/passwd", ".."} {
		args.Rel = rel
		if code, _ := RunWorker(args, strings.NewReader(""), &bytes.Buffer{}); code == ExitOK {
			t.Errorf("the worker accepted %q", rel)
		}
	}
}

func TestWorkerReportsASkipWithADistinctExitCode(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rel := "edge/bomb.png"
	fi, err := os.Stat(filepath.Join(lib.Root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	code, err := RunWorker(WorkerArgs{
		Root:          lib.Root,
		Rel:           rel,
		Out:           filepath.Join(t.TempDir(), "out.jpg"),
		ExpectSize:    fi.Size(),
		ExpectMtimeNS: fi.ModTime().UnixNano(),
		Options:       testOptions(),
	}, strings.NewReader(""), &stdout)
	if err != nil {
		t.Fatalf("RunWorker: %v", err)
	}
	if code != ExitSkipped {
		t.Errorf("exit code %d, want %d: a budget refusal is a skip and must be "+
			"distinguishable from a failure so it does not consume the retry budget",
			code, ExitSkipped)
	}
	events := decodeEvents(t, stdout.Bytes())
	if len(events) == 0 || events[len(events)-1].Outcome == nil {
		t.Fatal("no outcome was reported")
	}
	if got := events[len(events)-1].Outcome.State; got != StateSkipped {
		t.Errorf("outcome state %q, want %q", got, StateSkipped)
	}
}

func TestParseFormatsRoundTrip(t *testing.T) {
	f := ParseFormats("jpeg,png")
	if !f.JPEG || !f.PNG || f.GIF || f.WebP {
		t.Errorf("ParseFormats(\"jpeg,png\") = %+v", f)
	}
	if names := f.FormatNames(); !strings.Contains(names, "jpeg") || strings.Contains(names, "gif") {
		t.Errorf("FormatNames() = %q", names)
	}
	empty := ParseFormats("")
	if !empty.JPEG {
		t.Error("an empty format list must enable the default set, not disable everything")
	}
}

func TestNormalizeClampsNonsenseOptions(t *testing.T) {
	got := Options{MaxEdge: -5, Quality: 1000, MaxPixels: -1}.Normalize()
	if got.MaxEdge != 512 {
		t.Errorf("MaxEdge normalised to %d, want 512", got.MaxEdge)
	}
	if got.Quality < 40 || got.Quality > 95 {
		t.Errorf("Quality normalised to %d, outside 40..95", got.Quality)
	}
	if got.MaxPixels <= 0 {
		t.Errorf("MaxPixels normalised to %d", got.MaxPixels)
	}
	// A request for an enormous edge must be clamped: the whole design says a
	// thumbnail is one small size, and nothing may be scaled up to the original.
	if big := (Options{MaxEdge: 100000}).Normalize(); big.MaxEdge > 4096 {
		t.Errorf("MaxEdge normalised to %d; an unbounded edge would let a request "+
			"force a full-size decode", big.MaxEdge)
	}
}

func decodeEvents(t *testing.T, data []byte) []WorkerEvent {
	t.Helper()
	var out []WorkerEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e WorkerEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("protocol line %q is not valid JSON: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}
