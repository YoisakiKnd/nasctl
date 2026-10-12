// Package synth generates synthetic media files for tests.
//
// It exists so that the test suite exercises real bytes rather than mocks: a
// real JPEG with a hand-built EXIF IFD, a real PNG whose IHDR declares an
// impossible pixel count, a real truncated file. Every failure this project
// cares about is a failure of a decoder on real input, and a stub would not
// reproduce any of them.
//
// It is imported only from tests.
package synth

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"time"
)

// File describes one generated file so tests can assert against it.
type File struct {
	Rel      string
	Kind     string
	Bytes    int64
	Width    int
	Height   int
	HasEXIF  bool
	TakenUTC string // "2006:01:02 15:04:05" as EXIF would carry it, or empty
	Note     string
}

// Library is the generated set.
type Library struct {
	Root  string
	Files []File
}

// Find returns the record for a relative path.
func (l *Library) Find(rel string) (File, bool) {
	for _, f := range l.Files {
		if f.Rel == rel {
			return f, true
		}
	}
	return File{}, false
}

// Write creates the whole synthetic library under dir.
func Write(dir string) (*Library, error) {
	lib := &Library{Root: dir}
	add := func(rel string, data []byte) error {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return err
		}
		lib.Files = append(lib.Files, File{Rel: rel, Bytes: int64(len(data))})
		return nil
	}
	note := func(rel, kind, taken string, w, h int, hasEXIF bool, n string) {
		for i := range lib.Files {
			if lib.Files[i].Rel == rel {
				lib.Files[i].Kind = kind
				lib.Files[i].TakenUTC = taken
				lib.Files[i].Width, lib.Files[i].Height = w, h
				lib.Files[i].HasEXIF = hasEXIF
				lib.Files[i].Note = n
				return
			}
		}
	}

	// --- ordinary photographs -------------------------------------------
	j, err := jpegBytes(640, 480, 90)
	if err != nil {
		return nil, err
	}
	if err := add("photos/2021/trip/IMG_0100.jpg", j); err != nil {
		return nil, err
	}
	note("photos/2021/trip/IMG_0100.jpg", "image", "", 640, 480, false, "plain JPEG, no EXIF")

	// A JPEG carrying a real EXIF APP1 with capture time, camera and GPS.
	exifJPEG, err := jpegWithEXIF(1280, 960, EXIFInfo{
		DateTimeOriginal: "2019:07:04 12:34:56",
		OffsetTime:       "+02:00",
		SubSec:           "123",
		Make:             "OnePlus",
		Model:            "ONEPLUS A5000",
		Orientation:      6,
		Latitude:         48.858370,
		Longitude:        2.294481,
		Altitude:         35.5,
	})
	if err != nil {
		return nil, err
	}
	if err := add("photos/2019/paris/IMG_0007.jpg", exifJPEG); err != nil {
		return nil, err
	}
	note("photos/2019/paris/IMG_0007.jpg", "image", "2019:07:04 12:34:56", 1280, 960, true,
		"EXIF with offset, GPS and orientation 6")

	// EXIF without an offset: the naive wall clock case.
	noOffset, err := jpegWithEXIF(320, 240, EXIFInfo{
		DateTimeOriginal: "2018:03:01 08:00:00",
		Make:             "Xiaomi",
		Model:            "MI 8",
	})
	if err != nil {
		return nil, err
	}
	if err := add("photos/2018/IMG_0001.jpg", noOffset); err != nil {
		return nil, err
	}
	note("photos/2018/IMG_0001.jpg", "image", "2018:03:01 08:00:00", 320, 240, true,
		"EXIF without OffsetTime: naive local time")

	// A large but legal photograph: over the 512 px thumbnail edge, well inside
	// the pixel budget.
	large, err := jpegBytes(4000, 3000, 88)
	if err != nil {
		return nil, err
	}
	if err := add("photos/2020/big/DSC_0001.jpg", large); err != nil {
		return nil, err
	}
	note("photos/2020/big/DSC_0001.jpg", "image", "", 4000, 3000, false, "12 MP, downscaled for the thumbnail")

	// PNG with an alpha channel: exercises the white-background compositing.
	p, err := pngWithAlpha(240, 180)
	if err != nil {
		return nil, err
	}
	if err := add("photos/2022/Art/logo.png", p); err != nil {
		return nil, err
	}
	note("photos/2022/Art/logo.png", "image", "", 240, 180, false, "RGBA PNG with transparent pixels")

	// --- malformed and hostile input ------------------------------------
	if err := add("edge/broken.jpg", truncatedJPEG()); err != nil {
		return nil, err
	}
	note("edge/broken.jpg", "", "", 0, 0, false, "truncated JPEG: header ends mid-frame")

	if err := add("edge/empty.jpg", nil); err != nil {
		return nil, err
	}
	note("edge/empty.jpg", "", "", 0, 0, false, "zero byte file")

	// A 33 byte PNG whose IHDR declares 60000x60000. A naive decoder allocates
	// 14.4 GB for this. albumctl must refuse it from the header alone.
	if err := add("edge/bomb.png", pixelBombPNG(60000, 60000)); err != nil {
		return nil, err
	}
	note("edge/bomb.png", "", "", 60000, 60000, false, "PNG header declaring 3.6 gigapixels")

	// A tiny file that inflates enormously.
	if err := add("edge/bomb2.png", pixelBombPNG(40000, 40000)); err != nil {
		return nil, err
	}
	note("edge/bomb2.png", "", "", 40000, 40000, false, "second pixel bomb, smaller header")

	// EXIF that lies about its own size. The generator itself must not fail on
	// hostile input, so a construction error is reported rather than fatal.
	for _, hx := range []struct {
		rel, why string
		build    hostileEXIF
	}{
		{"edge/exif_overflow.jpg", "APP1 segment claiming to extend past EOF", overflowEXIF},
		{"edge/exif_many_entries.jpg", "IFD declaring 65535 entries", manyEntriesEXIF},
		{"edge/exif_loop.jpg", "IFD next-pointer pointing at itself", loopEXIF},
	} {
		data, herr := jpegWithHostileEXIF(hx.build)
		if herr != nil {
			return nil, fmt.Errorf("synth: %s: %w", hx.rel, herr)
		}
		if err := add(hx.rel, data); err != nil {
			return nil, err
		}
		note(hx.rel, "", "", 0, 0, false, hx.why)
	}
	note("edge/exif_overflow.jpg", "", "", 0, 0, false, "APP1 segment claiming to extend past EOF")

	// EXIF with an IFD entry count far larger than the segment.
	// --- animated and odd formats ---------------------------------------
	g, err := animatedGIF(160, 120, 6)
	if err != nil {
		return nil, err
	}
	if err := add("edge/anim.gif", g); err != nil {
		return nil, err
	}
	note("edge/anim.gif", "", "", 160, 120, false, "6 frame GIF: only the first frame is decoded")

	tiny, err := singlePixelGIF()
	if err != nil {
		return nil, err
	}
	if err := add("edge/tiny.gif", tiny); err != nil {
		return nil, err
	}
	note("edge/tiny.gif", "", "", 1, 1, false, "1x1 GIF")

	tall, terr := tallPNG(2, 9000)
	if terr != nil {
		return nil, terr
	}
	if err := add("edge/tall.png", tall); err != nil {
		return nil, err
	}
	note("edge/tall.png", "", "", 2, 9000, false, "extreme aspect ratio")

	// --- names that are awkward or hostile -------------------------------
	small, err := jpegBytes(80, 60, 80)
	if err != nil {
		return nil, err
	}
	for _, n := range []struct{ rel, why string }{
		{"names/中文名字 照片.jpg", "CJK with a space"},
		{"names/emoji 🎞️ name.jpg", "emoji and a variation selector"},
		{"names/quote'and\"quote.jpg", "single and double quotes"},
		{"names/<img src=x onerror=alert(1)>.jpg", "HTML injection attempt"},
		{"names/</script><script>alert(1)</script>.jpg", "script tag attempt"},
		{"names/&amp;&lt;&gt;.jpg", "pre-escaped entities"},
		{"names/newline\nin\nname.jpg", "embedded newlines"},
		{"names/tab\tname.jpg", "embedded tab"},
		{"names/control\x01char.jpg", "control character"},
		{"names/.hidden.jpg", "dotfile"},
		{"names/trailing space .jpg", "trailing space before the extension"},
		{"names/UPPER.JPG", "uppercase extension"},
		{"names/no_extension", "no extension at all"},
	} {
		if err := add(n.rel, small); err != nil {
			return nil, err
		}
		note(n.rel, "image", "", 80, 60, false, n.why)
	}
	if err := add("names/deep/a/b/c/d/e/f/g/far.jpg", small); err != nil {
		return nil, err
	}
	note("names/deep/a/b/c/d/e/f/g/far.jpg", "image", "", 80, 60, false, "eight levels deep")

	// --- video ------------------------------------------------------------
	if err := add("video/clip.mp4", mp4WithMVHD(42*time.Second, time.Date(2021, 6, 1, 10, 0, 0, 0, time.UTC))); err != nil {
		return nil, err
	}
	note("video/clip.mp4", "video", "", 0, 0, false, "minimal ISOBMFF with mvhd duration and creation time")

	if err := add("video/broken.mp4", []byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p'}); err != nil {
		return nil, err
	}
	note("video/broken.mp4", "", "", 0, 0, false, "ftyp box truncated after the type field")

	// --- formats that are deliberately not supported ----------------------
	if err := add("unsupported/photo.heic", []byte("not really heic")); err != nil {
		return nil, err
	}
	note("unsupported/photo.heic", "", "", 0, 0, false, "HEIC: probe only, never decoded")

	if err := add("unsupported/raw.cr2", []byte("II\x2a\x00\x10\x00\x00\x00")); err != nil {
		return nil, err
	}
	note("unsupported/raw.cr2", "", "", 0, 0, false, "Canon RAW")

	if err := add("unsupported/scan.tiff", tiffHeader()); err != nil {
		return nil, err
	}
	note("unsupported/scan.tiff", "", "", 0, 0, false, "bare TIFF")

	// --- files that must be ignored ---------------------------------------
	if err := add("photos/2021/trip/IMG_0100.jpg.tmp", small); err != nil {
		return nil, err
	}
	note("photos/2021/trip/IMG_0100.jpg.tmp", "", "", 0, 0, false, "in-progress temporary file")
	if err := add(".albumctl-partial", small); err != nil {
		return nil, err
	}
	note(".albumctl-partial", "", "", 0, 0, false, "looks like one of our own temp files")
	if err := add("photos/notes.txt", []byte("not media")); err != nil {
		return nil, err
	}
	note("photos/notes.txt", "", "", 0, 0, false, "text file")
	if err := add("photos/thumbs.db", []byte("junk")); err != nil {
		return nil, err
	}
	note("photos/thumbs.db", "", "", 0, 0, false, "Windows thumbnail database")

	// A file large enough that a single read would be noticeable, used by the
	// streaming and Range tests.
	big := make([]byte, 3<<20)
	for i := range big {
		big[i] = byte(i % 251)
	}
	jhead, err := jpegBytes(64, 64, 70)
	if err != nil {
		return nil, err
	}
	copy(big, jhead)
	if err := add("video/streamable.mp4", big); err != nil {
		return nil, err
	}
	note("video/streamable.mp4", "video", "", 0, 0, false, "3 MiB file for streaming and Range tests")

	return lib, nil
}

