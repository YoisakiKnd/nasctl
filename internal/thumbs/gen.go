// Package thumbs produces one small thumbnail per image.
//
// Design constraints that shaped this package:
//
//   - One size only. The library keeps a single 512 px (configurable) longest
//     edge: no pyramid, no pre-rendered large sizes, and no way for a client to
//     ask for a decode at original resolution.
//   - Bounded decode. Dimensions are read with DecodeConfig and checked against
//     the pixel budget *before* any pixel is decoded, so an image with a
//     fabricated 100000x100000 header is refused instead of attempted.
//   - Bounded output. The destination is exactly the thumbnail; the full-size
//     decoded image is the peak, and it is released before encoding.
//   - Atomic publish. The file is written to a temporary name, fsynced and
//     renamed, so a cache entry that exists is always complete. An interrupted
//     run leaves a temp file that cleanup removes, never a truncated thumbnail
//     that a browser would happily cache.
//   - Animated sources decode one frame. gif.Decode (not DecodeAll) is used,
//     because DecodeAll allocates every frame at once and is the classic way to
//     take a small machine down with a harmless-looking file.
package thumbs

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"sort"
	"time"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/YoisakiKnd/nasctl/internal/exif"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
	"github.com/YoisakiKnd/nasctl/internal/probe"
)

// Formats is the enabled-format matrix for decoding.
type Formats struct {
	JPEG bool
	PNG  bool
	GIF  bool
	WebP bool
}

// DefaultFormats matches the shipped configuration.
func DefaultFormats() Formats { return Formats{JPEG: true, PNG: true, GIF: true, WebP: true} }

// Enabled reports whether a format name from image.Decode is permitted.
func (f Formats) Enabled(name string) bool {
	switch name {
	case "jpeg":
		return f.JPEG
	case "png":
		return f.PNG
	case "gif":
		return f.GIF
	case "webp":
		return f.WebP
	}
	return false
}

// Options bounds one generation.
type Options struct {
	MaxEdge        int
	Quality        int
	MaxPixels      int64
	MaxFileBytes   int64
	MaxDecodeBytes int64
	MaxFrames      int
	Formats        Formats
}

// Normalize fills defaults.
func (o Options) Normalize() Options {
	if o.MaxEdge <= 0 {
		o.MaxEdge = 512
	}
	if o.MaxEdge > 2048 {
		o.MaxEdge = 2048
	}
	if o.Quality < 40 || o.Quality > 95 {
		o.Quality = 82
	}
	if o.MaxPixels <= 0 {
		o.MaxPixels = 12_000_000
	}
	if o.Formats == (Formats{}) {
		o.Formats = DefaultFormats()
	}
	return o
}

// Outcome states.
const (
	StateOK      = "ok"
	StateSkipped = "skipped"
	StateFailed  = "failed"
)

// Outcome is the result of one generation attempt.
type Outcome struct {
	State  string
	Reason string
	Detail string

	Bytes        int64
	OutWidth     int
	OutHeight    int
	SourceWidth  int
	SourceHeight int

	MS int64

	// PeakRSSBytes is this process's VmHWM at the end of the run, measured from
	// /proc/self/status. It is the decoder's own high-water mark and is quite
	// different from the cgroup peak, which also includes page cache charged to
	// the cgroup.
	PeakRSSBytes int64

	Notes []string
}

func (o *Outcome) skip(reason, detail string) Outcome {
	o.State = StateSkipped
	o.Reason = reason
	o.Detail = detail
	return *o
}

func (o *Outcome) fail(reason, detail string) Outcome {
	o.State = StateFailed
	o.Reason = reason
	o.Detail = detail
	return *o
}

