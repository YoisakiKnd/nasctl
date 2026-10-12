// Package probe answers "what is this file and can we afford to touch it?"
// without decoding anything.
//
// Everything here reads at most a bounded prefix of the file. Dimensions come
// from headers and markers, never from a decode, so probing a 60 MP panorama
// costs the same as probing a thumbnail. The result is a pre-filter: it decides
// which files are worth handing to a decoder, and the process-level memory
// ceiling is what actually protects the machine.
package probe

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/exif"
	"github.com/YoisakiKnd/nasctl/internal/mimetype"
)

// Kind is the coarse classification stored in the index.
type Kind string

const (
	KindImage       Kind = "image"
	KindVideo       Kind = "video"
	KindOther       Kind = "other"
	KindUnsupported Kind = "unsupported"
)

// Limits bounds probing.
type Limits struct {
	ScanBytes       int
	MaxFileBytes    int64
	MaxPixels       int64
	MaxDecodeBytes  int64
	MaxFrames       int
	ProbeMaxMetaLen int
	ThumbMaxEdge    int
	Exif            exif.Limits
}

// Budget is the decode budget the probe is checked against.
type Budget struct {
	MaxPixels      int64
	MaxFileBytes   int64
	MaxDecodeBytes int64
	MaxFrames      int
	ThumbMaxEdge   int
}

// Result is what the probe learned.
type Result struct {
	Kind     Kind
	Format   string
	MIME     string
	Width    int
	Height   int
	Pixels   int64
	Animated bool
	Frames   int

	// Container metadata for video files.
	DurationMS     int64
	ContainerBrand string
	ContainerTime  string // ISO-ish wall clock from the container, when present
	ContainerTZUTC bool   // true when the container timestamp is defined as UTC

	Tags *exif.Tags

	// EstimatedDecodeBytes is the pre-filter estimate. It is not a promise.
	EstimatedDecodeBytes int64

	// UnsupportedReason is set when the file is recognised but this build will
	// not decode it. The index keeps the row so the UI can show a placeholder
	// with the original file's real facts.
	UnsupportedReason string

	// SkippedReason is set when the file is within a supported format but
	// exceeds a budget. It is never handed to a decoder.
	SkippedReason string

	Notes []string
}

// Thumbnailable reports whether a thumbnail could be produced for this file.
func (r *Result) Thumbnailable() bool {
	return r.UnsupportedReason == "" && r.SkippedReason == "" &&
		(r.Kind == KindImage || r.Kind == KindVideo)
}

// MIMEForExt is the serving allowlist, kept here as a thin alias so existing
// callers keep working. The implementation lives in internal/mimetype so that
// the web service can use it without importing this package's parsers.
func MIMEForExt(ext string) string { return mimetype.ForExt(ext) }

// ImageExts are extensions this program may decode.
func ImageExts() []string {
	return []string{"jpg", "jpeg", "png", "gif", "webp"}
}

// VideoExts are extensions indexed as video. They are streamed and may be
// played by the browser; albumctl never re-encodes them.
func VideoExts() []string {
	return []string{"mp4", "m4v", "mov", "webm", "mkv", "avi", "3gp", "mts", "m2ts", "ts", "mpg", "mpeg", "wmv", "flv"}
}

// OtherMediaExts are indexed as unsupported: recognised as media, shown as a
// placeholder tile with the original file's real attributes, never decoded.
func OtherMediaExts() []string {
	return []string{
		"heic", "heif", "avif", "bmp", "tif", "tiff",
		"dng", "cr2", "cr3", "nef", "arw", "raf", "orf", "rw2", "srw", "pef", "x3f",
	}
}

// ExtensionIndex lists every extension the scanner will look at.
func ExtensionIndex() map[string]Kind {
	out := map[string]Kind{}
	for _, e := range ImageExts() {
		out[e] = KindImage
	}
	for _, e := range VideoExts() {
		out[e] = KindVideo
	}
	for _, e := range OtherMediaExts() {
		out[e] = KindUnsupported
	}
	return out
}

