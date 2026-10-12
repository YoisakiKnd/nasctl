// Package mimetype maps a file extension to a Content-Type for serving.
//
// It is its own package, with no file I/O and no imports besides strings, so
// that the read-only web service can label a response without pulling a
// decoder, a header parser or a scanner into its import graph. That property is
// asserted by internal/server/arch_test.go rather than merely intended.
package mimetype

import "strings"

// ForExt returns the Content-Type for an extension, with or without a leading
// dot.
//
// The allowlist is explicit and closed: anything not listed becomes
// application/octet-stream rather than being guessed. A crafted file whose name
// ends in .html must never be served as text/html from the album's own origin,
// because that would turn a mislabelled file into stored cross-site scripting
// against every device that can reach the service.
func ForExt(ext string) string {
	ext = Normalize(ext)
	switch ext {
	case "jpg", "jpeg", "jpe":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "bmp":
		return "image/bmp"
	case "tif", "tiff":
		return "image/tiff"
	case "heic":
		return "image/heic"
	case "heif":
		return "image/heif"
	case "avif":
		return "image/avif"
	case "mp4", "m4v":
		return "video/mp4"
	case "mov":
		return "video/quicktime"
	case "webm":
		return "video/webm"
	case "mkv":
		return "video/x-matroska"
	case "avi":
		return "video/x-msvideo"
	case "3gp":
		return "video/3gpp"
	case "ts", "m2ts", "mts":
		return "video/mp2t"
	case "mpg", "mpeg":
		return "video/mpeg"
	case "wmv":
		return "video/x-ms-wmv"
	case "flv":
		return "video/x-flv"
	}
	return "application/octet-stream"
}

// Normalize lowercases an extension and strips a leading dot and surrounding
// space, so ".JPG", "JPG" and " jpg " all classify identically.
func Normalize(ext string) string {
	ext = strings.TrimSpace(ext)
	ext = strings.TrimPrefix(ext, ".")
	return strings.ToLower(ext)
}
