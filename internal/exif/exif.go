// Package exif reads capture metadata out of JPEG, PNG and bare TIFF files.
//
// It is written from scratch rather than delegated to a general EXIF library
// because its most important property is negative: it must never trust a length
// or an offset found in the file. Every read is bounds-checked against a single
// buffer that was read with an explicit cap, every count is clamped, every IFD
// chain is cycle-checked, and a malformed file produces warnings rather than a
// panic or a large allocation.
//
// The buffer is at most Limits.MaxScanBytes and the returned strings are capped
// at Limits.MaxStringBytes, so a 200 MB file full of a fabricated 4 GB string
// costs the same as a clean photo.
package exif

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// Limits bounds the parser.
type Limits struct {
	MaxScanBytes   int // bytes of the file handed to the parser
	MaxMetaBytes   int // cap on a single metadata block
	MaxIFDEntries  int // total entries walked across all IFDs
	MaxIFDs        int // IFD chain and sub-IFD budget
	MaxStringBytes int // cap on one returned string
	MaxKeys        int // cap on returned displayable keys
}

// DefaultLimits matches the shipped configuration defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxScanBytes:   1 << 20,
		MaxMetaBytes:   256 << 10,
		MaxIFDEntries:  512,
		MaxIFDs:        8,
		MaxStringBytes: 512,
		MaxKeys:        24,
	}
}

// Sanitize normalizes limits so a zero value is still safe.
func (l Limits) Sanitize() Limits {
	d := DefaultLimits()
	if l.MaxScanBytes <= 0 || l.MaxScanBytes > 64<<20 {
		l.MaxScanBytes = d.MaxScanBytes
	}
	if l.MaxMetaBytes <= 0 || l.MaxMetaBytes > 16<<20 {
		l.MaxMetaBytes = d.MaxMetaBytes
	}
	if l.MaxIFDEntries <= 0 || l.MaxIFDEntries > 65536 {
		l.MaxIFDEntries = d.MaxIFDEntries
	}
	if l.MaxIFDs <= 0 || l.MaxIFDs > 64 {
		l.MaxIFDs = d.MaxIFDs
	}
	if l.MaxStringBytes <= 0 || l.MaxStringBytes > 64<<10 {
		l.MaxStringBytes = d.MaxStringBytes
	}
	if l.MaxKeys <= 0 || l.MaxKeys > 256 {
		l.MaxKeys = d.MaxKeys
	}
	return l
}

// KV is one displayable metadata pair.
type KV struct{ Key, Value string }

// GPS holds coordinates that were actually present in the file. Nothing here is
// ever inferred.
type GPS struct {
	Lat       float64
	Lon       float64
	AltitudeM *float64
}

// Tags is the parsed subset. Zero values mean "not present", which is also what
// a caller must render as "not present" — no defaults are substituted.
type Tags struct {
	Make      string
	Model     string
	Software  string
	LensModel string

	Orientation int // 1..8, 0 when absent or invalid
	ColorSpace  int

	DateTime            string
	DateTimeOriginal    string
	DateTimeDigitized   string
	OffsetTime          string
	OffsetTimeOriginal  string
	OffsetTimeDigitized string
	SubSecTimeOriginal  string

	PixelXDimension int
	PixelYDimension int

	ISO              int
	Flash            int
	ExposureTime     string
	FNumber          string
	FocalLength      string
	FocalLength35mm  int
	ExposureBias     string
	MeteringMode     int
	ExposureProgram  int
	WhiteBalance     int
	SceneCaptureType int
	DigitalZoomRatio string
	SubjectDistance  string
	ExifVersion      string
	BodySerialNumber string
	LensSerialNumber string
	Artist           string
	ImageDescription string
	Copyright        string
	XResolution      string
	YResolution      string
	ResolutionUnit   int

	GPS *GPS

	// Extra is the bounded, ordered list the UI displays.
	Extra []KV

	// Warnings records anything malformed that was tolerated. They are surfaced
	// in the task record so a directory full of damaged files is visible
	// instead of silently producing empty metadata.
	Warnings []string

	// Truncated reports that parsing stopped early because a limit was hit.
	Truncated bool
}