// ---- image construction -------------------------------------------------

func gradient(w, h int) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.Set(x, y, color.RGBA{
				R: uint8((x * 255) / max(1, w)),
				G: uint8((y * 255) / max(1, h)),
				B: uint8(((x + y) * 255) / max(1, w+h)),
				A: 255,
			})
		}
	}
	return m
}

func jpegBytes(w, h, q int) ([]byte, error) {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, gradient(w, h), &jpeg.Options{Quality: q}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func pngWithAlpha(w, h int) ([]byte, error) {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// A fully transparent border, an opaque gradient in the middle.
			a := uint8(255)
			if x < 4 || y < 4 || x >= w-4 || y >= h-4 {
				a = 0
			}
			m.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, a})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func tallPNG(w, h int) ([]byte, error) {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.Set(x, y, color.RGBA{uint8(y % 256), 0, 0, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func singlePixelGIF() ([]byte, error) {
	img := image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{color.Black, color.White})
	img.SetColorIndex(0, 0, 1)
	var b bytes.Buffer
	if err := gif.Encode(&b, img, nil); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func animatedGIF(w, h, frames int) ([]byte, error) {
	pal := color.Palette{}
	for i := 0; i < 256; i++ {
		pal = append(pal, color.RGBA{uint8(i), uint8(255 - i), uint8(i / 2), 255})
	}
	g := &gif.GIF{}
	for f := 0; f < frames; f++ {
		p := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				p.SetColorIndex(x, y, uint8((x+y+f*17)%256))
			}
		}
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, 10)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func truncatedJPEG() []byte {
	full, err := jpegBytes(200, 150, 85)
	if err != nil {
		return []byte{0xff, 0xd8}
	}
	return full[:len(full)/3]
}

// pixelBombPNG writes a valid PNG signature and a valid IHDR chunk followed by
// an IEND. The image data is absent, but every header check a decoder performs
// happens before it would notice: the declared dimensions are what matter.
func pixelBombPNG(w, h uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8] = 8  // bit depth
	ihdr[9] = 6  // colour type RGBA
	ihdr[10] = 0 // deflate
	ihdr[11] = 0 // adaptive filtering
	ihdr[12] = 0 // no interlace
	b.Write(chunk("IHDR", ihdr))
	b.Write(chunk("IEND", nil))
	return b.Bytes()
}

func chunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(data)))
	b.Write(n[:])
	b.WriteString(typ)
	b.Write(data)
	crc := crc32IEEE(append([]byte(typ), data...))
	binary.BigEndian.PutUint32(n[:], crc)
	b.Write(n[:])
	return b.Bytes()
}

