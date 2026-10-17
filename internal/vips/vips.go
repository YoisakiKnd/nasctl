// Package vips runs libvips' command-line tool to make thumbnails, when the
// machine has it. libvips decodes JPEG at a reduced size, streams other
// formats, uses NEON/SSE where available and reads HEIC/AVIF/TIFF that the
// Go decoders cannot. It is optional: nasctl stays a single static binary and
// falls back to its own decoder when the tool is absent.
//
// The tool runs as a child of the thumbnail worker, so it inherits the
// worker's cgroup memory ceiling and process group: the same limits and the
// same kill path apply to it as to the Go decoder.
package vips

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Formats are the source formats handed to libvips. GIF stays with the Go
// decoder, which reads only the first frame by design.
var Formats = map[string]bool{"jpeg": true, "png": true, "webp": true, "tiff": true, "heic": true, "heif": true, "avif": true}

// ExtraExtensions are extensions that only libvips can turn into thumbnails.
var ExtraExtensions = map[string]bool{"heic": true, "heif": true, "avif": true, "tif": true, "tiff": true}

// Find resolves the vips executable, or returns "" when it is unavailable or
// disabled. An explicit path must be absolute; a bare name is looked up in PATH.
func Find(enabled bool, name string) string {
	if !enabled || name == "" {
		return ""
	}
	if strings.ContainsRune(name, os.PathSeparator) && !filepath.IsAbs(name) {
		return ""
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

// Version returns the first line of `vips --version`, for status reports.
func Version(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line, nil
}

// Thumbnail writes a JPEG whose longest edge is at most edge to dst. The
// image is turned upright from its orientation tag, metadata is stripped and
// loaders libvips marks as untrusted are refused.
func Thumbnail(ctx context.Context, path, src, dst string, edge, quality int) error {
	if edge < 1 || quality < 1 || quality > 100 {
		return errors.New("vips: invalid thumbnail size or quality")
	}
	// A leading dash would read as an option; the paths here are absolute.
	if !filepath.IsAbs(src) || !filepath.IsAbs(dst) {
		return errors.New("vips: paths must be absolute")
	}
	out := fmt.Sprintf("%s[Q=%d,strip,optimize_coding]", dst, quality)
	cmd := exec.CommandContext(ctx, path, "thumbnail", src, out, strconv.Itoa(edge), "--height", strconv.Itoa(edge), "--size", "down")
	cmd.Env = append(os.Environ(),
		"VIPS_BLOCK_UNTRUSTED=1", // no ImageMagick, Matlab or other risky loaders
		"VIPS_CONCURRENCY=1",     // one decode thread: memory stays predictable
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 300 {
			detail = detail[:300]
		}
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("vips: %s", detail)
	}
	return nil
}