// NormalizeExt lower-cases an extension without its dot.
func NormalizeExt(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 || i == len(name)-1 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

// ReadPrefix reads at most max bytes from the start of a file.
func ReadPrefix(path string, max int) ([]byte, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if st.IsDir() {
		return nil, st, fmt.Errorf("probe: %s is a directory", path)
	}
	if max <= 0 {
		max = 1 << 20
	}
	buf := make([]byte, max)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, st, err
	}
	return buf[:n], st, nil
}

// Probe inspects a file and decides whether it fits the budget.
func Probe(path string, ext string, budget Budget, lim Limits) (*Result, error) {
	if lim.ScanBytes <= 0 {
		lim.ScanBytes = 1 << 20
	}
	if lim.Exif == (exif.Limits{}) {
		lim.Exif = exif.DefaultLimits()
	}
	data, st, err := ReadPrefix(path, lim.ScanBytes)
	if err != nil {
		return nil, err
	}
	return ProbeBytes(data, st.Size(), ext, budget, lim)
}

// ProbeBytes is Probe for an in-memory prefix, used by tests.
func ProbeBytes(data []byte, fileSize int64, ext string, budget Budget, lim Limits) (*Result, error) {
	if lim.Exif == (exif.Limits{}) {
		lim.Exif = exif.DefaultLimits()
	}
	// The extension policy is decided first and wins over the content sniff.
	//
	// This ordering is a safety property, not a preference. Container formats
	// are ambiguous: a Canon RAW file, a TIFF, and a BMP all begin with bytes
	// that a sniffing classifier can legitimately call an image, and a RAW
	// whose IFD declares dimensions would then look thumbnailable. If content
	// sniffing were allowed to overrule the extension, a file the format
	// matrix says is never decoded could be handed to a decoder.
	ext = mimetype.Normalize(ext)
	extKind, extKnown := ExtensionIndex()[ext]

	r := &Result{Format: "unknown", Kind: KindOther, MIME: MIMEForExt(ext)}
	format := exif.DetectFormat(data)
	r.Format = format
	r.MIME = MIMEForExt(ext)
	if r.MIME == "application/octet-stream" {
		r.MIME = mimeForFormat(format)
	}

	// With the policy known, the cheap facts are still collected below: the UI
	// shows an unsupported file's real dimensions and capture time next to a
	// placeholder, which the design asks for.
	if !extKnown {
		r.Kind = KindUnsupported
		r.UnsupportedReason = "extension_not_allowed:" + ext
	} else if extKind == KindUnsupported {
		r.Kind = KindUnsupported
		r.UnsupportedReason = "format_not_enabled:" + ext
	}

	switch format {
	case "jpeg":
		r.Kind = kindFor(r, extKind, extKnown, KindImage)
		info, notes := jpegHeader(data)
		r.Width, r.Height = info.Width, info.Height
		r.Notes = append(r.Notes, notes...)
		if tags, terr := exif.ParseJPEG(data, lim.Exif); terr != nil {
			r.Notes = append(r.Notes, exifNote(terr))
		} else {
			r.Tags = tags
			r.Notes = append(r.Notes, tags.Warnings...)
		}
	case "png":
		r.Kind = kindFor(r, extKind, extKnown, KindImage)
		png, notes := pngHeader(data)
		r.Width, r.Height = png.Width, png.Height
		r.Animated, r.Frames = png.Animated, png.Frames
		r.Notes = append(r.Notes, notes...)
		if tags, terr := exif.ParsePNG(data, lim.Exif); terr != nil {
			r.Notes = append(r.Notes, exifNote(terr))
			if tags != nil {
				r.Tags = tags
			}
		} else {
			r.Tags = tags
			r.Notes = append(r.Notes, tags.Warnings...)
			if r.Width == 0 && tags.PixelXDimension > 0 {
				r.Width, r.Height = tags.PixelXDimension, tags.PixelYDimension
			}
		}
	case "gif":
		r.Kind = kindFor(r, extKind, extKnown, KindImage)
		g, notes := gifHeader(data)
		r.Width, r.Height = g.Width, g.Height
		r.Animated, r.Frames = g.Animated, g.Frames
		r.Notes = append(r.Notes, notes...)
	case "webp":
		r.Kind = kindFor(r, extKind, extKnown, KindImage)
		w, notes := webpHeader(data)
		r.Width, r.Height = w.Width, w.Height
		r.Animated = w.Animated
		r.Frames = w.Frames
		r.Notes = append(r.Notes, notes...)
		// WebP may carry an EXIF chunk; it is rare enough that we only report
		// its absence rather than parsing RIFF chunk lists twice.
	case "tiff":
		// A bare TIFF reaches the decoder nowhere in this build, and a
		// TIFF-based RAW must not become thumbnailable merely because its IFD
		// reports dimensions.
		r.Kind = kindFor(r, extKind, extKnown, KindImage)
		if tags, terr := exif.ParseTIFF(data, lim.Exif); terr != nil {
			r.Notes = append(r.Notes, exifNote(terr))
		} else {
			r.Tags = tags
			r.Width, r.Height = tags.PixelXDimension, tags.PixelYDimension
		}
	case "isobmff":
		brand, durMS, ctime, notes := parseISOBMFF(data, fileSize)
		r.ContainerBrand = brand
		r.DurationMS = durMS
		r.ContainerTime = ctime
		r.Notes = append(r.Notes, notes...)
		switch brandClass(brand) {
		case "image":
			r.Kind = KindUnsupported
			r.UnsupportedReason = "format_not_enabled:isobmff-image:" + brand
		case "video":
			r.Kind = kindFor(r, extKind, extKnown, KindVideo)
		default:
			r.Kind = kindFor(r, extKind, extKnown, KindVideo)
		}
		// ISO base media creation_time is defined by the specification as
		// seconds since 1904-01-01T00:00:00Z, so the value needs no local-time
		// interpretation. Saying so explicitly is what stops a caller from
		// treating it as a naive wall clock and shifting it by the host's zone.
		if r.ContainerTime != "" {
			r.ContainerTZUTC = true
		}
	default:
		if r.UnsupportedReason == "" {
			r.Kind = KindUnsupported
			r.UnsupportedReason = "unrecognised_format"
		}
	}

	// A name and its contents disagreeing is worth recording: it is the shape
	// of a deliberate attempt to have one type decoded as another.
	if r.UnsupportedReason == "" && extKnown && format != "unknown" && !extMatchesFormat(ext, format) {
		r.Notes = append(r.Notes, fmt.Sprintf(
			"extension_mismatch: the name says %s but the content looks like %s", ext, format))
	}

	if r.Width > 0 && r.Height > 0 {
		// Width*height as int64: a 32-bit build must not wrap on a crafted
		// 100000x100000 header.
		r.Pixels = int64(r.Width) * int64(r.Height)
	}
	if r.Tags != nil && r.Pixels == 0 && r.Tags.PixelXDimension > 0 && r.Tags.PixelYDimension > 0 {
		r.Pixels = int64(r.Tags.PixelXDimension) * int64(r.Tags.PixelYDimension)
	}
	if r.Pixels > 0 {
		r.EstimatedDecodeBytes = EstimateDecodeBytes(r.Kind, r.Format, r.Pixels, r.Width, r.Height,
			budget.ThumbMaxEdge)
	}

	if r.UnsupportedReason == "" {
		applyBudget(r, fileSize, budget)
	}
	return r, nil
}

