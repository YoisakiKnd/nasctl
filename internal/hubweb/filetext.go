package hubweb

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

// textSlot waits for one of the few text buffers. The wait ends with the
// request; text reads and saves are short, so they queue rather than fail.
func (s *Server) textSlot(r *http.Request) bool {
	select {
	case s.textSlots <- struct{}{}:
		return true
	case <-r.Context().Done():
		return false
	}
}

// handleFileText returns up to files.TextMaxBytes of a text file as the
// response body (or fewer, with limit), and what the editor needs to know in
// headers. Sending the bytes as they were read, rather than inside JSON,
// keeps one copy in memory instead of three.
func (s *Server) handleFileText(w http.ResponseWriter, r *http.Request) {
	if s.Files == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage", "no storage roots are configured")
		return
	}
	q := r.URL.Query()
	root, p := q.Get("root"), q.Get("path")
	if root == "" || p == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "the root and path parameters are required")
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if privateError(w, s.privateGate(r).check(root, p, useInside)) {
		return
	}
	if !s.textSlot(r) {
		return
	}
	defer func() { <-s.textSlots }()
	text, err := s.Files.ReadText(root, p, limit)
	if err != nil {
		status, code := mapFileErr(err)
		s.fail(w, status, code, err)
		return
	}
	writable := false
	for _, info := range s.Files.Roots() {
		writable = writable || info.ID == root && info.Writable && info.Available
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Nasctl-Size", strconv.FormatInt(text.Size, 10))
	h.Set("X-Nasctl-Limit", strconv.Itoa(files.TextMaxBytes))
	h.Set("X-Nasctl-Revision", text.Revision)
	h.Set("X-Nasctl-Truncated", strconv.FormatBool(text.Truncated))
	h.Set("X-Nasctl-Binary", strconv.FormatBool(text.Binary))
	h.Set("X-Nasctl-Editable", strconv.FormatBool(writable && text.Revision != ""))
	h.Set("Content-Length", strconv.Itoa(len(text.Content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(text.Content)
}

// handleFileTextSave writes the request body as the file's new content. The
// revision query parameter is the one the editor loaded; without it the file
// is created and an existing one is never replaced.
func (s *Server) handleFileTextSave(w http.ResponseWriter, r *http.Request) {
	if !s.fileOperator(w, r, true) {
		return
	}
	q := r.URL.Query()
	root, p, rev := q.Get("root"), q.Get("path"), q.Get("revision")
	if root == "" || p == "" {
		writeError(w, 400, "bad_request", "请选择存储目录和文件")
		return
	}
	if privateError(w, s.privateGate(r).check(root, p, useInside)) {
		return
	}
	if r.ContentLength > files.TextMaxBytes {
		s.fileWriteError(w, files.ErrTooLarge)
		return
	}
	if !s.textSlot(r) {
		return
	}
	defer func() { <-s.textSlots }()
	content, err := readTextBody(w, r)
	if err != nil {
		s.fileWriteError(w, files.ErrTooLarge)
		return
	}
	mode := "replace"
	if rev == "" {
		mode = "create"
	}
	args := operations.NewArgs().Str("root", root).Str("path", p).Int("size", int64(len(content))).Str("mode", mode)
	s.runFileWrite(w, r, "files.edit", args, func(ctx context.Context) (any, error) {
		e, err := s.Files.SaveText(ctx, root, p, content, rev)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": p, "entry": e, "revision": files.Revision(content)}, nil
	})
}

// readTextBody reads at most files.TextMaxBytes into one buffer sized from
// Content-Length when the client sent it, instead of growing it by doubling.
func readTextBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body := http.MaxBytesReader(w, r.Body, files.TextMaxBytes)
	if r.ContentLength < 0 {
		return io.ReadAll(body)
	}
	b := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(body, b); err != nil {
		return nil, err
	}
	if n, err := body.Read(make([]byte, 1)); n > 0 || err != nil && !errors.Is(err, io.EOF) {
		return nil, files.ErrTooLarge
	}
	return b, nil
}
