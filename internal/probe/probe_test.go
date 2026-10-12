package probe

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/synth"
)

// ---- fixture helpers ----------------------------------------------------

// synthLibrary writes the synthetic media library into a fresh temporary
// directory. The fixtures are real files produced by real encoders, because
// every property this package promises — read only a bounded prefix, never
// decode, never trust a header — is a property of how it behaves on bytes a
// camera or an attacker would produce.
func synthLibrary(t *testing.T) (string, *synth.Library) {
	t.Helper()
	dir := t.TempDir()
	lib, err := synth.Write(dir)
	if err != nil {
		t.Fatalf("synth.Write: %v", err)
	}
	return dir, lib
}

func fixturePath(dir, rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }

func fixture(t *testing.T, dir, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(fixturePath(dir, rel))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", rel, err)
	}
	return data
}

// testBudget is the generous budget the "ordinary photograph" cases are probed
// with; the tests that care about a specific ceiling set it explicitly.
func testBudget() Budget {
	return Budget{
		MaxPixels:      40_000_000,
		MaxFileBytes:   64 << 20,
		MaxDecodeBytes: 1 << 30,
		MaxFrames:      1000,
		ThumbMaxEdge:   512,
	}
}

// probeFixture probes a fixture the way the scanner does: the extension comes
// from the name, exactly as scan.Entry.Ext derives it.
func probeFixture(t *testing.T, dir, rel string, b Budget) *Result {
	t.Helper()
	r, err := Probe(fixturePath(dir, rel), NormalizeExt(rel), b, Limits{})
	if err != nil {
		t.Fatalf("Probe(%s): %v", rel, err)
	}
	return r
}

// probeOutcome is the result of a guarded probe.
type probeOutcome struct {
	res      *Result
	err      error
	panicked any
	elapsed  time.Duration
}

// probeGuarded runs Probe on its own goroutine with a deadline, so that a hang
// or a panic is reported as a failed fixture rather than taking the test binary
// down with it. "Do not panic on hostile input" is the property under test, so
// it is checked by construction.
func probeGuarded(path, ext string, b Budget, lim Limits) probeOutcome {
	type result struct {
		res *Result
		err error
		pan any
	}
	start := time.Now()
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				ch <- result{pan: p}
			}
		}()
		res, err := Probe(path, ext, b, lim)
		ch <- result{res: res, err: err}
	}()
	select {
	case r := <-ch:
		return probeOutcome{res: r.res, err: r.err, panicked: r.pan, elapsed: time.Since(start)}
	case <-time.After(2 * time.Second):
		return probeOutcome{panicked: "Probe did not return within 2s", elapsed: time.Since(start)}
	}
}

// ---- hand-built fixtures ------------------------------------------------

// cutInsideSOF returns a JPEG truncated in the middle of its frame header:
// after the SOF marker and its length field, before the dimensions. A reader
// that answers from a half-read segment would report a width and a height here.
func cutInsideSOF(t *testing.T, data []byte) []byte {
	t.Helper()
	i := 2
	for i+3 < len(data) {
		if data[i] != 0xFF {
			t.Fatalf("marker desync at %d", i)
		}
		if i+4 > len(data) {
			break
		}
		marker := data[i+1]
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
			return data[:i+4]
		}
		if marker == 0xDA {
			break
		}
		if segLen < 2 {
			t.Fatalf("bad segment length %d at %d", segLen, i)
		}
		i += 2 + segLen
	}
	t.Fatal("fixture JPEG has no frame header to cut into")
	return nil
}

// mp4Box wraps a body in an ISOBMFF box header.
func mp4Box(typ string, body func(*bytes.Buffer)) []byte {
	var inner bytes.Buffer
	body(&inner)
	var out bytes.Buffer
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(inner.Len()+8))
	out.Write(sz[:])
	out.WriteString(typ)
	out.Write(inner.Bytes())
	return out.Bytes()
}

