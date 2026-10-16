// Package albumlocal assembles the existing read-only album at NAS startup.
// It is deliberately outside hubweb's import graph: requests cannot acquire
// scanners, decoders or a writable album store through this capability.
package albumlocal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/YoisakiKnd/nasctl/internal/albumapi"
	"github.com/YoisakiKnd/nasctl/internal/buildinfo"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
	"github.com/YoisakiKnd/nasctl/internal/logx"
	"github.com/YoisakiKnd/nasctl/internal/store"
)

type Reader struct {
	mu      sync.RWMutex
	cfg     config.Config
	log     *logx.Logger
	st      *store.Store
	handler http.Handler
	closed  bool
	active  sync.WaitGroup
	files   *files.Manager
}

// New does not create an index on a fresh installation. Opening is retried
// after the bounded worker creates it, without requiring a console restart.
func New(cfg *config.Config, log *logx.Logger) *Reader {
	if log == nil {
		log = logx.Discard()
	}
	return &Reader{cfg: *cfg, log: log}
}

func (r *Reader) open() error {
	r.mu.RLock()
	ready, closed := r.handler != nil, r.closed
	r.mu.RUnlock()
	if closed {
		return errors.New("album reader is closed")
	}
	if ready {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("album reader is closed")
	}
	if r.handler != nil {
		return nil
	}
	st, err := store.Open(r.cfg.Paths.IndexFile, store.Options{ReadOnly: true, MaxOpenConns: 1})
	if err != nil {
		return err
	}
	copy := r.cfg
	srv, err := r.newServer(&copy, st)
	if err != nil {
		_ = st.Close()
		return err
	}
	r.st, r.handler = st, srv.Handler()
	return nil
}

func (r *Reader) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if err := r.open(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		if errors.Is(err, os.ErrNotExist) && req.URL.Path == "/api/v1/status" {
			_ = json.NewEncoder(w).Encode(map[string]any{"version": buildinfo.Version, "index_ready": false, "warnings": []string{}})
			return
		}
		w.Header().Set("X-Nasctl-Album-Problem", "index_unavailable")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "local album index is not ready"})
		return
	}
	r.mu.RLock()
	if r.closed {
		r.mu.RUnlock()
		http.Error(w, "album reader is closed", http.StatusServiceUnavailable)
		return
	}
	r.active.Add(1)
	handler, token := r.handler, r.cfg.Server.Token
	r.mu.RUnlock()
	defer r.active.Done()
	// Only fixed API endpoints are exposed by the injected client. Browser
	// cookies/credentials never arrive here; the optional existing server token
	// is added internally, so local mode needs no second credential or port.
	request := req.Clone(req.Context())
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	handler.ServeHTTP(w, request)
}

// Update changes only future requests. Active media streams keep their own
// immutable configuration snapshot and do not delay directory changes.
func (r *Reader) Update(cfg *config.Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("album reader is closed")
	}
	if r.st != nil {
		copy := *cfg
		srv, err := r.newServer(&copy, r.st)
		if err != nil {
			return err
		}
		r.handler = srv.Handler()
	}
	r.cfg = *cfg
	return nil
}

func (r *Reader) task(ctx context.Context, pid int) (store.Task, bool) {
	if r.open() != nil {
		return store.Task{}, false
	}
	r.mu.RLock()
	if r.closed {
		r.mu.RUnlock()
		return store.Task{}, false
	}
	r.active.Add(1)
	st := r.st
	r.mu.RUnlock()
	defer r.active.Done()
	tasks, err := st.RunningTasks(ctx)
	if err == nil {
		for _, task := range tasks {
			if task.PID == int64(pid) {
				return task, true
			}
		}
	}
	last, err := st.LastTask(ctx, "")
	return last, err == nil && last.PID == int64(pid)
}

func (r *Reader) newServer(cfg *config.Config, st *store.Store) (*albumapi.Server, error) {
	srv, err := albumapi.New(cfg, st, r.log)
	if err != nil || r.files == nil {
		return srv, err
	}
	srv.OpenOriginal = func(ctx context.Context, m store.Media) (*os.File, error) {
		col, ok := cfg.Collection(m.CollectionCode)
		if !ok {
			return nil, errors.New("album directory is no longer configured")
		}
		joined, err := fsutil.SafeRelJoin(col.Root, m.RelPath)
		if err != nil {
			return nil, err
		}
		for _, root := range cfg.Hub.Storage {
			if fsutil.IsWithin(root.Path, col.Root) {
				rel, err := filepath.Rel(root.Path, joined)
				if err != nil {
					return nil, err
				}
				f, _, err := r.files.Open(root.ID, path.Clean(filepath.ToSlash(rel)))
				return f, err
			}
		}
		// Main-config collections outside NAS roots retain the existing path
		// confinement; managed directories always match a configured NAS root.
		real, err := fsutil.ResolveWithin(col.Root, joined)
		if err != nil {
			return nil, err
		}
		return os.Open(real)
	}
	return srv, nil
}

// Close waits for active bounded requests before releasing the read-only pool.
func (r *Reader) Close() error {
	r.mu.Lock()
	r.closed = true
	st := r.st
	r.st = nil
	r.mu.Unlock()
	r.active.Wait()
	if st != nil {
		return st.Close()
	}
	return nil
}
