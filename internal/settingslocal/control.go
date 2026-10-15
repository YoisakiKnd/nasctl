package settingslocal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
	"github.com/YoisakiKnd/nasctl/internal/settings"
)

type Control struct {
	mu            sync.Mutex
	source, state string
	active        config.NASOverrides
}

func New(cfg *config.Config) *Control {
	return &Control{source: cfg.Source, state: cfg.Paths.StateDir, active: config.Options(cfg)}
}
func view(o config.NASOverrides) settings.View {
	a := o.Anime
	roots := append([]config.HubStorage{}, o.Storage...)
	return settings.View{Storage: roots, Files: o.Files, Jobs: o.Jobs, Anime: settings.Anime{Enabled: a.Enabled, AutoPoll: a.AutoPoll, PollIntervalSeconds: a.PollIntervalSeconds, MaxItemsPerFeed: a.MaxItemsPerFeed, QBittorrentURL: a.QBittorrentURL, DownloadDir: a.DownloadDir, QBittorrentDownloadDir: a.QBittorrentDownloadDir, LibraryDir: a.LibraryDir}, GuardMemory: o.GuardMemory}
}
func (c *Control) read() (config.NASOverrides, string, error) {
	var zero config.NASOverrides
	f, e := os.OpenFile(c.source, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return zero, "", e
	}
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Size() > 1<<20 {
		f.Close()
		return zero, "", errors.New("settings: base configuration exceeds 1 MiB or is not regular")
	}
	b, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	f.Close()
	if e != nil || len(b) > 1<<20 {
		return zero, "", errors.New("settings: base configuration unreadable")
	}
	cfg, e := config.Parse(string(b))
	if e != nil {
		return zero, "", errors.New("settings: base configuration invalid")
	}
	if filepath.Clean(cfg.Paths.StateDir) != filepath.Clean(c.state) {
		return zero, "", settings.ErrConflict
	}
	extra, e := config.ReadNASOptions(c.state)
	if e != nil && !os.IsNotExist(e) {
		return zero, "", e
	}
	if e == nil {
		o, e := config.DecodeNASOptions(extra)
		if e != nil {
			return zero, "", e
		}
		if e = config.ApplyNASOptions(cfg, o); e != nil {
			return zero, "", errors.New("settings: saved configuration invalid")
		}
	}
	h := sha256.New()
	h.Write(b)
	h.Write([]byte{0})
	h.Write(extra)
	return config.Options(cfg), hex.EncodeToString(h.Sum(nil)), nil
}
func (c *Control) snapshot(o config.NASOverrides, rev string) settings.Snapshot {
	a, _ := json.Marshal(o)
	b, _ := json.Marshal(c.active)
	return settings.Snapshot{Revision: rev, Saved: view(o), Active: view(c.active), RestartRequired: !bytes.Equal(a, b), Secrets: map[string]bool{"username": o.Anime.Username != "", "password": o.Anime.Password != "", "tmdb_token": o.Anime.TMDBToken != ""}}
}
func (c *Control) Get(ctx context.Context) (settings.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return settings.Snapshot{}, e
	}
	o, r, e := c.read()
	if e != nil {
		return settings.Snapshot{}, e
	}
	return c.snapshot(o, r), nil
}
func (c *Control) candidate(ctx context.Context, req settings.Request) (*config.Config, config.NASOverrides, string, error) {
	var zero config.NASOverrides
	if e := ctx.Err(); e != nil {
		return nil, zero, "", e
	}
	o, rev, e := c.read()
	if e != nil {
		return nil, zero, "", e
	}
	if req.Revision != rev || len(rev) != 64 {
		return nil, zero, "", settings.ErrConflict
	}
	v := req.Settings
	o.Storage = v.Storage
	o.Files = v.Files
	o.Jobs = v.Jobs
	o.GuardMemory = v.GuardMemory
	a := v.Anime
	o.Anime.Enabled = a.Enabled
	o.Anime.AutoPoll = a.AutoPoll
	o.Anime.PollIntervalSeconds = a.PollIntervalSeconds
	o.Anime.MaxItemsPerFeed = a.MaxItemsPerFeed
	o.Anime.QBittorrentURL = a.QBittorrentURL
	o.Anime.DownloadDir = a.DownloadDir
	o.Anime.QBittorrentDownloadDir = a.QBittorrentDownloadDir
	o.Anime.LibraryDir = a.LibraryDir
	for key, s := range req.Secrets {
		var p *string
		switch key {
		case "username":
			p = &o.Anime.Username
		case "password":
			p = &o.Anime.Password
		case "tmdb_token":
			p = &o.Anime.TMDBToken
		default:
			return nil, zero, "", errors.New("settings: unknown secret field")
		}
		if len(s.Value) > 4096 || strings.ContainsAny(s.Value, "\x00\r\n") {
			return nil, zero, "", errors.New("settings: invalid secret")
		}
		switch s.Action {
		case "keep":
			if s.Value != "" {
				return nil, zero, "", errors.New("settings: keep must not submit a value")
			}
		case "clear":
			if s.Value != "" {
				return nil, zero, "", errors.New("settings: clear must not submit a value")
			}
			*p = ""
		case "set":
			if s.Value == "" {
				return nil, zero, "", errors.New("settings: new secret cannot be empty")
			}
			*p = s.Value
		default:
			return nil, zero, "", errors.New("settings: choose keep, set or clear")
		}
	}
	cfg, e := config.Load(c.source)
	if e != nil {
		return nil, zero, "", errors.New("settings: base configuration invalid")
	}
	if e = config.ApplyNASOptions(cfg, o); e != nil {
		return nil, zero, "", e
	}
	if e = validateAlbums(cfg); e != nil {
		return nil, zero, "", e
	}
	return cfg, config.Options(cfg), rev, nil
}

