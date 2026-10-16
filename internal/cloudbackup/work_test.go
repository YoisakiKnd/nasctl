package cloudbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/files"
)

type davFixture struct {
	mu       sync.Mutex
	files    map[string][]byte
	dirs     map[string]bool
	failMove bool
	lostMove bool
	corrupt  bool
	password string
}

func (d *davFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	u, p, ok := r.BasicAuth()
	if !ok || u != "fixture" || p != d.password {
		w.WriteHeader(401)
		return
	}
	name := strings.TrimSuffix(r.URL.Path, "/")
	if name == "" {
		name = "/"
	}
	switch r.Method {
	case "PROPFIND":
		w.WriteHeader(207)
		io.WriteString(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat></d:response></d:multistatus>`)
	case "MKCOL":
		if d.dirs[name] {
			w.WriteHeader(405)
			return
		}
		d.dirs[name] = true
		w.WriteHeader(201)
	case "PUT":
		if _, ok := d.files[name]; ok && r.Header.Get("If-None-Match") == "*" {
			w.WriteHeader(412)
			return
		}
		b, e := io.ReadAll(r.Body)
		if e != nil {
			w.WriteHeader(400)
			return
		}
		d.files[name] = b
		w.WriteHeader(201)
	case "GET":
		b, ok := d.files[name]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if d.corrupt && strings.Contains(name, ".part") {
			b = append([]byte{}, b...)
			if len(b) > 0 {
				b[0] ^= 1
			}
		}
		w.Write(b)
	case "MOVE":
		if d.failMove {
			w.WriteHeader(503)
			return
		}
		dest, e := url.Parse(r.Header.Get("Destination"))
		if e != nil || r.Header.Get("Overwrite") != "F" {
			w.WriteHeader(400)
			return
		}
		if d.dirs[dest.Path] || d.files[dest.Path] != nil {
			w.WriteHeader(412)
			return
		}
		if b, ok := d.files[name]; ok {
			d.files[dest.Path] = b
			delete(d.files, name)
		} else if d.dirs[name] {
			for k, b := range d.files {
				if strings.HasPrefix(k, name+"/") {
					d.files[dest.Path+strings.TrimPrefix(k, name)] = b
					delete(d.files, k)
				}
			}
			for k := range d.dirs {
				if k == name || strings.HasPrefix(k, name+"/") {
					d.dirs[dest.Path+strings.TrimPrefix(k, name)] = true
					delete(d.dirs, k)
				}
			}
		} else {
			w.WriteHeader(404)
			return
		}
		if d.lostMove {
			d.lostMove = false
			h, ok := w.(http.Hijacker)
			if !ok {
				panic("fixture requires hijacking")
			}
			conn, _, e := h.Hijack()
			if e == nil {
				conn.Close()
			}
			return
		}
		w.WriteHeader(201)
	case "DELETE":
		delete(d.files, name)
		delete(d.dirs, name)
		for k := range d.files {
			if strings.HasPrefix(k, name+"/") {
				delete(d.files, k)
			}
		}
		for k := range d.dirs {
			if strings.HasPrefix(k, name+"/") {
				delete(d.dirs, k)
			}
		}
		w.WriteHeader(204)
	default:
		w.WriteHeader(405)
	}
}
func cloudRig(t *testing.T) (*Store, *davFixture, string, string, string) {
	t.Helper()
	source, destination, state := t.TempDir(), t.TempDir(), t.TempDir()
	if e := os.Mkdir(filepath.Join(source, "photos"), 0700); e != nil {
		t.Fatal(e)
	}
	for name, body := range map[string]string{"photos/private-name.txt": "private original bytes\n", "photos/empty": ""} {
		if e := os.WriteFile(filepath.Join(source, name), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.Symlink("/etc/passwd", filepath.Join(source, "photos/link")); e != nil {
		t.Fatal(e)
	}
	fm, e := files.New([]config.HubStorage{{ID: "source", Path: source}, {ID: "destination", Path: destination, Writable: true}}, files.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { fm.Close() })
	s, e := New(state, fm)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	dav := &davFixture{files: map[string][]byte{}, dirs: map[string]bool{"/": true}, password: "generated-webdav-secret"}
	server := httptest.NewServer(dav)
	t.Cleanup(server.Close)
	_, e = s.Save(context.Background(), ConnectionRequest{URL: server.URL, Username: "fixture", Password: dav.password, PasswordAction: "replace", AllowHTTP: true})
	if e != nil {
		t.Fatal(e)
	}
	return s, dav, source, destination, state
}
func requestFor(s *Store, mode string) RunRequest {
	password := ""
	if mode == "encrypted" {
		password = "generated archive password"
	}
	return RunRequest{Input: Input{Root: "source", Path: "photos", Mode: mode, Limit: 100, MaxBytes: 1 << 20, MaxRuntime: "1m"}, Password: password, Revision: s.Settings().Connection.Revision}
}
func TestDirectAndEncryptedBackupRestore(t *testing.T) {
	for _, mode := range []string{"direct", "encrypted"} {
		t.Run(mode, func(t *testing.T) {
			s, dav, source, destination, state := cloudRig(t)
			ctx := context.Background()
			req := requestFor(s, mode)
			r, e := s.Run(ctx, req, nil)
			if e != nil || r.State != "complete" || r.Bytes != 23 {
				t.Fatalf("run: %+v %v", r, e)
			}
			original, _ := os.ReadFile(filepath.Join(source, "photos/private-name.txt"))
			dav.mu.Lock()
			if mode == "direct" {
				if !bytes.Equal(dav.files["/"+r.ID+"/data/private-name.txt"], original) {
					t.Fatal("direct upload changed bytes")
				}
				if dav.files["/"+r.ID+"/data/empty"] == nil {
					t.Fatal("empty file absent")
				}
			} else {
				body := dav.files["/"+remoteName(r)]
				if !bytes.HasPrefix(body, []byte("age-encryption.org/v1")) || bytes.Contains(body, original) || bytes.Contains(body, []byte("private-name.txt")) {
					t.Fatal("archive exposed private content or filename")
				}
			}
			for name := range dav.files {
				if strings.Contains(name, ".part") {
					t.Fatal("temporary upload retained")
				}
			}
			dav.mu.Unlock()
			if mode == "encrypted" {
				if _, e = s.Restore(ctx, r.ID, "destination", "wrong-password", "incorrect archive password", nil); !errors.Is(e, ErrPassword) {
					t.Fatal(e)
				}
				if _, e = os.Stat(filepath.Join(destination, "wrong-password")); !os.IsNotExist(e) {
					t.Fatal("wrong password published target")
				}
			}
			restored, e := s.Restore(ctx, r.ID, "destination", "restored", req.Password, nil)
			if e != nil || !restored.Published {
				t.Fatal(restored, e)
			}
			body, e := os.ReadFile(filepath.Join(destination, "restored/private-name.txt"))
			if e != nil || !bytes.Equal(body, original) {
				t.Fatal(string(body), e)
			}
			if _, e = os.Lstat(filepath.Join(destination, "restored/link")); !os.IsNotExist(e) {
				t.Fatal("symlink restored")
			}
			if _, e = s.Restore(ctx, r.ID, "destination", "restored", req.Password, nil); !errors.Is(e, files.ErrExists) {
				t.Fatal("overwrite permitted", e)
			}
			for _, name := range []string{"connection.json", "history.json"} {
				target := filepath.Join(state, Directory, name)
				if name == "history.json" {
					target = filepath.Join(state, "webdav-history.json")
				}
				body, e := os.ReadFile(target)
				if e != nil || bytes.Contains(body, []byte(req.Password)) && req.Password != "" {
					t.Fatal("archive password persisted", e)
				}
			}
			if mode == "encrypted" {
				if _, e = os.Stat(filepath.Join(state, Directory, r.ID+".age")); !os.IsNotExist(e) {
					t.Fatal("successful spool retained")
				}
				download, size, _, e := s.Download(ctx, r.ID)
				if e != nil {
					t.Fatal(e)
				}
				b, e := io.ReadAll(download)
				download.Close()
				if e != nil || int64(len(b)) != size {
					t.Fatal("download failed", e)
				}
			}
		})
	}
}
func TestFailedReadbackCannotPublishAndEncryptedRetry(t *testing.T) {
	s, dav, _, _, state := cloudRig(t)
	dav.corrupt = true
	r, e := s.Run(context.Background(), requestFor(s, "encrypted"), nil)
	if !errors.Is(e, ErrRemote) || r.State != "failed" || !r.Retryable {
		t.Fatal(r, e)
	}
	if _, e = os.Stat(filepath.Join(state, Directory, r.ID+".age")); e != nil {
		t.Fatal("retry lost ciphertext", e)
	}
	dav.mu.Lock()
	if len(dav.files) != 0 {
		t.Fatal("unverified remote object retained", dav.files)
	}
	dav.corrupt = false
	dav.mu.Unlock()
	r, e = s.Retry(context.Background(), r.ID, nil)
	if e != nil || r.State != "complete" {
		t.Fatal(r, e)
	}
}
func TestDamagedRemoteCannotPublishRestore(t *testing.T) {
	for _, mode := range []string{"direct", "encrypted"} {
		t.Run(mode, func(t *testing.T) {
			s, dav, _, destination, _ := cloudRig(t)
			req := requestFor(s, mode)
			r, e := s.Run(context.Background(), req, nil)
			if e != nil {
				t.Fatal(e)
			}
			dav.mu.Lock()
			name := "/" + remoteName(r)
			if mode == "direct" {
				name += "/data/private-name.txt"
			}
			b := dav.files[name]
			b[len(b)-1] ^= 1
			dav.mu.Unlock()
			if result, e := s.Restore(context.Background(), r.ID, "destination", "damaged", req.Password, nil); e == nil || result.Published {
				t.Fatal(result, e)
			}
			if _, e = os.Stat(filepath.Join(destination, "damaged")); !os.IsNotExist(e) {
				t.Fatal("damaged restore target visible")
			}
		})
	}
}
func TestCredentialsRedactedRedirectsDeniedAndURLValidation(t *testing.T) {
	s, _, _, _, _ := cloudRig(t)
	b, _ := json.Marshal(s.Settings())
	if bytes.Contains(b, []byte("generated-webdav-secret")) {
		t.Fatal("credential exposed")
	}
	for _, raw := range []string{"https://user:secret@example.com/", "http://example.com/", "file:///tmp", "https://example.com/?secret=1", "https://example.com/a/../b/"} {
		if validateConnection(Connection{URL: raw}) == nil {
			t.Fatal("accepted", raw)
		}
	}
	called := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(200) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer redirect.Close()
	v, e := s.Save(context.Background(), ConnectionRequest{URL: redirect.URL, Username: "fixture", PasswordAction: "keep", AllowHTTP: true, Revision: s.Settings().Connection.Revision})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Probe(context.Background(), v.Connection.Revision); !errors.Is(e, ErrRemote) || called {
		t.Fatal("followed credential redirect", called, e)
	}
}
func TestCancelledAndOverBudgetBackupPreserveSource(t *testing.T) {
	s, dav, source, _, _ := cloudRig(t)
	req := requestFor(s, "direct")
	req.MaxBytes = 2
	r, e := s.Run(context.Background(), req, nil)
	if e == nil || r.State == "complete" {
		t.Fatal(r, e)
	}
	if _, e = os.Stat(filepath.Join(source, "photos/private-name.txt")); e != nil {
		t.Fatal(e)
	}
	dav.mu.Lock()
	defer dav.mu.Unlock()
	if len(dav.files) != 0 {
		t.Fatal("failed scope published remote files")
	}
}

func TestRetryRecognizesPublishedBackupAfterLostResponseWithoutSource(t *testing.T) {
	for _, mode := range []string{"direct", "encrypted"} {
		t.Run(mode, func(t *testing.T) {
			s, dav, source, _, _ := cloudRig(t)
			dav.lostMove = true
			r, e := s.Run(context.Background(), requestFor(s, mode), nil)
			if e == nil || !r.Retryable || len(r.SHA256) != 64 {
				t.Fatal(r, e)
			}
			if e = os.RemoveAll(filepath.Join(source, "photos")); e != nil {
				t.Fatal(e)
			}
			r, e = s.Retry(context.Background(), r.ID, nil)
			if e != nil || r.State != "complete" {
				t.Fatal("published backup not recognized", r, e)
			}
		})
	}
}
func TestCancelledPackingNeverPublishesOrKeepsPartialCiphertext(t *testing.T) {
	s, dav, source, _, state := cloudRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r, e := s.Run(ctx, requestFor(s, "encrypted"), func(p Progress) {
		if p.Phase == "pack" {
			cancel()
		}
	})
	if e == nil || r.State == "complete" {
		t.Fatal(r, e)
	}
	if _, e = os.Stat(filepath.Join(source, "photos/private-name.txt")); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(state, Directory, r.ID+".age")); !os.IsNotExist(e) {
		t.Fatal("partial packet retained")
	}
	dav.mu.Lock()
	defer dav.mu.Unlock()
	if len(dav.files) != 0 {
		t.Fatal("cancelled upload published")
	}
}

type pendingVault struct{}

func (pendingVault) HasPending() bool             { return true }
func (pendingVault) BackupScope() (string, error) { return "", ErrInvalid }
func (pendingVault) ArchiveBackup(context.Context, io.Writer, int, int64, files.CopyProgress) (files.ArchiveResult, error) {
	return files.ArchiveResult{}, ErrInvalid
}
func TestPendingVaultMovePreventsPlaintextUpload(t *testing.T) {
	s, dav, _, _, _ := cloudRig(t)
	s.Vault = pendingVault{}
	if _, e := s.Run(context.Background(), requestFor(s, "direct"), nil); !errors.Is(e, ErrPendingVault) {
		t.Fatal(e)
	}
	dav.mu.Lock()
	defer dav.mu.Unlock()
	if len(dav.files) != 0 {
		t.Fatal("pending original uploaded")
	}
}
