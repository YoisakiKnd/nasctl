package albumlocal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
	"github.com/YoisakiKnd/nasctl/internal/logx"
	"github.com/YoisakiKnd/nasctl/internal/settingslocal"
	"golang.org/x/sys/unix"
)

type Control struct {
	mu              sync.Mutex
	base            config.Config
	directories     []album.Directory
	files           *files.Manager
	Reader          *Reader
	binary, catalog string
	busy            bool
	launcher        *albumLauncher
}

func NewControl(cfg *config.Config, fm *files.Manager, log *logx.Logger) (*Control, error) {
	binary := cfg.Hub.Album.WorkerBinary
	if binary == "" {
		if exe, err := os.Executable(); err == nil {
			binary = filepath.Join(filepath.Dir(exe), "albumctl")
		}
	}
	c := &Control{base: *cfg, files: fm, binary: binary, catalog: filepath.Join(cfg.Paths.StateDir, "album-directories.json"), directories: []album.Directory{}, launcher: newAlbumLauncher(cfg)}
	f, err := os.Open(c.catalog)
	if err == nil {
		defer f.Close()
		if info, err := f.Stat(); err != nil || info.Size() > 65536 {
			return nil, errors.New("album directory configuration exceeds 64 KiB")
		}
		d := json.NewDecoder(io.LimitReader(f, 65537))
		d.DisallowUnknownFields()
		if err = d.Decode(&c.directories); err != nil {
			return nil, errors.New("invalid saved album directory configuration")
		}
		if err = d.Decode(new(any)); err != io.EOF {
			return nil, errors.New("album directory configuration exceeds its limit")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	effective, err := c.effective(c.directories)
	if err != nil {
		return nil, err
	}
	c.Reader = New(&effective, log)
	c.Reader.files = fm
	return c, nil
}

func (c *Control) effective(dirs []album.Directory) (config.Config, error) {
	cfg := c.base
	cfg.Collections = append([]config.Collection(nil), cfg.Collections...)
	// Finalize normalizes slice elements in place. Never share those writable
	// slices with an active immutable reader configuration.
	cfg.Hub.Storage = append([]config.HubStorage(nil), cfg.Hub.Storage...)
	cfg.Hub.Agents = append([]config.Agent(nil), cfg.Hub.Agents...)
	if len(dirs) > 64 {
		return cfg, errors.New("at most 64 managed album directories are allowed")
	}
	for _, d := range dirs {
		root := ""
		for _, r := range cfg.Hub.Storage {
			if r.ID == d.Root {
				root = r.Path
				break
			}
		}
		if root == "" {
			return cfg, errors.New("album directory refers to an unknown storage root")
		}
		p, err := fsutil.SafeRelJoin(root, d.Path)
		if err != nil {
			return cfg, errors.New("album directory must stay within its storage root")
		}
		if d.Name == "" || len(d.Name) > 128 {
			return cfg, errors.New("album directory name must contain 1–128 bytes")
		}
		cfg.Collections = append(cfg.Collections, config.Collection{Code: d.Code, Name: d.Name, Root: p, Enabled: d.Enabled, AllowSharedVolume: d.AllowSharedVolume})
	}
	if err := cfg.Finalize(); err != nil {
		return cfg, errors.New("album directory configuration is invalid; check code, duplicates and state/cache separation")
	}
	return cfg, nil
}

func (c *Control) Configuration() album.Configuration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := album.Configuration{Directories: []album.Directory{}, Busy: c.busy}
	if info, err := os.Stat(c.binary); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
		out.WorkerAvailable = true
	}
	for _, d := range c.base.Collections {
		out.Directories = append(out.Directories, album.Directory{Code: d.Code, Name: d.Name, Path: d.Root, Enabled: d.Enabled, AllowSharedVolume: d.AllowSharedVolume})
	}
	for _, d := range c.directories {
		d.Managed = true
		out.Directories = append(out.Directories, d)
	}
	return out
}