var crcTable = func() [256]uint32 {
	var t [256]uint32
	for n := 0; n < 256; n++ {
		c := uint32(n)
		for k := 0; k < 8; k++ {
			if c&1 != 0 {
				c = 0xedb88320 ^ (c >> 1)
			} else {
				c >>= 1
			}
		}
		t[n] = c
	}
	return t
}()

func crc32IEEE(p []byte) uint32 {
	c := ^uint32(0)
	for _, b := range p {
		c = crcTable[(c^uint32(b))&0xff] ^ (c >> 8)
	}
	return ^c
}

func tiffHeader() []byte {
	var b bytes.Buffer
	b.Write([]byte{'I', 'I', 0x2a, 0x00})
	var off [4]byte
	binary.LittleEndian.PutUint32(off[:], 8)
	b.Write(off[:])
	b.Write(make([]byte, 2)) // zero entries
	b.Write(make([]byte, 4)) // no next IFD
	return b.Bytes()
}

// ---- EXIF construction --------------------------------------------------

// EXIFInfo is the subset of tags the tests need written.
type EXIFInfo struct {
	DateTimeOriginal string // "2006:01:02 15:04:05"
	OffsetTime       string // "+02:00"
	SubSec           string
	Make             string
	Model            string
	Orientation      uint16
	Latitude         float64
	Longitude        float64
	Altitude         float64
}

