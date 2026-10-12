package mimetype

import "testing"

// TestForExtIsAClosedAllowlist is a security property, not a formatting detail.
// The web service labels every response with this function. If an unknown or
// hostile extension ever produced a text-ish type, a file dropped into a photo
// directory could be served as HTML from the album's own origin and run script
// in every browser on the tailnet.
func TestForExtIsAClosedAllowlist(t *testing.T) {
	allowed := map[string]string{
		"jpg": "image/jpeg", "jpeg": "image/jpeg", "JPG": "image/jpeg",
		".jpeg": "image/jpeg", " jpg ": "image/jpeg",
		"png": "image/png", "gif": "image/gif", "webp": "image/webp",
		"tif": "image/tiff", "tiff": "image/tiff",
		"mp4": "video/mp4", "mov": "video/quicktime", "webm": "video/webm",
		"mkv": "video/x-matroska", "m4v": "video/mp4",
	}
	for ext, want := range allowed {
		if got := ForExt(ext); got != want {
			t.Errorf("ForExt(%q) = %q, want %q", ext, got, want)
		}
	}

	// Everything here must fall through to octet-stream.
	hostile := []string{
		"html", "htm", "xhtml", "svg", "svgz", "js", "mjs", "css", "xml",
		"json", "txt", "pdf", "php", "wasm",
		"", " ", ".", "..", "JPG.html", "jpg.php", "jpg\x00.html",
		"a/b", "../../etc/passwd", "jpg/../html",
	}
	for _, ext := range hostile {
		got := ForExt(ext)
		if got != "application/octet-stream" {
			t.Errorf("ForExt(%q) = %q; an unlisted extension must not be given a real content type", ext, got)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"JPG": "jpg", ".JPG": "jpg", " jpg ": "jpg", "jpeg": "jpeg", "": "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
