package hubweb

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// handleFileDownload streams a file as an attachment.
//
// This is separate from preview, and the separation matters:
//
//   - A preview is for *looking at* something in the page. It is capped for
//     images (a browser renders an image whole, so a huge one would be pulled
//     through the page) and refuses types the browser cannot display.
//   - A download is for *taking* a file. There is no cap, because the user asked
//     for the file and the console streams it with a bounded buffer - the cost to
//     this process is the same for 4 KiB and 4 GiB. There is no type restriction,
//     because a download does not ask the browser to interpret anything.
//
// Collapsing the two into one endpoint is what produced the bug this replaces:
// the UI offered a Download link that returned 415 for a text file and 413 for a
// large image, so the button failed for exactly the files somebody would want to
// download.
//
// Range support is inherited from http.ServeContent, so a client can resume an
// interrupted transfer rather than starting over.
func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	if s.Files == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage",
			"no storage roots are configured")
		return
	}
	root, path := r.URL.Query().Get("root"), r.URL.Query().Get("path")
	if root == "" || path == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"the root and path parameters are required")
		return
	}

	// A transfer occupies one of the few stream slots, and it is held for the
	// whole transfer: this is the endpoint a user points a browser at and walks
	// away from, so it must not be able to consume every slot and starve the UI.
	select {
	case s.streamSlots <- struct{}{}:
		defer func() { <-s.streamSlots }()
	case <-r.Context().Done():
		return
	default:
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusServiceUnavailable, "busy",
			"too many transfers are in progress; try again shortly")
		return
	}

	f, fi, err := s.Files.Open(root, path)
	if err != nil {
		status, code := mapFileErr(err)
		s.fail(w, status, code, err)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition(fi.Name()))
	// The filename in the query is not echoed into any header other than the
	// disposition built above, and that one is escaped; see contentDisposition.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// contentDisposition builds a Content-Disposition value for a filename that came
// from a filesystem the console does not control.
//
// A filename is attacker-influenced data - it can contain a quote, a backslash, a
// newline or a non-ASCII byte sequence - and it is being placed in a response
// header. Writing it raw would let a crafted name inject further headers or
// confuse the parser about where the value ends.
//
// The construction follows RFC 6266: a conservative ASCII `filename=` for old
// clients, plus `filename*=UTF-8”...` with percent-encoding for everything else.
// Both are derived from the same name, so a client that understands only the
// first still gets a usable, safe filename.
func contentDisposition(name string) string {
	ascii := sanitiseASCIIFilename(name)
	utf8Encoded := rfc5987Encode(name)
	if ascii == utf8Encoded {
		// Nothing to percent-encode: one parameter is enough and is easier to
		// read in a proxy log.
		return `attachment; filename="` + ascii + `"`
	}
	return `attachment; filename="` + ascii + `"; filename*=UTF-8''` + utf8Encoded
}

// sanitiseASCIIFilename reduces a filename to characters that are safe inside a
// quoted-string header parameter.
//
// The rules are deliberately blunt: anything outside printable ASCII, and any
// character with meaning to the header grammar (quote, backslash, semicolon,
// control characters), becomes an underscore. A name mangled this way is still
// recognisable, and the percent-encoded `filename*` parameter carries the real
// one, so nothing is lost for a client that matters.
func sanitiseASCIIFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '"' || r == '\\' || r == ';' || r == '\r' || r == '\n' || r == 0:
			b.WriteByte('_')
		case r < 0x20 || r > 0x7e:
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	// A name that was entirely unsafe, or empty, would leave `filename=""`, which
	// some clients treat as a missing parameter. Give it something.
	out = strings.TrimSpace(out)
	if out == "" || strings.Trim(out, "_") == "" {
		return "download"
	}
	// A filename cannot be a path separator; a client that used one would be
	// writing outside where it intended.
	out = strings.ReplaceAll(out, "/", "_")
	// Bound the length: a 255-byte filesystem name is legal, but a header has its
	// own limits and the percent-encoded form is longer still.
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

// rfc5987Encode percent-encodes a filename for the `filename*` parameter.
func rfc5987Encode(name string) string {
	// url.PathEscape leaves some characters that RFC 5987 requires to be escaped
	// (`*`, `'`, `(`, `)`) and encodes others that are fine. Encoding the
	// conservative superset is simpler to reason about than getting the exact set
	// right, and every client decodes percent-escapes the same way.
	escaped := url.PathEscape(name)
	var b strings.Builder
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		switch c {
		case '*', '\'', '(', ')', '!', '$', '&', '+', ',', ':', ';', '=', '?', '@', '[', ']':
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
