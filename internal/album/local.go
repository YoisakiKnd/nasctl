package album

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// NewHandler connects the bounded client to a startup-injected read-only
// handler. No listener, port, loopback network or album credential is needed.
// Pipe backpressure prevents buffering entire originals in memory.
func NewHandler(handler http.Handler) (*Client, error) {
	if handler == nil {
		return nil, errors.New("album: local handler is required")
	}
	return &Client{base: "http://album.local", mode: "local", http: &http.Client{
		Transport:     handlerTransport{handler},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("album redirects are refused") },
	}}, nil
}

type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "http" || req.URL.Host != "album.local" || (req.Method != "GET" && req.Method != "HEAD") {
		return nil, errors.New("album: invalid local request")
	}
	ctx, cancel := context.WithCancel(req.Context())
	reader, writer := io.Pipe()
	w := &pipeResponse{header: make(http.Header), ready: make(chan *http.Response, 1), writer: writer, request: req}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = writer.CloseWithError(fmt.Errorf("album: local handler failed"))
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = writer.Close()
		}()
		t.handler.ServeHTTP(w, req.Clone(ctx))
	}()
	// Cancellation must close both ends even when a handler has written headers
	// and is blocked waiting for a body consumer that has already left.
	stop := context.AfterFunc(ctx, func() { _ = reader.CloseWithError(ctx.Err()); _ = writer.CloseWithError(ctx.Err()) })
	select {
	case response := <-w.ready:
		response.Body = &localBody{PipeReader: reader, cancel: cancel, stop: stop}
		return response, nil
	case <-ctx.Done():
		stop()
		cancel()
		_ = reader.Close()
		_ = writer.Close()
		return nil, ctx.Err()
	}
}

type localBody struct {
	*io.PipeReader
	cancel context.CancelFunc
	stop   func() bool
}

func (b *localBody) Close() error { b.stop(); b.cancel(); return b.PipeReader.Close() }

// Only the serving goroutine accesses the writer/header fields. The response
// receives a header snapshot, which remains immutable during streaming.
type pipeResponse struct {
	header  http.Header
	ready   chan *http.Response
	writer  *io.PipeWriter
	request *http.Request
	sent    bool
}

func (w *pipeResponse) Header() http.Header { return w.header }
func (w *pipeResponse) WriteHeader(code int) {
	if w.sent || code < 200 {
		return
	}
	w.sent = true
	length := int64(-1)
	if v, err := strconv.ParseInt(w.header.Get("Content-Length"), 10, 64); err == nil && v >= 0 {
		length = v
	}
	w.ready <- &http.Response{StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)), Header: w.header.Clone(), ContentLength: length, Request: w.request, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}
}
func (w *pipeResponse) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.writer.Write(p)
}
