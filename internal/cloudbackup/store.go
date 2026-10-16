// Package cloudbackup implements bounded WebDAV backups. Direct snapshots keep
// original files; encrypted snapshots stream tar -> zstd -> age into a private
// ciphertext spool. Only verified uploads are published with WebDAV MOVE.
package cloudbackup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
)

var ErrInvalid = errors.New("webdav: invalid or changed backup scope")
var ErrBusy = errors.New("webdav: another operation is active")
var ErrRemote = errors.New("webdav: remote request or content verification failed")
var ErrPendingVault = errors.New("webdav: vault move cleanup pending")
var ErrPassword = errors.New("webdav: incorrect archive password or damaged archive")
var netDialer = net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}

const Directory = ".nasctl-webdav"
const maxRecords = 128

type Connection struct {
	URL       string `json:"url"`
	Username  string `json:"username"`
	Password  string `json:"password,omitempty"`
	AllowHTTP bool   `json:"allow_http"`
	Revision  string `json:"revision"`
}
type ConnectionView struct {
	URL         string `json:"url"`
	Username    string `json:"username"`
	PasswordSet bool   `json:"password_set"`
	AllowHTTP   bool   `json:"allow_http"`
	Revision    string `json:"revision"`
}
type ConnectionRequest struct {
	URL            string `json:"url"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	PasswordAction string `json:"password_action"`
	AllowHTTP      bool   `json:"allow_http"`
	Revision       string `json:"revision"`
}
type Input struct {
	Root       string `json:"root"`
	Path       string `json:"path"`
	Mode       string `json:"mode"`
	Limit      int    `json:"limit"`
	MaxBytes   int64  `json:"max_bytes"`
	MaxRuntime string `json:"max_runtime"`
}
type RunRequest struct {
	Input
	Password string `json:"password"`
	Revision string `json:"revision"`
}
type Record struct {
	ID              string `json:"id"`
	Input           Input  `json:"source"`
	SourceScope     string `json:"source_scope"`
	ConnectionScope string `json:"connection_scope"`
	CreatedAt       int64  `json:"created_at"`
	State           string `json:"state"`
	Entries         int64  `json:"entries"`
	Bytes           int64  `json:"bytes"`
	Skipped         int    `json:"skipped"`
	StoredBytes     int64  `json:"stored_bytes"`
	SHA256          string `json:"sha256"`
	Error           string `json:"error,omitempty"`
	Retryable       bool   `json:"retryable"`
}
type View struct {
	VaultAvailable bool           `json:"vault_available"`
	Connection     ConnectionView `json:"connection"`
	Records        []Record       `json:"records"`
	Busy           bool           `json:"busy"`
	Limits         files.Limits   `json:"limits"`
}
type Progress struct {
	Phase                 string
	Entries, Bytes, Total int64
}
type Control interface {
	Settings() View
	Save(context.Context, ConnectionRequest) (View, error)
	Probe(context.Context, string) error
	Run(context.Context, RunRequest, func(Progress)) (Record, error)
	Retry(context.Context, string, func(Progress)) (Record, error)
	Restore(context.Context, string, string, string, string, func(Progress)) (files.CopyResult, error)
	Download(context.Context, string) (io.ReadCloser, int64, string, error)
}
type VaultExporter interface {
	BackupScope() (string, error)
	ArchiveBackup(context.Context, io.Writer, int, int64, files.CopyProgress) (files.ArchiveResult, error)
}
type Store struct {
	Vault      VaultExporter
	mu         sync.Mutex
	work       sync.Mutex
	busy       bool
	dir        string
	root       *os.Root
	files      *files.Manager
	connection Connection
	records    []Record
	client     *http.Client
	state      string
}

func randomID() (string, error) {
	var b [16]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	b, e := hex.DecodeString(id)
	return e == nil && len(b) == 16 && strings.ToLower(id) == id
}
func New(state string, fm *files.Manager) (*Store, error) {
	dir := filepath.Join(state, Directory)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	info, e := os.Lstat(dir)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, ErrInvalid
	}
	root, e := os.OpenRoot(dir)
	if e != nil {
		return nil, e
	}
	c := &Store{dir: dir, state: state, root: root, files: fm, records: []Record{}, client: &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&netDialer).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second, MaxConnsPerHost: 2, DisableCompression: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if e = c.read("connection.json", 16384, &c.connection); e != nil && !os.IsNotExist(e) {
		root.Close()
		return nil, e
	}
	if c.connection.URL != "" && validateConnection(c.connection) != nil {
		root.Close()
		return nil, ErrInvalid
	}
	if e = c.read("history.json", 512<<10, &c.records); e != nil && !os.IsNotExist(e) {
		root.Close()
		return nil, e
	}
	if len(c.records) > maxRecords {
		root.Close()
		return nil, ErrInvalid
	}
	for i := range c.records {
		r := &c.records[i]
		if (r.Input.Mode != "direct" && r.Input.Mode != "encrypted") || !validID(r.ID) || r.StoredBytes < 0 || r.StoredBytes > files.MaxTransferBytes+files.MaxTransferBytes/100+(64<<20) || r.Input.Limit < 1 || r.Input.Limit > 10000 || r.Input.MaxBytes < 1 || r.Input.MaxBytes > files.MaxTransferBytes {
			root.Close()
			return nil, ErrInvalid
		}
		if r.State == "running" {
			r.State = "interrupted"
			r.Error = "上次备份被中断，可核对后重试。"
		}
		if r.Input.Mode == "encrypted" {
			r.Retryable = r.State != "complete" && len(r.SHA256) == 64
		} else {
			r.Retryable = r.State != "complete"
		}
	}
	return c, nil
}
func (s *Store) Close() error { s.client.CloseIdleConnections(); return s.root.Close() }
func (s *Store) read(name string, max int64, out any) error {
	var f *os.File
	var e error
	if name == "history.json" {
		f, e = os.OpenFile(filepath.Join(s.state, "webdav-history.json"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	} else {
		f, e = s.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	}
	if e != nil {
		return e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Size() > max {
		return ErrInvalid
	}
	d := json.NewDecoder(io.LimitReader(f, max+1))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return ErrInvalid
	}
	return nil
}
func (s *Store) write(name string, value any) error {
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	budget := 512 << 10
	if name == "connection.json" {
		budget = 16384
	}
	if len(b) > budget {
		return ErrInvalid
	}
	target := filepath.Join(s.dir, name)
	if name == "history.json" {
		target = filepath.Join(s.state, "webdav-history.json")
	}
	return fsutil.AtomicWriteFile(target, b, 0600)
}
func validateConnection(c Connection) error {
	u, e := url.Parse(c.URL)
	if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "https" && !(u.Scheme == "http" && c.AllowHTTP)) || len(c.URL) > 2048 || strings.ContainsAny(c.URL, "\x00\r\n\\") || path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/") && u.Path != "/" && u.Path != "" || strings.Contains(c.Username, ":") || len(c.Username) > 256 || len(c.Password) > 4096 || strings.ContainsAny(c.Username+c.Password, "\x00\r\n") {
		return ErrInvalid
	}
	return nil
}
func scope(c Connection) string {
	b, _ := json.Marshal([]string{c.URL, c.Username})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (s *Store) Settings() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.connection
	available := false
	if s.Vault != nil {
		_, e := s.Vault.BackupScope()
		available = e == nil
	}
	return View{VaultAvailable: available, Connection: ConnectionView{c.URL, c.Username, c.Password != "", c.AllowHTTP, c.Revision}, Records: append([]Record{}, s.records...), Busy: s.busy, Limits: s.files.Limits()}
}
func (s *Store) Save(ctx context.Context, in ConnectionRequest) (View, error) {
	if !s.work.TryLock() {
		return View{}, ErrBusy
	}
	defer s.work.Unlock()
	s.mu.Lock()
	if in.Revision != s.connection.Revision {
		s.mu.Unlock()
		return View{}, ErrInvalid
	}
	c := Connection{URL: strings.TrimRight(strings.TrimSpace(in.URL), "/") + "/", Username: in.Username, Password: s.connection.Password, AllowHTTP: in.AllowHTTP}
	switch in.PasswordAction {
	case "keep":
	case "replace":
		c.Password = in.Password
	case "clear":
		c.Password = ""
	default:
		s.mu.Unlock()
		return View{}, ErrInvalid
	}
	if e := validateConnection(c); e != nil {
		s.mu.Unlock()
		return View{}, e
	}
	var e error
	c.Revision, e = randomID()
	if e == nil {
		e = ctx.Err()
	}
	if e == nil {
		e = s.write("connection.json", c)
	}
	if e == nil {
		s.connection = c
	}
	s.mu.Unlock()
	if e != nil {
		return View{}, e
	}
	return s.Settings(), nil
}
func (s *Store) begin() (Connection, error) {
	if !s.work.TryLock() {
		return Connection{}, ErrBusy
	}
	s.mu.Lock()
	s.busy = true
	c := s.connection
	s.mu.Unlock()
	return c, nil
}
func (s *Store) end() { s.mu.Lock(); s.busy = false; s.mu.Unlock(); s.work.Unlock() }
func (s *Store) update(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append([]Record{}, s.records...)
	found := false
	for i := range next {
		if next[i].ID == r.ID {
			next[i] = r
			found = true
			break
		}
	}
	if !found {
		if len(next) >= maxRecords {
			return ErrInvalid
		}
		next = append([]Record{r}, next...)
	}
	if e := s.write("history.json", next); e != nil {
		return e
	}
	s.records = next
	return nil
}

func (s *Store) find(id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validID(id) {
		return Record{}, ErrInvalid
	}
	for _, r := range s.records {
		if r.ID == id {
			return r, nil
		}
	}
	return Record{}, ErrInvalid
}
func (s *Store) validateInput(in Input) (string, time.Duration, error) {
	l := s.files.Limits()
	runtime, e := time.ParseDuration(in.MaxRuntime)
	if e != nil || runtime < time.Second || runtime > 2*time.Hour || in.Limit < 1 || in.Limit > l.CopyMaxEntries || in.MaxBytes < 1 || in.MaxBytes > l.CopyMaxBytes || len(in.Path) > 1024 || (in.Mode != "direct" && in.Mode != "encrypted") {
		return "", 0, ErrInvalid
	}
	if in.Root == "@photo-vault" {
		if s.Vault == nil || in.Mode != "encrypted" || in.Path != "." {
			return "", 0, ErrInvalid
		}
		v, e := s.Vault.BackupScope()
		return v, runtime, e
	}
	v, e := s.files.StorageScope(in.Root, in.Path)
	return v, runtime, e
}