func (t *Tags) warn(format string, args ...any) {
	if len(t.Warnings) >= 32 {
		t.Warnings = append(t.Warnings, "warning list truncated")
		return
	}
	t.Warnings = append(t.Warnings, fmt.Sprintf(format, args...))
}

// Capture returns the best available capture timestamp as a wall-clock string
// plus, when the file carried one, a UTC offset in minutes.
//
// It never invents a time. A file with no usable date returns ok=false and the
// caller falls back to the file's own modification time, recording that the
// value is a fallback.
func (t *Tags) Capture() (wall string, offsetMin *int, ok bool) {
	raw := ""
	offRaw := ""
	switch {
	case validExifDate(t.DateTimeOriginal):
		raw, offRaw = t.DateTimeOriginal, t.OffsetTimeOriginal
	case validExifDate(t.DateTimeDigitized):
		raw, offRaw = t.DateTimeDigitized, t.OffsetTimeDigitized
	case validExifDate(t.DateTime):
		raw, offRaw = t.DateTime, t.OffsetTime
	default:
		return "", nil, false
	}
	wall = normalizeExifDate(raw)
	if t.SubSecTimeOriginal != "" && wall == normalizeExifDate(t.DateTimeOriginal) {
		if frac := digitsOnly(t.SubSecTimeOriginal, 6); frac != "" {
			wall += "." + frac
		}
	}
	if m, ok := parseOffset(offRaw); ok {
		offsetMin = &m
	}
	return wall, offsetMin, true
}

func digitsOnly(s string, max int) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < max; i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// normalizeExifDate turns "2023:05:01 12:33:04" into "2023-05-01T12:33:04".
func normalizeExifDate(s string) string {
	s = strings.TrimSpace(strings.TrimRight(s, "\x00 "))
	if len(s) < 19 {
		return ""
	}
	b := []byte(s[:19])
	if b[4] == ':' {
		b[4] = '-'
	}
	if b[7] == ':' {
		b[7] = '-'
	}
	if b[10] == ' ' {
		b[10] = 'T'
	}
	return string(b)
}

// validExifDate accepts only a well-formed, plausible timestamp.
//
// Cameras fill unset fields with "0000:00:00 00:00:00"; treating that as a real
// capture time would put every unknown file at the start of the timeline, which
// is exactly the kind of invention the design forbids.
func validExifDate(s string) bool {
	n := normalizeExifDate(s)
	if n == "" {
		return false
	}
	t, err := time.Parse("2006-01-02T15:04:05", n)
	if err != nil {
		return false
	}
	return t.Year() >= 1900 && t.Year() <= 2199
}

// parseOffset reads "+08:00" / "-0500" / "Z".
func parseOffset(s string) (int, bool) {
	s = strings.TrimSpace(strings.TrimRight(s, "\x00 "))
	if s == "" {
		return 0, false
	}
	if s == "Z" || s == "z" {
		return 0, true
	}
	sign := 1
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		sign = -1
		s = s[1:]
	default:
		return 0, false
	}
	s = strings.ReplaceAll(s, ":", "")
	if len(s) < 4 {
		return 0, false
	}
	hh, err1 := atoiN(s[:2])
	mm, err2 := atoiN(s[2:4])
	if err1 != nil || err2 != nil || hh > 23 || mm > 59 {
		return 0, false
	}
	return sign * (hh*60 + mm), true
}

func atoiN(s string) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a digit")
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

// ---- container handling -------------------------------------------------

// DetectFormat identifies the container from a prefix of the file.
func DetectFormat(head []byte) string {
	switch {
	case len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return "jpeg"
	case len(head) >= 8 && string(head[0:8]) == "\x89PNG\r\n\x1a\n":
		return "png"
	case len(head) >= 4 && (string(head[0:4]) == "II*\x00" || string(head[0:4]) == "MM\x00*"):
		return "tiff"
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		return "isobmff"
	case len(head) >= 6 && (string(head[0:6]) == "GIF87a" || string(head[0:6]) == "GIF89a"):
		return "gif"
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return "webp"
	case len(head) >= 2 && head[0] == 'B' && head[1] == 'M':
		return "bmp"
	}
	return "unknown"
}

