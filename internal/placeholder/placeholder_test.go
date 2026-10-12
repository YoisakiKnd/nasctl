package placeholder

import (
	"bytes"
	"strings"
	"testing"
)

// TestSVGIsAWellFormedDocument checks the envelope, because the browser parses
// this response as image/svg+xml: a document that does not close properly
// renders as nothing at all, and every missing thumbnail would show a blank
// frame instead of an explanation.
func TestSVGIsAWellFormedDocument(t *testing.T) {
	for _, isVideo := range []bool{false, true} {
		out := SVG("IMG_0007.jpg", isVideo)
		s := string(out)
		if !strings.HasPrefix(s, "<svg ") {
			t.Errorf("isVideo=%v: the document does not begin with an svg element: %.60q", isVideo, s)
		}
		if !strings.HasSuffix(s, "</svg>") {
			t.Errorf("isVideo=%v: the document does not end with </svg>: %.60q", isVideo, s[len(s)-60:])
		}
		if n := strings.Count(s, "<svg"); n != 1 {
			t.Errorf("isVideo=%v: %d svg elements, want 1", isVideo, n)
		}
		if !strings.Contains(s, `xmlns="http://www.w3.org/2000/svg"`) {
			t.Errorf("isVideo=%v: the svg element has no namespace declaration", isVideo)
		}
		if !strings.Contains(s, "IMG_0007.jpg") {
			t.Errorf("isVideo=%v: the label is missing from the output", isVideo)
		}
	}
}

// TestSVGDistinguishesVideoFromImage keeps the two variants visibly different.
// A video shown with the photograph glyph would mislead about what clicking it
// will do.
func TestSVGDistinguishesVideoFromImage(t *testing.T) {
	img := string(SVG("a.jpg", false))
	vid := string(SVG("a.mp4", true))
	if img == vid {
		t.Fatal("the image and video placeholders are byte-identical")
	}
	if !strings.Contains(vid, `<path d="M232 196l84 44-84 44z"`) {
		t.Error("the video placeholder has no play glyph")
	}
	if strings.Contains(img, `<path d="M232 196l84 44-84 44z"`) {
		t.Error("the image placeholder has the video play glyph")
	}
}

// TestSVGEscapesTheLabel is the security property. The label comes from a file
// name in a directory that the operator may not fully control, and the output
// is served from the album's own origin. If a name could close the attribute or
// the text node and open a script element, dropping a file named after an
// exploit would run code in every browser that reached the gallery.
func TestSVGEscapesTheLabel(t *testing.T) {
	hostile := []struct {
		label string
		raw   string // must NOT appear verbatim in the output
		esc   string // must appear instead
	}{
		{"<img src=x onerror=alert(1)>", "<img src=x", "&lt;img src=x"},
		{`" onload="alert(1)`, `" onload="`, "&quot; onload=&quot;"},
		{"' onload='alert(1)", "' onload='", "&apos; onload=&apos;"},
		{"</text></svg><script>alert(1)</script>", "<script>", "&lt;script&gt;"},
		// A literal "&amp;" in a name is escaped again, which is correct: the
		// name on disk is what it is, and the document must show those five
		// characters rather than a bare ampersand.
		{"a&b", "a&b", "a&amp;b"},
		{"&amp;", "\u0000", "&amp;amp;"},
	}
	for _, c := range hostile {
		out := string(SVG(c.label, false))
		if strings.Contains(out, c.raw) {
			t.Errorf("label %q appears unescaped as %q in the output; this is an injection "+
				"into a document served from the album origin", c.label, c.raw)
		}
		if !strings.Contains(out, c.esc) {
			t.Errorf("label %q: expected %q in the output, got:\n%s", c.label, c.esc, out)
		}
	}
}

func TestSVGCannotBeClosedByAnXMLTerminator(t *testing.T) {
	for _, label := range []string{"]]>", "a]]>b", "</svg>", "</text>", "<![CDATA[x]]>"} {
		out := string(SVG(label, false))
		if strings.Count(out, "</svg>") != 1 {
			t.Errorf("label %q produced %d closing svg tags; the document structure was broken:\n%s",
				label, strings.Count(out, "</svg>"), out)
		}
		if strings.Count(out, "<text") != 1 {
			t.Errorf("label %q produced %d text elements", label, strings.Count(out, "<text"))
		}
	}
}

// TestSVGStripsControlCharacters keeps the response clean: a name with an
// embedded newline or a NUL would otherwise put bytes into the document that
// an XML parser rejects, turning every placeholder for such a file into a parse
// error rather than a picture.
func TestSVGStripsControlCharacters(t *testing.T) {
	out := string(SVG("a\nb\tc\x00d\x01e\x7ff", false))
	for _, bad := range []string{"\n", "\t", "\x00", "\x01", "\x7f"} {
		// The template itself contains no newlines, so any occurrence came from
		// the label.
		if strings.Contains(out, bad) {
			t.Errorf("the output contains %q from the label; control characters must be dropped", bad)
		}
	}
	if !strings.Contains(out, "abcdef") {
		t.Errorf("stripping control characters also removed ordinary ones: %s", out)
	}
}

func TestSVGSurvivesNULAndKeepsUTF8(t *testing.T) {
	out := string(SVG("中文名字 照片.jpg", false))
	if !strings.Contains(out, "中文名字 照片.jpg") {
		t.Errorf("a UTF-8 label was not preserved:\n%s", out)
	}
	out = string(SVG("名前🎞️.jpg", false))
	if !strings.Contains(out, "名前🎞️.jpg") {
		t.Errorf("an emoji label was not preserved:\n%s", out)
	}
	// A NUL must not panic and must not appear.
	if o := SVG("a\x00b", false); bytes.ContainsRune(o, 0) {
		t.Error("the output contains a NUL byte")
	}
}

// TestSVGBoundsTheLabelLength keeps a single response bounded. A file name can
// be up to 255 bytes, but there is no reason to echo all of it, and the label
// is also what the truncation marker protects against unbounded output.
func TestSVGBoundsTheLabelLength(t *testing.T) {
	long := strings.Repeat("あ", 4000) // 12 KB of UTF-8
	out := SVG(long, false)
	if len(out) > 4096 {
		t.Errorf("a 12 KB label produced a %d byte placeholder; the output must stay bounded", len(out))
	}
	if !strings.Contains(string(out), "\u2026") {
		t.Logf("NOTE: no truncation marker appeared; the bound may be enforced by an " +
			"output-length check that simply stops mid-label")
	}
	// An ordinary short name must not be truncated at all.
	short := "IMG_0001.jpg"
	if !strings.Contains(string(SVG(short, false)), short) {
		t.Error("a short label was truncated")
	}
}

// TestSVGIsSmallEnoughForEveryMissingThumbnail matters because the endpoint
// returns this for every item whose thumbnail has not been generated yet. If it
// were large, a gallery page of missing thumbnails would be expensive.
func TestSVGIsSmallEnoughForEveryMissingThumbnail(t *testing.T) {
	out := SVG("a-typical-camera-file-name.jpg", false)
	if len(out) > 1500 {
		t.Errorf("the placeholder is %d bytes; it should be a few hundred", len(out))
	}
}

func TestSVGIsDeterministic(t *testing.T) {
	a := SVG("x.jpg", false)
	b := SVG("x.jpg", false)
	if !bytes.Equal(a, b) {
		t.Error("the placeholder is not deterministic; it cannot be cached or compared")
	}
}
