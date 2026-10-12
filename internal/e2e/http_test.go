package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests exercise the web service against a real index and a real cache
// built by the real binary. They cover the promises the design makes about the
// read path: a bounded stream, correct Range handling, a placeholder instead of
// on-demand decoding, and no way for a crafted name to reach the browser as
// markup.

// serveIndexed builds a full index and thumbnail cache, then starts the service.
func serveIndexed(t *testing.T) (*rig, *server, *api) {
	t.Helper()
	r := newRig(t)
	r.indexAll()
	r.thumbnailAll()
	s := r.startServer()
	return r, s, r.api(s)
}

// idFor finds the media id for a path that the API already lists.
func (a *api) idFor(rel string) int64 {
	a.t.Helper()
	for _, page := range a.allMedia("smoke") {
		for _, it := range page.Items {
			if it.RelDir+"/"+it.FileName == rel || it.FileName == rel {
				return it.ID
			}
		}
	}
	a.t.Fatalf("no media row was listed for %q; the file is either not indexed or hidden", rel)
	return 0
}

// item fetches one media row. The endpoint wraps it as {"item": {...}}.
func (a *api) item(id int64) mediaItemDoc {
	a.t.Helper()
	var doc struct {
		Item mediaItemDoc `json:"item"`
	}
	if code := a.s.getJSON(fmt.Sprintf("/api/v1/media/%d", id), &doc); code != 200 {
		a.t.Fatalf("GET /api/v1/media/%d returned %d", id, code)
	}
	return doc.Item
}

