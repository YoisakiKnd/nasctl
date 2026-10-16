package hubweb

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// rememberTTL is the lifetime of a session the member explicitly asked to keep.
// Ordinary sessions keep the 12-hour lifetime and die with the process.
const rememberTTL = 30 * 24 * time.Hour

// rememberFile is where kept account sessions survive a restart. It holds only
// SHA-256 digests of the cookies, so a copy of the file cannot be replayed as
// a session. Each entry is bound to the account's version: a password, role
// or status change, or deleting the account, rejects it on the next request.
const rememberFile = "remembered-sessions.json"

type rememberedSession struct {
	Key     string `json:"key"`
	User    string `json:"user"`
	Epoch   string `json:"epoch"`
	Expires int64  `json:"expires"`
}

type rememberedFile struct {
	Version  int                 `json:"version"`
	Sessions []rememberedSession `json:"sessions"`
}

// EnableRememberedSessions loads kept sessions from dir and saves later
// changes there. Without it, "keep me signed in" lasts until the process ends.
func (s *Server) EnableRememberedSessions(dir string) error {
	a := &s.auth
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rememberPath = filepath.Join(dir, rememberFile)
	f, err := os.OpenFile(a.rememberPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 64<<10 {
		return errors.New("remembered sessions file must be a private regular file under 64 KiB")
	}
	var file rememberedFile
	d := json.NewDecoder(io.LimitReader(f, 64<<10))
	d.DisallowUnknownFields()
	if err = d.Decode(&file); err != nil || file.Version != 1 {
		return errors.New("remembered sessions file is not valid; delete it to sign everyone out")
	}
	now := time.Now()
	for _, r := range file.Sessions {
		raw, err := hex.DecodeString(r.Key)
		expires := time.Unix(r.Expires, 0)
		if err != nil || len(raw) != 32 || r.User == "" || r.Epoch == "" || !now.Before(expires) || expires.After(now.Add(rememberTTL)) || len(a.sessions) >= 64 {
			continue
		}
		var key [32]byte
		copy(key[:], raw)
		a.ensureMaps()
		a.sessions[key] = expires
		a.users[key] = userSession{ID: r.User, Epoch: r.Epoch}
		a.remembered[key] = true
	}
	return nil
}

func (a *browserAuth) ensureMaps() {
	if a.sessions == nil {
		a.sessions = make(map[[32]byte]time.Time)
	}
	if a.users == nil {
		a.users = map[[32]byte]userSession{}
	}
	if a.remembered == nil {
		a.remembered = map[[32]byte]bool{}
	}
}

// drop removes one session. The caller holds a.mu.
func (a *browserAuth) drop(key [32]byte) {
	delete(a.sessions, key)
	delete(a.users, key)
	if a.remembered[key] {
		delete(a.remembered, key)
		a.saveRemembered()
	}
}

// saveRemembered rewrites the kept-session file atomically. The caller holds
// a.mu. A failed write leaves sessions working until the next restart; it is
// not a reason to refuse a login.
func (a *browserAuth) saveRemembered() {
	if a.rememberPath == "" {
		return
	}
	file := rememberedFile{Version: 1, Sessions: []rememberedSession{}}
	for key := range a.remembered {
		u := a.users[key]
		file.Sessions = append(file.Sessions, rememberedSession{Key: hex.EncodeToString(key[:]), User: u.ID, Epoch: u.Epoch, Expires: a.sessions[key].Unix()})
	}
	body, err := json.Marshal(file)
	if err != nil {
		return
	}
	dir := filepath.Dir(a.rememberPath)
	tmp, err := os.CreateTemp(dir, ".remembered-sessions-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	defer os.Remove(name)
	if tmp.Chmod(0o600) != nil {
		tmp.Close()
		return
	}
	_, err = tmp.Write(body)
	if err = errors.Join(err, tmp.Sync(), tmp.Close()); err != nil {
		return
	}
	_ = os.Rename(name, a.rememberPath)
}
