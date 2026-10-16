// Package statebackup implements offline service-state packages. It never stops
// processes or contacts a downloader. All participating state writers must be
// stopped, and every configured state-directory lease is held exclusively.
package statebackup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/accountslocal"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/dbsnapshot"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
)

const dbBudget int64 = 1 << 30
const metadataBudget int64 = 64 << 20
const manifestBudget = 256 << 10
const journalName = statelease.RestoreJournal

var ErrInvalid = errors.New("state backup: invalid or changed package")

type Layout struct {
	State, Hub, Album, Config string
	Storage                   []config.HubStorage
}

func NewLayout(cfg *config.Config, configPath, hubPath string) Layout {
	if hubPath == "" {
		hubPath = filepath.Join(cfg.Paths.StateDir, "hub.db")
	}
	canonical := func(p string) string {
		p, _ = filepath.Abs(p)
		if parent, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			return filepath.Join(parent, filepath.Base(p))
		}
		return p
	}
	return Layout{canonical(cfg.Paths.StateDir), canonical(hubPath), canonical(cfg.Paths.IndexFile), canonical(configPath), cfg.Hub.Storage}
}
func (l Layout) targets() map[string]string {
	return map[string]string{"hub": l.Hub, "anime": filepath.Join(l.State, "anime.sqlite"), "album": l.Album, "album-directories": filepath.Join(l.State, "album-directories.json"), "backup-plans": filepath.Join(l.State, "backup-plans.json"), "desktop-apps": filepath.Join(l.State, "desktop-apps.json"), "webdav-history": filepath.Join(l.State, "webdav-history.json"), "metadata": filepath.Join(l.State, "anime-metadata-backups"), "settings": filepath.Join(l.State, config.NASSettingsFile), "users": filepath.Join(l.State, accounts.FileName), "users-enabled": filepath.Join(l.State, accountslocal.EnabledFile)}
}
func (l Layout) fingerprint() string {
	l.Storage = nil
	b, _ := json.Marshal(l)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (l Layout) validate() error {
	seen := map[string]bool{}
	paths := l.targets()
	paths["config"] = l.Config
	for _, p := range paths {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, "\x00\r\n?#") || seen[p] {
			return ErrInvalid
		}
		seen[p] = true
		if _, err := os.Stat(filepath.Dir(p)); err != nil {
			return err
		}
		real, err := filepath.EvalSymlinks(filepath.Dir(p))
		if err != nil || real != filepath.Dir(p) {
			return errors.New("state backup: state/config parent must use a canonical path without symbolic links")
		}
	}
	for _, a := range paths {
		for _, b := range paths {
			if a != b && within(a, b) {
				return errors.New("state backup: state targets overlap")
			}
		}
	}
	return nil
}
func within(child, parent string) bool {
	r, e := filepath.Rel(parent, child)
	return e == nil && (r == "." || r != ".." && !strings.HasPrefix(r, ".."+string(os.PathSeparator)))
}
func (l Layout) lock(recovery bool) (func(), error) {
	if err := l.validate(); err != nil {
		return nil, err
	}
	dirs := []string{l.State, filepath.Dir(l.Hub), filepath.Dir(l.Album)}
	sort.Strings(dirs)
	leases := []*statelease.Lease{}
	seen := map[string]bool{}
	close := func() {
		for i := len(leases) - 1; i >= 0; i-- {
			_ = leases[i].Close()
		}
	}
	for _, p := range dirs {
		if seen[p] {
			continue
		}
		seen[p] = true
		lease, err := statelease.Acquire(p, true)
		if err != nil {
			close()
			return nil, err
		}
		leases = append(leases, lease)
		if !recovery {
			if _, err = os.Lstat(filepath.Join(p, statelease.PendingRestore)); err == nil || !os.IsNotExist(err) {
				close()
				return nil, statelease.ErrRecoveryRequired
			}
		}
	}
	if !recovery {
		if _, err := os.Lstat(filepath.Join(l.State, journalName)); err == nil || !os.IsNotExist(err) {
			close()
			return nil, statelease.ErrRecoveryRequired
		}
	}
	return close, nil
}