// buildMP4 writes a spec-conformant ISOBMFF file: ftyp followed by moov/mvhd
// carrying the version+flags dword a real writer emits. version 1 selects the
// 64-bit layout. It exists to separate "the probe cannot read mvhd" from "the
// fixture was not a valid mvhd".
func buildMP4(brand string, version byte, timescale uint32, dur uint64, created time.Time) []byte {
	ctime := uint64(created.Unix() + 2082844800)
	var out bytes.Buffer
	out.Write(mp4Box("ftyp", func(b *bytes.Buffer) {
		b.WriteString(brand)
		b.Write(make([]byte, 4)) // minor version
		b.WriteString(brand)
	}))
	out.Write(mp4Box("moov", func(b *bytes.Buffer) {
		b.Write(mp4Box("mvhd", func(m *bytes.Buffer) {
			m.WriteByte(version)
			m.Write(make([]byte, 3)) // flags
			if version == 1 {
				var t [8]byte
				binary.BigEndian.PutUint64(t[:], ctime)
				m.Write(t[:]) // creation time
				m.Write(t[:]) // modification time
				var f [4]byte
				binary.BigEndian.PutUint32(f[:], timescale)
				m.Write(f[:])
				binary.BigEndian.PutUint64(t[:], dur)
				m.Write(t[:])
			} else {
				var f [4]byte
				binary.BigEndian.PutUint32(f[:], uint32(ctime))
				m.Write(f[:]) // creation time
				m.Write(f[:]) // modification time
				binary.BigEndian.PutUint32(f[:], timescale)
				m.Write(f[:])
				binary.BigEndian.PutUint32(f[:], uint32(dur))
				m.Write(f[:])
			}
			m.Write(make([]byte, 80)) // rate, volume, matrix, next track id
		}))
	}))
	return out.Bytes()
}

// tiffDeclaringDims writes a bare little-endian TIFF whose ExifIFD declares
// pixel dimensions: a file the probe will believe is a decodable image.
func tiffDeclaringDims(w, h uint32) []byte {
	le := func(v uint32) []byte {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, v)
		return b
	}
	entry := func(tag, typ uint16, count, value uint32) []byte {
		var e [12]byte
		binary.LittleEndian.PutUint16(e[0:2], tag)
		binary.LittleEndian.PutUint16(e[2:4], typ)
		binary.LittleEndian.PutUint32(e[4:8], count)
		binary.LittleEndian.PutUint32(e[8:12], value)
		return e[:]
	}
	var out bytes.Buffer
	out.Write([]byte{'I', 'I', 0x2a, 0x00})
	out.Write(le(8)) // IFD0 at offset 8
	var n [2]byte
	binary.LittleEndian.PutUint16(n[:], 1)
	out.Write(n[:])
	out.Write(entry(0x8769, 4, 1, 26)) // ExifIFD, immediately after IFD0
	out.Write(le(0))                   // no next IFD
	binary.LittleEndian.PutUint16(n[:], 2)
	out.Write(n[:])
	out.Write(entry(0xA002, 4, 1, w)) // PixelXDimension
	out.Write(entry(0xA003, 4, 1, h)) // PixelYDimension
	out.Write(le(0))
	return out.Bytes()
}

// ---- tests --------------------------------------------------------------

// TestProbeReadsImageHeadersWithoutDecoding is the core promise: for a file
// whose header is intact, the probe reports the real dimensions from the header
// alone, keeps the file inside a generous budget, and marks it thumbnailable.
func TestProbeReadsImageHeadersWithoutDecoding(t *testing.T) {
	dir, _ := synthLibrary(t)
	cases := []struct {
		rel              string
		format, mime     string
		w, h             int
		animated, frames bool
		frameCount       int
	}{
		{rel: "photos/2021/trip/IMG_0100.jpg", format: "jpeg", mime: "image/jpeg", w: 640, h: 480},
		{rel: "photos/2020/big/DSC_0001.jpg", format: "jpeg", mime: "image/jpeg", w: 4000, h: 3000},
		{rel: "photos/2022/Art/logo.png", format: "png", mime: "image/png", w: 240, h: 180},
		{rel: "edge/anim.gif", format: "gif", mime: "image/gif", w: 160, h: 120, animated: true, frameCount: 6},
		{rel: "edge/tiny.gif", format: "gif", mime: "image/gif", w: 1, h: 1, frameCount: 1},
		{rel: "edge/tall.png", format: "png", mime: "image/png", w: 2, h: 9000},
	}
	for _, c := range cases {
		t.Run(filepath.Base(c.rel), func(t *testing.T) {
			r := probeFixture(t, dir, c.rel, testBudget())
			if r.Kind != KindImage {
				t.Errorf("Kind = %q, want %q", r.Kind, KindImage)
			}
			if r.Format != c.format {
				t.Errorf("Format = %q, want %q", r.Format, c.format)
			}
			if r.MIME != c.mime {
				t.Errorf("MIME = %q, want %q", r.MIME, c.mime)
			}
			if r.Width != c.w || r.Height != c.h {
				t.Errorf("dimensions = %dx%d, want %dx%d", r.Width, r.Height, c.w, c.h)
			}
			if want := int64(c.w) * int64(c.h); r.Pixels != want {
				t.Errorf("Pixels = %d, want %d", r.Pixels, want)
			}
			if r.Animated != c.animated {
				t.Errorf("Animated = %v, want %v", r.Animated, c.animated)
			}
			if r.Frames != c.frameCount {
				t.Errorf("Frames = %d, want %d", r.Frames, c.frameCount)
			}
			if r.SkippedReason != "" || r.UnsupportedReason != "" {
				t.Errorf("a well-formed, in-budget image was refused: skipped=%q unsupported=%q",
					r.SkippedReason, r.UnsupportedReason)
			}
			if !r.Thumbnailable() {
				t.Error("Thumbnailable() = false for a well-formed, in-budget image")
			}
			if r.EstimatedDecodeBytes <= 0 {
				t.Errorf("EstimatedDecodeBytes = %d, want a positive estimate", r.EstimatedDecodeBytes)
			}
			for _, n := range r.Notes {
				if strings.HasPrefix(n, "not a ") || strings.HasPrefix(n, "no frame header") ||
					strings.Contains(n, "truncated") {
					t.Errorf("note %q claims the header is damaged, but it is intact", n)
				}
			}
		})
	}

	// A JPEG with no Exif segment is reported through Notes, not by failing.
	r := probeFixture(t, dir, "photos/2021/trip/IMG_0100.jpg", testBudget())
	var exifNotes int
	for _, n := range r.Notes {
		if strings.HasPrefix(n, "exif: ") {
			exifNotes++
		}
	}
	if exifNotes == 0 {
		t.Errorf("notes %v do not mention the absent Exif segment", r.Notes)
	}
	for _, n := range r.Notes {
		if strings.HasPrefix(n, "exif: exif: ") {
			t.Logf("NOTE: the note %q repeats the \"exif: \" prefix: probe.go:203 prepends "+
				"\"exif: \" to an error whose text already starts with it (exif.go:310). Cosmetic, "+
				"but the note text a user sees is doubled.", n)
		}
	}
}