func (c *Control) Configure(ctx context.Context, d album.Directory) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy {
		return errors.New("album task is running; stop it before changing directories")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entry, err := c.files.Stat(d.Root, d.Path)
	if err != nil || !entry.IsDir {
		return errors.New("choose an available directory in a configured storage root")
	}
	d.Path = strings.TrimPrefix(entry.Path, "/")
	if d.Path == "" {
		d.Path = "."
	}
	d.Managed = true
	dirs := append([]album.Directory(nil), c.directories...)
	found := false
	for i, v := range dirs {
		if v.Code == d.Code {
			dirs[i] = d
			found = true
			break
		}
	}
	if !found {
		dirs = append(dirs, d)
	}
	return c.save(dirs)
}
func (c *Control) RemoveDirectory(ctx context.Context, code string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy {
		return errors.New("album task is running; stop it before changing directories")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dirs := []album.Directory{}
	found := false
	for _, d := range c.directories {
		if d.Code == code {
			found = true
		} else {
			dirs = append(dirs, d)
		}
	}
	if !found {
		return errors.New("directory is defined in the main configuration; edit that configuration instead")
	}
	return c.save(dirs)
}
func (c *Control) save(dirs []album.Directory) error {
	unlock, err := settingslocal.LockConfiguration(c.base.Paths.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	if c.base.Source != "" {
		future, e := config.LoadNAS(c.base.Source)
		if e != nil {
			return errors.New("album configuration: saved NAS settings cannot be validated")
		}
		if future.Paths.StateDir != c.base.Paths.StateDir {
			return errors.New("album configuration: base state location changed; restart before editing directories")
		}
		if e = settingslocal.ValidateAlbumDirectories(future, dirs); e != nil {
			return e
		}
		for _, row := range dirs {
			unchanged := false
			for _, old := range c.directories {
				if old == row {
					unchanged = true
					break
				}
			}
			if unchanged {
				continue
			}
			active, pending := "", ""
			for _, r := range c.base.Hub.Storage {
				if r.ID == row.Root {
					active = r.Path
				}
			}
			for _, r := range future.Hub.Storage {
				if r.ID == row.Root {
					pending = r.Path
				}
			}
			if active == "" || pending != active {
				return errors.New("album configuration: storage location has a pending change; restart before adding or editing its directories")
			}
		}
	}
	cfg, err := c.effective(dirs)
	if err != nil {
		return err
	}
	data, err := json.Marshal(dirs)
	if err != nil || len(data) > 65536 {
		return errors.New("album directory configuration exceeds 64 KiB")
	}
	if err = fsutil.AtomicWriteFile(c.catalog, data, 0600); err != nil {
		return err
	}
	if err = c.Reader.Update(&cfg); err != nil {
		return err
	}
	c.directories = dirs
	return nil
}

func (c *Control) Run(ctx context.Context, w album.Work, progress func(album.WorkProgress)) (result album.WorkResult, workErr error) {
	duration, err := album.ValidateWork(w)
	if err != nil {
		return album.WorkResult{}, err
	}
	c.mu.Lock()
	if c.busy {
		c.mu.Unlock()
		return album.WorkResult{}, errors.New("another album task is running")
	}
	cfg, err := c.effective(c.directories)
	if err == nil && (w.Operation == "album.scan" || w.Operation == "album.thumbs") {
		col, ok := cfg.Collection(w.Collection)
		if !ok || !col.Enabled {
			err = errors.New("album collection does not exist or is disabled")
		}
	}
	if err != nil {
		c.mu.Unlock()
		return album.WorkResult{}, err
	}
	c.busy = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.busy = false; c.mu.Unlock() }()
	if info, err := os.Stat(c.binary); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return album.WorkResult{}, errors.New("album worker is unavailable; install albumctl beside nasctl or configure hub.album.worker_binary")
	}
	// Workers receive only album settings, never NAS user/agent/downloader
	// credentials. The private snapshot lives outside all browsable media roots.
	cfg.Hub = config.Hub{}
	cfg.Server.Token = ""
	cfg.Paths.LogFile = ""
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(cfg); err != nil {
		return album.WorkResult{}, err
	}
	f, err := os.CreateTemp(cfg.Paths.StateDir, ".album-worker-*.toml")
	if err != nil {
		return album.WorkResult{}, err
	}
	name := f.Name()
	defer os.Remove(name)
	_, err = f.Write(encoded.Bytes())
	closeErr := f.Close()
	if err != nil {
		return album.WorkResult{}, err
	}
	if closeErr != nil {
		return album.WorkResult{}, closeErr
	}
	args := []string{strings.TrimPrefix(w.Operation, "album."), "--config", name}
	if w.Operation == "album.scan" || w.Operation == "album.thumbs" {
		args = append(args, "--collection", w.Collection, "--limit", strconv.Itoa(w.Limit))
	}
	if w.Operation == "album.thumbs" {
		args = append(args, "--max-runtime", w.MaxRuntime)
	}
	if w.Operation == "album.pause" && w.Reason != "" {
		args = append(args, "--reason", w.Reason)
	}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.binary, args...)
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	// Let the existing album supervisor cancel/reap its isolated decoder first.
	// The group is ours for this invocation; never adopt a PID from disk.
	cmd.Cancel = func() error { return unix.Kill(-cmd.Process.Pid, unix.SIGINT) }
	cmd.WaitDelay = 15 * time.Second
	output := &boundedOutput{limit: 4 << 10}
	cmd.Stdout = output
	cmd.Stderr = output
	var launch *admittedLaunch
	if w.Operation == "album.thumbs" {
		launch, err = c.launcher.prepare(cmd)
		if err != nil {
			return album.WorkResult{StopReason: "isolation_unavailable"}, err
		}
		defer func() {
			if e := launch.finish(); e != nil {
				result.Completed = false
				workErr = errors.Join(workErr, e)
			}
		}()
	}
	if err = cmd.Start(); err != nil {
		return album.WorkResult{}, errors.New("album worker could not start")
	}
	if err = launch.admit(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return album.WorkResult{StopReason: "isolation_unavailable"}, fmt.Errorf("album worker could not enter the delegated task cgroup: %w", err)
	}
	report := func() {
		if progress == nil {
			return
		}
		p := album.WorkProgress{PID: cmd.Process.Pid, PGID: cmd.Process.Pid, Detail: "正在执行有界相册任务"}
		queryCtx, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		if task, ok := c.Reader.task(queryCtx, cmd.Process.Pid); ok {
			p.Processed = task.Processed
			p.Total = task.LimitN
			p.Detail = task.Detail
		}
		progress(p)
	}
	report()
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
waiting:
	for {
		select {
		case err = <-wait:
			break waiting
		case <-ticker.C:
			if ctx.Err() == nil {
				report()
			}
		}
	}
	if ctx.Err() == nil {
		report()
	}
	if ctx.Err() != nil {
		return album.WorkResult{StopReason: "interrupted", Detail: "相册任务已请求停止；已保存的索引和缩略图保留"}, ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return album.WorkResult{StopReason: fmt.Sprintf("worker_exit_%d", exit.ExitCode()), Detail: output.String()}, fmt.Errorf("album task stopped (exit %d): %s", exit.ExitCode(), output.String())
		}
		return album.WorkResult{}, errors.New("album worker could not start")
	}
	return album.WorkResult{Completed: true, Detail: output.String()}, nil
}

type boundedOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	limit  int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	// Retain the tail, so verbose per-file logs cannot displace the final
	// bounded summary. Memory stays independent of the number of files.
	if len(p) >= b.limit {
		b.buffer.Reset()
		p = p[len(p)-b.limit:]
	} else if excess := b.buffer.Len() + len(p) - b.limit; excess > 0 {
		b.buffer.Next(excess)
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}
func (b *boundedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.ToValidUTF8(b.buffer.String(), "�")
}