// jpegWithEXIF builds a real JPEG and splices a real, well-formed EXIF APP1
// segment in directly after SOI.
func jpegWithEXIF(w, h int, info EXIFInfo) ([]byte, error) {
	base, err := jpegBytes(w, h, 85)
	if err != nil {
		return nil, err
	}
	app1 := exifAPP1(info)
	out := make([]byte, 0, len(base)+len(app1))
	// SOI is the first two bytes. EXIF must follow it before any other marker.
	out = append(out, base[:2]...)
	out = append(out, app1...)
	out = append(out, base[2:]...)
	return out, nil
}

type ifdEntry struct {
	tag   uint16
	typ   uint16
	count uint32
	data  []byte // raw payload, little endian

	// placeholder marks a sub-IFD pointer whose value is not known until the
	// layout is computed; offset is the resolved position of an external value.
	placeholder bool
	offset      int
}

func (e ifdEntry) size() int {
	switch e.typ {
	case 1, 7:
		return int(e.count)
	case 2:
		return int(e.count)
	case 3:
		return int(e.count) * 2
	case 4:
		return int(e.count) * 4
	case 5, 10:
		return int(e.count) * 8
	}
	return int(e.count) * 4
}

func asciiEntry(tag uint16, s string) ifdEntry {
	b := append([]byte(s), 0)
	return ifdEntry{tag: tag, typ: 2, count: uint32(len(b)), data: b}
}