// kindFor decides the kind for a content-detected image or video.
//
// When the extension is one this build knows, the extension's own kind is used,
// so the index row written by the scanner and the probe result cannot disagree.
// When the extension is unknown the policy has already marked the file
// unsupported and the detected kind is irrelevant.
func kindFor(r *Result, extKind Kind, extKnown bool, detected Kind) Kind {
	if r.UnsupportedReason != "" {
		return KindUnsupported
	}
	if extKnown {
		return extKind
	}
	return detected
}

// extMatchesFormat reports whether a name's extension is a plausible name for
// the format the bytes actually are.
func extMatchesFormat(ext, format string) bool {
	switch format {
	case "jpeg":
		return ext == "jpg" || ext == "jpeg" || ext == "jpe"
	case "isobmff":
		switch ext {
		case "mp4", "m4v", "mov", "3gp":
			return true
		}
		return false
	case "tiff":
		return ext == "tif" || ext == "tiff"
	default:
		return ext == format
	}
}

// exifNote turns an EXIF error into a note, without repeating a prefix the
// error already carries.
func exifNote(err error) string {
	msg := err.Error()
	if strings.HasPrefix(msg, "exif:") {
		return msg
	}
	return "exif: " + msg
}

func mimeForFormat(format string) string {
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "tiff":
		return "image/tiff"
	case "isobmff":
		return "video/mp4"
	}
	return "application/octet-stream"
}