// ParseJPEG walks JPEG markers up to the first scan, looking for an APP1 Exif
// segment. Marker walking itself is bounded by len(data).
func ParseJPEG(data []byte, lim Limits) (*Tags, error) {
	lim = lim.Sanitize()
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, fmt.Errorf("exif: not a JPEG")
	}
	i := 2
	for i+3 < len(data) {
		if data[i] != 0xFF {
			// Not synchronised; give up rather than scan the whole file.
			return nil, fmt.Errorf("exif: lost marker synchronisation at byte %d", i)
		}
		marker := data[i+1]
		switch {
		case marker == 0xFF:
			i++
			continue
		case marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			i += 2
			continue
		case marker == 0xDA: // start of scan: metadata must precede it
			return nil, fmt.Errorf("exif: no Exif segment before the image scan")
		case marker == 0xD9: // end of image
			return nil, fmt.Errorf("exif: no Exif segment")
		}
		if i+4 > len(data) {
			break
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if segLen < 2 {
			return nil, fmt.Errorf("exif: invalid segment length %d", segLen)
		}
		segStart := i + 4
		segEnd := segStart + segLen - 2
		if segEnd > len(data) {
			return nil, fmt.Errorf("exif: segment at %d extends past the scanned window", i)
		}
		if marker == 0xE1 && segLen >= 2+6 && string(data[segStart:segStart+6]) == "Exif\x00\x00" {
			return parseTIFFBlock(data[segStart+6:segEnd], lim)
		}
		i = segEnd
	}
	return nil, fmt.Errorf("exif: no Exif segment found in the scanned window")
}

// ParsePNG reads IHDR-adjacent metadata: an eXIf chunk if present, otherwise a
// tEXt "Creation Time" entry. Compressed text chunks are skipped on purpose:
// inflating attacker-controlled data to look for a date is not worth the risk.
func ParsePNG(data []byte, lim Limits) (*Tags, error) {
	lim = lim.Sanitize()
	if len(data) < 8 || string(data[0:8]) != "\x89PNG\r\n\x1a\n" {
		return nil, fmt.Errorf("exif: not a PNG")
	}
	t := &Tags{}
	i := 8
	chunks := 0
	for i+8 <= len(data) && chunks < 64 {
		length := int(binary.BigEndian.Uint32(data[i : i+4]))
		typ := string(data[i+4 : i+8])
		if length < 0 {
			return t, fmt.Errorf("exif: negative PNG chunk length")
		}
		bodyStart := i + 8
		bodyEnd := bodyStart + length
		if bodyEnd < bodyStart || bodyEnd > len(data) || bodyEnd+4 > len(data) {
			t.warn("PNG chunk %q at %d is truncated or absurdly long (%d bytes)", typ, i, length)
			t.Truncated = true
			return t, nil
		}
		body := data[bodyStart:bodyEnd]
		chunks++
		switch typ {
		case "IHDR":
			if len(body) >= 8 {
				w := int(binary.BigEndian.Uint32(body[0:4]))
				h := int(binary.BigEndian.Uint32(body[4:8]))
				if w > 0 && h > 0 {
					t.PixelXDimension, t.PixelYDimension = w, h
				}
			}
		case "eXIf":
			if len(body) > lim.MaxMetaBytes {
				t.warn("PNG eXIf chunk of %d bytes exceeds the %d byte cap and was skipped", len(body), lim.MaxMetaBytes)
				break
			}
			sub, err := parseTIFFBlock(body, lim)
			if err != nil {
				t.warn("PNG eXIf chunk rejected: %v", err)
				break
			}
			mergeTags(t, sub)
		case "tEXt":
			k, v, found := strings.Cut(string(body), "\x00")
			if found && strings.EqualFold(strings.TrimSpace(k), "Creation Time") && t.DateTime == "" {
				t.DateTime = sanitizeString(v, lim.MaxStringBytes)
			}
		case "IEND":
			i = len(data)
			continue
		}
		i = bodyEnd + 4
	}
	// PNG text/eXIf chunks in our subset never carry GPS, so nothing is
	// combined here; Extra is still built so the UI has a consistent shape.
	t.Extra = t.buildExtra(lim.MaxKeys)
	return t, nil
}

// ParseTIFF parses a bare TIFF file.
func ParseTIFF(data []byte, lim Limits) (*Tags, error) {
	return parseTIFFBlock(data, lim.Sanitize())
}

