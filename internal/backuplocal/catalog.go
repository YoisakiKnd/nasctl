package backuplocal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/backups"
	"github.com/YoisakiKnd/nasctl/internal/files"
)

var ErrBusy = backups.ErrBusy
var ErrInvalid = backups.ErrInvalid
var ErrCorrupt = backups.ErrCorrupt
var ErrFull = backups.ErrFull
var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
var versionID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Control struct {
	mu      sync.Mutex
	plansMu sync.RWMutex
	busy    atomic.Bool
	files   *files.Manager
	state   *os.Root
	plans   []backups.Plan
}

func New(stateDirectory string, fm *files.Manager) (*Control, error) {
	if err := os.MkdirAll(stateDirectory, 0750); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(stateDirectory)
	if err != nil {
		return nil, err
	}
	c := &Control{files: fm, state: root, plans: []backups.Plan{}}
	f, err := root.OpenFile("backup-plans.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err == nil {
		defer f.Close()
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
			root.Close()
			return nil, ErrInvalid
		}
		d := json.NewDecoder(io.LimitReader(f, 65537))
		d.DisallowUnknownFields()
		if d.Decode(&c.plans) != nil || d.Decode(new(any)) != io.EOF || len(c.plans) > 16 {
			root.Close()
			return nil, ErrInvalid
		}
	}
	if err != nil && !os.IsNotExist(err) {
		root.Close()
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range c.plans {
		if _, err = c.validate(p, false); err != nil || seen[p.ID] {
			root.Close()
			return nil, ErrInvalid
		}
		seen[p.ID] = true
	}
	return c, nil
}
func (c *Control) Close() error { return c.state.Close() }
func (c *Control) Settings() backups.Settings {
	c.plansMu.RLock()
	defer c.plansMu.RUnlock()
	digests := map[string]string{}
	for _, p := range c.plans {
		digests[p.ID] = p.Digest()
	}
	return backups.Settings{Plans: append([]backups.Plan{}, c.plans...), Digests: digests, Busy: c.busy.Load(), MaxPlans: 16}
}
func (c *Control) begin() bool {
	if !c.mu.TryLock() {
		return false
	}
	c.busy.Store(true)
	return true
}
func (c *Control) end() { c.busy.Store(false); c.mu.Unlock() }
func (c *Control) validate(p backups.Plan, available bool) (backups.Plan, error) {
	l := files.DefaultLimits()
	// Persisted catalogs use the structural hard ceiling. Execution still
	// checks the current local policy, including when it has been lowered.
	l.CopyMaxBytes = files.MaxTransferBytes
	if available {
		l = c.files.Limits()
	}
	if !identifier.MatchString(p.ID) || !identifier.MatchString(p.Root) || !identifier.MatchString(p.ToRoot) || len(strings.TrimSpace(p.Name)) == 0 || len(p.Name) > 128 || strings.ContainsAny(p.Name, "\x00\r\n") || p.Root == p.ToRoot || p.Limit < 1 || p.Limit > l.CopyMaxEntries || p.MaxBytes < 1 || p.MaxBytes > l.CopyMaxBytes || p.Keep < 1 || p.Keep > 32 {
		return p, ErrInvalid
	}
	runtime, err := time.ParseDuration(p.MaxRuntime)
	if err != nil || runtime < time.Second || runtime > 2*time.Hour {
		return p, ErrInvalid
	}
	if p.Path == "" || p.Path == "/" {
		p.Path = "."
	}
	if path.IsAbs(p.Path) || path.Clean(p.Path) != p.Path || p.Path == ".." || strings.HasPrefix(p.Path, "../") || len(p.Path) > 1024 || strings.ContainsRune(p.Path, 0) {
		return p, ErrInvalid
	}
	for _, part := range strings.Split(p.Path, "/") {
		if part == ".nasctl-backups" || part == ".nasctl-trash" || part == ".nasctl-staging" || part == ".nasctl-copy" || part == ".nasctl-write.lock" {
			return p, ErrInvalid
		}
	}
	if !available {
		return p, nil
	}
	var source, destination *files.RootInfo
	for _, r := range c.files.Roots() {
		v := r
		if r.ID == p.Root {
			source = &v
		}
		if r.ID == p.ToRoot {
			destination = &v
		}
	}
	if source == nil || destination == nil || !destination.Writable {
		return p, ErrInvalid
	}
	if available {
		if !source.Available || !destination.Available {
			return p, files.ErrRootUnavailable
		}
		if _, err := c.files.Stat(p.Root, p.Path); err != nil {
			return p, err
		}
	}
	return p, nil
}
func (c *Control) Save(ctx context.Context, p backups.Plan) error {
	if !c.begin() {
		return ErrBusy
	}
	defer c.end()
	c.plansMu.Lock()
	defer c.plansMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := c.validate(p, true)
	if err != nil {
		return err
	}
	p.SourceScope, err = c.files.StorageScope(p.Root, p.Path)
	if err != nil {
		return err
	}
	p.TargetScope, err = c.files.StorageScope(p.ToRoot, ".")
	if err != nil {
		return err
	}
	plans := append([]backups.Plan{}, c.plans...)
	found := false
	for i, old := range plans {
		if old.ID == p.ID {
			plans[i] = p
			found = true
		}
	}
	if !found {
		if len(plans) >= 16 {
			return ErrFull
		}
		plans = append(plans, p)
	}
	return c.savePlans(plans)
}
func (c *Control) RemovePlan(ctx context.Context, id string) error {
	if !c.begin() {
		return ErrBusy
	}
	defer c.end()
	c.plansMu.Lock()
	defer c.plansMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !identifier.MatchString(id) {
		return ErrInvalid
	}
	plans := []backups.Plan{}
	found := false
	for _, p := range c.plans {
		if p.ID == id {
			found = true
		} else {
			plans = append(plans, p)
		}
	}
	if !found {
		return files.ErrNotExist
	}
	return c.savePlans(plans)
}
func (c *Control) savePlans(plans []backups.Plan) error {
	b, err := json.Marshal(plans)
	if err != nil || len(b) > 65536 {
		return ErrFull
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	name := "backup-plans-" + id + ".tmp"
	f, err := c.state.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer c.state.Remove(name)
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = c.state.Rename(name, "backup-plans.json"); err != nil {
		return err
	}
	if err = syncRoot(c.state); err != nil {
		return err
	}
	c.plans = plans
	return nil
}
func randomID() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func syncRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (c *Control) plan(id string) (backups.Plan, error) {
	c.plansMu.RLock()
	defer c.plansMu.RUnlock()
	for _, p := range c.plans {
		if p.ID == id {
			p, err := c.validate(p, true)
			if err != nil {
				return p, err
			}
			source, err := c.files.StorageScope(p.Root, p.Path)
			if err != nil {
				return p, err
			}
			target, err := c.files.StorageScope(p.ToRoot, ".")
			if err != nil {
				return p, err
			}
			if p.SourceScope == "" || p.TargetScope == "" || p.SourceScope != source || p.TargetScope != target {
				return p, ErrInvalid
			}
			return p, nil
		}
	}
	return backups.Plan{}, files.ErrNotExist
}
func normalDestination(p string) (string, error) {
	if path.IsAbs(p) || p == "" || p == "." || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || len(p) > 1024 {
		return "", ErrInvalid
	}
	for _, s := range strings.Split(p, "/") {
		if strings.HasPrefix(s, ".nasctl-") {
			return "", ErrInvalid
		}
	}
	return p, nil
}
