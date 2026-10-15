// Package accountslocal stores bounded private NAS users with salted password
// derivation. No account API returns credential material. All mutators share
// the state restore lease and a cross-process writer lock.
package accountslocal

import (
	"bytes"
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
	"golang.org/x/sys/unix"
)

const iterations = 600000
const maxBytes = 65536

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)

type record struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Disabled    bool   `json:"disabled"`
	CreatedAt   int64  `json:"created_at"`
	Epoch       string `json:"epoch"`
	Algorithm   string `json:"algorithm"`
	Iterations  int    `json:"iterations"`
	Salt        string `json:"salt"`
	Hash        string `json:"hash"`
}
type document struct {
	Version  int      `json:"version"`
	Revision string   `json:"revision"`
	Users    []record `json:"users"`
}
type Store struct {
	mu    sync.Mutex
	root  *os.Root
	state string
	lease *statelease.Lease
	data  document
	info  os.FileInfo
	slots chan struct{}
}

func randomHex() (string, error) {
	var b [16]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}
func validSetupKey(b []byte) bool {
	if len(b) != 44 || b[43] != '\n' {
		return false
	}
	raw, e := base64.RawURLEncoding.DecodeString(string(b[:43]))
	return e == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == string(b[:43])
}
func roleValid(r string) bool { return r == "admin" || r == "operator" || r == "viewer" }
func hexID(s string) bool {
	if len(s) != 32 || strings.ToLower(s) != s {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil
}
func validDocument(d document) error {
	if d.Version != 1 || !hexID(d.Revision) || len(d.Users) < 1 || len(d.Users) > accounts.MaxUsers {
		return accounts.ErrInvalid
	}
	ids, names := map[string]bool{}, map[string]bool{}
	admins := 0
	for _, u := range d.Users {
		salt, se := base64.RawURLEncoding.DecodeString(u.Salt)
		hash, he := base64.RawURLEncoding.DecodeString(u.Hash)
		if !hexID(u.ID) || !hexID(u.Epoch) || ids[u.ID] || names[u.Username] || !usernamePattern.MatchString(u.Username) || !roleValid(u.Role) || !validDisplay(u.DisplayName) || u.CreatedAt <= 0 || u.Algorithm != "pbkdf2-sha256" || u.Iterations != iterations || se != nil || len(salt) != 16 || he != nil || len(hash) != 32 {
			return accounts.ErrInvalid
		}
		ids[u.ID] = true
		names[u.Username] = true
		if u.Role == "admin" && !u.Disabled {
			admins++
		}
	}
	if admins == 0 {
		return accounts.ErrLastAdmin
	}
	return nil
}
func validDisplay(s string) bool {
	return utf8.ValidString(s) && len(s) > 0 && len(s) <= 128 && !strings.ContainsAny(s, "\x00\r\n") && strings.TrimSpace(s) == s
}
func parse(b []byte) (document, error) {
	var d document
	if len(b) > maxBytes {
		return d, accounts.ErrInvalid
	}
	r := json.NewDecoder(bytes.NewReader(b))
	r.DisallowUnknownFields()
	if r.Decode(&d) != nil || r.Decode(new(any)) != io.EOF {
		return d, accounts.ErrInvalid
	}
	return d, validDocument(d)
}

// ValidateFile verifies a private staged account store before offline restore.
func ValidateFile(path string) error {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Mode().Perm()&0077 != 0 || i.Size() > maxBytes {
		return accounts.ErrInvalid
	}
	b, e := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if e != nil {
		return e
	}
	_, e = parse(b)
	return e
}
func Open(state string) (*Store, error) {
	root, e := os.OpenRoot(state)
	if e != nil {
		return nil, e
	}
	lease, e := statelease.AcquireRoot(root, false)
	if e != nil {
		root.Close()
		return nil, e
	}
	s := &Store{root: root, state: state, lease: lease, slots: make(chan struct{}, 1), data: document{Version: 1, Users: []record{}}}
	s.mu.Lock()
	e = s.refresh()
	if e == nil && len(s.data.Users) > 0 {
		e = s.enabledMarker()
	}
	s.mu.Unlock()
	if e != nil {
		s.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) Close() error { e := s.root.Close(); return errors.Join(e, s.lease.Close()) }
func (s *Store) refresh() error {
	rootHandle, e := s.root.Open(".")
	if e != nil {
		return e
	}
	info, e := rootHandle.Stat()
	_ = rootHandle.Close()
	linked, le := os.Stat(s.state)
	if e != nil || le != nil || !os.SameFile(info, linked) {
		return errors.New("accounts: configured state directory changed")
	}
	i, e := s.root.Lstat(accounts.FileName)
	if os.IsNotExist(e) {
		_, markerErr := s.root.Lstat(EnabledFile)
		if s.info != nil || len(s.data.Users) > 0 || markerErr == nil || !os.IsNotExist(markerErr) {
			return errors.New("accounts: existing account store disappeared; refusing anonymous fallback")
		}
		return nil
	}
	if e != nil {
		return e
	}
	if !i.Mode().IsRegular() || i.Mode().Perm()&0077 != 0 || i.Size() > maxBytes {
		return accounts.ErrInvalid
	}
	if s.info != nil && os.SameFile(i, s.info) && i.Size() == s.info.Size() && i.ModTime().Equal(s.info.ModTime()) {
		return nil
	}
	f, e := s.root.OpenFile(accounts.FileName, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	opened, e := f.Stat()
	if e != nil || !os.SameFile(i, opened) {
		return accounts.ErrInvalid
	}
	b, e := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if e != nil {
		return e
	}
	d, e := parse(b)
	if e != nil {
		return e
	}
	after, e := s.root.Lstat(accounts.FileName)
	if e != nil || !os.SameFile(i, after) || i.Size() != after.Size() || !i.ModTime().Equal(after.ModTime()) {
		return accounts.ErrConflict
	}
	s.data = d
	s.info = after
	return nil
}
func public(u record) accounts.User {
	return accounts.User{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role, Disabled: u.Disabled, CreatedAt: u.CreatedAt, Epoch: u.Epoch}
}
func (s *Store) Snapshot(ctx context.Context) (accounts.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return accounts.Snapshot{}, e
	}
	if e := s.refresh(); e != nil {
		return accounts.Snapshot{}, e
	}
	out := accounts.Snapshot{Users: []accounts.User{}, Revision: s.data.Revision, Enabled: len(s.data.Users) > 0, MaxUsers: accounts.MaxUsers}
	for _, u := range s.data.Users {
		out.Users = append(out.Users, public(u))
	}
	if !out.Enabled {
		if i, e := s.root.Lstat(accounts.SetupFile); e == nil && i.Mode().IsRegular() && i.Mode().Perm()&0077 == 0 {
			out.SetupAvailable = true
		}
	}
	return out, nil
}
func (s *Store) Lookup(ctx context.Context, id string) (accounts.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return accounts.User{}, e
	}
	if e := s.refresh(); e != nil {
		return accounts.User{}, e
	}
	for _, u := range s.data.Users {
		if u.ID == id && !u.Disabled {
			return public(u), nil
		}
	}
	return accounts.User{}, accounts.ErrCredential
}
func (s *Store) derive(ctx context.Context, password string, salt []byte) ([]byte, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return nil, accounts.ErrBusy
	}
	key, e := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if e != nil {
		return nil, e
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	return key, nil
}
func (s *Store) password(ctx context.Context, password string) (string, string, error) {
	if !utf8.ValidString(password) || len(password) > 1024 || utf8.RuneCountInString(password) < 12 || strings.ContainsAny(password, "\x00\r\n") {
		return "", "", fmt.Errorf("%w: use a password of at least 12 characters, at most 1024 bytes, without line breaks", accounts.ErrInvalid)
	}
	salt := make([]byte, 16)
	if _, e := rand.Read(salt); e != nil {
		return "", "", e
	}
	key, e := s.derive(ctx, password, salt)
	if e != nil {
		return "", "", e
	}
	return base64.RawURLEncoding.EncodeToString(salt), base64.RawURLEncoding.EncodeToString(key), nil
}
func (s *Store) Authenticate(ctx context.Context, name, password string) (accounts.User, error) {
	if len(password) > 1024 || strings.ContainsAny(password, "\x00\r\n") || !usernamePattern.MatchString(name) {
		return accounts.User{}, accounts.ErrCredential
	}
	s.mu.Lock()
	if e := s.refresh(); e != nil {
		s.mu.Unlock()
		return accounts.User{}, e
	}
	candidate := record{}
	for _, u := range s.data.Users {
		if u.Username == name {
			candidate = u
			break
		}
	}
	s.mu.Unlock()
	salt := make([]byte, 16)
	expected := make([]byte, 32)
	if candidate.ID != "" {
		salt, _ = base64.RawURLEncoding.DecodeString(candidate.Salt)
		expected, _ = base64.RawURLEncoding.DecodeString(candidate.Hash)
	}
	key, e := s.derive(ctx, password, salt)
	if e != nil {
		return accounts.User{}, e
	}
	if subtle.ConstantTimeCompare(key, expected) != 1 || candidate.ID == "" || candidate.Disabled {
		return accounts.User{}, accounts.ErrCredential
	}
	current, e := s.Lookup(ctx, candidate.ID)
	if e != nil || current.Epoch != candidate.Epoch {
		return accounts.User{}, accounts.ErrCredential
	}
	return current, nil
}
func (s *Store) writer() (func(), error) {
	f, e := s.root.OpenFile(".nasctl-users.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if e != nil {
		return nil, e
	}
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Size() != 0 {
		f.Close()
		return nil, accounts.ErrInvalid
	}
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, accounts.ErrConflict
	}
	linked, e := s.root.Lstat(".nasctl-users.lock")
	if e != nil || !os.SameFile(i, linked) {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
		return nil, accounts.ErrInvalid
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}
func (s *Store) publish(ctx context.Context, d document) error {
	rev, e := randomHex()
	if e != nil {
		return e
	}
	d.Revision = rev
	if e = validDocument(d); e != nil {
		return e
	}
	b, e := json.Marshal(d)
	if e != nil || len(b) > maxBytes {
		return accounts.ErrInvalid
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	tmp := ".nasctl-users-" + rev + ".tmp"
	f, e := s.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	defer s.root.Remove(tmp)
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = s.root.Rename(tmp, accounts.FileName); e != nil {
		return e
	}
	dir, e := s.root.Open(".")
	if e != nil {
		return e
	}
	e = dir.Sync()
	_ = dir.Close()
	if e != nil {
		return e
	}
	s.info = nil
	return errors.Join(s.refresh(), s.enabledMarker())
}

// EnsureSetupKey creates a local-only one-time first-admin challenge. It never
// prints it, and it remains unusable after the first account has been saved.
func (s *Store) EnsureSetupKey(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, e := s.writer()
	if e != nil {
		return e
	}
	defer unlock()
	if e = s.refresh(); e != nil {
		return e
	}
	if len(s.data.Users) > 0 {
		return nil
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if f, e := s.root.OpenFile(accounts.SetupFile, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0); e == nil {
		defer f.Close()
		i, e := f.Stat()
		if e != nil || !i.Mode().IsRegular() || i.Mode().Perm()&0077 != 0 || i.Size() != 44 {
			return accounts.ErrInvalid
		}
		b, e := io.ReadAll(io.LimitReader(f, 45))
		if e != nil || !validSetupKey(b) {
			return accounts.ErrInvalid
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	var raw [32]byte
	if _, e = rand.Read(raw[:]); e != nil {
		return e
	}
	value := base64.RawURLEncoding.EncodeToString(raw[:]) + "\n"
	f, e := s.root.OpenFile(accounts.SetupFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	_, e = f.WriteString(value)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	dir, e := s.root.Open(".")
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Store) SetupKey(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return "", e
	}
	if e := s.refresh(); e != nil {
		return "", e
	}
	if len(s.data.Users) > 0 {
		return "", accounts.ErrConflict
	}
	f, e := s.root.OpenFile(accounts.SetupFile, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return "", e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Mode().Perm()&0077 != 0 || i.Size() != 44 {
		return "", accounts.ErrInvalid
	}
	b, e := io.ReadAll(io.LimitReader(f, 45))
	if e != nil || !validSetupKey(b) {
		return "", accounts.ErrInvalid
	}
	return string(b[:43]), nil
}