// Parse dispatches on the container signature.
func Parse(data []byte, lim Limits) (*Tags, error) {
	switch DetectFormat(data) {
	case "jpeg":
		return ParseJPEG(data, lim)
	case "png":
		return ParsePNG(data, lim)
	case "tiff":
		return ParseTIFF(data, lim)
	default:
		return nil, fmt.Errorf("exif: unsupported container")
	}
}

func mergeTags(dst, src *Tags) {
	if src == nil {
		return
	}
	if dst.Make == "" {
		dst.Make = src.Make
	}
	if dst.Model == "" {
		dst.Model = src.Model
	}
	if dst.Software == "" {
		dst.Software = src.Software
	}
	if dst.Orientation == 0 {
		dst.Orientation = src.Orientation
	}
	if dst.DateTime == "" {
		dst.DateTime = src.DateTime
	}
	if dst.DateTimeOriginal == "" {
		dst.DateTimeOriginal = src.DateTimeOriginal
	}
	if dst.PixelXDimension == 0 {
		dst.PixelXDimension, dst.PixelYDimension = src.PixelXDimension, src.PixelYDimension
	}
	if dst.GPS == nil {
		dst.GPS = src.GPS
	}
	dst.Warnings = append(dst.Warnings, src.Warnings...)
}

// ---- TIFF ---------------------------------------------------------------

type byteOrder int

const (
	little byteOrder = iota
	big
)

type entry struct {
	tag    uint16
	typ    uint16
	count  uint32
	offset uint32 // for out-of-line values, relative to the TIFF header
	inline [4]byte
}

type tiff struct {
	data    []byte // starts at the TIFF header
	order   byteOrder
	lim     Limits
	entries int
	ifds    int
	out     *Tags

	// GPS reference letters may arrive before or after the value, so the three
	// components are accumulated and only combined in finalize.
	gpsLatRef string
	gpsLonRef string
	gpsLat    *float64
	gpsLon    *float64
	gpsAlt    *float64
}

func parseTIFFBlock(data []byte, lim Limits) (*Tags, error) {
	if len(data) > lim.MaxMetaBytes {
		return nil, fmt.Errorf("exif: metadata block of %d bytes exceeds the %d byte cap",
			len(data), lim.MaxMetaBytes)
	}
	if len(data) < 8 {
		return nil, fmt.Errorf("exif: TIFF header truncated (%d bytes)", len(data))
	}
	t := &tiff{data: data, lim: lim, out: &Tags{}}
	switch {
	case data[0] == 'I' && data[1] == 'I':
		t.order = little
	case data[0] == 'M' && data[1] == 'M':
		t.order = big
	default:
		return nil, fmt.Errorf("exif: unknown byte order marker %02x%02x", data[0], data[1])
	}
	if t.u16(2) != 42 {
		return nil, fmt.Errorf("exif: bad TIFF magic %d", t.u16(2))
	}
	first := t.u32(4)
	if first == 0 {
		return nil, fmt.Errorf("exif: no IFD0 offset")
	}
	t.walk(first, ifdRoot, 0)
	t.out.Truncated = t.entries >= lim.MaxIFDEntries || t.ifds >= lim.MaxIFDs
	t.finalize()
	return t.out, nil
}

type ifdKind int

const (
	ifdRoot ifdKind = iota
	ifdExif
	ifdGPS
	ifdInterop
)