// applyBudget marks the file as skipped when any hard budget is exceeded.
//
// The point of this function is to make "try it and see if it fits" impossible:
// an over-budget file never reaches a decoder, so there is no window in which
// the machine can be hurt.
func applyBudget(r *Result, fileSize int64, b Budget) {
	if b.MaxFileBytes > 0 && fileSize > b.MaxFileBytes {
		r.SkippedReason = fmt.Sprintf("file_too_large:%d>%d", fileSize, b.MaxFileBytes)
		return
	}
	if r.Kind == KindVideo {
		// No decoder is involved for a video: we only stream it. The poster
		// generator has its own budget. Videos are never marked as skipped for
		// pixel reasons.
		return
	}
	if r.Animated && b.MaxFrames > 0 && r.Frames > b.MaxFrames {
		r.SkippedReason = fmt.Sprintf("animated_over_frame_budget:%d>%d", r.Frames, b.MaxFrames)
		return
	}
	if b.MaxPixels > 0 && r.Pixels > b.MaxPixels {
		r.SkippedReason = fmt.Sprintf("over_pixel_budget:%d>%d", r.Pixels, b.MaxPixels)
		return
	}
	if r.Pixels == 0 && r.Width == 0 {
		// We could not establish the dimensions, so we cannot bound a decode.
		r.SkippedReason = "dimensions_unknown"
		return
	}
	if b.MaxDecodeBytes > 0 && r.EstimatedDecodeBytes > b.MaxDecodeBytes {
		r.SkippedReason = fmt.Sprintf("over_decode_budget:%d>%d", r.EstimatedDecodeBytes, b.MaxDecodeBytes)
		return
	}
}

// EstimateDecodeBytes is the conservative pre-filter for image decoding.
//
// It is deliberately pessimistic and its terms are documented so the number can
// be audited:
//
//	pixels * 1.5   decoded luma/chroma planes (image.YCbCr for JPEG)
//	pixels * 4     worst case where the decoder materialises RGBA
//	edge^2 * 4     the destination thumbnail
//	8 MiB          Go runtime and decoder scratch, observed not guessed
//
// The total is *not* a guarantee: it is compared against the worker's hard
// cgroup ceiling, and the cgroup is what enforces the outcome.
func EstimateDecodeBytes(kind Kind, format string, pixels int64, w, h, thumbEdge int) int64 {
	if kind != KindImage {
		return 0
	}
	if thumbEdge <= 0 {
		thumbEdge = 512
	}
	const overhead = 8 << 20
	perPixel := 5.5
	switch format {
	case "png":
		// PNG decoders hold both the filtered scanline buffer and the decoded
		// image, and paletted sources are expanded.
		perPixel = 6.5
	case "gif":
		perPixel = 6.0
	case "webp":
		perPixel = 5.5
	}
	est := int64(float64(pixels) * perPixel)
	est += int64(thumbEdge) * int64(thumbEdge) * 4
	est += overhead
	return est
}

// ---- per-format header readers ------------------------------------------

// Dims is a width/height pair.
type Dims struct {
	Width, Height int
	Animated      bool
	Frames        int
	Progressive   bool
}

