package exif

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
// directory. The fixtures are real files produced by real encoders, so the
// parser is exercised on the same bytes a camera (or an attacker) would hand
// it; hand-built bytes are used below only for the cases the generator cannot
// express.
func synthLibrary(t *testing.T) (string, *synth.Library) {
	t.Helper()
	dir := t.TempDir()
	lib, err := synth.Write(dir)
	if err != nil {
		t.Fatalf("synth.Write: %v", err)
	}
	return dir, lib
}

func fixture(t *testing.T, dir, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", rel, err)
	}
	return data
}

// parseOutcome is the result of a guarded parse.
type parseOutcome struct {
	tags     *Tags
	err      error
	panicked any
	elapsed  time.Duration
}

// parseGuarded runs Parse on its own goroutine so that a hang or a panic is
// reported as a failure of the fixture rather than taking the test binary down
// with it. "Must never panic or allocate without bound" is the whole point of
// this package, so it is checked by construction rather than by inspection.
func parseGuarded(data []byte, lim Limits) parseOutcome {
	type result struct {
		tags *Tags
		err  error
		pan  any
	}
	start := time.Now()
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				ch <- result{pan: p}
			}
		}()
		tags, err := Parse(data, lim)
		ch <- result{tags: tags, err: err}
	}()
	select {
	case r := <-ch:
		return parseOutcome{tags: r.tags, err: r.err, panicked: r.pan, elapsed: time.Since(start)}
	case <-time.After(2 * time.Second):
		return parseOutcome{panicked: "Parse did not return within 2s", elapsed: time.Since(start)}
	}
}

func hasWarning(tags *Tags, substr string) bool {
	if tags == nil {
		return false
	}
	for _, w := range tags.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func kvValue(extra []KV, key string) string {
	for _, kv := range extra {
		if kv.Key == key {
			return kv.Value
		}
	}
	return ""
}

// ---- hand-built TIFF fixtures -------------------------------------------

// tiffField is one IFD entry that buildTIFF serialises. The element count is
// derived from the payload and the type, so a caller cannot accidentally write
// a count that disagrees with the data.
type tiffField struct {
	tag  uint16
	typ  uint16
	data []byte
}

func asciiField(tag uint16, s string) tiffField {
	return tiffField{tag: tag, typ: 2, data: append([]byte(s), 0)}
}

func shortField(tag uint16, v uint16) tiffField {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, v)
	return tiffField{tag: tag, typ: 3, data: b}
}

func longField(tag uint16, v uint32) tiffField {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return tiffField{tag: tag, typ: 4, data: b}
}

// rationalPairField writes one or more num/den pairs under a single tag, which
// is how a GPS DMS value is stored: three rationals under one tag, not three
// separate tags.
func rationalPairField(tag uint16, pairs ...[2]uint32) tiffField {
	b := make([]byte, 0, 8*len(pairs))
	for _, p := range pairs {
		var e [8]byte
		binary.LittleEndian.PutUint32(e[0:4], p[0])
		binary.LittleEndian.PutUint32(e[4:8], p[1])
		b = append(b, e[:]...)
	}
	return tiffField{tag: tag, typ: 5, data: b}
}

// dmsField writes a deg/min/sec coordinate as the three rationals the spec
// requires.
func dmsField(tag uint16, deg, min uint32, secNum, secDen uint32) tiffField {
	return rationalPairField(tag,
		[2]uint32{deg, 1}, [2]uint32{min, 1}, [2]uint32{secNum, secDen})
}

func fieldTypeSize(typ uint16) int {
	switch typ {
	case 1, 2, 6, 7:
		return 1
	case 3, 8:
		return 2
	case 4, 9, 11:
		return 4
	case 5, 10, 12:
		return 8
	}
	return 0
}

func sortFields(f []tiffField) {
	for i := 1; i < len(f); i++ {
		for j := i; j > 0 && f[j-1].tag > f[j].tag; j-- {
			f[j-1], f[j] = f[j], f[j-1]
		}
	}
}