func (t *tiff) walk(offset uint32, kind ifdKind, depth int) {
	if depth > 4 {
		t.out.warn("IFD nesting deeper than 4 levels; stopped")
		return
	}
	seen := map[uint32]bool{}
	for offset != 0 {
		if seen[offset] {
			t.out.warn("IFD chain revisits offset %d; stopped to avoid a loop", offset)
			return
		}
		seen[offset] = true
		if t.ifds >= t.lim.MaxIFDs {
			t.out.warn("IFD budget of %d exhausted", t.lim.MaxIFDs)
			return
		}
		t.ifds++

		base := int(offset)
		if base < 0 || base+2 > len(t.data) {
			t.out.warn("IFD offset %d is outside the metadata block", offset)
			return
		}
		n := int(t.u16(uint32(base)))
		if n == 0 {
			return
		}
		if n > 4096 {
			t.out.warn("IFD at %d declares %d entries; clamped", offset, n)
			n = 4096
		}
		if base+2+n*12+4 > len(t.data) {
			t.out.warn("IFD at %d declares %d entries beyond the metadata block", offset, n)
			return
		}
		next := uint32(0)
		for i := 0; i < n; i++ {
			if t.entries >= t.lim.MaxIFDEntries {
				t.out.warn("entry budget of %d exhausted", t.lim.MaxIFDEntries)
				return
			}
			t.entries++
			ep := base + 2 + i*12
			e := entry{
				tag:    t.u16(uint32(ep)),
				typ:    t.u16(uint32(ep + 2)),
				count:  t.u32(uint32(ep + 4)),
				offset: t.u32(uint32(ep + 8)),
			}
			copy(e.inline[:], t.data[ep+8:ep+12])
			t.handle(e, kind, depth)
		}
		next = t.u32(uint32(base + 2 + n*12))
		offset = next
	}
}

func (t *tiff) u16(off uint32) uint16 {
	i := int(off)
	if i < 0 || i+2 > len(t.data) {
		return 0
	}
	if t.order == little {
		return binary.LittleEndian.Uint16(t.data[i : i+2])
	}
	return binary.BigEndian.Uint16(t.data[i : i+2])
}

func (t *tiff) u32(off uint32) uint32 {
	i := int(off)
	if i < 0 || i+4 > len(t.data) {
		return 0
	}
	if t.order == little {
		return binary.LittleEndian.Uint32(t.data[i : i+4])
	}
	return binary.BigEndian.Uint32(t.data[i : i+4])
}

// typeSize returns the size of one element of a TIFF field type, or 0 when the
// type is unknown.
func typeSize(typ uint16) int {
	switch typ {
	case 1, 2, 6, 7: // BYTE, ASCII, SBYTE, UNDEFINED
		return 1
	case 3, 8: // SHORT, SSHORT
		return 2
	case 4, 9, 11, 13: // LONG, SLONG, FLOAT, IFD
		return 4
	case 5, 10, 12: // RATIONAL, SRATIONAL, DOUBLE
		return 8
	}
	return 0
}

// valueBytes returns the raw bytes of an entry's value, or ok=false when the
// entry points outside the metadata block or exceeds the cap.
//
// The multiplication uses 64-bit arithmetic on purpose: on a 32-bit ARM build
// count*size can overflow int and a negative length would otherwise pass a
// naive bounds check.
func (t *tiff) valueBytes(e entry) ([]byte, bool) {
	sz := typeSize(e.typ)
	if sz == 0 {
		return nil, false
	}
	total := uint64(e.count) * uint64(sz)
	if total == 0 || total > uint64(t.lim.MaxMetaBytes) {
		return nil, false
	}
	if total <= 4 {
		return e.inline[:total], true
	}
	start := uint64(e.offset)
	end := start + total
	if end < start || end > uint64(len(t.data)) {
		return nil, false
	}
	return t.data[start:end], true
}

func (t *tiff) uints(e entry) []uint32 {
	b, ok := t.valueBytes(e)
	if !ok {
		return nil
	}
	n := len(b) / typeSize(e.typ)
	if n > 64 {
		n = 64
	}
	out := make([]uint32, 0, n)
	for i := 0; i < n; i++ {
		switch e.typ {
		case 1, 7:
			out = append(out, uint32(b[i]))
		case 3:
			out = append(out, uint32(t.orderUint16(b[i*2:])))
		case 4, 9, 13:
			out = append(out, t.orderUint32(b[i*4:]))
		case 6:
			out = append(out, uint32(int32(int8(b[i]))))
		case 8:
			out = append(out, uint32(int32(int16(t.orderUint16(b[i*2:])))))
		default:
			return out
		}
	}
	return out
}

func (t *tiff) orderUint16(b []byte) uint16 {
	if len(b) < 2 {
		return 0
	}
	if t.order == little {
		return binary.LittleEndian.Uint16(b)
	}
	return binary.BigEndian.Uint16(b)
}