func shortEntry(tag uint16, v uint16) ifdEntry {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	return ifdEntry{tag: tag, typ: 3, count: 1, data: b[:]}
}

func rationalEntry(tag uint16, vals ...float64) ifdEntry {
	var b bytes.Buffer
	for _, v := range vals {
		den := uint32(1000000)
		num := uint32(math.Round(v * float64(den)))
		var t [4]byte
		binary.LittleEndian.PutUint32(t[:], num)
		b.Write(t[:])
		binary.LittleEndian.PutUint32(t[:], den)
		b.Write(t[:])
	}
	return ifdEntry{tag: tag, typ: 5, count: uint32(len(vals)), data: b.Bytes()}
}

// exifAPP1 serialises a complete little-endian TIFF with IFD0, ExifIFD and
// GPSIFD, then wraps it in an APP1 marker segment.
func exifAPP1(info EXIFInfo) []byte {
	var ifd0 []ifdEntry

	if info.Make != "" {
		ifd0 = append(ifd0, asciiEntry(0x010F, info.Make))
	}
	if info.Model != "" {
		ifd0 = append(ifd0, asciiEntry(0x0110, info.Model))
	}
	if info.Orientation != 0 {
		ifd0 = append(ifd0, shortEntry(0x0112, info.Orientation))
	}

	var exifEntries []ifdEntry
	if info.DateTimeOriginal != "" {
		exifEntries = append(exifEntries, asciiEntry(0x9003, info.DateTimeOriginal))
	}
	if info.OffsetTime != "" {
		exifEntries = append(exifEntries, asciiEntry(0x9011, info.OffsetTime))
	}
	if info.SubSec != "" {
		exifEntries = append(exifEntries, asciiEntry(0x9291, info.SubSec))
	}

	// GPS works in degrees/minutes/seconds, not decimal degrees: the standard
	// stores three rationals per coordinate. A single rational is what a naive
	// writer produces, and any conforming reader discards it.
	var gpsEntries []ifdEntry
	if info.Latitude != 0 || info.Longitude != 0 {
		gpsEntries = append(gpsEntries,
			asciiEntry(0x0001, ref('N', 'S', info.Latitude)),
			rationalEntry(0x0002, dms(info.Latitude)...),
			asciiEntry(0x0003, ref('E', 'W', info.Longitude)),
			rationalEntry(0x0004, dms(info.Longitude)...),
		)
		if info.Altitude != 0 {
			// 0x0005 is GPSAltitude, an unsigned rational.
			gpsEntries = append(gpsEntries, rationalEntry(0x0005, math.Abs(info.Altitude)))
		}
	}

	// Pointers into the sub-IFDs.
	if len(exifEntries) > 0 {
		ifd0 = append(ifd0, ifdEntry{tag: 0x8769, typ: 4, count: 1, data: nil, placeholder: true})
	}
	if len(gpsEntries) > 0 {
		ifd0 = append(ifd0, ifdEntry{tag: 0x8825, typ: 4, count: 1, data: nil, placeholder: true})
	}
	sortEntries(ifd0)
	sortEntries(exifEntries)
	sortEntries(gpsEntries)

	// Layout: TIFF header (8) + IFD0 + ExifIFD + GPSIFD, then all external
	// data. Offsets are relative to the start of the TIFF header, which begins
	// immediately after the six byte "Exif\0\0" identifier.
	ifdSize := func(n int) int { return 2 + 12*n + 4 }
	exifOff := 8 + ifdSize(len(ifd0))
	gpsOff := exifOff + ifdSize(len(exifEntries))
	dataOff := gpsOff + ifdSize(len(gpsEntries))

	// Patch the sub-IFD pointers now that their offsets are known.
	for i := range ifd0 {
		switch ifd0[i].tag {
		case 0x8769:
			ifd0[i].data = le32(uint32(exifOff))
			ifd0[i].placeholder = false
		case 0x8825:
			ifd0[i].data = le32(uint32(gpsOff))
			ifd0[i].placeholder = false
		}
	}

	var data bytes.Buffer
	assign := func(entries []ifdEntry) {
		for i := range entries {
			if entries[i].placeholder {
				continue
			}
			if entries[i].size() <= 4 {
				continue
			}
			padTo4(&data)
			entries[i].offset = dataOff + data.Len()
			data.Write(entries[i].data)
		}
	}
	assign(ifd0)
	assign(exifEntries)
	assign(gpsEntries)

	var tiff bytes.Buffer
	tiff.Write([]byte{'I', 'I', 0x2a, 0x00})
	tiff.Write(le32(8))
	tiff.Write(serializeIFD(ifd0))
	tiff.Write(serializeIFD(exifEntries))
	tiff.Write(serializeIFD(gpsEntries))

	var full bytes.Buffer
	full.Write([]byte("Exif\x00\x00"))
	full.Write(tiff.Bytes())
	full.Write(data.Bytes())

	payload := full.Bytes()
	var seg bytes.Buffer
	seg.Write([]byte{0xFF, 0xE1})
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(payload)+2))
	seg.Write(l[:])
	seg.Write(payload)
	return seg.Bytes()
}