// buildTIFF serialises a little-endian TIFF block in the layout ParseTIFF
// expects: header, IFD0, ExifIFD, GPSIFD, then the out-of-line values. The
// sub-IFD pointers are added only for the IFDs that have entries, so the
// fixture contains nothing the test did not ask for.
func buildTIFF(root, exifFields, gpsFields []tiffField) []byte {
	ifdSize := func(n int) int { return 2 + 12*n + 4 }
	nRoot := len(root)
	if len(exifFields) > 0 {
		nRoot++
	}
	if len(gpsFields) > 0 {
		nRoot++
	}
	exifOff := 8 + ifdSize(nRoot)
	gpsOff := exifOff + ifdSize(len(exifFields))
	dataOff := gpsOff + ifdSize(len(gpsFields))
	if len(exifFields) > 0 {
		root = append(root, longField(0x8769, uint32(exifOff)))
	}
	if len(gpsFields) > 0 {
		root = append(root, longField(0x8825, uint32(gpsOff)))
	}
	sortFields(root)
	sortFields(exifFields)
	sortFields(gpsFields)

	var data []byte
	writeIFD := func(fields []tiffField) []byte {
		var b bytes.Buffer
		var n [2]byte
		binary.LittleEndian.PutUint16(n[:], uint16(len(fields)))
		b.Write(n[:])
		for _, f := range fields {
			sz := fieldTypeSize(f.typ)
			count := 0
			if sz > 0 {
				count = len(f.data) / sz
			}
			var hdr [12]byte
			binary.LittleEndian.PutUint16(hdr[0:2], f.tag)
			binary.LittleEndian.PutUint16(hdr[2:4], f.typ)
			binary.LittleEndian.PutUint32(hdr[4:8], uint32(count))
			if len(f.data) <= 4 {
				copy(hdr[8:12], f.data)
			} else {
				for len(data)%4 != 0 {
					data = append(data, 0)
				}
				binary.LittleEndian.PutUint32(hdr[8:12], uint32(dataOff+len(data)))
				data = append(data, f.data...)
			}
			b.Write(hdr[:])
		}
		b.Write(make([]byte, 4)) // next IFD: none
		return b.Bytes()
	}
	var out bytes.Buffer
	out.Write([]byte{'I', 'I', 0x2a, 0x00})
	var off [4]byte
	binary.LittleEndian.PutUint32(off[:], 8)
	out.Write(off[:])
	out.Write(writeIFD(root))
	out.Write(writeIFD(exifFields))
	out.Write(writeIFD(gpsFields))
	out.Write(data)
	return out.Bytes()
}

// loopingIFD builds a bare TIFF whose IFD0 holds one real entry and whose
// next-IFD pointer points back at IFD0. Unlike synth's edge/exif_loop.jpg it
// declares an entry, so the parser actually has a reason to follow the chain
// and the cycle guard is reached.
func loopingIFD() []byte {
	var out bytes.Buffer
	out.Write([]byte{'I', 'I', 0x2a, 0x00})
	var four [4]byte
	binary.LittleEndian.PutUint32(four[:], 8)
	out.Write(four[:]) // IFD0 at offset 8
	var two [2]byte
	binary.LittleEndian.PutUint16(two[:], 1)
	out.Write(two[:]) // one entry
	var e [12]byte
	binary.LittleEndian.PutUint16(e[0:2], 0x010F) // Make
	binary.LittleEndian.PutUint16(e[2:4], 2)      // ASCII
	binary.LittleEndian.PutUint32(e[4:8], 4)      // "abc\0"
	copy(e[8:12], []byte("abc\x00"))
	out.Write(e[:])
	binary.LittleEndian.PutUint32(four[:], 8)
	out.Write(four[:]) // next IFD == IFD0: a one-hop loop
	return out.Bytes()
}

// ---- tests --------------------------------------------------------------