// Generate decodes srcPath and writes a thumbnail to dstPath.
//
// The expected size and mtime are re-verified immediately before decoding and
// again immediately after: a file that was replaced while it was being read
// produces no cache entry at all, because publishing a thumbnail of a different
// picture than the index describes would be worse than showing a placeholder.
func Generate(srcPath, dstPath string, expectSize, expectMtimeNS int64, opt Options) Outcome {
	start := time.Now()
	opt = opt.Normalize()
	var out Outcome

	fi, err := os.Lstat(srcPath)
	if err != nil {
		return out.fail("stat_failed", err.Error())
	}
	if !fi.Mode().IsRegular() {
		return out.skip("not_a_regular_file", "the indexed path is no longer a regular file")
	}
	if expectSize > 0 && fi.Size() != expectSize {
		return out.skip("file_changed", fmt.Sprintf("size is now %d, index says %d; deferred", fi.Size(), expectSize))
	}
	if expectMtimeNS > 0 && fi.ModTime().UnixNano() != expectMtimeNS {
		return out.skip("file_changed", "modification time changed since indexing; deferred")
	}
	if opt.MaxFileBytes > 0 && fi.Size() > opt.MaxFileBytes {
		return out.skip("file_too_large", fmt.Sprintf("%d bytes exceeds the %d byte limit", fi.Size(), opt.MaxFileBytes))
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return out.fail("open_failed", err.Error())
	}
	defer f.Close()

	// Pass 1: headers only. Nothing large is allocated here.
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return out.skip("undecodable_header", "header could not be decoded: "+err.Error())
	}
	out.SourceWidth, out.SourceHeight = cfg.Width, cfg.Height

	if !opt.Formats.Enabled(format) {
		return out.skip("format_not_enabled", "decoder for "+format+" is disabled in the configuration")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return out.skip("bad_dimensions", fmt.Sprintf("header declares %dx%d", cfg.Width, cfg.Height))
	}
	if cfg.Width > 1_000_000 || cfg.Height > 1_000_000 {
		return out.skip("bad_dimensions", fmt.Sprintf("header declares an implausible %dx%d", cfg.Width, cfg.Height))
	}
	pixels := int64(cfg.Width) * int64(cfg.Height)
	if opt.MaxPixels > 0 && pixels > opt.MaxPixels {
		return out.skip("over_pixel_budget", fmt.Sprintf(
			"%d pixels exceeds the configured limit of %d; not attempted", pixels, opt.MaxPixels))
	}
	est := probe.EstimateDecodeBytes(probe.KindImage, format, pixels, cfg.Width, cfg.Height, opt.MaxEdge)
	if opt.MaxDecodeBytes > 0 && est > opt.MaxDecodeBytes {
		return out.skip("over_decode_budget", fmt.Sprintf(
			"the conservative decode estimate of %d bytes exceeds the %d byte budget; not attempted",
			est, opt.MaxDecodeBytes))
	}
	if opt.MaxFrames > 0 && format == "gif" {
		// Only the first frame is decoded; the file itself is not refused. The
		// note exists so an animated GIF appearing as a still is explainable.
		out.Notes = append(out.Notes, "animated source: only the first frame is decoded")
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return out.fail("seek_failed", err.Error())
	}

	// Pass 2: the decode. This is the allocation the whole design is built to
	// contain, and it is contained by the process ceiling rather than by good
	// intentions.
	src, _, err := image.Decode(f)
	if err != nil {
		return out.skip("undecodable", "decode failed: "+err.Error())
	}

	dst := downscale(src, opt.MaxEdge)
	if dst == nil {
		return out.fail("scale_failed", "downscale produced no image")
	}
	out.OutWidth, out.OutHeight = dst.Bounds().Dx(), dst.Bounds().Dy()

	// Release the source before encoding. On a machine with 981 MiB this is the
	// difference between a comfortable peak and a tight one.
	src = nil

	written, werr := writeThumb(dstPath, dst, opt.Quality)
	dst = nil
	if werr != nil {
		return out.fail("write_failed", werr.Error())
	}
	out.Bytes = written

	// Re-verify: if the original changed while we were working, this thumbnail
	// describes content the index no longer points at, so it is discarded
	// rather than published.
	fi2, err := os.Lstat(srcPath)
	if err != nil || fi2.Size() != fi.Size() || fi2.ModTime().UnixNano() != fi.ModTime().UnixNano() {
		_ = os.Remove(dstPath)
		return out.skip("file_changed", "the original changed during processing; the new thumbnail was discarded")
	}

	out.State = StateOK
	out.MS = time.Since(start).Milliseconds()
	return out
}

