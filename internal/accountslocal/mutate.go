package accountslocal

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
)

const EnabledFile = ".nasctl-users.enabled"

func (s *Store) enabledMarker() error {
	f, e := s.root.OpenFile(EnabledFile, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Size() != 0 || i.Mode().Perm()&0077 != 0 {
		return accounts.ErrInvalid
	}
	if e = f.Sync(); e != nil {
		return e
	}
	dir, e := s.root.Open(".")
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func requestRecord(r accounts.Request) (record, error) {
	if !usernamePattern.MatchString(r.Username) || !roleValid(r.Role) {
		return record{}, accounts.ErrInvalid
	}
	display := strings.TrimSpace(r.DisplayName)
	if display == "" {
		display = r.Username
	}
	if !validDisplay(display) {
		return record{}, accounts.ErrInvalid
	}
	return record{ID: r.ID, Username: r.Username, DisplayName: display, Role: r.Role, Disabled: r.Disabled}, nil
}
func (s *Store) CreateFirst(ctx context.Context, r accounts.Setup) (accounts.User, error) {
	initial, e := requestRecord(accounts.Request{Username: r.Username, DisplayName: r.DisplayName, Role: "admin"})
	if e != nil {
		return accounts.User{}, e
	}
	salt, hash, e := s.password(ctx, r.Password)
	if e != nil {
		return accounts.User{}, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, e := s.writer()
	if e != nil {
		return accounts.User{}, e
	}
	defer unlock()
	if e = s.refresh(); e != nil {
		return accounts.User{}, e
	}
	if len(s.data.Users) > 0 {
		return accounts.User{}, accounts.ErrConflict
	}
	f, e := s.root.OpenFile(accounts.SetupFile, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return accounts.User{}, accounts.ErrCredential
	}
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Mode().Perm()&0077 != 0 || i.Size() != 44 {
		f.Close()
		return accounts.User{}, accounts.ErrInvalid
	}
	b, e := io.ReadAll(io.LimitReader(f, 45))
	f.Close()
	if e != nil || !validSetupKey(b) {
		return accounts.User{}, accounts.ErrInvalid
	}
	expected := sha256.Sum256(b[:43])
	got := sha256.Sum256([]byte(r.Code))
	if len(r.Code) != 43 || subtle.ConstantTimeCompare(expected[:], got[:]) != 1 {
		return accounts.User{}, accounts.ErrCredential
	}
	initial.ID, e = randomHex()
	if e != nil {
		return accounts.User{}, e
	}
	initial.Epoch, e = randomHex()
	if e != nil {
		return accounts.User{}, e
	}
	initial.CreatedAt = time.Now().Unix()
	initial.Algorithm = "pbkdf2-sha256"
	initial.Iterations = iterations
	initial.Salt = salt
	initial.Hash = hash
	if e = s.publish(ctx, document{Version: 1, Users: []record{initial}}); e != nil {
		return accounts.User{}, e
	}
	// A stale key left by a crash is still refused once a user store exists.
	_ = s.root.Remove(accounts.SetupFile)
	return public(initial), nil
}
func (s *Store) Save(ctx context.Context, r accounts.Request) (accounts.User, error) {
	requested, e := requestRecord(r)
	if e != nil {
		return accounts.User{}, e
	}
	if r.ID != "" && !hexID(r.ID) {
		return accounts.User{}, accounts.ErrInvalid
	}
	if r.ID == "" && r.Password == "" {
		return accounts.User{}, accounts.ErrInvalid
	}
	var salt, hash string
	if r.Password != "" {
		salt, hash, e = s.password(ctx, r.Password)
		if e != nil {
			return accounts.User{}, e
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, e := s.writer()
	if e != nil {
		return accounts.User{}, e
	}
	defer unlock()
	if e = s.refresh(); e != nil {
		return accounts.User{}, e
	}
	if len(s.data.Users) == 0 || r.Revision != s.data.Revision || !hexID(r.Revision) {
		return accounts.User{}, accounts.ErrConflict
	}
	d := s.data
	d.Users = append([]record{}, d.Users...)
	index := -1
	for i, u := range d.Users {
		if u.ID == r.ID {
			index = i
		}
		if u.Username == r.Username && u.ID != r.ID {
			return accounts.User{}, accounts.ErrConflict
		}
	}
	if r.ID != "" && index < 0 {
		return accounts.User{}, accounts.ErrConflict
	}
	if index < 0 {
		if len(d.Users) >= accounts.MaxUsers {
			return accounts.User{}, accounts.ErrInvalid
		}
		requested.ID, e = randomHex()
		if e != nil {
			return accounts.User{}, e
		}
		requested.CreatedAt = time.Now().Unix()
		d.Users = append(d.Users, requested)
		index = len(d.Users) - 1
	} else {
		current := d.Users[index]
		if current.Username != r.Username {
			return accounts.User{}, errors.New("accounts: username is immutable; create a separate account instead")
		}
		requested.CreatedAt = current.CreatedAt
		requested.Algorithm = current.Algorithm
		requested.Iterations = current.Iterations
		requested.Salt = current.Salt
		requested.Hash = current.Hash
	}
	requested.Epoch, e = randomHex()
	if e != nil {
		return accounts.User{}, e
	}
	if r.Password != "" {
		requested.Algorithm = "pbkdf2-sha256"
		requested.Iterations = iterations
		requested.Salt = salt
		requested.Hash = hash
	}
	d.Users[index] = requested
	if e = s.publish(ctx, d); e != nil {
		return accounts.User{}, e
	}
	return public(requested), nil
}
func (s *Store) Remove(ctx context.Context, id, revision string) error {
	if !hexID(id) || !hexID(revision) {
		return accounts.ErrInvalid
	}
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
	if s.data.Revision != revision {
		return accounts.ErrConflict
	}
	d := s.data
	d.Users = []record{}
	found := false
	for _, u := range s.data.Users {
		if u.ID == id {
			found = true
		} else {
			d.Users = append(d.Users, u)
		}
	}
	if !found {
		return accounts.ErrConflict
	}
	if len(d.Users) == 0 {
		return accounts.ErrLastAdmin
	}
	return s.publish(ctx, d)
}