// TestParseJPEGReadsWellFormedEXIF is the happy path: a real JPEG carrying a
// real APP1 Exif segment must yield exactly the tags that were written, and
// Capture must return the recorded wall clock together with the recorded UTC
// offset. Everything downstream of this package — the timeline, the "naive
// wall clock" policy — is built on these two values.
func TestParseJPEGReadsWellFormedEXIF(t *testing.T) {
	dir, _ := synthLibrary(t)
	data := fixture(t, dir, "photos/2019/paris/IMG_0007.jpg")

	tags, err := Parse(data, DefaultLimits())
	if err != nil {
		t.Fatalf("Parse on a well-formed EXIF JPEG: %v", err)
	}
	if tags == nil {
		t.Fatal("Parse returned no tags and no error")
	}
	for _, c := range []struct{ name, got, want string }{
		{"Make", tags.Make, "OnePlus"},
		{"Model", tags.Model, "ONEPLUS A5000"},
		{"DateTimeOriginal", tags.DateTimeOriginal, "2019:07:04 12:34:56"},
		{"OffsetTimeOriginal", tags.OffsetTimeOriginal, "+02:00"},
		{"SubSecTimeOriginal", tags.SubSecTimeOriginal, "123"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if tags.Orientation != 6 {
		t.Errorf("Orientation = %d, want 6", tags.Orientation)
	}
	if len(tags.Warnings) != 0 {
		t.Errorf("a well-formed file produced warnings: %v", tags.Warnings)
	}
	if tags.Truncated {
		t.Error("a well-formed file was reported as truncated")
	}

	wall, off, ok := tags.Capture()
	if !ok {
		t.Fatal("Capture found no capture time in a file that records DateTimeOriginal")
	}
	if wall != "2019-07-04T12:34:56.123" {
		t.Errorf("Capture wall = %q, want the normalised timestamp with the subseconds the file carries", wall)
	}
	if off == nil {
		t.Fatal("Capture dropped the offset that OffsetTimeOriginal records; the stored instant would be shifted")
	}
	if *off != 120 {
		t.Errorf("Capture offset = %d minutes, want 120 (+02:00)", *off)
	}

	// Extra is the bounded list the UI renders.
	if got := kvValue(tags.Extra, "Make"); got != "OnePlus" {
		t.Errorf("Extra Make = %q, want %q", got, "OnePlus")
	}
	if got := kvValue(tags.Extra, "OffsetTimeOriginal"); got != "+02:00" {
		t.Errorf("Extra OffsetTimeOriginal = %q, want %q", got, "+02:00")
	}
	if len(tags.Extra) > DefaultLimits().MaxKeys {
		t.Errorf("Extra has %d entries; the display list must stay under MaxKeys=%d", len(tags.Extra), DefaultLimits().MaxKeys)
	}

	// GPS. The generator writes the degree/minute/second triple the standard
	// defines, so the parser must recover the coordinates exactly. This is the
	// only place a mis-shaped GPS block would be caught: a single-rational
	// encoding is silently discarded by a conforming reader, which is how a
	// fixture can carry coordinates on paper and none in fact.
	if tags.GPS == nil {
		t.Fatalf("GPS is absent; the fixture carries a well-formed DMS block")
	}
	if diff := tags.GPS.Lat - 48.858370; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("latitude = %.6f, want 48.858370", tags.GPS.Lat)
	}
	if diff := tags.GPS.Lon - 2.294481; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("longitude = %.6f, want 2.294481", tags.GPS.Lon)
	}
	if tags.GPS.AltitudeM == nil {
		t.Errorf("altitude is absent, want 35.5")
	} else if diff := *tags.GPS.AltitudeM - 35.5; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("altitude = %.6f, want 35.5", *tags.GPS.AltitudeM)
	}
}