// jpegHeader walks markers to the first SOF and reads the frame size.
//
// No pixels are touched. A truncated marker sequence stops the walk and is
// reported rather than scanned for.
func jpegHeader(data []byte) (Dims, []string) {
	var d Dims
	var notes []string
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return d, []string{"not a JPEG"}
	}
	i := 2
	for i+3 < len(data) {
		if data[i] != 0xFF {
			notes = append(notes, fmt.Sprintf("marker desync at byte %d", i))
			return d, notes
		}
		marker := data[i+1]
		if marker == 0xFF {
			i++
			continue
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD9) {
			i += 2
			continue
		}
		if i+4 > len(data) {
			break
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if segLen < 2 || i+2+segLen > len(data) {
			notes = append(notes, fmt.Sprintf("segment at %d truncated", i))
			return d, notes
		}
		// SOF0..SOF15 except DHT(0xC4), JPG(0xC8) and DAC(0xCC).
		if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
			if segLen < 7 {
				notes = append(notes, "SOF segment too short")
				return d, notes
			}
			h := int(binary.BigEndian.Uint16(data[i+5 : i+7]))
			w := int(binary.BigEndian.Uint16(data[i+7 : i+9]))
			d.Width, d.Height = w, h
			d.Progressive = marker == 0xC2 || marker == 0xC6 || marker == 0xCA || marker == 0xCE
			return d, notes
		}
		if marker == 0xDA {
			notes = append(notes, "reached scan data without a frame header")
			return d, notes
		}
		i += 2 + segLen
	}
	notes = append(notes, "no frame header found in the scanned window")
	return d, notes
}

func pngHeader(data []byte) (Dims, []string) {
	var d Dims
	var notes []string
	if len(data) < 8 || string(data[0:8]) != "\x89PNG\r\n\x1a\n" {
		return d, []string{"not a PNG"}
	}
	i := 8
	chunks := 0
	for i+8 <= len(data) && chunks < 128 {
		length := int(binary.BigEndian.Uint32(data[i : i+4]))
		typ := string(data[i+4 : i+8])
		if length < 0 || i+8+length+4 > len(data) {
			notes = append(notes, fmt.Sprintf("chunk %q at %d truncated (declared %d bytes)", typ, i, length))
			return d, notes
		}
		body := data[i+8 : i+8+length]
		chunks++
		switch typ {
		case "IHDR":
			if len(body) >= 8 {
				d.Width = int(binary.BigEndian.Uint32(body[0:4]))
				d.Height = int(binary.BigEndian.Uint32(body[4:8]))
			}
		case "acTL":
			if len(body) >= 8 {
				d.Animated = true
				n := int(binary.BigEndian.Uint32(body[0:4]))
				if n < 0 || n > 1<<20 {
					notes = append(notes, fmt.Sprintf("APNG declares an implausible frame count %d", n))
					n = 1 << 20
				}
				d.Frames = n
			}
		case "IDAT", "IEND":
			return d, notes
		}
		i += 8 + length + 4
	}
	return d, notes
}

func gifHeader(data []byte) (Dims, []string) {
	var d Dims
	var notes []string
	if len(data) < 13 || (!strings.HasPrefix(string(data), "GIF87a") && !strings.HasPrefix(string(data), "GIF89a")) {
		return d, []string{"not a GIF"}
	}
	d.Width = int(binary.LittleEndian.Uint16(data[6:8]))
	d.Height = int(binary.LittleEndian.Uint16(data[8:10]))
	// Count image descriptors within the scanned window only; an animated GIF
	// is refused on the budget check anyway and does not need an exact count.
	frames := 0
	i := 13
	if len(data) > 13 {
		flags := data[10]
		if flags&0x80 != 0 {
			i += 3 * (1 << ((flags & 0x07) + 1))
		}
	}
	for i < len(data) {
		switch data[i] {
		case 0x2C:
			frames++
			if frames >= 2 {
				d.Animated = true
			}
			i++
			if i+8 > len(data) {
				notes = append(notes, "GIF image descriptor truncated")
				d.Frames = frames
				return d, notes
			}
			lflags := data[i+8]
			i += 9
			if lflags&0x80 != 0 {
				i += 3 * (1 << ((lflags & 0x07) + 1))
			}
			if i >= len(data) {
				d.Frames = frames
				return d, notes
			}
			i++ // LZW minimum code size
			i = skipGIFSubBlocks(data, i)
		case 0x21:
			i++
			if i >= len(data) {
				d.Frames = frames
				return d, notes
			}
			i++ // label
			i = skipGIFSubBlocks(data, i)
		case 0x3B:
			d.Frames = frames
			return d, notes
		default:
			notes = append(notes, fmt.Sprintf("unknown GIF block 0x%02x at %d", data[i], i))
			d.Frames = frames
			return d, notes
		}
	}
	d.Frames = frames
	return d, notes
}