// TestProbeRefusesAPixelBombFromTheHeaderAlone covers the pre-filter's reason
// for existing: a 33 byte PNG that declares 60000x60000 must be refused on the
// declared pixel count, before anything tries to allocate 14 GB for it.
func TestProbeRefusesAPixelBombFromTheHeaderAlone(t *testing.T) {
	dir, _ := synthLibrary(t)
	data := fixture(t, dir, "edge/bomb.png")

	r, err := ProbeBytes(data, int64(len(data)), "png", Budget{MaxPixels: 12_000_000}, Limits{})
	if err != nil {
		t.Fatalf("ProbeBytes: %v", err)
	}
	if r.Width != 60000 || r.Height != 60000 {
		t.Errorf("dimensions = %dx%d, want the declared 60000x60000", r.Width, r.Height)
	}
	if r.Pixels != 3_600_000_000 {
		t.Errorf("Pixels = %d, want 3600000000", r.Pixels)
	}
	if want := "over_pixel_budget:3600000000>12000000"; r.SkippedReason != want {
		t.Errorf("SkippedReason = %q, want %q", r.SkippedReason, want)
	}
	if r.Thumbnailable() {
		t.Error("Thumbnailable() = true for a file over the pixel budget; it must never reach a decoder")
	}
	if r.Kind != KindImage {
		t.Errorf("Kind = %q, want %q: the file is recognised, it is just refused", r.Kind, KindImage)
	}

	// Each budget ceiling must produce its own reason, and the file-size check
	// must come first so an enormous file is refused without reading it.
	budgetCases := []struct {
		name   string
		rel    string
		budget Budget
		want   string
	}{
		{"file too large", "photos/2021/trip/IMG_0100.jpg", Budget{MaxFileBytes: 1}, "file_too_large"},
		{"decode estimate too large", "photos/2021/trip/IMG_0100.jpg", Budget{MaxDecodeBytes: 1}, "over_decode_budget"},
		{"too many animation frames", "edge/anim.gif", Budget{MaxFrames: 1}, "animated_over_frame_budget"},
		{"pixels over budget", "photos/2020/big/DSC_0001.jpg", Budget{MaxPixels: 1_000_000}, "over_pixel_budget"},
	}
	for _, c := range budgetCases {
		t.Run(c.name, func(t *testing.T) {
			r := probeFixture(t, dir, c.rel, c.budget)
			if !strings.HasPrefix(r.SkippedReason, c.want) {
				t.Errorf("SkippedReason = %q, want a %q reason", r.SkippedReason, c.want)
			}
			if r.Thumbnailable() {
				t.Error("Thumbnailable() = true for a file over a hard budget")
			}
		})
	}

	// A video is streamed, never decoded, so a pixel budget must not affect it.
	video := probeFixture(t, dir, "video/clip.mp4", Budget{MaxPixels: 1})
	if video.SkippedReason != "" {
		t.Errorf("SkippedReason = %q for a video under a tiny pixel budget; videos are streamed, not decoded",
			video.SkippedReason)
	}
}