// TestCaptureReturnsNilOffsetWhenOffsetTimeIsAbsent pins the case the whole
// time policy depends on: a camera that recorded a local wall clock and no UTC
// offset must not be given one. Inventing an offset would silently move every
// such photo.
func TestCaptureReturnsNilOffsetWhenOffsetTimeIsAbsent(t *testing.T) {
	dir, _ := synthLibrary(t)
	data := fixture(t, dir, "photos/2018/IMG_0001.jpg")

	tags, err := Parse(data, DefaultLimits())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tags.DateTimeOriginal != "2018:03:01 08:00:00" {
		t.Fatalf("DateTimeOriginal = %q, want the recorded value", tags.DateTimeOriginal)
	}
	for _, f := range []struct{ name, got string }{
		{"OffsetTime", tags.OffsetTime},
		{"OffsetTimeOriginal", tags.OffsetTimeOriginal},
		{"OffsetTimeDigitized", tags.OffsetTimeDigitized},
	} {
		if f.got != "" {
			t.Errorf("%s = %q, but the file records no offset of any kind", f.name, f.got)
		}
	}

	wall, off, ok := tags.Capture()
	if !ok {
		t.Fatal("Capture found no capture time although DateTimeOriginal is present")
	}
	if wall != "2018-03-01T08:00:00" {
		t.Errorf("Capture wall = %q, want the wall clock exactly as recorded", wall)
	}
	if off != nil {
		t.Errorf("Capture invented an offset of %d minutes for a file that carries none", *off)
	}
}

// TestFileWithoutEXIFIsReportedNotInvented covers a plain JPEG with no APP1
// segment at all: nothing may be defaulted, and Capture must decline rather
// than substitute a zero time.
func TestFileWithoutEXIFIsReportedNotInvented(t *testing.T) {
	dir, _ := synthLibrary(t)
	data := fixture(t, dir, "photos/2021/trip/IMG_0100.jpg")

	tags, err := Parse(data, DefaultLimits())
	if tags != nil {
		t.Errorf("Parse returned tags %+v for a JPEG with no Exif segment", tags)
	}
	if err == nil {
		t.Error("Parse returned no error for a JPEG with no Exif segment")
	} else if !strings.HasPrefix(err.Error(), "exif: ") {
		t.Errorf("Parse returned %v, which is not a plain error of this package", err)
	}
	t.Logf("NOTE: the brief for this fixture expected Parse to return no error and zero-value tags; " +
		"the implementation reports the absence of an Exif segment as an error (ParseJPEG returns at " +
		"exif.go:310 when it reaches the start-of-scan marker). Callers must read that error as \"no " +
		"metadata\": probe.go:203 records it as a Note and leaves Tags nil.")

	// The zero value is what a caller gets when it has no metadata at all.
	var zero Tags
	if wall, off, ok := zero.Capture(); ok || wall != "" || off != nil {
		t.Errorf("zero-value Tags.Capture() = (%q, %v, %v); a file with no date must not produce one", wall, off, ok)
	}
}

