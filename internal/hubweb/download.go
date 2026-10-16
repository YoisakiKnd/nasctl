package hubweb

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/files"
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

	http.ServeContent(transfer(w), r, fi.Name(), fi.ModTime(), f)
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

// transferIdle is how long a download may stall before it is dropped.
const transferIdle = 30 * time.Second

// idleDeadlineWriter moves the write deadline forward before every write. A
// transfer then fails only when it stalls, not after the server's total
// WriteTimeout: a large file or a playing video legitimately takes longer than
// that, while a vanished client still frees its stream slot within seconds.
type idleDeadlineWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (w idleDeadlineWriter) Write(p []byte) (int, error) {
	_ = w.rc.SetWriteDeadline(time.Now().Add(transferIdle))
	return w.ResponseWriter.Write(p)
}
func (w idleDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// transfer wraps w for a long streaming response.
func transfer(w http.ResponseWriter) http.ResponseWriter {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(transferIdle))
	return idleDeadlineWriter{ResponseWriter: w, rc: rc}
}

// handleFolderDownload streams a folder as a ZIP. The folder is planned first,
// so one that exceeds the copy budget is refused with an error instead of an
// archive that stops halfway. Like a file download it holds a stream slot.
func (s *Server) handleFolderDownload(w http.ResponseWriter, r *http.Request) {
	if s.Files == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage", "尚未配置存储目录")
		return
	}
	root, path := r.URL.Query().Get("root"), r.URL.Query().Get("path")
	if root == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "缺少存储目录参数")
		return
	}
	// Paths are relative to the root; "/" or an empty path is the root itself.
	path = strings.Trim(path, "/")
	if path == "" {
		path = "."
	}
	select {
	case s.streamSlots <- struct{}{}:
		defer func() { <-s.streamSlots }()
	case <-r.Context().Done():
		return
	default:
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusServiceUnavailable, "busy", "同时进行的下载太多，请稍后再试")
		return
	}
	limits := s.Files.Limits()
	plan, err := s.Files.PlanZip(r.Context(), root, path, limits.CopyMaxEntries, limits.CopyMaxBytes)
	if errors.Is(err, files.ErrTreeLimit) {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("文件夹超过打包下载上限（最多 %d 项、%s 或 32 层），请分开下载", limits.CopyMaxEntries, sizeLabel(limits.CopyMaxBytes)))
		return
	}
	if errors.Is(err, files.ErrSourceChanged) {
		writeError(w, http.StatusConflict, "interrupted", "文件夹正在变化，请稍后重试")
		return
	}
	if err != nil {
		status, code := mapFileErr(err)
		s.fail(w, status, code, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(plan.Name+".zip"))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Nasctl-Archive-Entries", fmt.Sprint(plan.Entries))
	if r.Method == http.MethodHead {
		return
	}
	if err := plan.Write(r.Context(), transfer(w)); err != nil {
		if s.Log != nil {
			s.Log.Error("folder download stopped", "root", root, "error", err.Error())
		}
		// The ZIP's directory is written last; dropping the connection makes
		// the browser report a failed download rather than a broken archive.
		panic(http.ErrAbortHandler)
	}
}

func sizeLabel(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.0f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d 字节", n)
}