func skipGIFSubBlocks(data []byte, i int) int {
	for i < len(data) {
		n := int(data[i])
		if n == 0 {
			return i + 1
		}
		i += 1 + n
	}
	return i
}

func webpHeader(data []byte) (Dims, []string) {
	var d Dims
	var notes []string
	if len(data) < 16 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return d, []string{"not a WebP"}
	}
	switch string(data[12:16]) {
	case "VP8 ":
		if len(data) < 30 {
			return d, []string{"VP8 lossy header truncated"}
		}
		if data[23] != 0x9D || data[24] != 0x01 || data[25] != 0x2A {
			return d, []string{"VP8 start code missing"}
		}
		d.Width = int(binary.LittleEndian.Uint16(data[26:28]) & 0x3FFF)
		d.Height = int(binary.LittleEndian.Uint16(data[28:30]) & 0x3FFF)
	case "VP8L":
		if len(data) < 25 || data[20] != 0x2F {
			return d, []string{"VP8L header truncated or unsigned"}
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		d.Width = int(bits&0x3FFF) + 1
		d.Height = int((bits>>14)&0x3FFF) + 1
	case "VP8X":
		if len(data) < 30 {
			return d, []string{"VP8X header truncated"}
		}
		flags := data[20]
		d.Animated = flags&0x02 != 0
		d.Width = int(readUint24LE(data[24:27])) + 1
		d.Height = int(readUint24LE(data[27:30])) + 1
	default:
		notes = append(notes, fmt.Sprintf("unknown WebP chunk %q", string(data[12:16])))
	}
	return d, notes
}

func readUint24LE(b []byte) uint32 {
	if len(b) < 3 {
		return 0
	}
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
}

// ---- ISO base media file format (MP4 / MOV / HEIC) ----------------------

// brandClass classifies an ftyp brand.
func brandClass(brand string) string {
	switch brand {
	case "heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs", "mif1", "msf1", "avif", "avis":
		return "image"
	case "isom", "iso2", "iso4", "iso5", "iso6", "mp41", "mp42", "avc1", "M4V ", "M4A ", "qt  ",
		"3gp4", "3gp5", "3g2a", "dash", "mmp4", "msdh", "msix":
		return "video"
	}
	return "unknown"
}

// mp4EpochOffset converts the 1904-01-01 epoch used by ISO base media files to
// the Unix epoch.
const mp4EpochOffset = 2082844800