type mediaItemDoc struct {
	ID              int64   `json:"id"`
	Name            string  `json:"name"`
	Kind            string  `json:"kind"`
	MIME            string  `json:"mime"`
	Bytes           int64   `json:"bytes"`
	TakenAt         *string `json:"taken_at"`
	TakenSrc        string  `json:"taken_src"`
	TakenIsFallback bool    `json:"taken_is_fallback"`
	TakenLabel      string  `json:"taken_label"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	Make            string  `json:"make"`
	Model           string  `json:"model"`
	Orientation     int     `json:"orientation"`
	ProbeState      string  `json:"probe_state"`
	Note            string  `json:"note"`
	GPS             *struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	} `json:"gps"`
	ThumbState  string `json:"thumb_state"`
	ThumbURL    string `json:"thumb_url"`
	RawURL      string `json:"raw_url"`
	Playable    bool   `json:"playable"`
	Placeholder bool   `json:"placeholder"`
	Missing     bool   `json:"missing"`
}

// TestThumbnailIsServedAtTheConfiguredBound checks the one thumbnail size the
// first version generates, and that it comes back as an image the browser can
// decode without the server touching the original again.
func TestThumbnailIsServedAtTheConfiguredBound(t *testing.T) {
	_, s, a := serveIndexed(t)

	id := a.idFor("photos/2020/big/DSC_0001.jpg")
	doc := a.item(id)
	if doc.ThumbState != "ok" {
		t.Fatalf("the 12 MP image has thumb_state %q after a thumbnail run, want ok", doc.ThumbState)
	}

	resp, body := s.get(doc.ThumbURL)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s returned %d", doc.ThumbURL, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/jpeg") {
		t.Errorf("thumbnail Content-Type is %q, want image/jpeg", ct)
	}
	if resp.Header.Get("Content-Length") != fmt.Sprint(len(body)) {
		t.Errorf("Content-Length %q does not match the %d bytes sent",
			resp.Header.Get("Content-Length"), len(body))
	}

	cfg, format, err := image.DecodeConfig(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("the thumbnail is not a decodable image: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("the thumbnail decoded as %q, want jpeg", format)
	}
	// The source is 4000x3000, so the longest edge is bounded to 512 and the
	// shape is preserved. A 4000-wide result would mean the full-size image
	// was sent.
	if cfg.Width != 512 {
		t.Errorf("thumbnail width is %d, want 512 (4000 scaled to the 512 bound)", cfg.Width)
	}
	if cfg.Height != 384 {
		t.Errorf("thumbnail height is %d, want 384; the aspect ratio must be preserved", cfg.Height)
	}
	if len(body) >= 291754 {
		t.Errorf("the thumbnail is %d bytes, no smaller than the %d byte original", len(body), 291754)
	}
}

// TestPortraitOrientationIsNotAutoRotated pins the current, documented
// behaviour rather than desirable behaviour: the stored orientation is
// reported to the client, and the client is what rotates. If this ever starts
// failing because the server rotates instead, that is a deliberate change and
// the metadata contract should be updated with it.
func TestPortraitOrientationIsNotAutoRotated(t *testing.T) {
	_, s, a := serveIndexed(t)

	id := a.idFor("photos/2019/paris/IMG_0007.jpg")
	doc := a.item(id)
	if doc.Orientation != 6 {
		t.Fatalf("orientation is %d, want the 6 recorded in the EXIF", doc.Orientation)
	}
	resp, body := s.get(doc.ThumbURL)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s returned %d", doc.ThumbURL, resp.StatusCode)
	}
	cfg, _, err := image.DecodeConfig(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("thumbnail is not decodable: %v", err)
	}
	// 1280x960 stored, longest edge 512 -> 512x384. A server-side rotation
	// would produce 384x512 and leave nothing for the client to do.
	if cfg.Width != 512 || cfg.Height != 384 {
		t.Errorf("thumbnail is %dx%d; the pixel data is stored unrotated and the orientation "+
			"field is what tells the client to turn it", cfg.Width, cfg.Height)
	}
}

// TestMissingThumbnailReturnsAPlaceholderImmediately is the core promise that
// keeps the service cheap: a request never decodes anything.
func TestMissingThumbnailReturnsAPlaceholderImmediately(t *testing.T) {
	r := newRig(t)
	r.indexAll()
	// No thumbnail run: nothing has a cached thumbnail.
	s := r.startServer()
	a := r.api(s)

	before, beforeBytes := r.countCacheFiles()

	// Pick a file that would decode perfectly well if anything tried.
	id := a.idFor("photos/2021/trip/IMG_0100.jpg")
	doc := a.item(id)
	if doc.ThumbState == "ok" {
		t.Fatalf("thumb_state is %q before any thumbnail run ran", doc.ThumbState)
	}
	if !doc.Placeholder {
		t.Error("the item does not report itself as a placeholder, so the client cannot tell " +
			"the difference between a real thumbnail and a stand-in")
	}

	started := time.Now()
	resp, body := s.get(doc.ThumbURL)
	elapsed := time.Since(started)

	if resp.StatusCode != 200 {
		t.Fatalf("GET %s returned %d, want a placeholder rather than an error", doc.ThumbURL, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Errorf("placeholder Content-Type is %q, want image/svg+xml", ct)
	}
	if !strings.Contains(string(body), "<svg") {
		t.Errorf("the placeholder is not SVG: %s", truncate(string(body), 200))
	}
	if elapsed > 2*time.Second {
		t.Errorf("the placeholder took %v; it must be produced without decoding the original",
			elapsed)
	}

	// The decisive check: serving the request must not have created any cache
	// file. A single HTTP request must never start decoding work.
	after, afterBytes := r.countCacheFiles()
	if after != before || afterBytes != beforeBytes {
		t.Errorf("a thumbnail request changed the cache from %d files/%d bytes to %d files/%d bytes; "+
			"the web service must not generate a thumbnail on demand",
			before, beforeBytes, after, afterBytes)
	}

	doc = a.item(id)
	if doc.ThumbState == "ok" {
		t.Error("the item now claims a cached thumbnail; the request must not have created one")
	}
}

// TestHostileEXIFIsSurvivedButNotTrusted draws the line that matters for a
// malformed metadata block. These three files are valid JPEGs carrying an APP1
// segment with an IFD that overflows its segment, declares 65535 entries, or
// points its next-IFD link at itself.
//
// The pixels in all three are fine, so refusing to thumbnail them would be
// wrong: a thumbnail is a decode of the image, and the image decodes. What must
// not happen is the *metadata* being believed. The test therefore checks both
// halves - the file is still browsable, and nothing invented from the hostile
// block (a date, a camera, coordinates) is published as fact.
func TestHostileEXIFIsSurvivedButNotTrusted(t *testing.T) {
	_, s, a := serveIndexed(t)

	for _, rel := range []string{
		"edge/exif_overflow.jpg",
		"edge/exif_many_entries.jpg",
		"edge/exif_loop.jpg",
	} {
		t.Run(rel, func(t *testing.T) {
			id := a.idFor(rel)
			doc := a.item(id)

			// The probe must not have crashed and must not have refused the file
			// for a metadata reason: it is a decodable image.
			if doc.Kind != "image" {
				t.Errorf("%s was classified %q; a valid JPEG is an image whatever its EXIF says",
					rel, doc.Kind)
			}
			if doc.ProbeState == "failed" {
				t.Errorf("%s failed to probe; a hostile EXIF block must be survived, not fatal", rel)
			}

			// A time is allowed here only as an explicitly labelled fallback to
			// the file's own timestamp. A value attributed to EXIF would have to
			// have been invented by the malformed block.
			switch doc.TakenSrc {
			case "", "none":
				if doc.TakenAt != nil {
					t.Errorf("%s has taken_at %q but no source for it", rel, *doc.TakenAt)
				}
			case "filetime":
				if doc.TakenAt == nil {
					t.Errorf("%s says the time came from the file but published none", rel)
				} else if !doc.TakenIsFallback {
					t.Errorf("%s took the time from the file (%q) without marking it as a fallback; "+
						"the interface must tell the operator it is not a capture time", rel, *doc.TakenAt)
				} else if !strings.Contains(doc.TakenLabel, "fallback") {
					t.Errorf("%s has taken_label %q, which does not say it is a fallback",
						rel, doc.TakenLabel)
				}
			case "exif":
				t.Errorf("%s attributed capture time %q to EXIF, but its EXIF block is malformed; "+
					"an unreadable value must not be guessed", rel, strOrNil(doc.TakenAt))
			default:
				t.Errorf("%s has unexpected taken_src %q", rel, doc.TakenSrc)
			}
			if doc.GPS != nil {
				t.Errorf("%s published coordinates from a malformed block", rel)
			}
			if doc.Make != "" || doc.Model != "" {
				t.Errorf("%s published camera %q/%q from a malformed block", rel, doc.Make, doc.Model)
			}
			if doc.Orientation != 0 && doc.Orientation != 1 {
				t.Errorf("%s published orientation %d from a malformed block; only a parsed "+
					"value may be reported, and unrotated is the only safe default", rel, doc.Orientation)
			}

			// It remains viewable: either a real thumbnail, or a placeholder
			// that explains itself. Both are 200 and neither is an error.
			resp, body := s.get(doc.ThumbURL)
			if resp.StatusCode != 200 {
				t.Fatalf("GET %s returned %d", doc.ThumbURL, resp.StatusCode)
			}
			isPlaceholder := strings.Contains(string(body), "<svg")
			if isPlaceholder && doc.ThumbState == "ok" {
				t.Errorf("%s has an ok thumbnail in the index but served a placeholder", rel)
			}
			if !isPlaceholder && t.Failed() == false && !strings.HasPrefix(
				resp.Header.Get("Content-Type"), "image/jpeg") {
				t.Errorf("%s served %q for a thumbnail", rel, resp.Header.Get("Content-Type"))
			}
			t.Logf("%s: probe=%s thumb_state=%s placeholder=%v w=%d h=%d",
				rel, doc.ProbeState, doc.ThumbState, isPlaceholder, doc.Width, doc.Height)
		})
	}
}

// TestUnsupportedAndCorruptFilesFallBackToThePlaceholder checks that files the
// thumbs worker refuses still produce a browsable entry rather than an error.
func TestUnsupportedAndCorruptFilesFallBackToThePlaceholder(t *testing.T) {
	_, s, a := serveIndexed(t)

	cases := []struct {
		rel    string
		reason string
	}{
		{"unsupported/photo.heic", "a format that is deliberately not enabled"},
		{"unsupported/raw.cr2", "a RAW format that is deliberately not enabled"},
		{"edge/bomb.png", "a PNG header declaring 3.6 gigapixels"},
		{"edge/broken.jpg", "a truncated JPEG"},
		{"edge/empty.jpg", "a zero byte file"},
	}
	for _, tc := range cases {
		t.Run(tc.rel, func(t *testing.T) {
			id := a.idFor(tc.rel)
			doc := a.item(id)
			if doc.ThumbState == "ok" {
				t.Fatalf("%s has a cached thumbnail; %s must never be published", tc.rel, tc.reason)
			}
			resp, body := s.get(doc.ThumbURL)
			if resp.StatusCode != 200 {
				t.Fatalf("GET %s returned %d; %s must still display a placeholder",
					doc.ThumbURL, resp.StatusCode, tc.rel)
			}
			if !strings.Contains(string(body), "<svg") {
				t.Errorf("the response for %s is not a placeholder SVG", tc.rel)
			}
			// The original must remain fetchable so the operator can inspect
			// it, even when it cannot be decoded for display.
			raw, _ := s.get(fmt.Sprintf("/api/v1/media/%d/raw", id))
			if raw.StatusCode != 200 {
				t.Errorf("the original of %s is not reachable (status %d); an unreadable-by-the-decoder "+
					"file must still be downloadable", tc.rel, raw.StatusCode)
			}
		})
	}
}

// TestOriginalIsStreamedWithCorrectHeaders covers the read path for a whole
// file.
func TestOriginalIsStreamedWithCorrectHeaders(t *testing.T) {
	_, s, a := serveIndexed(t)

	id := a.idFor("video/streamable.mp4")
	resp, body := s.get(fmt.Sprintf("/api/v1/media/%d/raw", id))
	if resp.StatusCode != 200 {
		t.Fatalf("GET raw returned %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != fmt.Sprint(len(body)) {
		t.Errorf("Content-Length is %q but %d bytes arrived", got, len(body))
	}
	if len(body) != 3145728 {
		t.Errorf("received %d bytes, want the full 3145728 byte file", len(body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "video/") {
		t.Errorf("Content-Type is %q, want a video type from the extension allowlist", ct)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Error("Accept-Ranges is not advertised, so a player will not know it can seek")
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options is %q, want nosniff: the declared type must be the one used",
			resp.Header.Get("X-Content-Type-Options"))
	}
	if et := resp.Header.Get("ETag"); et == "" {
		t.Error("no ETag was sent, so a re-request cannot be revalidated")
	}
	// The download variant must not be served inline.
	dl, _ := s.get(fmt.Sprintf("/api/v1/media/%d/download", id))
	if got := dl.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("download Content-Disposition is %q, want attachment", got)
	}
}

// TestRangeRequestsAreServedCorrectly is the video-seek promise: seeking must
// not require the whole file.
func TestRangeRequestsAreServedCorrectly(t *testing.T) {
	_, s, a := serveIndexed(t)
	id := a.idFor("video/streamable.mp4")
	url := fmt.Sprintf("%s/api/v1/media/%d/raw", s.Base, id)

	const size = 3145728

	get := func(t *testing.T, rangeHeader string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Range", rangeHeader)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("range request %q: %v", rangeHeader, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			t.Fatalf("reading range body: %v", err)
		}
		return resp, body
	}

	t.Run("a prefix", func(t *testing.T) {
		resp, body := get(t, "bytes=0-1023")
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status is %d, want 206", resp.StatusCode)
		}
		if len(body) != 1024 {
			t.Errorf("received %d bytes, want 1024", len(body))
		}
		if got := resp.Header.Get("Content-Range"); got != fmt.Sprintf("bytes 0-1023/%d", size) {
			t.Errorf("Content-Range is %q, want bytes 0-1023/%d", got, size)
		}
		if got := resp.Header.Get("Content-Length"); got != "1024" {
			t.Errorf("Content-Length is %q, want 1024", got)
		}
	})

	t.Run("a middle window", func(t *testing.T) {
		resp, body := get(t, "bytes=1048576-1049599")
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status is %d, want 206", resp.StatusCode)
		}
		if len(body) != 1024 {
			t.Errorf("received %d bytes, want 1024: a seek must transfer only the window asked for, "+
				"not the file up to that offset", len(body))
		}
		if got := resp.Header.Get("Content-Range"); got != fmt.Sprintf("bytes 1048576-1049599/%d", size) {
			t.Errorf("Content-Range is %q", got)
		}
	})

	t.Run("an open ended range", func(t *testing.T) {
		resp, body := get(t, fmt.Sprintf("bytes=%d-", size-512))
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status is %d, want 206", resp.StatusCode)
		}
		if len(body) != 512 {
			t.Errorf("received %d bytes, want the last 512", len(body))
		}
		if got := resp.Header.Get("Content-Range"); got != fmt.Sprintf("bytes %d-%d/%d", size-512, size-1, size) {
			t.Errorf("Content-Range is %q", got)
		}
	})

	t.Run("a suffix range", func(t *testing.T) {
		resp, body := get(t, "bytes=-256")
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status is %d, want 206", resp.StatusCode)
		}
		if len(body) != 256 {
			t.Errorf("received %d bytes, want the final 256", len(body))
		}
	})

	t.Run("a range past the end", func(t *testing.T) {
		resp, body := get(t, fmt.Sprintf("bytes=%d-%d", size+10, size+100))
		if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("status is %d, want 416", resp.StatusCode)
		}
		if !strings.Contains(resp.Header.Get("Content-Range"), "bytes */") {
			t.Errorf("a 416 must still report the total size; Content-Range is %q",
				resp.Header.Get("Content-Range"))
		}
		_ = body
	})

	t.Run("HEAD", func(t *testing.T) {
		req, _ := http.NewRequest("HEAD", url, nil)
		req.Header.Set("Range", "bytes=0-99")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if len(body) != 0 {
			t.Errorf("HEAD returned a %d byte body", len(body))
		}
		if resp.Header.Get("Content-Range") == "" {
			t.Error("HEAD did not describe the range; a player probing for seek support would not learn it")
		}
	})
}

// TestRangeWorksForTheLargestImageToo makes sure the same path is used for
// images, so a large photo is not loaded into memory to display it.
func TestRangeWorksForTheLargestImageToo(t *testing.T) {
	_, s, a := serveIndexed(t)
	id := a.idFor("photos/2020/big/DSC_0001.jpg")
	url := fmt.Sprintf("%s/api/v1/media/%d/raw", s.Base, id)

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Range", "bytes=0-511")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status is %d, want 206", resp.StatusCode)
	}
	if len(body) != 512 {
		t.Errorf("received %d bytes, want 512", len(body))
	}
}

// TestCraftedNamesAreInertInJSONAndOnDisk is the XSS and integrity test. The
// fixture names include HTML, a script tag, quotes, newlines, a tab, a control
// character and pre-escaped entities.
//
// The contract has two halves, and both are checked here: the transport keeps a
// name as data (valid JSON, byte-for-byte round trip, no rewriting), and the
// renderer never parses HTML, so a name can only ever become text.
func TestCraftedNamesAreInertInJSONAndOnDisk(t *testing.T) {
	r, s, a := serveIndexed(t)

	// The raw bytes on the wire must be valid JSON. A name containing a quote,
	// a newline or a tab breaks hand-built JSON, so this is a real check of
	// escaping rather than of the decoded value.
	resp, body := s.get("/api/v1/media?collection=smoke&limit=200")
	if resp.StatusCode != 200 {
		t.Fatalf("listing returned %d", resp.StatusCode)
	}
	var decoded mediaPage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("the listing is not valid JSON, so a crafted name was not escaped: %v", err)
	}
	// A raw, unescaped control byte inside a JSON string would be a protocol
	// violation; the encoder must have turned it into \uXXXX.
	if bytes.ContainsAny(body, "\x00\x01\x02\x1b") {
		t.Error("the raw listing contains a control byte; it must be escaped as \\uXXXX")
	}
	if !bytes.Contains(body, []byte(`\u000a`)) {
		t.Log("note: no \\u000a escape seen; the fixture's newline name may sort past this page")
	}

	// Every name must survive the round trip byte for byte. Rewriting a name in
	// transport would corrupt what is displayed and what a download is called.
	for _, page := range a.allMedia("smoke") {
		for _, it := range page.Items {
			if it.FileName == "" {
				t.Error("a media row has an empty file name")
				continue
			}
			doc := a.item(it.ID)
			if doc.Name != it.FileName {
				t.Errorf("the item endpoint returned name %q for the listing's %q; the name must not "+
					"change between the two endpoints", doc.Name, it.FileName)
			}
		}
	}

	// The injection attempt is transported verbatim as a JSON string, which is
	// inert. The check is that it is exactly that - not mangled, and not
	// rendered.
	injected := "<img src=x onerror=alert(1)>.jpg"
	id := a.idFor("names/" + injected)
	doc := a.item(id)
	if doc.Name != injected {
		t.Errorf("the name came back as %q, want the original %q byte for byte", doc.Name, injected)
	}
	if strings.Contains(string(body), "<script") && strings.Contains(string(body), "alert") {
		t.Log("note: the script-tag name is present in the raw JSON, as string data; that is expected")
	}

	// A search response builds its own WHERE clause from user text; the payload
	// must not break it, and the response must still be valid JSON.
	sresp, sbody := s.get("/api/v1/media?collection=smoke&q=alert")
	if sresp.StatusCode != 200 {
		t.Fatalf("search returned %d", sresp.StatusCode)
	}
	if err := json.Unmarshal(sbody, &decoded); err != nil {
		t.Fatalf("the search response is not valid JSON: %v", err)
	}
	// A search is a literal substring match, never SQL and never LIKE syntax.
	// The comparison is exact and is made against the fixture, not against
	// zero: the check that a query for "_" matches only the media whose name
	// contains an underscore - and not one of them more - is what catches an
	// unescaped LIKE wildcard, which would quietly turn a literal search into
	// match-everything.
	indexed := r.indexedNames(t)
	for _, q := range []string{"%", "_", "'", "\\", ";", "''", "DROP TABLE media; --"} {
		want := 0
		for _, name := range indexed {
			if strings.Contains(strings.ToLower(name), strings.ToLower(q)) {
				want++
			}
		}
		rr, rb := s.get("/api/v1/media?collection=smoke&limit=200&q=" + urlQueryEscape(q))
		if rr.StatusCode != 200 {
			t.Errorf("searching for %q returned %d; it must be treated as literal text", q, rr.StatusCode)
			continue
		}
		var page mediaPage
		if err := json.Unmarshal(rb, &page); err != nil {
			t.Errorf("searching for %q did not return JSON: %v", q, err)
			continue
		}
		if len(page.Items) != want {
			got := make([]string, 0, len(page.Items))
			for _, it := range page.Items {
				got = append(got, it.FileName)
			}
			t.Errorf("searching for the literal %q matched %d media, want %d (the fixture files "+
				"whose name contains it): %v", q, len(page.Items), want, got)
		}
	}
	// Query length is refused, not trimmed. A silent trim would answer a
	// question nobody asked, so a search longer than the index's limit must be
	// a clear 400, and a search just under it must be searched in full.
	over := strings.Repeat("z", 500)
	rr2, rb2 := s.get("/api/v1/media?collection=smoke&q=" + over)
	if rr2.StatusCode != 400 {
		t.Errorf("a %d character query returned %d, want 400", len(over), rr2.StatusCode)
	} else if !bytes.Contains(bytes.ToLower(rb2), []byte("limit")) {
		t.Errorf("the refusal does not say what the limit is: %s", strings.TrimSpace(string(rb2)))
	}
	under := strings.Repeat("z", 200)
	if rr3, _ := s.get("/api/v1/media?collection=smoke&q=" + under); rr3.StatusCode != 200 {
		t.Errorf("a %d character query returned %d; it is within the limit and must be searched",
			len(under), rr3.StatusCode)
	}

	// And the renderer must never parse HTML. This is the other half of the
	// XSS defence and the reason transporting a raw name is safe.
	_, appjs := s.get("/app.js")
	// The check is on the sink form used at a call or assignment site
	// (".innerHTML" as a property access), not on the bare word: a comment
	// saying "never innerHTML" is the opposite of a defect.
	code := stripComments(string(appjs))
	for _, sink := range []string{".innerHTML", ".outerHTML", ".insertAdjacentHTML",
		"document.write(", "createContextualFragment", "eval("} {
		if strings.Contains(code, sink) {
			t.Errorf("the frontend uses %s; a name from the disk would then be able to introduce markup", sink)
		}
	}
	if !bytes.Contains(appjs, []byte("textContent")) {
		t.Error("the frontend never uses textContent, so it must build the DOM some other way; " +
			"that needs to be checked against the no-markup rule")
	}

	// The file on disk must be untouched: nothing here may rewrite a name.
	full := filepath.Join(r.Media, "names", injected)
	if _, err := os.Stat(full); err != nil {
		t.Errorf("the original file is no longer at its recorded name: %v", err)
	}
}

// stripComments removes // line comments and /* */ blocks so that a rule such
// as "never use innerHTML" can be stated in a comment without tripping a check
// for the word itself.
func strOrNil(p *string) string {
	if p == nil {
		return "(none)"
	}
	return *p
}

func stripComments(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "//"):
			if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
				i += j
			} else {
				i = len(s)
			}
		case strings.HasPrefix(s[i:], "/*"):
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				i = len(s)
			} else {
				i += j + 4
			}
		default:
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}

func urlQueryEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// TestPathParametersCannotEscapeTheRoot feeds every path-shaped parameter a
// traversal attempt and checks that only controlled values are ever resolved.
func TestPathParametersCannotEscapeTheRoot(t *testing.T) {
	_, s, a := serveIndexed(t)
	id := a.idFor("photos/2021/trip/IMG_0100.jpg")

	attempts := []string{
		"../config.toml",
		"..%2f..%2fconfig.toml",
		"/etc/passwd",
		"%2fetc%2fpasswd",
		"....//....//etc/passwd",
		"photos/../../config.toml",
		".",
		"..",
		"%00",
		"\\\\etc\\\\passwd",
	}
	for _, at := range attempts {
		t.Run(at, func(t *testing.T) {
			// A traversal in the query string must not select a file outside
			// the collection.
			for _, q := range []string{"path=", "file=", "root=", "dir="} {
				resp, body := s.get("/api/v1/media?collection=smoke&" + q + at)
				if resp.StatusCode == 200 {
					// Any 200 is acceptable only if it is the ordinary listing
					// of indexed media, which the traversal cannot influence.
					if strings.Contains(string(body), "root:") {
						t.Fatalf("query parameter %s%s returned the contents of a file outside the "+
							"collection: %s", q, at, truncate(string(body), 200))
					}
				}
			}
			// A traversal in the id position must be rejected outright.
			resp, _ := s.get("/api/v1/media/" + at + "/raw")
			if resp.StatusCode == 200 {
				t.Errorf("the traversal %q in the id position was served", at)
			}
		})
	}

	// The real file is still served, so the refusals above are not just a
	// blanket denial.
	resp, body := s.get(fmt.Sprintf("/api/v1/media/%d/raw", id))
	if resp.StatusCode != 200 || len(body) == 0 {
		t.Fatalf("the legitimate original is not reachable (status %d, %d bytes)",
			resp.StatusCode, len(body))
	}
}

// TestMalformedIdentifiersAreRejected checks the boundary around the id
// parameter, which is the only user-controlled value that reaches a filesystem
// lookup.
func TestMalformedIdentifiersAreRejected(t *testing.T) {
	_, s, a := serveIndexed(t)
	// A real file that exists on disk but is deliberately not indexed: the
	// service must never serve it by guessing a name.
	id := a.idFor("photos/2021/trip/IMG_0100.jpg")

	cases := []struct {
		path string
		want int
	}{
		// "0" and every zero-padded or signed spelling of a real id are bad
		// requests, not lookups that miss: this service mints exactly one
		// spelling per row, so "0", "007" and "+7" are all refused before the
		// index is consulted.
		{"/api/v1/media/0", 400},
		{"/api/v1/media/007", 400},
		{"/api/v1/media/1_0", 400},
		{"/api/v1/media/%2B1", 400},
		{"/api/v1/media/-1", 400},
		{"/api/v1/media/abc", 400},
		{"/api/v1/media/99999999", 404},
		{"/api/v1/media/1.5", 400},
		{"/api/v1/media/0x10", 400},
		{"/api/v1/media/ 1", 400},
		{"/api/v1/media/+1", 400},
		{"/api/v1/media/9223372036854775808", 400},
	}
	for _, tc := range cases {
		resp, _ := s.get(tc.path)
		if resp.StatusCode != tc.want {
			t.Errorf("GET %s returned %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
	}

	for _, suffix := range []string{"/raw", "/thumb", "/download"} {
		resp, _ := s.get(fmt.Sprintf("/api/v1/media/%d%s", id, suffix))
		if resp.StatusCode != 200 {
			t.Errorf("GET %s returned %d for a legitimate id", suffix, resp.StatusCode)
		}
	}
}

// TestListingIsBoundedAndPaginates checks that paging never returns the whole
// library at once and that the cursor protocol terminates.
func TestListingIsBoundedAndPaginates(t *testing.T) {
	_, s, a := serveIndexed(t)

	// A requested page larger than the configured maximum must be clamped, not
	// honoured.
	var big mediaPage
	if code := s.getJSON("/api/v1/media?collection=smoke&limit=100000", &big); code != 200 {
		t.Fatalf("an oversized limit returned %d", code)
	}
	status := a.status()
	if len(big.Items) > status.Totals.Media {
		t.Errorf("a page of %d items was returned for a library of %d", len(big.Items), status.Totals.Media)
	}
	if len(big.Items) > 1000 {
		t.Errorf("a single page returned %d items; the response must be bounded", len(big.Items))
	}

	// Every item must be reachable, and no cursor may repeat.
	seen := map[int64]bool{}
	pages := a.allMedia("smoke")
	for _, p := range pages {
		for _, it := range p.Items {
			if seen[it.ID] {
				t.Errorf("media %d was returned on more than one page", it.ID)
			}
			seen[it.ID] = true
		}
	}
	if len(seen) != status.Totals.Media {
		t.Errorf("paging returned %d distinct items but the status reports %d media; "+
			"files would be unreachable in the browser", len(seen), status.Totals.Media)
	}
	if len(pages) > 1 && len(pages[0].Items) == 0 {
		t.Error("a page was returned with no items but with a cursor, which would loop")
	}
}

// TestStatusIsServedWithoutTouchingTheCache checks that the endpoint the UI
// polls is cheap and does not start any work.
func TestStatusIsServedWithoutTouchingTheCache(t *testing.T) {
	r, _, a := serveIndexed(t)
	before, beforeBytes := r.countCacheFiles()

	for i := 0; i < 5; i++ {
		started := time.Now()
		_ = a.status()
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("status took %v; it must return without waiting for any media work", elapsed)
		}
	}

	after, afterBytes := r.countCacheFiles()
	if after != before || afterBytes != beforeBytes {
		t.Errorf("polling status changed the cache from %d/%d to %d/%d", before, beforeBytes, after, afterBytes)
	}

	// The status must describe the collection's mount, because the UI has to
	// be able to say when a share is not mounted.
	doc := a.status()
	if len(doc.Collections) == 0 {
		t.Fatal("status reports no collections")
	}
	if !doc.Collections[0].MountOK {
		t.Error("the configured mount is reported as unavailable")
	}
	if !doc.Resource.ReadOnly {
		t.Error("the service does not report itself as read-only, so an operator cannot confirm " +
			"from outside that it holds a write-refusing index handle")
	}
}

// TestTheEmbeddedFrontendIsServedWithoutExternalReferences checks that the
// page loads from the binary alone and pulls nothing from the network.
func TestTheEmbeddedFrontendIsServedWithoutExternalReferences(t *testing.T) {
	_, s, _ := serveIndexed(t)

	resp, body := s.get("/")
	if resp.StatusCode != 200 {
		t.Fatalf("GET / returned %d", resp.StatusCode)
	}
	html := string(body)
	if !strings.Contains(html, "<html") {
		t.Fatalf("the root path is not an HTML document: %s", truncate(html, 200))
	}

	for _, asset := range []string{"/app.css", "/app.js"} {
		ar, _ := s.get(asset)
		if ar.StatusCode != 200 {
			t.Errorf("GET %s returned %d; the embedded asset is missing", asset, ar.StatusCode)
		}
	}

	// Nothing may be fetched from the network: no analytics, no remote fonts,
	// no map tiles, no CDN. This is a security boundary as much as an offline
	// requirement.
	var everything strings.Builder
	everything.WriteString(html)
	for _, asset := range []string{"/app.css", "/app.js"} {
		_, b := s.get(asset)
		everything.Write(b)
	}
	page := everything.String()
	// Any absolute URL in a load position is a network dependency. The
	// frontend also has no <img src=...> or <link href=...> pointing off-host.
	for _, bad := range []string{"http://", "https://", "url(http", "@import", "srcset="} {
		if strings.Contains(page, bad) {
			t.Errorf("the frontend contains %q; the first version must load entirely from the "+
				"binary and contact nothing", bad)
		}
	}
	for _, bad := range []string{".innerHTML", "insertAdjacentHTML", "eval("} {
		if strings.Contains(stripComments(page), bad) {
			t.Errorf("the served frontend contains %q", bad)
		}
	}
	// A missing external reference is only a problem if the page also fails to
	// render, so the shell must be self-contained.
	if !strings.Contains(page, "albumctl") {
		t.Error("the served frontend does not identify itself")
	}
}

// TestPauseIsHonouredByBothTheWorkerAndTheService is the control-path
// integration test: the state a browser can see must agree with the state the
// worker obeys.
func TestPauseIsHonouredByBothTheWorkerAndTheService(t *testing.T) {
	r := newRig(t)
	r.indexAll()
	r.mustRun("pause", "--reason", "operator is inspecting the machine")

	s := r.startServer()
	a := r.api(s)
	if doc := a.status(); !doc.Paused {
		t.Error("the service does not report the persisted pause")
	} else if doc.PauseReason == "" {
		t.Error("the pause has no reason recorded, so the UI cannot say why nothing is happening")
	}

	// A thumbnail run under pause must decline to do anything, and must say so
	// rather than failing.
	out, _, code := r.run("thumbs", "--collection", "smoke", "--limit", "5", "--max-runtime", "30s")
	if code == 0 {
		t.Errorf("a thumbnail run under pause exited 0; it must refuse. Output:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "pause") {
		t.Errorf("the refusal does not mention the pause:\n%s", out)
	}
	if n, _ := r.countCacheFiles(); n != 0 {
		t.Errorf("%d cache files exist after a paused run; pause must stop the worker before it "+
			"touches anything", n)
	}

	// Requests must still be served: pause stops work, not reading.
	id := a.idFor("photos/2021/trip/IMG_0100.jpg")
	resp, _ := s.get(fmt.Sprintf("/api/v1/media/%d/raw", id))
	if resp.StatusCode != 200 {
		t.Errorf("reading an original returned %d while paused; pause must not stop the read path",
			resp.StatusCode)
	}
	// And the pause must survive a restart of the process, which is what makes
	// it a persistent state rather than a flag in memory.
	s.stop()
	r.mustRun("resume")
	s2 := r.startServer()
	if doc := r.api(s2).status(); doc.Paused {
		t.Error("resume did not clear the pause")
	}
}

// TestConcurrentRequestsAreBounded checks the file descriptor and buffer
// budget: streaming is capped, and exceeding it is reported rather than
// queued.
func TestConcurrentRequestsAreBounded(t *testing.T) {
	_, s, a := serveIndexed(t)
	id := a.idFor("video/streamable.mp4")
	url := fmt.Sprintf("%s/api/v1/media/%d/raw", s.Base, id)

	// Open many requests but read nothing, so each holds its slot. The cap
	// must reject rather than let an unbounded number of files stay open.
	type held struct {
		resp *http.Response
		err  error
	}
	results := make(chan held, 64)
	for i := 0; i < 64; i++ {
		go func() {
			req, _ := http.NewRequest("GET", url, nil)
			req.Header.Set("Range", "bytes=0-15")
			resp, err := http.DefaultClient.Do(req)
			results <- held{resp, err}
		}()
	}
	var ok, busy int
	for i := 0; i < 64; i++ {
		h := <-results
		if h.err != nil {
			busy++
			continue
		}
		switch h.resp.StatusCode {
		case 200, 206:
			ok++
		case 503:
			busy++
			if h.resp.Header.Get("Retry-After") == "" {
				t.Error("a 503 did not send Retry-After, so a client cannot back off sensibly")
			}
		default:
			t.Errorf("unexpected status %d", h.resp.StatusCode)
		}
		h.resp.Body.Close()
	}
	if ok == 0 {
		t.Error("every concurrent request was refused; the cap is too small to be usable")
	}
	t.Logf("%d concurrent requests: %d served, %d shed", ok+busy, ok, busy)

	// After the burst the service must still work.
	resp, _ := s.get(fmt.Sprintf("/api/v1/media/%d/raw", id))
	if resp.StatusCode != 200 {
		t.Errorf("the service did not recover after the burst (status %d)", resp.StatusCode)
	}
}

// TestUnindexedFilesAreNotServed checks that the service serves what the index
// knows rather than what happens to be on disk: the media tree contains files
// that a scan deliberately ignores.
func TestUnindexedFilesAreNotServed(t *testing.T) {
	r, _, a := serveIndexed(t)

	// These exist in the fixture but must not appear in the index.
	for _, rel := range []string{
		"photos/2021/trip/IMG_0100.jpg.tmp",
		"photos/notes.txt",
		"photos/thumbs.db",
	} {
		name := filepath.Base(rel)
		for _, page := range a.allMedia("smoke") {
			for _, it := range page.Items {
				if it.FileName == name {
					t.Errorf("%s was indexed; non-media and in-progress files must be left alone", rel)
				}
			}
		}
	}

	doc := a.status()
	if doc.Totals.Media == 0 {
		t.Fatal("nothing was indexed at all, so this test proves nothing")
	}

	// The exact size of the index is asserted, with every exclusion named. A
	// test that only checked "more than zero" would not notice a whole class of
	// file disappearing from the library.
	//
	// A file is indexed when its extension is one the walker recognises, it is
	// not one of our own temporary artifacts, and its name is not hidden. The
	// fixture has 39 files, of which exactly 5 are excluded:
	excluded := map[string]string{
		"photos/2021/trip/IMG_0100.jpg.tmp": "a partially written file, recognisable by its extension",
		"photos/notes.txt":                  "not a media extension",
		"photos/thumbs.db":                  "not a media extension",
		"names/.hidden.jpg": "a hidden file: SMB and camera shares are full of dot-junk, " +
			"and a dot-prefixed name is the one signal that reliably means 'not a photo of mine'",
		"names/no_extension": "no extension at all, so its type is a guess",
		".albumctl-partial":  "one of our own interrupted-write artifacts, with no extension",
	}
	// Basenames are unique in this fixture, so comparing them identifies a
	// missing file precisely without depending on how the API splits a path.
	want := 0
	kept := map[string]string{}
	for _, f := range r.Library.Files {
		if reason, ok := excluded[f.Rel]; ok {
			t.Logf("excluded %s: %s", f.Rel, reason)
			continue
		}
		want++
		kept[filepath.Base(f.Rel)] = f.Rel
	}
	indexed := map[string]bool{}
	for _, page := range a.allMedia("smoke") {
		for _, it := range page.Items {
			indexed[it.FileName] = true
		}
	}
	for base, rel := range kept {
		if !indexed[base] {
			t.Errorf("%s is missing from the index", rel)
		}
	}
	if doc.Totals.Media != want {
		t.Errorf("the index holds %d media but %d of the fixture's %d files should be indexed; "+
			"a class of file is being dropped or added silently",
			doc.Totals.Media, want, len(r.Library.Files))
	}

	// Every exclusion must actually be absent from the listing, so the table
	// cannot drift out of step with the walker.
	listed := map[string]bool{}
	for _, page := range a.allMedia("smoke") {
		for _, it := range page.Items {
			listed[it.FileName] = true
		}
	}
	for rel := range excluded {
		if name := filepath.Base(rel); listed[name] {
			t.Errorf("%s is in the exclusion table but was indexed anyway", rel)
		}
	}
}

// TestADeletedFileDisappearsFromTheNextScanImmediately is the regression test
// for a bug that made the "missing" feature useless in exactly the situation it
// is used in.
//
// A deleted file was only marked missing if the previous scan's last_seen was
// strictly older than this scan's start, both recorded in whole seconds. Two
// scans inside the same second therefore could not tell "seen by this run" from
// "seen by the previous run", and the deleted file stayed on the timeline as a
// frame with no original behind it. An operator checking that a delete took
// effect, or two scans in one batch, hits this immediately - so this test does
// not sleep, and asserts on the second scan in the same second.
//
// It also checks the two halves of the visible behaviour: the row must leave the
// default gallery, and the row itself must survive so that a mount that came back
// can be told apart from a file that was genuinely removed. Nothing here may
// delete an index row.
func TestADeletedFileDisappearsFromTheNextScanImmediately(t *testing.T) {
	r := newRig(t)

	// Index first, then start the service: a service with no index refuses to
	// serve rather than inventing an empty gallery, and that is worth keeping.
	r.indexAll()
	s := r.startServer()
	a := r.api(s)

	// Pick a file the scanner really indexed, from the fixture inventory rather
	// than from the index.
	victim := filepath.Join(r.Media, "photos", "2019", "paris", "IMG_0007.jpg")
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("the fixture file this test deletes is not there: %v", err)
	}
	before := a.status()
	if before.Totals.Missing != 0 {
		t.Fatalf("a fresh index reports %d missing files before anything was deleted",
			before.Totals.Missing)
	}
	mediaBefore := before.Totals.Media
	if mediaBefore == 0 {
		t.Fatal("the fixture library indexed nothing, so this test proves nothing")
	}

	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}

	// No sleep on purpose. This is the whole point of the test.
	out := r.mustRun("scan", "--collection", "smoke", "--limit", "5000")
	if !strings.Contains(out, "marked gone") {
		t.Errorf("the second scan does not report what it marked; output:\n%s", out)
	}
	if strings.Contains(out, "marked gone:  none") {
		t.Errorf("the second scan did not treat itself as a complete walk; output:\n%s", out)
	}

	after := a.status()
	if after.Totals.Missing != 1 {
		t.Errorf("after deleting one indexed file the index reports %d missing, want 1\n%s",
			after.Totals.Missing, out)
	}
	if after.Totals.Media != mediaBefore {
		t.Errorf("the row count changed from %d to %d; a missing original must never delete a row",
			mediaBefore, after.Totals.Media)
	}

	// The default listing hides it: offering a frame whose original cannot be
	// read is how the gallery shows broken images.
	shown := map[string]bool{}
	for _, p := range a.allMedia("smoke") {
		for _, it := range p.Items {
			shown[it.FileName] = true
			if it.Missing {
				t.Errorf("%q is flagged missing yet still appears in the default listing", it.FileName)
			}
		}
	}
	if shown["IMG_0007.jpg"] {
		t.Error("the deleted file is still offered by the default listing")
	}
	if !shown["DSC_0001.jpg"] {
		t.Error("the default listing lost unrelated files as well, which would mean the walk " +
			"marked more than the one deleted file")
	}

	// Asking for it explicitly shows the row and its flag, which is what an
	// operator needs when a share fails to mount: the files are not gone, the
	// mount is.
	var found bool
	for _, p := range a.allMedia("smoke&include_missing=1") {
		for _, it := range p.Items {
			if it.FileName == "IMG_0007.jpg" {
				found = true
				if !it.Missing {
					t.Error("IMG_0007.jpg is in the index but is not flagged missing")
				}
			}
		}
	}
	if !found {
		t.Error("the row for the deleted file was removed from the index; it must be kept and " +
			"flagged, so that a missing mount can be distinguished from a real deletion")
	}
}

// TestTheRunStopsAtTheCacheCeiling is the cache's bound, exercised the only way
// it can be: by making it reachable. The requirement is that the cache has a
// ceiling at all, and the failure it prevents is a cache that grows until the
// system SSD holding the index is full - which on the target also holds the
// operating system.
//
// Two ceilings are used. One is so small that no file can be attempted; the
// other admits a couple of files and then stops, which is the case that matters:
// the ceiling stops the run part-way, it does not have to be hit by luck at a
// file boundary.
func TestTheRunStopsAtTheCacheCeiling(t *testing.T) {
	t.Run("a ceiling too small for any file stops before the first", func(t *testing.T) {
		r := newRig(t)
		r.cacheMaxBytes = 1
		r.writeConfig("")
		r.indexAll()

		out, _, code := r.run("thumbs", "--collection", "smoke", "--limit", "5",
			"--max-runtime", "1m", "--allow-no-cgroup")

		if !strings.Contains(out, "cache_full") {
			t.Errorf("the run did not report the cache ceiling as its stop reason:\n%s", out)
		}
		if code != 4 {
			t.Errorf("exit code = %d, want 4 (a resource ceiling stopped the run)", code)
		}
		files, bytes := r.countCacheFiles()
		if files != 0 || bytes != 0 {
			t.Errorf("the cache holds %d file(s) / %d bytes; a run that could not start must "+
				"not have written anything", files, bytes)
		}

		// Nothing was lost or marked done: the files are still pending.
		doc := r.api(r.startServer()).status()
		if doc.Totals.ThumbDue == 0 {
			t.Error("every file was recorded as done even though nothing was processed")
		}
	})

	t.Run("a file-count ceiling stops the run part-way", func(t *testing.T) {
		r := newRig(t)
		// The byte ceiling is generous here; what is under test is the other
		// half of the same guarantee. A cache ceiling has to hold for the number
		// of files as well as for their size, because tens of thousands of small
		// files fill an SSD just as effectively as a few large ones.
		r.cacheMaxFiles = 3
		r.writeConfig("")
		r.indexAll()

		out, _, code := r.run("thumbs", "--collection", "smoke", "--limit", "20",
			"--max-runtime", "1m", "--allow-no-cgroup")

		if !strings.Contains(out, "cache_full") {
			t.Errorf("the file ceiling was reached but not reported as the stop reason:\n%s", out)
		}
		if code != 4 {
			t.Errorf("exit code = %d, want 4", code)
		}
		files, bytes := r.countCacheFiles()
		if files != 3 {
			t.Errorf("the cache holds %d file(s), want exactly the ceiling of 3: the check must "+
				"run before the write", files)
		}
		if bytes > r.cacheMaxBytes {
			t.Errorf("the cache holds %d bytes, above max_bytes %d", bytes, r.cacheMaxBytes)
		}
		t.Logf("stopped with %d cached file(s), %d bytes", files, bytes)
	})

	t.Run("a byte ceiling below one thumbnail stops before the first", func(t *testing.T) {
		// The estimate per thumbnail is a generous upper bound, so a ceiling that
		// admits no file at all is a ceiling below that estimate. This is the
		// safety direction being tested: when in doubt the run stops rather than
		// writing and then discovering it went over.
		r := newRig(t)
		r.cacheMaxBytes = 1024
		r.writeConfig("")
		r.indexAll()

		out, _, code := r.run("thumbs", "--collection", "smoke", "--limit", "5",
			"--max-runtime", "1m", "--allow-no-cgroup")
		if !strings.Contains(out, "cache_full") {
			t.Errorf("stop reason was not the cache ceiling:\n%s", out)
		}
		if code != 4 {
			t.Errorf("exit code = %d, want 4", code)
		}
		if files, bytes := r.countCacheFiles(); files != 0 || bytes != 0 {
			t.Errorf("the cache holds %d file(s) / %d bytes, want nothing written", files, bytes)
		}
	})
}