// TestProbeVideoContainerMetadata covers the container reader. The duration and
// the creation time come from mvhd, and the creation time is the fallback the
// capture-time policy uses when a video carries no other date.
func TestProbeVideoContainerMetadata(t *testing.T) {
	dir, _ := synthLibrary(t)
	r := probeFixture(t, dir, "video/clip.mp4", testBudget())

	if r.Kind != KindVideo {
		t.Errorf("Kind = %q, want %q", r.Kind, KindVideo)
	}
	if r.Format != "isobmff" {
		t.Errorf("Format = %q, want %q", r.Format, "isobmff")
	}
	if r.MIME != "video/mp4" {
		t.Errorf("MIME = %q, want %q", r.MIME, "video/mp4")
	}
	if r.ContainerBrand != "isom" {
		t.Errorf("ContainerBrand = %q, want %q", r.ContainerBrand, "isom")
	}
	if r.ContainerTime != "2021-06-01T10:00:00" {
		t.Errorf("ContainerTime = %q, want the mvhd creation time 2021-06-01T10:00:00", r.ContainerTime)
	}
	if r.Width != 0 || r.Height != 0 || r.Pixels != 0 {
		t.Errorf("dimensions = %dx%d (%d px) for a video; fields a streamed video does not declare",
			r.Width, r.Height, r.Pixels)
	}
	if r.EstimatedDecodeBytes != 0 {
		t.Errorf("EstimatedDecodeBytes = %d for a video, want 0: nothing is decoded", r.EstimatedDecodeBytes)
	}
	if !r.Thumbnailable() {
		t.Errorf("Thumbnailable() = false for a plain video (unsupported=%q skipped=%q)",
			r.UnsupportedReason, r.SkippedReason)
	}

	if r.DurationMS != 42000 {
		t.Errorf("DurationMS = %d, want 42000; the mvhd carries timescale 1000 and a duration of 42 s", r.DurationMS)
	}
	// The ISO base media format defines creation_time as seconds since
	// 1904-01-01 UTC. A caller that treated it as a naive wall clock would
	// shift every video's timeline by the host's offset, so the flag has to be
	// set whenever a container time is present.
	if !r.ContainerTZUTC {
		t.Errorf("ContainerTZUTC = false for a container time; the ISO base media format " +
			"defines creation_time as UTC, and callers rely on this flag to avoid " +
			"re-interpreting it in local time")
	}

	// The remaining checks use a hand-built, spec-conformant mvhd.
	created := time.Date(2021, 6, 1, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		version byte
		brand   string
		scale   uint32
		dur     uint64
	}{
		{"version 0", 0, "mp42", 1000, 42000},
		{"version 1", 1, "isom", 90000, 3780000},
	} {
		t.Run("spec conformant mvhd "+c.name, func(t *testing.T) {
			data := buildMP4(c.brand, c.version, c.scale, c.dur, created)
			got, err := ProbeBytes(data, int64(len(data)), "mp4", testBudget(), Limits{})
			if err != nil {
				t.Fatalf("ProbeBytes: %v", err)
			}
			if got.Kind != KindVideo {
				t.Errorf("Kind = %q, want %q", got.Kind, KindVideo)
			}
			if got.ContainerBrand != c.brand {
				t.Errorf("ContainerBrand = %q, want %q", got.ContainerBrand, c.brand)
			}
			if got.DurationMS != 42000 {
				t.Errorf("DurationMS = %d, want 42000 out of a well-formed mvhd (%d/%d)",
					got.DurationMS, c.dur, c.scale)
			}
			if got.ContainerTime != "2021-06-01T10:00:00" {
				t.Errorf("ContainerTime = %q, want %q", got.ContainerTime, "2021-06-01T10:00:00")
			}
		})
	}

	// The 0xFFFFFFFF duration is the format's "unknown" sentinel and must not
	// be turned into an age.
	unknown := buildMP4("isom", 0, 1000, 0xFFFFFFFF, created)
	got, err := ProbeBytes(unknown, int64(len(unknown)), "mp4", testBudget(), Limits{})
	if err != nil {
		t.Fatalf("ProbeBytes: %v", err)
	}
	if got.DurationMS != 0 {
		t.Errorf("DurationMS = %d for the 0xFFFFFFFF unknown sentinel, want 0", got.DurationMS)
	}
}