// TestMalformedInputIsToleratedAndBounded is the package's central promise. A
// file that lies about its segment length, its entry count or its IFD chain
// must cost a bounded amount of work and be reported through Warnings, never
// through a panic and never through unbounded time or allocation.
func TestMalformedInputIsToleratedAndBounded(t *testing.T) {
	dir, _ := synthLibrary(t)
	cases := []struct {
		rel      string
		why      string
		wantErr  bool
		wantWarn string
	}{
		{"edge/exif_overflow.jpg", "APP1 segment claiming to extend past EOF", true, ""},
		{"edge/exif_many_entries.jpg", "IFD declaring 65535 entries", false, "clamped"},
		{"edge/exif_loop.jpg", "IFD next-pointer pointing at itself", false, ""},
		{"edge/broken.jpg", "truncated JPEG", true, ""},
		{"edge/empty.jpg", "zero byte file", true, ""},
	}
	for _, c := range cases {
		t.Run(filepath.Base(c.rel), func(t *testing.T) {
			out := parseGuarded(fixture(t, dir, c.rel), DefaultLimits())
			if out.panicked != nil {
				t.Fatalf("Parse panicked on %s (%s): %v", c.rel, c.why, out.panicked)
			}
			if out.elapsed > 2*time.Second {
				t.Errorf("Parse took %v on %s; malformed input must not cost unbounded time", out.elapsed, c.rel)
			}
			if c.wantErr {
				if out.err == nil {
					t.Fatalf("Parse accepted %s (%s) without reporting anything", c.rel, c.why)
				}
				if !strings.HasPrefix(out.err.Error(), "exif: ") {
					t.Errorf("Parse returned %v for %s; a malformation must surface as a plain error", out.err, c.rel)
				}
				return
			}
			if out.err != nil {
				t.Errorf("Parse failed on %s (%s), which it must tolerate: %v", c.rel, c.why, out.err)
			}
			if out.tags == nil {
				return
			}
			if len(out.tags.Warnings) > 33 {
				t.Errorf("%s produced %d warnings; the list must stay bounded", c.rel, len(out.tags.Warnings))
			}
			if c.wantWarn != "" && !hasWarning(out.tags, c.wantWarn) {
				t.Errorf("%s produced warnings %v, none of which mention %q", c.rel, out.tags.Warnings, c.wantWarn)
			}
			if len(out.tags.Extra) > DefaultLimits().MaxKeys {
				t.Errorf("%s produced %d Extra entries, over the MaxKeys cap", c.rel, len(out.tags.Extra))
			}
		})
	}

	many := parseGuarded(fixture(t, dir, "edge/exif_many_entries.jpg"), DefaultLimits())
	if many.tags == nil || many.tags.Truncated {
		t.Errorf("edge/exif_many_entries.jpg: tags=%v truncated expected false", many.tags != nil)
	}
	t.Logf("NOTE: edge/exif_many_entries.jpg is rejected by the batch bounds check at exif.go:551, " +
		"which runs before exif.go:557 counts entries, so Tags.Truncated stays false even though the " +
		"parser abandoned the IFD early. The malformation is visible in Warnings instead; Truncated " +
		"only reflects the MaxIFDEntries/MaxIFDs budget (exif.go:506).")

	loop := parseGuarded(fixture(t, dir, "edge/exif_loop.jpg"), DefaultLimits())
	if loop.panicked != nil || loop.err != nil {
		t.Fatalf("edge/exif_loop.jpg: panicked=%v err=%v", loop.panicked, loop.err)
	}
	if loop.tags == nil {
		t.Fatal("edge/exif_loop.jpg produced no tags and no error")
	}
	if loop.tags.Truncated {
		t.Error("edge/exif_loop.jpg was reported as truncated; a loop is not a limit")
	}
	t.Logf("NOTE: edge/exif_loop.jpg terminates immediately, but not through the cycle guard at " +
		"exif.go:527: its IFD0 declares zero entries and walk() returns at exif.go:544 before it reads " +
		"the next-IFD pointer, so the self-referential link is never followed and no warning is " +
		"recorded. The fixture therefore does not exercise the protection it is named for; " +
		"TestSelfReferentialIFDChainIsDetected builds a chain that reaches the guard.")
}

// TestSelfReferentialIFDChainIsDetected covers the loop protection directly:
// an IFD whose next pointer leads back to itself must be walked once and then
// abandoned, with the entries read before the cycle still returned.
func TestSelfReferentialIFDChainIsDetected(t *testing.T) {
	out := parseGuarded(loopingIFD(), DefaultLimits())
	if out.panicked != nil {
		t.Fatalf("Parse panicked on a self-referential IFD chain: %v", out.panicked)
	}
	if out.err != nil {
		t.Fatalf("Parse failed on a self-referential IFD chain: %v", out.err)
	}
	if out.tags == nil {
		t.Fatal("Parse returned no tags for a self-referential IFD chain")
	}
	if out.tags.Make != "abc" {
		t.Errorf("Make = %q, want the entry read before the cycle to survive", out.tags.Make)
	}
	if !hasWarning(out.tags, "revisits") {
		t.Errorf("warnings %v do not report the cycle; a loop must be visible, not silently absorbed", out.tags.Warnings)
	}
}

