// Package photovault stores only authenticated ciphertext. Password wrapping,
// encrypted metadata and encrypted thumbnails are independent of NAS login.
package photovault

import (
	"bytes"
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
)

const iterations = 600000
const maxObjects = 10000
const lifetime = 15 * time.Minute
const Directory = ".nasctl-photo-vault"

type object struct {
	ID       string `json:"id"`
	SourceID int64  `json:"source_id"`
	Pending  bool   `json:"pending"`
}
type document struct {
	Version int      `json:"version"`
	Salt    string   `json:"salt"`
	Wrapped string   `json:"wrapped"`
	Objects []object `json:"objects"`
}
type Record struct {
	Details    album.Details `json:"details"`
	SourceRoot string        `json:"source_root"`
	SourcePath string        `json:"source_path"`
	CacheKey   string        `json:"cache_key"`
	MtimeNS    int64         `json:"mtime_ns"`
	Digest     string        `json:"digest"`
	ThumbMIME  string        `json:"thumb_mime"`
}
type Item struct {
	album.Item
	ID       string `json:"id"`
	SourceID int64  `json:"source_id,omitempty"`
	Pending  bool   `json:"pending"`
}
type Snapshot struct {
	Available  bool  `json:"available"`
	Configured bool  `json:"configured"`
	Unlocked   bool  `json:"unlocked"`
	ExpiresAt  int64 `json:"expires_at,omitempty"`
}
type session struct {
	key     []byte
	owner   string
	expires time.Time
}
type Store struct {
	mu            sync.Mutex
	root          *os.Root
	lease         *statelease.Lease
	doc           document
	sessions      map[[32]byte]*session
	kdf           chan struct{}
	syncDirectory func() error
}
type Mover interface {
	MoveToVault(context.Context, *Store, string, string, int64, album.Details) (string, error)
}

func randomID() (string, error) {
	var b [16]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}
