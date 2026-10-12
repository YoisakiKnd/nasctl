package scan

import (
	"regexp"
	"strconv"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/probe"
	"github.com/YoisakiKnd/nasctl/internal/store"
)

// CaptureResolution is the outcome of the capture-time policy.
type CaptureResolution struct {
	// Unix is the timestamp stored in the index. For a "naive" timezone this is
	// the camera's wall-clock reading interpreted as UTC, so that ordering and
	// display both reproduce what the camera recorded without converting it.
	Unix *int64

	Src       string // exif | container | filename | filetime | none
	TZ        string // offset | naive
	OffsetMin *int
	Note      string
}

// ResolveCapture applies the documented order of preference:
//
//  1. a real capture timestamp recorded in the file (EXIF DateTimeOriginal,
//     then DateTimeDigitized, then the TIFF DateTime);
//  2. the container's creation time for a video;
//  3. an optional, off-by-default filename heuristic;
//  4. the file's own modification time, always marked as a fallback.
//
// It never guesses GPS, a person, or a capture time. When nothing is available
// it returns Src "none" and a nil timestamp, and the UI shows "no date" instead
// of inventing one at the epoch, which would silently reorder the whole
// timeline.
func ResolveCapture(res *probe.Result, baseName string, mtime time.Time, useFilename bool) CaptureResolution {
	var out CaptureResolution

	if res != nil && res.Tags != nil {
		if wall, offsetMin, ok := res.Tags.Capture(); ok {
			if t, err := time.Parse("2006-01-02T15:04:05.999999999", wall); err == nil {
				u := t.UTC().Unix()
				out.Unix = &u
				out.Src = store.TakenSrcExif
				out.OffsetMin = offsetMin
				if offsetMin != nil {
					out.TZ = "offset"
				} else {
					out.TZ = "naive"
					out.Note = "EXIF carried no UTC offset; the time is shown exactly as recorded, unshifted"
				}
				return out
			}
		}
	}

	if res != nil && res.ContainerTime != "" {
		if t, err := time.Parse("2006-01-02T15:04:05", res.ContainerTime); err == nil {
			// The container field is defined as UTC. Many cameras write local
			// time into it anyway, so the value is used but labelled as a
			// container timestamp rather than a verified capture time.
			u := t.UTC().Unix()
			zero := 0
			out.Unix = &u
			out.Src = store.TakenSrcContainer
			out.TZ = "offset"
			out.OffsetMin = &zero
			out.Note = "video container creation time; treated as UTC per the format specification"
			return out
		}
	}

	if useFilename && res != nil {
		// The heuristic is opt-in and is never applied to a file that already
		// carried a real timestamp.
		if t, ok := ParseFilenameTime(baseName); ok {
			u := t.Unix()
			out.Unix = &u
			out.Src = store.TakenSrcFilename
			out.TZ = "naive"
			out.Note = "date taken from the file name because the file carried none; " +
				"enable/disable with time.filename_heuristic"
			return out
		}
	}

	// Fallback: the file's own modification time.
	if !mtime.IsZero() && mtime.Unix() > 0 {
		u := mtime.Unix()
		out.Unix = &u
		out.Src = store.TakenSrcFiletime
		out.TZ = "naive"
		out.Note = "the file carried no capture time; the file modification time is shown and labelled as a fallback"
		return out
	}

	out.Src = store.TakenSrcNone
	out.TZ = "naive"
	out.Note = "no usable timestamp of any kind"
	return out
}

// filenamePatterns are the well-known camera naming conventions. They are only
// consulted when the heuristic is explicitly enabled.
var filenamePatterns = []*regexp.Regexp{
	// IMG_20230501_123304, VID_20230501_123304, PXL_20230501_123304123
	regexp.MustCompile(`(?i)\b(19\d{2}|20\d{2})(0[1-9]|1[0-2])(0[1-9]|[12]\d|3[01])[_-](\d{2})(\d{2})(\d{2})`),
	// 2023-05-01 12.33.04, 2023-05-01_12-33-04
	regexp.MustCompile(`\b(19\d{2}|20\d{2})-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])[ T_]([01]\d|2[0-3])[.:\-]([0-5]\d)[.:\-]([0-5]\d)`),
	// 20230501 only
	regexp.MustCompile(`\b(19\d{2}|20\d{2})(0[1-9]|1[0-2])(0[1-9]|[12]\d|3[01])\b`),
}

// ParseFilenameTime extracts a date (and time, when present) from a file name.
func ParseFilenameTime(name string) (time.Time, bool) {
	for _, re := range filenamePatterns {
		m := re.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		year, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		hour, minute, sec := 0, 0, 0
		if len(m) >= 7 {
			hour, _ = strconv.Atoi(m[4])
			minute, _ = strconv.Atoi(m[5])
			sec, _ = strconv.Atoi(m[6])
		}
		if year < 1900 || year > 2199 || month < 1 || month > 12 || day < 1 || day > 31 {
			continue
		}
		if hour > 23 || minute > 59 || sec > 60 {
			continue
		}
		t := time.Date(year, time.Month(month), day, hour, minute, sec, 0, time.UTC)
		// Reject a date that time.Date silently normalized (e.g. 31 February).
		if t.Day() != day || t.Month() != time.Month(month) {
			continue
		}
		return t, true
	}
	return time.Time{}, false
}