func validateAlbums(cfg *config.Config) error {
	f, e := os.OpenFile(filepath.Join(cfg.Paths.StateDir, "album-directories.json"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Size() > 65536 {
		return errors.New("settings: saved album directories invalid")
	}
	var rows []album.Directory
	d := json.NewDecoder(io.LimitReader(f, 65537))
	d.DisallowUnknownFields()
	if d.Decode(&rows) != nil || d.Decode(new(any)) != io.EOF || len(rows) > 64 {
		return errors.New("settings: saved album directories invalid")
	}
	return ValidateAlbumDirectories(cfg, rows)
}

func ValidateAlbumDirectories(cfg *config.Config, rows []album.Directory) error {
	if len(rows) > 64 {
		return errors.New("settings: too many managed album directories")
	}
	copyConfig := *cfg
	copyConfig.Collections = append([]config.Collection{}, cfg.Collections...)
	for _, row := range rows {
		root := ""
		for _, r := range cfg.Hub.Storage {
			if r.ID == row.Root {
				root = r.Path
				break
			}
		}
		if root == "" {
			return errors.New("settings: remove managed album directories before removing their storage root")
		}
		p, e := fsutil.SafeRelJoin(root, row.Path)
		if e != nil {
			return e
		}
		copyConfig.Collections = append(copyConfig.Collections, config.Collection{Code: row.Code, Name: row.Name, Root: p, Enabled: row.Enabled, AllowSharedVolume: row.AllowSharedVolume})
	}
	return copyConfig.Finalize()
}
func (c *Control) Validate(ctx context.Context, req settings.Request) (settings.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, o, r, e := c.candidate(ctx, req)
	if e != nil {
		return settings.Snapshot{}, e
	}
	return c.snapshot(o, r), nil
}
func (c *Control) Save(ctx context.Context, req settings.Request) (settings.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	unlock, e := LockConfiguration(c.state)
	if e != nil {
		return settings.Snapshot{}, e
	}
	defer unlock()
	_, o, _, e := c.candidate(ctx, req)
	if e != nil {
		return settings.Snapshot{}, e
	}
	b, e := json.Marshal(o)
	if e != nil || len(b) > 65536 {
		return settings.Snapshot{}, errors.New("settings: configuration exceeds 64 KiB")
	}
	if e = ctx.Err(); e != nil {
		return settings.Snapshot{}, e
	}
	if e = fsutil.AtomicWrite(filepath.Join(c.state, config.NASSettingsFile), 0600, func(w io.Writer) error { _, e := w.Write(b); return e }); e != nil {
		return settings.Snapshot{}, e
	}
	saved, r, e := c.read()
	if e != nil {
		return settings.Snapshot{}, e
	}
	return c.snapshot(saved, r), nil
}
