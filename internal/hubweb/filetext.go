package hubweb

import (
	"context"
	"io"
	"net/http"

	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

// handleFileText returns a text file for the viewer and editor, bounded to
// files.TextMaxBytes.
func (s *Server) handleFileText(w http.ResponseWriter, r *http.Request) {
	if s.Files == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage", "no storage roots are configured")
		return
	}
	root, p := r.URL.Query().Get("root"), r.URL.Query().Get("path")
	if root == "" || p == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "the root and path parameters are required")
		return
	}
	if privateError(w, s.privateGate(r).check(root, p, useInside)) {
		return
	}
	text, err := s.Files.ReadText(root, p)
	if err != nil {
		status, code := mapFileErr(err)
		s.fail(w, status, code, err)
		return
	}
	writable := false
	for _, info := range s.Files.Roots() {
		writable = writable || info.ID == root && info.Writable && info.Available
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"text":     text,
		"limit":    files.TextMaxBytes,
		"editable": writable && text.Revision != "",
	})
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
	content, err := io.ReadAll(http.MaxBytesReader(w, r.Body, files.TextMaxBytes))
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
		text, err := s.Files.ReadText(root, p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": p, "entry": e, "revision": text.Revision}, nil
	})
}