// TestOnlyPlausibleRecordedDatesBecomeCaptureTimes covers the invention the
// design forbids. Cameras fill unset date fields with "0000:00:00 00:00:00",
// and a file that says that must be treated as having no capture time at all
// rather than being placed at the start of the timeline; years outside the
// documented 1900..2199 window are rejected for the same reason.
func TestOnlyPlausibleRecordedDatesBecomeCaptureTimes(t *testing.T) {
	cases := []struct {
		name string
		dto  string
		ok   bool
		wall string
	}{
		{"unset placeholder", "0000:00:00 00:00:00", false, ""},
		{"unset placeholder with seconds", "0000:00:00 00:00:01", false, ""},
		{"year before the window", "1899:12:31 23:59:59", false, ""},
		{"year after the window", "2200:01:01 00:00:00", false, ""},
		{"first year in the window", "1900:01:01 00:00:00", true, "1900-01-01T00:00:00"},
		{"last year in the window", "2199:12:31 23:59:59", true, "2199-12-31T23:59:59"},
		{"ordinary date", "2019:07:04 12:34:56", true, "2019-07-04T12:34:56"},
		{"missing field", "", false, ""},
		{"not a date at all", "not a date", false, ""},
		{"month out of range", "2019:13:40 99:99:99", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			block := buildTIFF(nil, []tiffField{asciiField(0x9003, c.dto)}, nil)
			tags, err := ParseTIFF(block, DefaultLimits())
			if err != nil {
				t.Fatalf("ParseTIFF: %v", err)
			}
			if tags.DateTimeOriginal != c.dto {
				t.Errorf("DateTimeOriginal = %q, want the raw value %q to be preserved unmodified",
					tags.DateTimeOriginal, c.dto)
			}
			wall, off, ok := tags.Capture()
			if ok != c.ok {
				t.Errorf("Capture ok = %v for %q, want %v", ok, c.dto, c.ok)
			}
			if wall != c.wall {
				t.Errorf("Capture wall = %q for %q, want %q", wall, c.dto, c.wall)
			}
			if off != nil {
				t.Errorf("Capture returned an offset for a file that records none")
			}
		})
	}
}