func (t *tiff) orderUint32(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	if t.order == little {
		return binary.LittleEndian.Uint32(b)
	}
	return binary.BigEndian.Uint32(b)
}

// rationals returns num/den pairs, skipping divisions by zero.
func (t *tiff) rationals(e entry) [][2]float64 {
	b, ok := t.valueBytes(e)
	if !ok || (e.typ != 5 && e.typ != 10) {
		return nil
	}
	n := len(b) / 8
	if n > 8 {
		n = 8
	}
	out := make([][2]float64, 0, n)
	for i := 0; i < n; i++ {
		num := t.orderUint32(b[i*8:])
		den := t.orderUint32(b[i*8+4:])
		if den == 0 {
			continue
		}
		out = append(out, [2]float64{float64(num), float64(den)})
	}
	return out
}

// ascii returns a bounded, control-character-free string.
func (t *tiff) ascii(e entry) string {
	b, ok := t.valueBytes(e)
	if !ok {
		return ""
	}
	if e.typ != 2 && e.typ != 7 && e.typ != 1 {
		// Some writers store text under a numeric type; read whatever bytes are
		// there rather than losing the value, still bounded.
	}
	return sanitizeString(string(b), t.lim.MaxStringBytes)
}

// sanitizeString strips NULs and control characters and caps the length.
//
// The string is left otherwise verbatim: escaping for HTML is done by the JSON
// encoder, and mangling data here would make a legitimate filename unreadable.
func sanitizeString(s string, max int) string {
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\t' {
			b.WriteByte(' ')
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		if r == 0xFFFD {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func (t *tiff) handle(e entry, kind ifdKind, depth int) {
	switch kind {
	case ifdRoot:
		t.handleRoot(e, depth)
	case ifdExif:
		t.handleExif(e)
	case ifdGPS:
		t.handleGPS(e)
	}
}

func (t *tiff) handleRoot(e entry, depth int) {
	switch e.tag {
	case 0x010F:
		t.set(&t.out.Make, t.ascii(e))
	case 0x0110:
		t.set(&t.out.Model, t.ascii(e))
	case 0x0131:
		t.set(&t.out.Software, t.ascii(e))
	case 0x0112:
		if v := t.uints(e); len(v) > 0 && v[0] >= 1 && v[0] <= 8 {
			t.out.Orientation = int(v[0])
		}
	case 0x0132:
		t.set(&t.out.DateTime, t.ascii(e))
	case 0x010E:
		t.set(&t.out.ImageDescription, t.ascii(e))
	case 0x013B:
		t.set(&t.out.Artist, t.ascii(e))
	case 0x8298:
		t.set(&t.out.Copyright, t.ascii(e))
	case 0x011A:
		if r := t.rationals(e); len(r) > 0 {
			t.out.XResolution = ratString(r[0])
		}
	case 0x011B:
		if r := t.rationals(e); len(r) > 0 {
			t.out.YResolution = ratString(r[0])
		}
	case 0x0128:
		if v := t.uints(e); len(v) > 0 {
			t.out.ResolutionUnit = int(v[0])
		}
	case 0x8769:
		if v := t.uints(e); len(v) > 0 {
			t.walk(v[0], ifdExif, depth+1)
		}
	case 0x8825:
		if v := t.uints(e); len(v) > 0 {
			t.walk(v[0], ifdGPS, depth+1)
		}
	}
}

func (t *tiff) handleExif(e entry) {
	switch e.tag {
	case 0x9003:
		t.set(&t.out.DateTimeOriginal, t.ascii(e))
	case 0x9004:
		t.set(&t.out.DateTimeDigitized, t.ascii(e))
	case 0x9010:
		t.set(&t.out.OffsetTime, t.ascii(e))
	case 0x9011:
		t.set(&t.out.OffsetTimeOriginal, t.ascii(e))
	case 0x9012:
		t.set(&t.out.OffsetTimeDigitized, t.ascii(e))
	case 0x9291:
		t.set(&t.out.SubSecTimeOriginal, t.ascii(e))
	case 0xA002:
		if v := t.uints(e); len(v) > 0 {
			t.out.PixelXDimension = clampDim(v[0])
		}
	case 0xA003:
		if v := t.uints(e); len(v) > 0 {
			t.out.PixelYDimension = clampDim(v[0])
		}
	case 0x8827:
		if v := t.uints(e); len(v) > 0 {
			t.out.ISO = int(v[0])
		}
	case 0x9209:
		if v := t.uints(e); len(v) > 0 {
			t.out.Flash = int(v[0])
		}
	case 0x829A:
		if r := t.rationals(e); len(r) > 0 {
			t.out.ExposureTime = exposureString(r[0])
		}
	case 0x829D:
		if r := t.rationals(e); len(r) > 0 {
			t.out.FNumber = "f/" + trimFloat(r[0][0]/r[0][1], 1)
		}
	case 0x920A:
		if r := t.rationals(e); len(r) > 0 {
			t.out.FocalLength = trimFloat(r[0][0]/r[0][1], 1) + " mm"
		}
	case 0xA405:
		if v := t.uints(e); len(v) > 0 {
			t.out.FocalLength35mm = int(v[0])
		}
	case 0x9204:
		if r := t.rationals(e); len(r) > 0 {
			t.out.ExposureBias = trimFloat(r[0][0]/r[0][1], 2) + " EV"
		}
	case 0x9207:
		if v := t.uints(e); len(v) > 0 {
			t.out.MeteringMode = int(v[0])
		}
	case 0x8822:
		if v := t.uints(e); len(v) > 0 {
			t.out.ExposureProgram = int(v[0])
		}
	case 0xA403:
		if v := t.uints(e); len(v) > 0 {
			t.out.WhiteBalance = int(v[0])
		}
	case 0xA406:
		if v := t.uints(e); len(v) > 0 {
			t.out.SceneCaptureType = int(v[0])
		}
	case 0xA404:
		if r := t.rationals(e); len(r) > 0 && r[0][1] != 0 {
			t.out.DigitalZoomRatio = trimFloat(r[0][0]/r[0][1], 2)
		}
	case 0x9206:
		if r := t.rationals(e); len(r) > 0 {
			t.out.SubjectDistance = trimFloat(r[0][0]/r[0][1], 2) + " m"
		}
	case 0x9000:
		t.set(&t.out.ExifVersion, t.ascii(e))
	case 0xA431:
		t.set(&t.out.BodySerialNumber, t.ascii(e))
	case 0xA435:
		t.set(&t.out.LensSerialNumber, t.ascii(e))
	case 0xA434:
		t.set(&t.out.LensModel, t.ascii(e))
	case 0xA001:
		if v := t.uints(e); len(v) > 0 {
			t.out.ColorSpace = int(v[0])
		}
	}
}

// clampDim rejects absurd stored dimensions rather than trusting them.
func clampDim(v uint32) int {
	if v > 1_000_000 {
		return 0
	}
	return int(v)
}

func (t *tiff) handleGPS(e entry) {
	if t.out.GPS == nil {
		t.out.GPS = &GPS{}
	}
	switch e.tag {
	case 0x0001, 0x0003: // refs
		s := t.ascii(e)
		if e.tag == 0x0001 {
			t.gpsLatRef = strings.ToUpper(strings.TrimSpace(s))
		} else {
			t.gpsLonRef = strings.ToUpper(strings.TrimSpace(s))
		}
	case 0x0002:
		if v, ok := t.dms(e); ok {
			t.gpsLat = &v
		}
	case 0x0004:
		if v, ok := t.dms(e); ok {
			t.gpsLon = &v
		}
	case 0x0005:
		if r := t.rationals(e); len(r) > 0 && r[0][1] != 0 {
			alt := r[0][0] / r[0][1]
			t.gpsAlt = &alt
		}
	case 0x0006:
		if v := t.uints(e); len(v) > 0 && v[0] == 1 && t.gpsAlt != nil {
			neg := -*t.gpsAlt
			t.gpsAlt = &neg
		}
	}
}

func (t *tiff) dms(e entry) (float64, bool) {
	parts := t.rationals(e)
	if len(parts) < 3 {
		return 0, false
	}
	d := parts[0][0] / parts[0][1]
	m := parts[1][0] / parts[1][1]
	s := parts[2][0] / parts[2][1]
	v := d + m/60 + s/3600
	if v < -180 || v > 180 {
		return 0, false
	}
	return v, true
}

func (t *tiff) set(dst *string, v string) {
	if *dst == "" && v != "" {
		*dst = v
	}
}

func (t *tiff) finalize() {
	out := t.out
	if out.GPS != nil {
		if t.gpsLat == nil || t.gpsLon == nil {
			// A half-present coordinate is not a coordinate.
			out.GPS = nil
		} else {
			lat, lon := *t.gpsLat, *t.gpsLon
			if t.gpsLatRef == "S" {
				lat = -lat
			}
			if t.gpsLonRef == "W" {
				lon = -lon
			}
			if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
				out.warn("GPS coordinates out of range (%f, %f); discarded", lat, lon)
				out.GPS = nil
			} else if lat == 0 && lon == 0 {
				// 0,0 is the classic "GPS unit had no fix" placeholder.
				out.warn("GPS coordinates are exactly 0,0 which usually means no fix; discarded")
				out.GPS = nil
			} else {
				out.GPS = &GPS{Lat: lat, Lon: lon, AltitudeM: t.gpsAlt}
			}
		}
	}
	out.Extra = out.buildExtra(t.lim.MaxKeys)
}

// add appends a key/value pair, skipping empties.
func (t *Tags) add(dst *[]KV, key, value string) {
	if value == "" {
		return
	}
	*dst = append(*dst, KV{Key: key, Value: value})
}

func (t *Tags) buildExtra(max int) []KV {
	var out []KV
	t.add(&out, "Make", t.Make)
	t.add(&out, "Model", t.Model)
	t.add(&out, "Lens", t.LensModel)
	t.add(&out, "Software", t.Software)
	t.add(&out, "DateTime", t.DateTime)
	t.add(&out, "DateTimeOriginal", t.DateTimeOriginal)
	t.add(&out, "DateTimeDigitized", t.DateTimeDigitized)
	t.add(&out, "OffsetTimeOriginal", t.OffsetTimeOriginal)
	t.add(&out, "ExifVersion", t.ExifVersion)
	t.add(&out, "ExposureTime", t.ExposureTime)
	t.add(&out, "FNumber", t.FNumber)
	t.add(&out, "ExposureBias", t.ExposureBias)
	t.add(&out, "ISO", itoaOrEmpty(t.ISO))
	t.add(&out, "FocalLength", t.FocalLength)
	t.add(&out, "FocalLength35mm", itoaOrEmpty(t.FocalLength35mm))
	t.add(&out, "SubjectDistance", t.SubjectDistance)
	t.add(&out, "DigitalZoomRatio", t.DigitalZoomRatio)
	t.add(&out, "MeteringMode", itoaOrEmpty(t.MeteringMode))
	t.add(&out, "ExposureProgram", itoaOrEmpty(t.ExposureProgram))
	t.add(&out, "WhiteBalance", itoaOrEmpty(t.WhiteBalance))
	t.add(&out, "SceneCaptureType", itoaOrEmpty(t.SceneCaptureType))
	t.add(&out, "Flash", itoaOrEmpty(t.Flash))
	t.add(&out, "ColorSpace", itoaOrEmpty(t.ColorSpace))
	t.add(&out, "Artist", t.Artist)
	t.add(&out, "Copyright", t.Copyright)
	t.add(&out, "ImageDescription", t.ImageDescription)
	t.add(&out, "BodySerialNumber", t.BodySerialNumber)
	t.add(&out, "LensSerialNumber", t.LensSerialNumber)
	t.add(&out, "XResolution", t.XResolution)
	t.add(&out, "YResolution", t.YResolution)
	if len(out) > max {
		out = out[:max]
	}
	return out
}

func itoaOrEmpty(v int) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprintf("%d", v)
}

func ratString(r [2]float64) string {
	if r[1] == 0 {
		return ""
	}
	return trimFloat(r[0]/r[1], 2)
}

func exposureString(r [2]float64) string {
	if r[1] == 0 || r[0] == 0 {
		return ""
	}
	v := r[0] / r[1]
	if v >= 1 {
		return trimFloat(v, 1) + " s"
	}
	return fmt.Sprintf("1/%s s", trimFloat(1/v, 0))
}

func trimFloat(v float64, decimals int) string {
	s := fmt.Sprintf("%.*f", decimals, v)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}
