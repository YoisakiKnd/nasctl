// Package desktopapps persists explicitly configured links, never launches or
// proxies applications. Templates resolve the NAS host in the user's browser.
package desktopapps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/YoisakiKnd/nasctl/internal/fsutil"
)

var ErrInvalid = errors.New("apps: invalid application link")
var ErrConflict = errors.New("apps: configuration changed")
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

type App struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	Icon  string `json:"icon"`
	Color string `json:"color"`
}
type View struct {
	Apps     []App  `json:"apps"`
	Revision string `json:"revision"`
}
type Control interface {
	Get(context.Context) (View, error)
	Save(context.Context, string, []App) (View, error)
}
type Store struct {
	mu   sync.Mutex
	path string
}

func New(state string) *Store { return &Store{path: filepath.Join(state, "desktop-apps.json")} }
func Validate(a App) error {
	if !idPattern.MatchString(a.ID) || strings.TrimSpace(a.Name) == "" || len(a.Name) > 64 || strings.ContainsAny(a.Name, "\x00\r\n") || len(a.URL) > 2048 || strings.ContainsAny(a.URL, "\x00\r\n\\") {
		return ErrInvalid
	}
	candidate := strings.ReplaceAll(a.URL, "{host}", "nas.example")
	if strings.ContainsAny(candidate, "{}") {
		return ErrInvalid
	}
	u, e := url.Parse(candidate)
	if e != nil || u.User != nil || u.Opaque != "" {
		return ErrInvalid
	}
	if u.Scheme == "" {
		if !strings.HasPrefix(candidate, "/") || strings.HasPrefix(candidate, "//") || u.Host != "" {
			return ErrInvalid
		}
	} else if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ErrInvalid
	}
	for key := range u.Query() {
		switch strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", "")) {
		case "password", "passwd", "token", "accesstoken", "apikey", "secret", "authorization":
			return ErrInvalid
		}
	}
	icons := map[string]bool{"external": true, "anime": true, "cloud": true, "folder": true, "album": true, "disk": true, "home": true, "jobs": true, "play": true, "backup": true}
	colors := map[string]bool{"blue": true, "teal": true, "rose": true, "violet": true, "amber": true, "slate": true}
	if !icons[a.Icon] || !colors[a.Color] {
		return ErrInvalid
	}
	return nil
}
func (s *Store) read() (View, error) {
	v := View{Apps: []App{}}
	b := []byte("[]")
	f, e := os.OpenFile(s.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e == nil {
		defer f.Close()
		i, e := f.Stat()
		if e != nil || !i.Mode().IsRegular() || i.Size() > 65536 {
			return v, ErrInvalid
		}
		b, e = io.ReadAll(io.LimitReader(f, 65537))
		if e != nil {
			return v, e
		}
		if json.Unmarshal(b, &v.Apps) != nil || len(v.Apps) > 32 {
			return v, ErrInvalid
		}
	} else if !os.IsNotExist(e) {
		return v, e
	}
	sum := sha256.Sum256(b)
	v.Revision = hex.EncodeToString(sum[:])
	seen := map[string]bool{}
	for _, a := range v.Apps {
		if Validate(a) != nil || seen[a.ID] {
			return v, ErrInvalid
		}
		seen[a.ID] = true
	}
	return v, nil
}
func (s *Store) Get(ctx context.Context) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return View{}, e
	}
	return s.read()
}
func (s *Store) Save(ctx context.Context, revision string, apps []App) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, e := s.read()
	if e != nil {
		return old, e
	}
	if revision != old.Revision {
		return old, ErrConflict
	}
	if len(apps) > 32 {
		return old, ErrInvalid
	}
	seen := map[string]bool{}
	for _, a := range apps {
		if Validate(a) != nil || seen[a.ID] {
			return old, ErrInvalid
		}
		seen[a.ID] = true
	}
	if apps == nil {
		apps = []App{}
	}
	b, e := json.Marshal(apps)
	if e != nil {
		return old, e
	}
	if len(b) > 65536 {
		return old, ErrInvalid
	}
	if e = ctx.Err(); e != nil {
		return old, e
	}
	if e = fsutil.AtomicWriteFile(s.path, b, 0600); e != nil {
		return old, e
	}
	return s.read()
}