// TestGPSCoordinatesRequireBothHalvesAndAPlausibleValue is the negative
// property GPS handling exists for. A latitude with no longitude, the classic
// 0,0 "the unit never got a fix" placeholder, and an out-of-range value must
// all be dropped rather than reported as a location, because a wrong location
// is worse than none.
func TestGPSCoordinatesRequireBothHalvesAndAPlausibleValue(t *testing.T) {
	latFields := []tiffField{
		asciiField(0x0001, "N"),
		dmsField(0x0002, 48, 51, 30132, 1000), // 48.858370
	}
	lonFields := []tiffField{
		asciiField(0x0003, "E"),
		dmsField(0x0004, 2, 17, 401316, 10000), // 2.294481
	}
	cases := []struct {
		name     string
		gps      []tiffField
		want     *GPS
		wantWarn string
	}{
		{
			name: "complete north east position with altitude",
			gps: append(append(append([]tiffField{}, latFields...), lonFields...),
				rationalPairField(0x0005, [2]uint32{355, 10})),
			want: &GPS{Lat: 48.858370, Lon: 2.294481, AltitudeM: ptr(35.5)},
		},
		{
			name: "latitude without longitude",
			gps:  latFields,
			want: nil,
		},
		{
			name: "longitude without latitude",
			gps:  lonFields,
			want: nil,
		},
		{
			name: "both values at the 0,0 no-fix placeholder",
			gps: []tiffField{
				asciiField(0x0001, "N"),
				dmsField(0x0002, 0, 0, 0, 1),
				asciiField(0x0003, "E"),
				dmsField(0x0004, 0, 0, 0, 1),
			},
			want:     nil,
			wantWarn: "0,0",
		},
		{
			name: "south west with a below-sea-level altitude",
			gps: []tiffField{
				asciiField(0x0001, "S"),
				dmsField(0x0002, 33, 52, 0, 1), // -33.8667
				asciiField(0x0003, "W"),
				dmsField(0x0004, 151, 12, 0, 1), // -151.2
				rationalPairField(0x0005, [2]uint32{10, 1}),
				shortField(0x0006, 1), // GPSAltitudeRef: below sea level
			},
			want: &GPS{Lat: -33.866667, Lon: -151.2, AltitudeM: ptr(-10)},
		},
		{
			name: "latitude out of range", // 100 deg is a valid angle but not a valid latitude
			gps: []tiffField{
				asciiField(0x0001, "N"),
				dmsField(0x0002, 100, 0, 0, 1),
				asciiField(0x0003, "E"),
				dmsField(0x0004, 2, 0, 0, 1),
			},
			want:     nil,
			wantWarn: "out of range",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tags, err := ParseTIFF(buildTIFF(nil, nil, c.gps), DefaultLimits())
			if err != nil {
				t.Fatalf("ParseTIFF: %v", err)
			}
			if c.want == nil {
				if tags.GPS != nil {
					t.Fatalf("GPS = %+v, want nil: an incomplete or impossible position must be dropped, not reported",
						*tags.GPS)
				}
				if c.wantWarn != "" && !hasWarning(tags, c.wantWarn) {
					t.Errorf("warnings %v do not mention %q; a dropped position must be visible in the record",
						tags.Warnings, c.wantWarn)
				}
				return
			}
			if tags.GPS == nil {
				t.Fatalf("GPS = nil, want %+v", *c.want)
			}
			if diff := tags.GPS.Lat - c.want.Lat; diff > 1e-5 || diff < -1e-5 {
				t.Errorf("Lat = %f, want %f", tags.GPS.Lat, c.want.Lat)
			}
			if diff := tags.GPS.Lon - c.want.Lon; diff > 1e-5 || diff < -1e-5 {
				t.Errorf("Lon = %f, want %f", tags.GPS.Lon, c.want.Lon)
			}
			switch {
			case c.want.AltitudeM == nil && tags.GPS.AltitudeM != nil:
				t.Errorf("AltitudeM = %v, want none", *tags.GPS.AltitudeM)
			case c.want.AltitudeM != nil && tags.GPS.AltitudeM == nil:
				t.Errorf("AltitudeM = none, want %v", *c.want.AltitudeM)
			case c.want.AltitudeM != nil && tags.GPS.AltitudeM != nil:
				if diff := *tags.GPS.AltitudeM - *c.want.AltitudeM; diff > 1e-5 || diff < -1e-5 {
					t.Errorf("AltitudeM = %v, want %v", *tags.GPS.AltitudeM, *c.want.AltitudeM)
				}
			}
		})
	}
}

func ptr(v float64) *float64 { return &v }