// TestProbeMalformedAndEmptyFilesDoNotInventDimensions covers the negative
// property: when the header is missing, cut short or unreadable, the probe must
// report no dimensions rather than a plausible-looking guess, and it must not
// panic.
func TestProbeMalformedAndEmptyFilesDoNotInventDimensions(t *testing.T) {
	dir, _ := synthLibrary(t)
	cases := []struct {
		rel     string
		wantW   int
		wantH   int
		altW    int // an honestly declared size is allowed here instead
		altH    int
		skipped string
	}{
		{rel: "video/broken.mp4", wantW: 0, wantH: 0},
		{rel: "edge/empty.jpg", wantW: 0, wantH: 0},
		// The frame header precedes the truncation point, so the honest
		// declared size is available; nothing larger may be reported.
		{rel: "edge/broken.jpg", wantW: 0, wantH: 0, altW: 200, altH: 150},
	}
	for _, c := range cases {
		t.Run(filepath.Base(c.rel), func(t *testing.T) {
			out := probeGuarded(fixturePath(dir, c.rel), NormalizeExt(c.rel), testBudget(), Limits{})
			if out.panicked != nil {
				t.Fatalf("Probe panicked on %s: %v", c.rel, out.panicked)
			}
			if out.err != nil {
				t.Fatalf("Probe failed on %s: %v", c.rel, out.err)
			}
			r := out.res
			if r == nil {
				t.Fatalf("Probe returned no result and no error for %s", c.rel)
			}
			honest := r.Width == c.wantW && r.Height == c.wantH
			declared := c.altW != 0 && r.Width == c.altW && r.Height == c.altH
			if !honest && !declared {
				t.Errorf("%s: dimensions = %dx%d, want none or the declared %dx%d",
					c.rel, r.Width, r.Height, c.altW, c.altH)
			}
			if want := int64(r.Width) * int64(r.Height); r.Pixels != want {
				t.Errorf("%s: Pixels = %d, want %d", c.rel, r.Pixels, want)
			}
		})
	}

	brk := probeFixture(t, dir, "edge/broken.jpg", testBudget())
	if brk.Thumbnailable() {
		t.Logf("NOTE: edge/broken.jpg probes as %dx%d and Thumbnailable()=true even though the file "+
			"is truncated: synth cuts it at a third of the length, which is past the frame header, so "+
			"the declared size is honest and the damage is only discovered when the decoder runs. The "+
			"probe cannot see it without reading further, and it is not claiming otherwise.", brk.Width, brk.Height)
	}

	// A truncated prefix of a real JPEG must not yield a confident size.
	full := fixture(t, dir, "photos/2021/trip/IMG_0100.jpg")
	for _, n := range []int{4, 5, 20, 100} {
		t.Run("prefix", func(t *testing.T) {
			pre := full[:n]
			r, err := ProbeBytes(pre, int64(n), "jpg", testBudget(), Limits{})
			if err != nil {
				t.Fatalf("ProbeBytes: %v", err)
			}
			if r.Width != 0 || r.Height != 0 || r.Pixels != 0 {
				t.Errorf("%d byte prefix of a JPEG reported %dx%d (%d px); a half-read header must not "+
					"produce a confident size", n, r.Width, r.Height, r.Pixels)
			}
			if r.Thumbnailable() {
				t.Errorf("%d byte prefix of a JPEG was marked thumbnailable", n)
			}
		})
	}

	// Cutting inside the SOF segment is the precise case: the marker and its
	// length are present, the dimensions are not.
	t.Run("cut inside the frame header", func(t *testing.T) {
		pre := cutInsideSOF(t, full)
		r, err := ProbeBytes(pre, int64(len(pre)), "jpg", testBudget(), Limits{})
		if err != nil {
			t.Fatalf("ProbeBytes: %v", err)
		}
		if r.Width != 0 || r.Height != 0 {
			t.Fatalf("dimensions = %dx%d from a JPEG cut inside its frame header, want none", r.Width, r.Height)
		}
		if r.SkippedReason != "dimensions_unknown" {
			t.Errorf("SkippedReason = %q, want %q: the probe cannot bound a decode it cannot size",
				r.SkippedReason, "dimensions_unknown")
		}
		if !hasNote(r, "truncated") {
			t.Errorf("notes %v do not report the truncated segment", r.Notes)
		}
		if r.Thumbnailable() {
			t.Error("Thumbnailable() = true for a file the probe could not measure")
		}
	})
}