func validID(id string) bool {
	if len(id) != 32 || strings.ToLower(id) != id {
		return false
	}
	_, e := hex.DecodeString(id)
	return e == nil
}
func Open(state string) (*Store, error) {
	base, e := os.OpenRoot(state)
	if e != nil {
		return nil, e
	}
	defer base.Close()
	lease, e := statelease.AcquireRoot(base, false)
	if e != nil {
		return nil, e
	}
	fail := func(e error) (*Store, error) { lease.Close(); return nil, e }
	info, e := base.Lstat(Directory)
	if os.IsNotExist(e) {
		e = base.Mkdir(Directory, 0700)
		if e == nil {
			info, e = base.Lstat(Directory)
		}
	}
	if e != nil {
		return fail(e)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return fail(ErrInvalid)
	}
	root, e := base.OpenRoot(Directory)
	if e != nil {
		return fail(e)
	}
	s := &Store{root: root, lease: lease, sessions: map[[32]byte]*session{}, kdf: make(chan struct{}, 1)}
	b, e := s.read("vault.json", 2<<20)
	if os.IsNotExist(e) {
		return s, nil
	}
	if e == nil {
		e = json.Unmarshal(b, &s.doc)
	}
	if e == nil {
		e = s.validate()
	}
	if e != nil {
		s.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) validate() error {
	salt, e := base64.RawStdEncoding.DecodeString(s.doc.Salt)
	wrapped, we := base64.RawStdEncoding.DecodeString(s.doc.Wrapped)
	if s.doc.Version != 1 || e != nil || len(salt) != 16 || we != nil || len(wrapped) != 60 || len(s.doc.Objects) > maxObjects {
		return ErrInvalid
	}
	ids, source := map[string]bool{}, map[int64]bool{}
	for _, o := range s.doc.Objects {
		if !validID(o.ID) || o.SourceID < 0 || o.Pending && o.SourceID == 0 || !o.Pending && o.SourceID != 0 || ids[o.ID] || o.SourceID > 0 && source[o.SourceID] {
			return ErrInvalid
		}
		ids[o.ID] = true
		source[o.SourceID] = true
	}
	return nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.sessions {
		clear(v.key)
	}
	clear(s.sessions)
	e := s.root.Close()
	if le := s.lease.Close(); e == nil {
		e = le
	}
	return e
}
func (s *Store) read(name string, limit int64) ([]byte, error) {
	f, e := s.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrInvalid
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, ErrInvalid
	}
	return b, e
}
func (s *Store) syncDir() error {
	if s.syncDirectory != nil {
		return s.syncDirectory()
	}
	f, e := s.root.Open(".")
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

// Report publication separately: a directory fsync can fail after rename.
// In that case retaining ciphertext and the new in-memory index is essential.
func (s *Store) atomic(name string, b []byte) (bool, error) {
	id, e := randomID()
	if e != nil {
		return false, e
	}
	temp := ".tmp-" + id
	f, e := s.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return false, e
	}
	defer s.root.Remove(temp)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return false, e
	}
	if e = s.root.Rename(temp, name); e != nil {
		return false, e
	}
	return true, s.syncDir()
}
func (s *Store) save(next document) error {
	b, e := json.Marshal(next)
	if e != nil {
		return e
	}
	published, e := s.atomic("vault.json", b)
	if published {
		s.doc = next
		if e != nil {
			return errors.Join(ErrSyncPending, e)
		}
	}
	return e
}
func validPassword(p string) bool {
	return utf8.ValidString(p) && utf8.RuneCountInString(p) >= 12 && len(p) <= 256 && !strings.ContainsRune(p, 0)
}
func wrap(password string, key []byte) (string, string, error) {
	if !validPassword(password) {
		return "", "", errors.New("密码至少 12 个字符，最多 256 字节")
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	derived, e := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if e != nil {
		return "", "", e
	}
	defer clear(derived)
	a, e := aead(derived)
	if e != nil {
		return "", "", e
	}
	n := make([]byte, a.NonceSize())
	rand.Read(n)
	sealed := a.Seal(n, n, key, []byte("nasctl-photo-vault-key-v1"))
	return base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(sealed), nil
}
func unwrap(password string, d document) ([]byte, error) {
	if len(password) > 256 {
		return nil, ErrLocked
	}
	salt, e := base64.RawStdEncoding.DecodeString(d.Salt)
	if e != nil {
		return nil, ErrInvalid
	}
	sealed, e := base64.RawStdEncoding.DecodeString(d.Wrapped)
	if e != nil || len(sealed) != 60 {
		return nil, ErrInvalid
	}
	derived, e := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if e != nil {
		return nil, e
	}
	defer clear(derived)
	a, e := aead(derived)
	if e != nil {
		return nil, e
	}
	key, e := a.Open(nil, sealed[:12], sealed[12:], []byte("nasctl-photo-vault-key-v1"))
	if e != nil {
		return nil, ErrLocked
	}
	return key, nil
}
func (s *Store) Create(password string) error {
	select {
	case s.kdf <- struct{}{}:
		defer func() { <-s.kdf }()
	default:
		return ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.doc.Version != 0 {
		return errors.New("保险箱已设置密码")
	}
	key := make([]byte, 32)
	if _, e := rand.Read(key); e != nil {
		return e
	}
	defer clear(key)
	salt, wrapped, e := wrap(password, key)
	if e != nil {
		return e
	}
	return s.save(document{Version: 1, Salt: salt, Wrapped: wrapped, Objects: []object{}})
}
func (s *Store) session(token, owner string) (*session, bool) {
	v, ok := s.sessions[sha256.Sum256([]byte(token))]
	if !ok || v.owner != owner || !time.Now().Before(v.expires) {
		if ok && !time.Now().Before(v.expires) {
			clear(v.key)
			delete(s.sessions, sha256.Sum256([]byte(token)))
		}
		return nil, false
	}
	return v, true
}
func (s *Store) Status(token, owner string) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Snapshot{Available: true, Configured: s.doc.Version == 1}
	if v, ok := s.session(token, owner); ok {
		out.Unlocked = true
		out.ExpiresAt = v.expires.Unix()
	}
	return out
}
func (s *Store) Unlock(password, owner string) (string, error) {
	select {
	case s.kdf <- struct{}{}:
		defer func() { <-s.kdf }()
	default:
		return "", ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, e := unwrap(password, s.doc)
	if e != nil {
		return "", e
	}
	for k, v := range s.sessions {
		if v.owner == owner || !time.Now().Before(v.expires) {
			clear(v.key)
			delete(s.sessions, k)
		}
	}
	if len(s.sessions) >= 32 {
		clear(key)
		return "", ErrBusy
	}
	b := make([]byte, 32)
	if _, e = rand.Read(b); e != nil {
		clear(key)
		return "", e
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	s.sessions[sha256.Sum256([]byte(token))] = &session{key: key, owner: owner, expires: time.Now().Add(lifetime)}
	return token, nil
}
func (s *Store) Lock(token, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.session(token, owner); ok {
		clear(v.key)
		delete(s.sessions, sha256.Sum256([]byte(token)))
	}
}
func (s *Store) ChangePassword(token, owner, old, next string) error {
	select {
	case s.kdf <- struct{}{}:
		defer func() { <-s.kdf }()
	default:
		return ErrBusy
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.session(token, owner); !ok {
		return ErrLocked
	}
	key, e := unwrap(old, s.doc)
	if e != nil {
		return e
	}
	defer clear(key)
	salt, wrapped, e := wrap(next, key)
	if e != nil {
		return e
	}
	d := s.doc
	d.Salt = salt
	d.Wrapped = wrapped
	e = s.save(d)
	if s.doc.Wrapped == wrapped {
		for _, v := range s.sessions {
			clear(v.key)
		}
		clear(s.sessions)
	}
	return e
}
func (s *Store) key(token, owner string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.session(token, owner)
	if !ok {
		return nil, ErrLocked
	}
	return bytes.Clone(v.key), nil
}
func (s *Store) Valid(token, owner string) bool { return s.Status(token, owner).Unlocked }

// Extend keeps a session open while it is in use: every use moves the
// automatic lock a full lifetime ahead, so a long video does not lock midway.
func (s *Store) Extend(token, owner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.session(token, owner)
	if ok {
		v.expires = time.Now().Add(lifetime)
	}
	return ok
}
func (s *Store) Protected(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.doc.Objects {
		if o.SourceID == id && o.Pending {
			return true
		}
	}
	return false
}
func (s *Store) Find(token, owner string, sourceID int64) (string, Record, bool, error) {
	key, e := s.key(token, owner)
	if e != nil {
		return "", Record{}, false, e
	}
	defer clear(key)
	s.mu.Lock()
	var found object
	for _, o := range s.doc.Objects {
		if o.SourceID == sourceID {
			found = o
			break
		}
	}
	s.mu.Unlock()
	if found.ID == "" {
		return "", Record{}, false, nil
	}
	record, e := s.record(context.Background(), key, found.ID)
	return found.ID, record, true, e
}
func (s *Store) record(ctx context.Context, key []byte, id string) (Record, error) {
	var out Record
	f, e := s.root.OpenFile(id+".meta", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return out, e
	}
	r, e := openReader(ctx, f, key, id+"/meta", nil)
	if e != nil {
		f.Close()
		return out, e
	}
	defer r.Close()
	if r.size > 32768 {
		return out, ErrInvalid
	}
	b, e := io.ReadAll(r)
	if e != nil {
		return out, e
	}
	e = json.Unmarshal(b, &out)
	return out, e
}
func publicItem(id string, record Record, pending bool) Item {
	it := record.Details.Item
	it.ThumbURL = "/api/v1/album/vault/media/" + id + "/thumb"
	it.RawURL = "/api/v1/album/vault/media/" + id + "/raw"
	sourceID := int64(0)
	if pending {
		sourceID = record.Details.Item.ID
	}
	return Item{Item: it, ID: id, Pending: pending, SourceID: sourceID}
}
func (s *Store) List(ctx context.Context, token, owner string, page int) ([]Item, int, error) {
	key, e := s.key(token, owner)
	if e != nil {
		return nil, 0, e
	}
	defer clear(key)
	s.mu.Lock()
	objects := append([]object{}, s.doc.Objects...)
	s.mu.Unlock()
	if page < 1 || page > maxObjects/48+1 {
		return nil, 0, ErrInvalid
	}
	items := []Item{}
	start := (page - 1) * 48
	for i := len(objects) - 1 - start; i >= 0 && len(items) < 48; i-- {
		if !s.Valid(token, owner) {
			return nil, 0, ErrLocked
		}
		o := objects[i]
		record, e := s.record(ctx, key, o.ID)
		if e != nil {
			return nil, 0, e
		}
		items = append(items, publicItem(o.ID, record, o.Pending))
	}
	return items, len(objects), nil
}
func (s *Store) Details(ctx context.Context, token, owner, id string) (Item, Record, error) {
	key, e := s.key(token, owner)
	if e != nil {
		return Item{}, Record{}, e
	}
	defer clear(key)
	s.mu.Lock()
	var found object
	for _, o := range s.doc.Objects {
		if o.ID == id {
			found = o
			break
		}
	}
	s.mu.Unlock()
	if !validID(id) || found.ID == "" {
		return Item{}, Record{}, os.ErrNotExist
	}
	r, e := s.record(ctx, key, id)
	return publicItem(id, r, found.Pending), r, e
}
func (s *Store) OpenMedia(ctx context.Context, token, owner, id, part string) (*Reader, Item, string, error) {
	it, record, e := s.Details(ctx, token, owner, id)
	if e != nil {
		return nil, it, "", e
	}
	if part != "thumb" && part != "raw" && part != "download" {
		return nil, it, "", ErrInvalid
	}
	name, mime := "original", it.MIME
	if part == "thumb" {
		name, mime = "thumb", record.ThumbMIME
	}
	key, e := s.key(token, owner)
	if e != nil {
		return nil, it, "", e
	}
	defer clear(key)
	f, e := s.root.OpenFile(id+"."+name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, it, "", e
	}
	r, e := openReader(ctx, f, key, id+"/"+name, func() bool { return s.Valid(token, owner) })
	if e != nil {
		f.Close()
	}
	return r, it, mime, e
}

// Put completes and re-reads the encrypted original before publishing the object.
// A durable pending record hides old album links before the caller removes plaintext.
func (s *Store) Put(ctx context.Context, token, owner string, record Record, source io.Reader, thumbnail []byte) (string, string, error) {
	key, e := s.key(token, owner)
	if e != nil {
		return "", "", e
	}
	defer clear(key)
	if record.Details.Item.Kind != "image" || record.Details.Item.Bytes < 1 || record.Details.Item.Bytes > MaxPhotoBytes || len(thumbnail) > 2<<20 {
		return "", "", ErrInvalid
	}
	s.mu.Lock()
	full := len(s.doc.Objects) >= maxObjects
	s.mu.Unlock()
	if full {
		return "", "", errors.New("保险箱最多收藏 10,000 张照片")
	}
	id, e := randomID()
	if e != nil {
		return "", "", e
	}
	published := false
	defer func() {
		if !published {
			for _, suffix := range []string{"original", "thumb", "meta"} {
				s.root.Remove(id + "." + suffix)
			}
		}
	}()
	write := func(part string, input io.Reader, size int64) (string, error) {
		f, e := s.root.OpenFile(id+"."+part, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if e != nil {
			return "", e
		}
		defer f.Close()
		return encrypt(ctx, f, key, id+"/"+part, input, size)
	}
	digest, e := write("original", source, record.Details.Item.Bytes)
	if e != nil {
		return "", "", e
	}
	f, e := s.root.Open(id + ".original")
	if e != nil {
		return "", "", e
	}
	r, e := openReader(ctx, f, key, id+"/original", func() bool { return s.Valid(token, owner) })
	if e != nil {
		f.Close()
		return "", "", e
	}
	hash := sha256.New()
	_, e = io.CopyBuffer(hash, r, make([]byte, chunkSize))
	r.Close()
	if e != nil || hex.EncodeToString(hash.Sum(nil)) != digest {
		return "", "", ErrInvalid
	}
	record.Digest = digest
	if _, e = write("thumb", bytes.NewReader(thumbnail), int64(len(thumbnail))); e != nil {
		return "", "", e
	}
	b, e := json.Marshal(record)
	if e != nil || len(b) > 32768 {
		return "", "", ErrInvalid
	}
	if _, e = write("meta", bytes.NewReader(b), int64(len(b))); e != nil {
		return "", "", e
	}
	if e = s.syncDir(); e != nil {
		return "", "", e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.session(token, owner); !ok {
		return "", "", ErrLocked
	}
	if len(s.doc.Objects) >= maxObjects {
		return "", "", ErrBusy
	}
	for _, existing := range s.doc.Objects {
		if existing.SourceID == record.Details.Item.ID {
			return "", "", ErrBusy
		}
	}
	next := s.doc
	next.Objects = append(append([]object{}, next.Objects...), object{ID: id, SourceID: record.Details.Item.ID, Pending: true})
	e = s.save(next)
	for _, o := range s.doc.Objects {
		if o.ID == id {
			published = true
			break
		}
	}
	if e != nil {
		if published {
			return id, digest, e
		}
		return "", "", e
	}
	return id, digest, nil
}
func (s *Store) Complete(token, owner, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.session(token, owner); !ok {
		return ErrLocked
	}
	next := s.doc
	next.Objects = append([]object{}, next.Objects...)
	found := false
	for i, o := range next.Objects {
		if o.ID == id {
			next.Objects[i].Pending = false
			next.Objects[i].SourceID = 0
			found = true
		}
	}
	if !found {
		return ErrInvalid
	}
	return s.save(next)
}

// Path is for local backup instructions, never returned by the browser API.
func Path(state string) string { return filepath.Join(state, Directory) }

// Verify reauthenticates the durable copy before a retry may remove plaintext.
func (s *Store) Verify(ctx context.Context, token, owner, id string) error {
	_, record, e := s.Details(ctx, token, owner, id)
	if e != nil {
		return e
	}
	r, _, _, e := s.OpenMedia(ctx, token, owner, id, "raw")
	if e != nil {
		return e
	}
	defer r.Close()
	hash := sha256.New()
	n, e := io.CopyBuffer(hash, r, make([]byte, chunkSize))
	if e != nil {
		return e
	}
	if n != record.Details.Item.Bytes || hex.EncodeToString(hash.Sum(nil)) != record.Digest {
		return ErrInvalid
	}
	return nil
}