func serializeIFD(entries []ifdEntry) []byte {
	var b bytes.Buffer
	var n [2]byte
	binary.LittleEndian.PutUint16(n[:], uint16(len(entries)))
	b.Write(n[:])
	for _, e := range entries {
		var hdr bytes.Buffer
		var t [4]byte
		binary.LittleEndian.PutUint16(t[:2], e.tag)
		hdr.Write(t[:2])
		binary.LittleEndian.PutUint16(t[:2], e.typ)
		hdr.Write(t[:2])
		binary.LittleEndian.PutUint32(t[:], e.count)
		hdr.Write(t[:])
		if e.size() <= 4 {
			pad := make([]byte, 4)
			copy(pad, e.data)
			hdr.Write(pad)
		} else {
			binary.LittleEndian.PutUint32(t[:], uint32(e.offset))
			hdr.Write(t[:])
		}
		b.Write(hdr.Bytes())
	}
	b.Write(make([]byte, 4)) // next IFD: none
	return b.Bytes()
}

func sortEntries(e []ifdEntry) {
	for i := 1; i < len(e); i++ {
		for j := i; j > 0 && e[j-1].tag > e[j].tag; j-- {
			e[j-1], e[j] = e[j], e[j-1]
		}
	}
}

func le16(v uint16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, v)
	return b
}

func le32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

func padTo4(b *bytes.Buffer) {
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}

func abs32(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// dms splits a decimal degree value into the degree/minute/second triple the
// GPS IFD requires.
func dms(v float64) []float64 {
	v = abs32(v)
	deg := math.Floor(v)
	rem := (v - deg) * 60
	min := math.Floor(rem)
	sec := (rem - min) * 60
	return []float64{deg, min, sec}
}

func ref(pos, neg rune, v float64) string {
	if v < 0 {
		return string(neg)
	}
	return string(pos)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---- hostile EXIF -------------------------------------------------------

type hostileEXIF func() []byte

// overflowEXIF declares an APP1 segment far larger than the file that follows.
func overflowEXIF() []byte {
	var seg bytes.Buffer
	seg.Write([]byte{0xFF, 0xE1})
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], 0xFFF0) // claims ~64 KiB
	seg.Write(l[:])
	seg.Write([]byte("Exif\x00\x00II\x2a\x00"))
	seg.Write(make([]byte, 64)) // far less than claimed
	return seg.Bytes()
}