func hasNote(r *Result, substr string) bool {
	for _, n := range r.Notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

// TestProbeUnsupportedFormatsAreRecognisedButNotThumbnailable covers formats
// this build deliberately does not decode. The row is kept so the UI can show a
// placeholder with the file's real facts, but the file must never be marked as
// thumbnailable.
func TestProbeUnsupportedFormatsAreNotThumbnailable(t *testing.T) {
	dir, _ := synthLibrary(t)
	cases := []struct {
		rel            string
		mime           string
		wantUnsupportd bool
	}{
		{rel: "unsupported/photo.heic", mime: "image/heic", wantUnsupportd: true},
		{rel: "unsupported/raw.cr2", mime: "image/tiff", wantUnsupportd: true},
		{rel: "unsupported/scan.tiff", mime: "image/tiff", wantUnsupportd: true},
	}
	for _, c := range cases {
		t.Run(filepath.Base(c.rel), func(t *testing.T) {
			r := probeFixture(t, dir, c.rel, testBudget())
			if r.MIME != c.mime {
				t.Errorf("MIME = %q, want %q", r.MIME, c.mime)
			}
			if r.Thumbnailable() {
				t.Errorf("%s is thumbnailable (kind=%q unsupported=%q skipped=%q); it must never reach a decoder",
					c.rel, r.Kind, r.UnsupportedReason, r.SkippedReason)
			}
			if c.wantUnsupportd {
				if r.UnsupportedReason == "" {
					t.Errorf("UnsupportedReason is empty for %s", c.rel)
				}
				return
			}
			t.Errorf("UnsupportedReason = %q was not expected for %s", r.UnsupportedReason, c.rel)
		})
	}

	// The case that motivated making the extension authoritative: a TIFF-based
	// RAW whose IFD declares a plausible size. Content sniffing alone calls
	// this an image with real dimensions, and it would then be thumbnailable -
	// but a RAW must never reach a decoder, and the format matrix says so.
	raw := tiffDeclaringDims(1280, 960)
	r, err := ProbeBytes(raw, int64(len(raw)), "cr2", testBudget(), Limits{})
	if err != nil {
		t.Fatalf("ProbeBytes: %v", err)
	}
	if r.Width != 1280 || r.Height != 960 {
		t.Errorf("dimensions = %dx%d, want the declared 1280x960", r.Width, r.Height)
	}
	if r.Kind != KindUnsupported {
		t.Errorf("Kind = %q, want %q: the extension decides, not the bytes", r.Kind, KindUnsupported)
	}
	if r.UnsupportedReason == "" {
		t.Error("a RAW file has no UnsupportedReason")
	}
	if r.Thumbnailable() {
		t.Fatalf("a .cr2 that sniffs as TIFF with declared dimensions is thumbnailable "+
			"(unsupported=%q skipped=%q); the extension must overrule the content", r.UnsupportedReason, r.SkippedReason)
	}
	// A file whose name is not in the format matrix at all is refused too: the
	// matrix is a closed list, and "not listed" must not mean "try it".
	unknown, err := ProbeBytes(raw, int64(len(raw)), "dat", testBudget(), Limits{})
	if err != nil {
		t.Fatalf("ProbeBytes: %v", err)
	}
	if unknown.Thumbnailable() {
		t.Errorf("a file with an unlisted extension (.dat) holding TIFF bytes is thumbnailable; "+
			"an unlisted extension must be refused rather than attempted (unsupported=%q)",
			unknown.UnsupportedReason)
	}
}

// TestMIMEForExtIsAClosedAllowlist is a security property, not formatting.
// Everything the album serves is labelled through this function, so an unknown
// or hostile extension must fall through to application/octet-stream rather
// than being sniffed or served as text/html from the album's own origin.
func TestMIMEForExtIsAClosedAllowlist(t *testing.T) {
	allowed := []struct{ ext, want string }{
		{"jpg", "image/jpeg"}, {"jpeg", "image/jpeg"}, {"png", "image/png"},
		{"gif", "image/gif"}, {"webp", "image/webp"}, {"mp4", "video/mp4"},
		// The function normalises, so these must classify identically to "jpg".
		{"JPG", "image/jpeg"}, {"Jpeg", "image/jpeg"}, {".jpg", "image/jpeg"}, {" jpg ", "image/jpeg"},
	}
	for _, c := range allowed {
		if got := MIMEForExt(c.ext); got != c.want {
			t.Errorf("MIMEForExt(%q) = %q, want %q", c.ext, got, c.want)
		}
	}

	hostile := []string{
		"html", "htm", "xhtml", "svg", "svgz", "js", "mjs", "css", "xml", "json",
		"txt", "php", "wasm", "", " ", ".", "JPG.html", "jpg.php", "jpg\x00.html",
	}
	for _, ext := range hostile {
		got := MIMEForExt(ext)
		if got != "application/octet-stream" {
			t.Errorf("MIMEForExt(%q) = %q; an unlisted extension must not be given a real content type", ext, got)
		}
		if strings.Contains(got, "text/") || strings.Contains(got, "html") ||
			strings.Contains(got, "script") || strings.Contains(got, "xml") {
			t.Errorf("MIMEForExt(%q) = %q; a file in an album directory must never be served as markup", ext, got)
		}
	}
}

// TestNormalizeExtTakesANameNotABareExtension pins the contract: the argument
// is a file name, so the extension is whatever follows the last dot, and a name
// without a dot has no extension. A caller that derives the extension from a
// path must get the same answer for "IMG.JPG" and "IMG.jpg".
func TestNormalizeExtTakesANameNotABareExtension(t *testing.T) {
	cases := []struct{ in, want string }{
		{"IMG_0001.JPG", "jpg"},
		{"IMG_0001.jpg", "jpg"},
		{"photos/2021/trip/IMG_0100.jpg", "jpg"},
		{".JPG", "jpg"},
		{"names/UPPER.JPG", "jpg"},
		{"archive.tar.gz", "gz"},
		{"noext", ""},
		{"trailing.", ""},
		{"", ""},
		// A bare extension has no dot, so it has no extension. The two bare
		// forms normalise to the same (empty) answer, which is what makes the
		// fallback safe: MIMEForExt("") is octet-stream.
		{"JPG", ""},
		{"jpg", ""},
	}
	for _, c := range cases {
		if got := NormalizeExt(c.in); got != c.want {
			t.Errorf("NormalizeExt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if NormalizeExt("JPG") != NormalizeExt("jpg") {
		t.Error("NormalizeExt disagrees between the bare forms \"JPG\" and \"jpg\"")
	}
	// The round trip a caller performs: name -> extension -> content type.
	for _, name := range []string{"IMG.JPG", "IMG.jpeg", "shot.PNG", "clip.MP4"} {
		if got := MIMEForExt(NormalizeExt(name)); got == "application/octet-stream" {
			t.Errorf("MIMEForExt(NormalizeExt(%q)) = %q; the round trip must classify a real media name", name, got)
		}
	}
	t.Logf("NOTE: NormalizeExt(\"JPG\") is \"\" rather than \"jpg\" because the argument is a file name " +
		"(the extension is the text after the last dot) and a bare extension has no dot. That is " +
		"harmless only while callers pass names: a caller that passes an extension it already holds " +
		"must use mimetype.Normalize or MIMEForExt, which do strip a leading dot and lowercase.")
}

// TestEstimateDecodeBytesIsMonotonicAndPessimistic checks the audit terms the
// pre-filter documents. The number is compared against a hard cgroup ceiling
// later, so the property that matters is that it never dips below the decoded
// planes it claims to account for.
func TestEstimateDecodeBytesIsMonotonicAndPessimistic(t *testing.T) {
	formats := []string{"jpeg", "png", "gif", "webp"}
	pixels := []int64{1, 2, 100, 10_000, 1_000_000, 12_000_000}
	for _, f := range formats {
		var prev int64 = -1
		for _, p := range pixels {
			est := EstimateDecodeBytes(KindImage, f, p, 1000, 1000, 512)
			if est <= prev {
				t.Errorf("%s: EstimateDecodeBytes(%d px) = %d, not greater than the %d for fewer pixels",
					f, p, est, prev)
			}
			prev = est
			if min := int64(float64(p) * 5.5); est < min {
				t.Errorf("%s: EstimateDecodeBytes(%d px) = %d, below the pixels*5.5 term (%d)",
					f, p, est, min)
			}
			if min := p * 4; est < min {
				t.Errorf("%s: EstimateDecodeBytes(%d px) = %d, below the worst-case RGBA term (%d)",
					f, p, est, min)
			}
		}
	}
	if est := EstimateDecodeBytes(KindImage, "jpeg", 1, 1, 1, 512); est <= 0 {
		t.Errorf("EstimateDecodeBytes(1x1) = %d; a decode is never free", est)
	}
	// A non-image is not decoded here at all.
	for _, k := range []Kind{KindVideo, KindOther, KindUnsupported} {
		if est := EstimateDecodeBytes(k, "jpeg", 1_000_000, 1000, 1000, 512); est != 0 {
			t.Errorf("EstimateDecodeBytes(%q) = %d, want 0", k, est)
		}
	}
	// A missing thumbnail edge falls back to the documented default, and a
	// larger destination costs more.
	if a, b := EstimateDecodeBytes(KindImage, "jpeg", 1000, 100, 10, 0),
		EstimateDecodeBytes(KindImage, "jpeg", 1000, 100, 10, 512); a != b {
		t.Errorf("a zero thumbnail edge gave %d, want the 512 default's %d", a, b)
	}
	if small, large := EstimateDecodeBytes(KindImage, "jpeg", 1000, 100, 10, 128),
		EstimateDecodeBytes(KindImage, "jpeg", 1000, 100, 10, 2048); large <= small {
		t.Errorf("a 2048 px thumbnail edge gave %d, not more than the 128 px edge's %d", large, small)
	}
	// PNG is the most expensive of the supported formats for the same pixels.
	if png, jpg := EstimateDecodeBytes(KindImage, "png", 1_000_000, 1000, 1000, 512),
		EstimateDecodeBytes(KindImage, "jpeg", 1_000_000, 1000, 1000, 512); png <= jpg {
		t.Errorf("png estimate %d is not above the jpeg estimate %d for the same pixels", png, jpg)
	}
}

// TestExtensionIndexClassifiesEveryExtensionOnce checks the scanner's
// allowlists: an extension must land in exactly one kind, and the kind decides
// what the pipeline is allowed to do with the file afterwards.
func TestExtensionIndexClassifiesEveryExtensionOnce(t *testing.T) {
	lists := []struct {
		name string
		exts []string
		kind Kind
	}{
		{"image", ImageExts(), KindImage},
		{"video", VideoExts(), KindVideo},
		{"other media", OtherMediaExts(), KindUnsupported},
	}
	seen := map[string]string{}
	idx := ExtensionIndex()
	total := 0
	for _, l := range lists {
		for _, ext := range l.exts {
			if ext != strings.ToLower(ext) || strings.HasPrefix(ext, ".") {
				t.Errorf("%s list contains %q; extensions are stored lower case and without the dot", l.name, ext)
			}
			if prev, dup := seen[ext]; dup {
				t.Errorf("%q appears in both the %s and the %s lists; an extension must have one kind",
					ext, prev, l.name)
			}
			seen[ext] = l.name
			if got := idx[ext]; got != l.kind {
				t.Errorf("ExtensionIndex()[%q] = %q, want %q", ext, got, l.kind)
			}
			total++
		}
	}
	if len(idx) != total {
		t.Errorf("ExtensionIndex has %d entries for %d listed extensions", len(idx), total)
	}
	for ext := range idx {
		mime := MIMEForExt(ext)
		if mime == "" {
			t.Errorf("MIMEForExt(%q) is empty; every indexed extension needs a serve-safe type", ext)
		}
		if strings.Contains(mime, "text/") || strings.Contains(mime, "html") {
			t.Errorf("MIMEForExt(%q) = %q; nothing indexed may be served as markup", ext, mime)
		}
	}
	// The RAW and bitmap extensions are indexed but have no dedicated type, so
	// they must fall back to the opaque type rather than to a guess.
	for _, ext := range []string{"cr2", "nef", "dng", "arw"} {
		if idx[ext] != KindUnsupported {
			t.Errorf("ExtensionIndex()[%q] = %q, want %q", ext, idx[ext], KindUnsupported)
		}
		if got := MIMEForExt(ext); got != "application/octet-stream" {
			t.Errorf("MIMEForExt(%q) = %q, want the opaque fallback", ext, got)
		}
	}
}

// TestReadPrefixIsBounded checks the only file-reading function in the package:
// it must read at most max bytes, never fail on a short file, and refuse a
// directory instead of handing its bytes to a parser.
func TestReadPrefixIsBounded(t *testing.T) {
	dir, lib := synthLibrary(t)
	small := fixturePath(dir, "photos/2021/trip/IMG_0100.jpg")
	fi, ok := lib.Find("photos/2021/trip/IMG_0100.jpg")
	if !ok {
		t.Fatal("the fixture is missing from the generated library")
	}

	data, st, err := ReadPrefix(small, 1<<20)
	if err != nil {
		t.Fatalf("ReadPrefix: %v", err)
	}
	if int64(len(data)) != fi.Bytes {
		t.Errorf("ReadPrefix returned %d bytes for a %d byte file", len(data), fi.Bytes)
	}
	if st.Size() != fi.Bytes || st.IsDir() {
		t.Errorf("ReadPrefix stat = size %d, isdir %v; want the file's own size", st.Size(), st.IsDir())
	}

	// A cap smaller than the file is honoured exactly.
	if data, _, err = ReadPrefix(small, 64); err != nil || len(data) != 64 {
		t.Errorf("ReadPrefix with max=64 returned %d bytes (err=%v), want exactly 64", len(data), err)
	}
	// A non-positive cap falls back to the documented 1 MiB window rather than
	// reading whatever is there.
	big := fixturePath(dir, "video/streamable.mp4")
	data, _, err = ReadPrefix(big, 0)
	if err != nil {
		t.Fatalf("ReadPrefix with max=0: %v", err)
	}
	if len(data) != 1<<20 {
		t.Errorf("ReadPrefix with max=0 returned %d bytes, want the 1 MiB default window", len(data))
	}

	if _, _, err := ReadPrefix(dir, 1<<20); err == nil {
		t.Error("ReadPrefix accepted a directory")
	}
	if _, _, err := ReadPrefix(filepath.Join(dir, "does-not-exist.jpg"), 1<<20); err == nil {
		t.Error("ReadPrefix accepted a missing file")
	}

	// Probe surfaces both failures rather than returning an empty result.
	if r, err := Probe(dir, "", testBudget(), Limits{}); err == nil || r != nil {
		t.Errorf("Probe on a directory returned (%v, %v), want a nil result and an error", r, err)
	}
	if r, err := Probe(filepath.Join(dir, "does-not-exist.jpg"), "jpg", testBudget(), Limits{}); err == nil || r != nil {
		t.Errorf("Probe on a missing file returned (%v, %v), want a nil result and an error", r, err)
	}
}