// parseISOBMFF walks top-level atoms to find ftyp and moov/mvhd.
//
// Atom sizes come from the file, so every advance is bounds-checked against the
// scanned window and a bogus size stops the walk with a note. Walking stops at
// the end of the window; moov normally precedes mdat for fast-start files, and
// for the rest we simply report that the duration is unknown rather than
// seeking into a multi-gigabyte file.
func parseISOBMFF(data []byte, fileSize int64) (brand string, durationMS int64, creation string, notes []string) {
	i := 0
	atoms := 0
	for i+8 <= len(data) && atoms < 64 {
		size := int64(binary.BigEndian.Uint32(data[i : i+4]))
		typ := string(data[i+4 : i+8])
		headerLen := int64(8)
		switch size {
		case 1:
			if i+16 > len(data) {
				notes = append(notes, "64-bit atom header truncated")
				return brand, 0, "", notes
			}
			size = int64(binary.BigEndian.Uint64(data[i+8 : i+16]))
			headerLen = 16
		case 0:
			size = fileSize - int64(i)
		}
		if size < headerLen {
			notes = append(notes, fmt.Sprintf("atom %q at %d declares an impossible size %d", typ, i, size))
			return brand, 0, "", notes
		}
		body := data[i+int(headerLen):]
		end := i + int(size)
		if end > len(data) {
			body = data[i+int(headerLen):]
		}
		switch typ {
		case "ftyp":
			if len(body) >= 4 {
				brand = string(body[0:4])
			}
		case "moov":
			if d, c, n := parseMoov(body); d > 0 || c != "" {
				notes = append(notes, n...)
				return brand, d, c, notes
			} else {
				notes = append(notes, n...)
			}
		}
		atoms++
		if end > len(data) {
			if typ != "mdat" && int64(end) < fileSize {
				notes = append(notes, fmt.Sprintf("atom %q extends past the %d byte scan window", typ, len(data)))
			}
			return brand, durationMS, creation, notes
		}
		i = end
	}
	return brand, durationMS, creation, notes
}

// parseMoov descends into a moov atom looking for mvhd.
func parseMoov(body []byte) (durationMS int64, creation string, notes []string) {
	i := 0
	for i+8 <= len(body) && i < 64 {
		size := int64(binary.BigEndian.Uint32(body[i : i+4]))
		typ := string(body[i+4 : i+8])
		headerLen := int64(8)
		if size == 1 {
			if i+16 > len(body) {
				return 0, "", []string{"moov child header truncated"}
			}
			size = int64(binary.BigEndian.Uint64(body[i+8 : i+16]))
			headerLen = 16
		}
		if size < headerLen || i+int(size) > len(body) {
			return 0, "", []string{fmt.Sprintf("moov child %q at %d is truncated", typ, i)}
		}
		if typ == "mvhd" {
			d, c, n := parseMvhd(body[i+int(headerLen) : i+int(size)])
			return d, c, n
		}
		i += int(size)
	}
	return 0, "", nil
}

func parseMvhd(b []byte) (durationMS int64, creation string, notes []string) {
	if len(b) < 20 {
		return 0, "", []string{"mvhd too short"}
	}
	version := b[0]
	var ctime uint64
	var timescale, dur uint64
	if version == 1 {
		if len(b) < 32 {
			return 0, "", []string{"mvhd version 1 truncated"}
		}
		ctime = binary.BigEndian.Uint64(b[4:12])
		timescale = uint64(binary.BigEndian.Uint32(b[20:24]))
		dur = binary.BigEndian.Uint64(b[24:32])
	} else {
		ctime = uint64(binary.BigEndian.Uint32(b[4:8]))
		timescale = uint64(binary.BigEndian.Uint32(b[12:16]))
		dur = uint64(binary.BigEndian.Uint32(b[16:20]))
	}
	if timescale > 0 && dur > 0 && dur != 0xFFFFFFFF {
		ms := dur * 1000 / timescale
		if ms > 0 && ms < 1000*3600*24*30 {
			durationMS = int64(ms)
		} else {
			notes = append(notes, fmt.Sprintf("mvhd duration %d/%d is implausible", dur, timescale))
		}
	}
	if ctime > mp4EpochOffset && ctime < mp4EpochOffset+(200*365*24*3600) {
		unixSecs := int64(ctime) - mp4EpochOffset
		t := time.Unix(unixSecs, 0).UTC()
		if t.Year() >= 1900 && t.Year() <= 2199 {
			creation = t.Format("2006-01-02T15:04:05")
		}
	} else if ctime != 0 {
		notes = append(notes, fmt.Sprintf("mvhd creation_time %d is outside the plausible range", ctime))
	}
	return durationMS, creation, notes
}