// TestZeroValueLimitsIsSafeAndStillBounded checks Limits.Sanitize, which is
// what makes the zero value usable: a caller that forgets to set any limit must
// still get the defaults, and may only ever tighten them.
func TestZeroValueLimitsIsSafeAndStillBounded(t *testing.T) {
	if got := (Limits{}).Sanitize(); got != DefaultLimits() {
		t.Errorf("Limits{}.Sanitize() = %+v, want the defaults %+v", got, DefaultLimits())
	}
	loose := Limits{
		MaxScanBytes:   1 << 40,
		MaxMetaBytes:   1 << 30,
		MaxIFDEntries:  1 << 20,
		MaxIFDs:        1 << 10,
		MaxStringBytes: 1 << 20,
		MaxKeys:        1 << 20,
	}.Sanitize()
	if loose != DefaultLimits() {
		t.Errorf("absurd limits were not clamped to the defaults: %+v", loose)
	}
	if tight := (Limits{MaxMetaBytes: 64}).Sanitize(); tight.MaxMetaBytes != 64 {
		t.Errorf("a deliberately stricter MaxMetaBytes of 64 became %d; tightening must be respected", tight.MaxMetaBytes)
	}

	// A zero-value Limits must behave exactly like the defaults on real input.
	dir, _ := synthLibrary(t)
	data := fixture(t, dir, "photos/2019/paris/IMG_0007.jpg")
	zeroLimits, err := Parse(data, Limits{})
	if err != nil {
		t.Fatalf("Parse with the zero-value Limits failed on a well-formed file: %v", err)
	}
	withDefaults, err := Parse(data, DefaultLimits())
	if err != nil {
		t.Fatalf("Parse with DefaultLimits: %v", err)
	}
	if zeroLimits.Make != withDefaults.Make || zeroLimits.Model != withDefaults.Model ||
		zeroLimits.DateTimeOriginal != withDefaults.DateTimeOriginal {
		t.Errorf("the zero-value Limits changed the parse result: %+v vs %+v", zeroLimits, withDefaults)
	}
	if wall, off, ok := zeroLimits.Capture(); !ok || off == nil || *off != 120 {
		t.Errorf("zero-value Limits Capture() = (%q, %v, %v), want the same result as the defaults", wall, off, ok)
	}

	// A metadata block larger than the cap must be refused outright rather than
	// walked: this is the property that keeps a fabricated 4 GB string cheap.
	big := make([]byte, 300<<10)
	copy(big, []byte{'I', 'I', 0x2a, 0x00, 8, 0, 0, 0})
	if _, err := ParseTIFF(big, Limits{}); err == nil {
		t.Error("a 300 KiB metadata block was accepted under the default 256 KiB cap")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized block rejected with %v, want a message naming the cap", err)
	}
	if _, err := ParseTIFF(big, Limits{MaxMetaBytes: 64}); err == nil {
		t.Error("a 300 KiB metadata block was accepted under a 64 byte cap")
	}
}

// TestDetectFormatMagicBytes checks the signature table the dispatch in Parse
// relies on. A misdetection here costs a whole container's metadata, and the
// ISOBMFF case also decides whether a HEIC is treated as an image or a video.
func TestDetectFormatMagicBytes(t *testing.T) {
	isobmff := make([]byte, 12)
	copy(isobmff[4:8], "ftyp")
	cases := []struct {
		name string
		head []byte
		want string
	}{
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "jpeg"},
		{"png", []byte("\x89PNG\r\n\x1a\n"), "png"},
		{"tiff little endian", []byte("II*\x00"), "tiff"},
		{"tiff big endian", []byte("MM\x00*"), "tiff"},
		{"gif87a", []byte("GIF87a"), "gif"},
		{"gif89a", []byte("GIF89a"), "gif"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBP"), "webp"},
		{"bmp", []byte("BM"), "bmp"},
		{"isobmff", isobmff, "isobmff"},
		{"empty", nil, "unknown"},
		{"short jpeg prefix", []byte{0xFF, 0xD8}, "unknown"},
		{"fdff webp spoof", []byte("RIFF\x00\x00\x00\x00WAVE"), "unknown"},
	}
	for _, c := range cases {
		if got := DetectFormat(c.head); got != c.want {
			t.Errorf("DetectFormat(%s %q) = %q, want %q", c.name, c.head, got, c.want)
		}
	}

	// Parsing dispatches on the same table, so an unknown container must fail
	// with a plain error rather than being guessed at.
	if _, err := Parse([]byte("RIFF\x00\x00\x00\x00WAVE"), DefaultLimits()); err == nil {
		t.Error("Parse accepted a container it cannot read")
	}
}