// manyEntriesEXIF declares 65535 IFD entries inside a tiny segment.
func manyEntriesEXIF() []byte {
	var seg bytes.Buffer
	seg.Write([]byte{0xFF, 0xE1})
	body := []byte("Exif\x00\x00II\x2a\x00")
	body = append(body, le32(8)...)
	body = append(body, []byte{0xFF, 0xFF}...) // 65535 entries
	body = append(body, make([]byte, 32)...)
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(body)+2))
	seg.Write(l[:])
	seg.Write(body)
	return seg.Bytes()
}

// loopEXIF points IFD0's next-IFD field back at IFD0.
func loopEXIF() []byte {
	var seg bytes.Buffer
	seg.Write([]byte{0xFF, 0xE1})
	body := []byte("Exif\x00\x00II\x2a\x00")
	body = append(body, le32(8)...)
	body = append(body, []byte{0, 0}...) // zero entries
	body = append(body, le32(8)...)      // next IFD == IFD0: a loop
	body = append(body, make([]byte, 16)...)
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(body)+2))
	seg.Write(l[:])
	seg.Write(body)
	return seg.Bytes()
}

// jpegWithHostileEXIF splices a malformed APP1 into an otherwise valid JPEG.
func jpegWithHostileEXIF(build hostileEXIF) ([]byte, error) {
	base, err := jpegBytes(64, 48, 70)
	if err != nil {
		return nil, err
	}
	app1 := build()
	out := make([]byte, 0, len(base)+len(app1))
	out = append(out, base[:2]...)
	out = append(out, app1...)
	out = append(out, base[2:]...)
	return out, nil
}

// ---- ISOBMFF ------------------------------------------------------------

// mp4WithMVHD writes the smallest ISOBMFF file the probe can read duration and
// creation time from: ftyp, then moov containing mvhd.
func mp4WithMVHD(dur time.Duration, created time.Time) []byte {
	const mp4Epoch = 2082844800 // seconds between 1904-01-01 and 1970-01-01

	body := make([]byte, 0, 16+32)
	body = append(body, box("ftyp", func(b *bytes.Buffer) {
		b.WriteString("isom")
		b.Write(le32(0x200))
		b.WriteString("isom")
		b.WriteString("mp41")
	})...)

	body = append(body, box("moov", func(b *bytes.Buffer) {
		b.Write(box("mvhd", func(m *bytes.Buffer) {
			// version 0 and the three flag bytes. Omitting this dword shifts
			// every subsequent field by four bytes, which a conforming reader
			// then reads as garbage rather than as an error.
			m.Write([]byte{0, 0, 0, 0})
			createdSecs := uint32(created.Unix() + mp4Epoch)
			m.Write(be32(createdSecs))
			m.Write(be32(createdSecs))
			m.Write(be32(1000))                       // timescale
			m.Write(be32(uint32(dur.Milliseconds()))) // duration in timescale units
			m.Write(be32(0x00010000))                 // rate
			m.Write(be16(0x0100))                     // volume
			m.Write(make([]byte, 10))                 // reserved
			m.Write(make([]byte, 36))                 // matrix
			m.Write(make([]byte, 24))                 // predefined
			m.Write(be32(2))                          // next track id
		}))
	})...)
	return body
}

func box(typ string, write func(*bytes.Buffer)) []byte {
	var inner bytes.Buffer
	write(&inner)
	var out bytes.Buffer
	out.Write(be32(uint32(inner.Len() + 8)))
	out.WriteString(typ)
	out.Write(inner.Bytes())
	return out.Bytes()
}

func be32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func be16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

// Describe renders the library for a test log.
func (l *Library) Describe() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "synthetic library at %s (%d files)\n", l.Root, len(l.Files))
	for _, f := range l.Files {
		fmt.Fprintf(&b, "  %-52s %7d B  %s\n", f.Rel, f.Bytes, f.Note)
	}
	return b.String()
}
