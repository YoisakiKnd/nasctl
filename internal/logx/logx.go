// Package logx provides a small structured logger. It writes to stderr and,
// optionally, to a single append-only file. There is deliberately no log
// rotation daemon and no unbounded in-memory buffer: a weak NAS must never
// have its memory or disk consumed by logging.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger is a thin wrapper that keeps the call sites short.
type Logger struct {
	*slog.Logger
	closer io.Closer
}

// Options configures a Logger.
type Options struct {
	Level  string // debug, info, warn, error
	File   string // optional path; empty means stderr only
	JSON   bool   // structured JSON instead of text
	Prefix string // component prefix for every record
}

// New builds a Logger according to opts.
func New(opts Options) (*Logger, error) {
	var lvl slog.Level
	switch opts.Level {
	case "debug":
		lvl = slog.LevelDebug
	case "", "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("logx: unknown level %q", opts.Level)
	}

	var (
		w      io.Writer = os.Stderr
		closer io.Closer
	)
	if opts.File != "" && opts.File != "-" {
		if err := os.MkdirAll(filepath.Dir(opts.File), 0o750); err != nil {
			return nil, fmt.Errorf("logx: create log dir: %w", err)
		}
		f, err := os.OpenFile(opts.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return nil, fmt.Errorf("logx: open log file: %w", err)
		}
		w = io.MultiWriter(os.Stderr, &cappedWriter{w: f, max: 8 << 20})
		closer = f
	}

	var h slog.Handler
	ho := &slog.HandlerOptions{Level: lvl}
	if opts.JSON {
		h = slog.NewJSONHandler(w, ho)
	} else {
		h = slog.NewTextHandler(w, ho)
	}
	if opts.Prefix != "" {
		h = h.WithAttrs([]slog.Attr{slog.String("component", opts.Prefix)})
	}
	return &Logger{Logger: slog.New(h), closer: closer}, nil
}

// Close releases the optional log file.
func (l *Logger) Close() error {
	if l == nil || l.closer == nil {
		return nil
	}
	return l.closer.Close()
}

// cappedWriter stops writing once max bytes have been seen so that a runaway
// loop cannot fill the system SSD. It still writes to stderr via MultiWriter.
type cappedWriter struct {
	mu      sync.Mutex
	w       io.Writer
	written int64
	max     int64
	warned  bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.written >= c.max {
		if !c.warned {
			c.warned = true
			_, _ = fmt.Fprintf(c.w, "%s level=WARN msg=%q\n",
				time.Now().UTC().Format(time.RFC3339), "log file size cap reached; file output suppressed")
		}
		return len(p), nil
	}
	n, err := c.w.Write(p)
	c.written += int64(n)
	return n, err
}

// Discard returns a logger that writes nowhere. Useful in tests.
func Discard() *Logger {
	return &Logger{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// With returns a child logger with extra attributes.
func (l *Logger) With(args ...any) *Logger {
	if l == nil {
		return Discard()
	}
	return &Logger{Logger: l.Logger.With(args...), closer: l.closer}
}

// Ctx is a helper for callers that want a logger in a context.
type ctxKey struct{}

// Into attaches l to ctx.
func Into(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// From returns the logger in ctx, or a discard logger.
func From(ctx context.Context) *Logger {
	if l, ok := ctx.Value(ctxKey{}).(*Logger); ok && l != nil {
		return l
	}
	return Discard()
}