type Entry struct {
	Key    string `json:"key"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Schema int64  `json:"schema,omitempty"`
}
type Manifest struct {
	Version      int     `json:"version"`
	ID           string  `json:"id"`
	CreatedAt    int64   `json:"created_at"`
	Consistency  string  `json:"consistency"`
	LayoutSHA256 string  `json:"layout_sha256"`
	Entries      []Entry `json:"entries"`
}
type Result struct {
	ID             string `json:"id"`
	Files          int    `json:"files"`
	Bytes          int64  `json:"bytes"`
	ManifestSHA256 string `json:"manifest_sha256"`
	Checkpoint     string `json:"checkpoint,omitempty"`
	ConfigRestored bool   `json:"config_restored"`
}

func nonce() (string, error) {
	var b [16]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}
func keyValid(k string) bool {
	switch k {
	case "config", "hub", "anime", "album", "album-directories", "backup-plans", "desktop-apps", "webdav-history", "settings", "users", "users-enabled":
		return true
	}
	if !strings.HasPrefix(k, "metadata/") {
		return false
	}
	name := strings.TrimPrefix(k, "metadata/")
	if len(name) != 71 || !strings.HasSuffix(name, ".backup") {
		return false
	}
	_, err := hex.DecodeString(name[:64])
	return err == nil && name == strings.ToLower(name)
}
func payload(dir, key string) string { return filepath.Join(dir, "data", filepath.FromSlash(key)) }
func hashFile(ctx context.Context, p string, limit int64) (Entry, error) {
	var out Entry
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return out, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return out, err
	}
	if !before.Mode().IsRegular() || before.Size() > limit || before.Mode().Perm()&0077 != 0 {
		return out, ErrInvalid
	}
	h := sha256.New()
	buf := make([]byte, 64<<10)
	for {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		n, e := f.Read(buf)
		out.Bytes += int64(n)
		if out.Bytes > limit {
			return out, dbsnapshot.ErrLimit
		}
		_, _ = h.Write(buf[:n])
		if e == io.EOF {
			break
		}
		if e != nil {
			return out, e
		}
	}
	after, e := f.Stat()
	linked, le := os.Lstat(p)
	if e != nil || le != nil || !os.SameFile(before, after) || !os.SameFile(before, linked) || !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		return out, ErrInvalid
	}
	out.SHA256 = hex.EncodeToString(h.Sum(nil))
	return out, nil
}
func copyFile(ctx context.Context, src, dst string, limit int64) (Entry, error) {
	var out Entry
	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return out, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > limit {
		return out, ErrInvalid
	}
	w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return out, err
	}
	ok := false
	defer func() {
		_ = w.Close()
		if !ok {
			_ = os.Remove(dst)
		}
	}()
	h := sha256.New()
	buf := make([]byte, 64<<10)
	for {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		n, e := f.Read(buf)
		out.Bytes += int64(n)
		if out.Bytes > limit {
			return out, dbsnapshot.ErrLimit
		}
		if n > 0 {
			if _, err = w.Write(buf[:n]); err != nil {
				return out, err
			}
			_, _ = h.Write(buf[:n])
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return out, e
		}
	}
	after, e := f.Stat()
	linked, le := os.Lstat(src)
	if e != nil || le != nil || !os.SameFile(before, after) || !os.SameFile(before, linked) || !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		return out, ErrInvalid
	}
	if err = w.Sync(); err != nil {
		return out, err
	}
	if err = w.Close(); err != nil {
		return out, err
	}
	out.SHA256 = hex.EncodeToString(h.Sum(nil))
	ok = true
	return out, nil
}
func syncDir(dir string) error {
	f, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func privateDir(dir string) error {
	i, e := os.Lstat(dir)
	if e != nil {
		return e
	}
	if !i.IsDir() || i.Mode().Perm()&0077 != 0 {
		return errors.New("state backup: directory must be private (0700)")
	}
	real, e := filepath.EvalSymlinks(dir)
	if e != nil || real != dir {
		return ErrInvalid
	}
	return nil
}
func (l Layout) destination(dir string, internal bool) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return ErrInvalid
	}
	if err := privateDir(filepath.Dir(dir)); err != nil {
		return err
	}
	if !internal {
		for _, r := range l.Storage {
			p, e := filepath.EvalSymlinks(r.Path)
			if e != nil {
				return e
			}
			if within(dir, p) || within(p, dir) {
				return errors.New("state backup: credentials must not be stored in a browsable storage root")
			}
		}
		if within(dir, l.State) || within(l.State, dir) {
			return ErrInvalid
		}
	}
	return nil
}
func database(ctx context.Context, key, p string) (dbsnapshot.Result, error) {
	r, err := dbsnapshot.Verify(ctx, p, dbBudget)
	if err != nil {
		return r, err
	}
	maximum := map[string]int64{"hub": 1, "anime": 5, "album": 2}[key]
	if r.Schema < 1 || r.Schema > maximum {
		return r, errors.New("state backup: unsupported database schema")
	}
	u := url.URL{Scheme: "file", Path: p}
	u.RawQuery = "mode=ro&_pragma=query_only(1)&_pragma=cache_size(-1024)&_pragma=mmap_size(0)"
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return r, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tables := map[string][]string{"hub": {"jobs", "job_events", "audit", "modes", "approvals"}, "anime": {"subscriptions", "episodes"}, "album": {"media", "collections"}}[key]
	for _, t := range tables {
		var n int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?", t).Scan(&n); err != nil || n != 1 {
			return r, ErrInvalid
		}
	}
	return r, nil
}

// Create requires all writers stopped. Exclusive leases coordinate all three
// databases and catalogs; SQLite backup APIs also include any committed WAL.
func Create(ctx context.Context, l Layout, dir string, report func(string)) (result Result, workErr error) {
	ctx, stop := context.WithTimeout(ctx, 20*time.Minute)
	defer stop()
	close, err := l.lock(false)
	if err != nil {
		return Result{}, err
	}
	defer close()
	if err = audit(l, "backup.start", Result{}, nil); err != nil {
		return result, err
	}
	defer func() {
		if e := audit(l, "backup", result, workErr); e != nil {
			workErr = errors.Join(workErr, e)
		}
	}()
	return capture(ctx, l, dir, false, report)
}
func capture(ctx context.Context, l Layout, dir string, internal bool, report func(string)) (Result, error) {
	var result Result
	if err := l.destination(dir, internal); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	id, err := nonce()
	if err != nil {
		return result, err
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return result, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(dir)
		}
	}()
	for _, sub := range []string{"data", "data/metadata"} {
		if err = os.Mkdir(filepath.Join(dir, sub), 0700); err != nil {
			return result, err
		}
	}
	m := Manifest{Version: 1, ID: id, CreatedAt: time.Now().Unix(), Consistency: "offline-exclusive", LayoutSHA256: l.fingerprint(), Entries: []Entry{}}
	add := func(key, src string, required bool) error {
		if report != nil {
			report("snapshot " + key)
		}
		if _, e := os.Lstat(src); os.IsNotExist(e) && !required {
			return nil
		} else if e != nil {
			return e
		}
		var entry Entry
		if key == "hub" || key == "anime" || key == "album" {
			r, e := dbsnapshot.Create(ctx, src, payload(dir, key), dbBudget, nil)
			if e != nil {
				return e
			}
			if _, e = database(ctx, key, payload(dir, key)); e != nil {
				return e
			}
			entry = Entry{Bytes: r.Bytes, SHA256: r.SHA256, Schema: r.Schema}
		} else {
			limit := int64(65536)
			if key == "config" {
				limit = 1 << 20
			}
			if strings.HasPrefix(key, "metadata/") {
				limit = metadataBudget
			}
			var e error
			entry, e = copyFile(ctx, src, payload(dir, key), limit)
			if e != nil {
				return e
			}
		}
		entry.Key = key
		m.Entries = append(m.Entries, entry)
		return nil
	}
	if err = add("config", l.Config, true); err != nil {
		return result, fmt.Errorf("state backup config: %w", err)
	}
	targets := l.targets()
	for _, key := range []string{"hub", "anime", "album", "album-directories", "backup-plans", "desktop-apps", "webdav-history", "settings", "users", "users-enabled"} {
		if err = add(key, targets[key], key == "hub"); err != nil {
			return result, fmt.Errorf("state backup %s: %w", key, err)
		}
	}
	if f, e := os.OpenFile(targets["metadata"], os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0); e == nil {
		entries, e := f.ReadDir(1001)
		_ = f.Close()
		if e != nil && e != io.EOF {
			return result, e
		}
		if len(entries) > 1000 {
			return result, dbsnapshot.ErrLimit
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		var total int64
		for _, entry := range entries {
			key := "metadata/" + entry.Name()
			if !keyValid(key) {
				return result, ErrInvalid
			}
			if err = add(key, filepath.Join(targets["metadata"], entry.Name()), true); err != nil {
				return result, err
			}
			total += m.Entries[len(m.Entries)-1].Bytes
			if total > metadataBudget {
				return result, dbsnapshot.ErrLimit
			}
		}
	} else if !os.IsNotExist(e) {
		return result, e
	}
	b, err := json.Marshal(m)
	if err != nil || len(b) > manifestBudget {
		return result, ErrInvalid
	}
	if err = fsutil.AtomicWrite(filepath.Join(dir, "manifest.json"), 0600, func(w io.Writer) error { _, e := w.Write(b); return e }); err != nil {
		return result, err
	}
	for _, p := range []string{filepath.Join(dir, "data", "metadata"), filepath.Join(dir, "data"), dir, filepath.Dir(dir)} {
		if err = syncDir(p); err != nil {
			return result, err
		}
	}
	result, _, err = verify(ctx, dir)
	if err != nil {
		return result, fmt.Errorf("state package verification: %w", err)
	}
	complete = true
	return result, nil
}
func Verify(ctx context.Context, dir string) (Result, error) {
	r, _, e := verify(ctx, dir)
	return r, e
}
func verify(ctx context.Context, dir string) (Result, Manifest, error) {
	var result Result
	var m Manifest
	if err := privateDir(dir); err != nil {
		return result, m, err
	}
	for _, sub := range []string{"data", "data/metadata"} {
		if err := privateDir(filepath.Join(dir, sub)); err != nil {
			return result, m, err
		}
	}
	b, err := readPrivate(filepath.Join(dir, "manifest.json"), manifestBudget)
	if err != nil {
		return result, m, err
	}
	if err = decode(b, &m); err != nil {
		return result, m, err
	}
	if m.Version != 1 || len(m.ID) != 32 || m.Consistency != "offline-exclusive" || len(m.LayoutSHA256) != 64 || len(m.Entries) < 2 || len(m.Entries) > 1009 {
		return result, m, ErrInvalid
	}
	if _, err = hex.DecodeString(m.ID); err != nil {
		return result, m, ErrInvalid
	}
	seen := map[string]bool{}
	var metadataBytes int64
	for _, entry := range m.Entries {
		if !keyValid(entry.Key) || seen[entry.Key] || entry.Bytes < 0 || len(entry.SHA256) != 64 {
			return result, m, ErrInvalid
		}
		seen[entry.Key] = true
		limit := int64(65536)
		switch entry.Key {
		case "config":
			limit = 1 << 20
		case "hub", "anime", "album":
			limit = dbBudget
		}
		if strings.HasPrefix(entry.Key, "metadata/") {
			limit = metadataBudget
			metadataBytes += entry.Bytes
		}
		got, e := hashFile(ctx, payload(dir, entry.Key), limit)
		if e != nil {
			return result, m, fmt.Errorf("payload %s: %w", entry.Key, e)
		}
		if got.Bytes != entry.Bytes || got.SHA256 != entry.SHA256 {
			return result, m, fmt.Errorf("payload digest %s: %w", entry.Key, ErrInvalid)
		}
		if entry.Key == "hub" || entry.Key == "anime" || entry.Key == "album" {
			r, e := database(ctx, entry.Key, payload(dir, entry.Key))
			if e != nil {
				return result, m, e
			}
			if r.Schema != entry.Schema {
				return result, m, ErrInvalid
			}
		} else if entry.Schema != 0 {
			return result, m, ErrInvalid
		}
		result.Bytes += entry.Bytes
	}
	if !seen["hub"] || !seen["config"] || metadataBytes > metadataBudget {
		return result, m, ErrInvalid
	}
	// Refuse unlisted payloads and sidecars. A closed snapshot needs neither WAL
	// nor SHM, and restoring unrelated files is never permitted.
	for _, sub := range []string{"", "data", "data/metadata"} {
		f, e := os.Open(filepath.Join(dir, sub))
		if e != nil {
			return result, m, e
		}
		rows, e := f.ReadDir(1010)
		_ = f.Close()
		if e != nil && e != io.EOF {
			return result, m, e
		}
		if len(rows) > 1008 {
			return result, m, ErrInvalid
		}
		for _, row := range rows {
			allowed := false
			switch sub {
			case "":
				allowed = row.Name() == "manifest.json" || row.Name() == "data"
			case "data":
				allowed = row.Name() == "metadata" || seen[row.Name()]
			default:
				allowed = seen["metadata/"+row.Name()]
			}
			if !allowed || row.Type()&os.ModeSymlink != 0 {
				return result, m, fmt.Errorf("unlisted payload %s/%s: %w", sub, row.Name(), ErrInvalid)
			}
		}
	}
	cfgBytes, e := readPrivate(payload(dir, "config"), 1<<20)
	if e != nil {
		return result, m, e
	}
	savedConfig, e := config.Parse(string(cfgBytes))
	if e != nil {
		return result, m, errors.New("state backup: saved configuration is invalid")
	}
	if seen["settings"] {
		data, e := readPrivate(payload(dir, "settings"), 65536)
		if e != nil {
			return result, m, e
		}
		o, e := config.DecodeNASOptions(data)
		if e != nil {
			return result, m, e
		}
		if e = config.ApplyNASOptions(savedConfig, o); e != nil {
			return result, m, errors.New("state backup: saved settings invalid")
		}
	}
	if seen["users"] {
		if !savedConfig.Hub.Setup && savedConfig.Server.Token == "" {
			return result, m, errors.New("state backup: account protection must persist in the saved base configuration")
		}
		if e = accountslocal.ValidateFile(payload(dir, "users")); e != nil {
			return result, m, e
		}
	}
	if seen["users"] != seen["users-enabled"] || seen["users-enabled"] && entriesBytes(m, "users-enabled") != 0 {
		return result, m, ErrInvalid
	}
	h := sha256.Sum256(b)
	result.ID = m.ID
	result.Files = len(m.Entries)
	result.ManifestSHA256 = hex.EncodeToString(h[:])
	return result, m, nil
}
func decode(b []byte, v any) error {
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return ErrInvalid
	}
	if d.Decode(new(any)) != io.EOF {
		return ErrInvalid
	}
	return nil
}
func readPrivate(p string, max int64) ([]byte, error) {
	f, e := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Mode().Perm()&0077 != 0 || i.Size() > max {
		return nil, ErrInvalid
	}
	b, e := io.ReadAll(io.LimitReader(f, max+1))
	if e != nil || int64(len(b)) > max {
		return nil, ErrInvalid
	}
	return b, nil
}

// String intentionally omits configuration and database rows.
func (r Result) String() string {
	return fmt.Sprintf("state package %s: %d files, %d bytes, manifest %s", r.ID, r.Files, r.Bytes, r.ManifestSHA256)
}

func entriesBytes(m Manifest, key string) int64 {
	for _, entry := range m.Entries {
		if entry.Key == key {
			return entry.Bytes
		}
	}
	return -1
}