// downscale scales src down so that its longest edge is at most maxEdge, and
// never enlarges it. The result is an opaque RGBA image on a white background,
// because the output format has no alpha channel and a transparent region would
// otherwise render as black.
func downscale(src image.Image, maxEdge int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return nil
	}
	tw, th := sw, sh
	if sw > maxEdge || sh > maxEdge {
		if sw >= sh {
			tw = maxEdge
			th = int(float64(sh)*float64(maxEdge)/float64(sw) + 0.5)
		} else {
			th = maxEdge
			tw = int(float64(sw)*float64(maxEdge)/float64(sh) + 0.5)
		}
	}
	if tw < 1 {
		tw = 1
	}
	if th < 1 {
		th = 1
	}
	rect := image.Rect(0, 0, tw, th)
	dst := image.NewRGBA(rect)
	// White first, then composite the source over it.
	draw.Draw(dst, rect, image.NewUniform(color.White), image.Point{}, draw.Src)
	if tw == sw && th == sh {
		draw.Draw(dst, rect, src, b.Min, draw.Over)
		return dst
	}
	xdraw.ApproxBiLinear.Scale(dst, rect, src, b, draw.Over, nil)
	return dst
}

// writeThumb encodes and atomically publishes the thumbnail file.
func writeThumb(dstPath string, img *image.RGBA, quality int) (int64, error) {
	var n int64
	err := fsutil.AtomicWrite(dstPath, 0o640, func(w io.Writer) error {
		cw := &countingWriter{w: w}
		if err := jpeg.Encode(cw, img, &jpeg.Options{Quality: quality}); err != nil {
			return err
		}
		n = cw.n
		return nil
	})
	return n, err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// ErrNoDecoder is returned when the format cannot be handled at all.
var ErrNoDecoder = errors.New("no decoder available for this format")

// AvailableFormats lists the decoders this build registered, for `doctor`.
func AvailableFormats() []string {
	out := []string{}
	for _, f := range []string{"jpeg", "png", "gif", "webp"} {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// ProbeOnly returns the header facts for a file without decoding pixels. Used
// by the worker's pre-flight so the budget decision is made against the file as
// it is now, not as the index recorded it.
func ProbeOnly(path string, maxPixels int64, maxFileBytes int64, maxDecodeBytes int64, maxEdge int) (w, h int, est int64, reason string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, "", err
	}
	defer f.Close()
	if maxFileBytes > 0 {
		fi, serr := f.Stat()
		if serr == nil && fi.Size() > maxFileBytes {
			return 0, 0, 0, "file_too_large", nil
		}
	}
	cfg, _, derr := image.DecodeConfig(f)
	if derr != nil {
		return 0, 0, 0, "undecodable_header", nil
	}
	w, h = cfg.Width, cfg.Height
	if w <= 0 || h <= 0 {
		return w, h, 0, "bad_dimensions", nil
	}
	pixels := int64(w) * int64(h)
	if maxPixels > 0 && pixels > maxPixels {
		return w, h, 0, "over_pixel_budget", nil
	}
	est = probe.EstimateDecodeBytes(probe.KindImage, "unknown", pixels, w, h, maxEdge)
	if maxDecodeBytes > 0 && est > maxDecodeBytes {
		return w, h, est, "over_decode_budget", nil
	}
	return w, h, est, "", nil
}

// FormatOf returns the registered decoder name for a file, or an empty string.
func FormatOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	_ = n
	return exif.DetectFormat(head[:n])
}
