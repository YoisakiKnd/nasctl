package albumlocal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	"github.com/YoisakiKnd/nasctl/internal/scan"
	"github.com/YoisakiKnd/nasctl/internal/settingslocal"
	"github.com/YoisakiKnd/nasctl/internal/store"
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
	private         []album.Folder
}

func NewControl(cfg *config.Config, fm *files.Manager, log *logx.Logger) (*Control, error) {
	// Bounded album work re-executes this binary as `nasctl album ...`.
	// worker_binary remains an explicit override for tests and packaging.
	binary := cfg.Hub.Album.WorkerBinary
	if binary == "" {
		if exe, err := os.Executable(); err == nil {
			binary = exe
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				binary = resolved
			}
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
	c.private = privateFolders(effective)
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
		cfg.Collections = append(cfg.Collections, config.Collection{Code: d.Code, Name: d.Name, Root: p, Enabled: d.Enabled, AllowSharedVolume: d.AllowSharedVolume, Private: d.Private})
	}
	for i, a := range cfg.Collections {
		for _, b := range cfg.Collections[i+1:] {
			if a.Private != b.Private && (fsutil.IsWithin(a.Root, b.Root) || fsutil.IsWithin(b.Root, a.Root)) {
				return cfg, fmt.Errorf("私密目录「%s」和普通目录「%s」互相包含；私密目录必须单独存放，不能放在普通相册目录里面，也不能包含普通相册目录", privateName(a, b), publicName(a, b))
			}
		}
	}
	if err := cfg.Finalize(); err != nil {
		return cfg, errors.New("album directory configuration is invalid; check code, duplicates and state/cache separation")
	}
	return cfg, nil
}

func privateName(a, b config.Collection) string {
	if a.Private {
		return a.Name
	}
	return b.Name
}
func publicName(a, b config.Collection) string {
	if a.Private {
		return b.Name
	}
	return a.Name
}

// PrivateFolders maps every private directory onto the storage root that
// holds it. A main-config directory outside every root has no file-browser
// presence, so only its album listing needs hiding.
func (c *Control) PrivateFolders() []album.Folder {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]album.Folder(nil), c.private...)
}
func privateFolders(cfg config.Config) []album.Folder {
	out := []album.Folder{}
	for _, col := range cfg.Collections {
		if !col.Private {
			continue
		}
		for _, root := range cfg.Hub.Storage {
			if !fsutil.IsWithin(root.Path, col.Root) {
				continue
			}
			rel, err := filepath.Rel(root.Path, col.Root)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if rel == "." {
				rel = ""
			}
			out = append(out, album.Folder{Root: root.ID, Path: rel})
		}
	}
	return out
}

func (c *Control) Configuration() album.Configuration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := album.Configuration{Directories: []album.Directory{}, Busy: c.busy}
	if info, err := os.Stat(c.binary); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
		out.WorkerAvailable = true
	}
	for _, d := range c.base.Collections {
		out.Directories = append(out.Directories, album.Directory{Code: d.Code, Name: d.Name, Path: d.Root, Enabled: d.Enabled, AllowSharedVolume: d.AllowSharedVolume, Private: d.Private})
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
	c.private = privateFolders(cfg)
	return nil
}

// batchPause separates batches so the resource guard sees the machine at rest.
var batchPause = 2 * time.Second

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
		return album.WorkResult{}, errors.New("album worker is unavailable; check hub.album.worker_binary or the nasctl executable")
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
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	if w.Operation != "album.scan" && w.Operation != "album.thumbs" {
		res, _, err := c.batch(ctx, w, name, progress)
		return res, err
	}
	return c.batches(ctx, w, name, progress)
}

// batches runs one directory's scan or thumbnail work to the end as a series
// of bounded worker runs. Each run handles at most w.Limit files and is
// isolated and resource-checked as before; the series stops when a run
// reports nothing left, on a failure, or when the runtime budget is spent.
func (c *Control) batches(ctx context.Context, w album.Work, name string, progress func(album.WorkProgress)) (album.WorkResult, error) {
	scanning := w.Operation == "album.scan"
	before, _, _ := c.Reader.totals(ctx, w.Collection)
	describe := func(n int, t store.Totals, probe int64) (int64, int64, string) {
		if scanning {
			return t.Media, 0, fmt.Sprintf("第 %d 批 · 已索引 %d 项，待读取信息 %d 项", n, t.Media, probe)
		}
		done := t.ThumbOK - before.ThumbOK
		return done, done + t.ThumbDue, fmt.Sprintf("第 %d 批 · 已生成 %d 张缩略图，剩余 %d 张", n, done, t.ThumbDue)
	}
	for n := 1; ; n++ {
		report := progress
		if progress != nil {
			report = func(p album.WorkProgress) {
				queryCtx, stop := context.WithTimeout(ctx, time.Second)
				defer stop()
				if t, probe, ok := c.Reader.totals(queryCtx, w.Collection); ok {
					p.Processed, p.Total, p.Detail = describe(n, t, probe)
				}
				progress(p)
			}
		}
		res, more, err := c.batch(ctx, w, name, report)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return album.WorkResult{StopReason: "max_runtime", Detail: "已达到本次最长运行时间，已处理的部分保留；再次开始会从剩下的文件继续"}, nil
			}
			return res, err
		}
		if !more {
			summary := "整个目录已处理完"
			if t, probe, ok := c.Reader.totals(ctx, w.Collection); ok {
				_, _, line := describe(n, t, probe)
				summary += "（" + line + "）"
			}
			return album.WorkResult{Completed: true, Detail: summary}, nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return album.WorkResult{StopReason: "max_runtime", Detail: "已达到本次最长运行时间，已处理的部分保留；再次开始会从剩下的文件继续"}, nil
			}
			return album.WorkResult{StopReason: "interrupted", Detail: "相册任务已请求停止；已保存的索引和缩略图保留"}, ctx.Err()
		case <-time.After(batchPause):
		}
	}
}

// batch runs the worker once and reports whether it stopped at its limit with
// work left.
func (c *Control) batch(ctx context.Context, w album.Work, name string, progress func(album.WorkProgress)) (result album.WorkResult, more bool, workErr error) {
	args := []string{"album", strings.TrimPrefix(w.Operation, "album."), "--config", name}
	if w.Operation == "album.scan" || w.Operation == "album.thumbs" {
		args = append(args, "--collection", w.Collection, "--limit", strconv.Itoa(w.Limit))
	}
	if w.Operation == "album.thumbs" {
		// Each batch is bounded by its file limit; the series is bounded by
		// the context, so a batch may use whatever time remains.
		budget := time.Hour
		if deadline, ok := ctx.Deadline(); ok {
			budget = min(budget, time.Until(deadline))
		}
		if budget < time.Second {
			return album.WorkResult{}, false, context.DeadlineExceeded
		}
		args = append(args, "--max-runtime", (time.Duration(math.Ceil(budget.Seconds())) * time.Second).String())
	}
	if w.Operation == "album.pause" && w.Reason != "" {
		args = append(args, "--reason", w.Reason)
	}
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
	var err error
	if w.Operation == "album.thumbs" {
		launch, err = c.launcher.prepare(cmd)
		if err != nil {
			return album.WorkResult{StopReason: "isolation_unavailable"}, false, err
		}
		defer func() {
			if e := launch.finish(); e != nil {
				result.Completed = false
				workErr = errors.Join(workErr, e)
			}
		}()
	}
	if err = cmd.Start(); err != nil {
		return album.WorkResult{}, false, errors.New("album worker could not start")
	}
	if err = launch.admit(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return album.WorkResult{StopReason: "isolation_unavailable"}, false, fmt.Errorf("album worker could not enter the delegated task cgroup: %w", err)
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
		return album.WorkResult{StopReason: "interrupted", Detail: "相册任务已请求停止；已保存的索引和缩略图保留"}, false, ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return album.WorkResult{StopReason: fmt.Sprintf("worker_exit_%d", exit.ExitCode()), Detail: output.String()}, false, fmt.Errorf("album task stopped (exit %d): %s", exit.ExitCode(), output.String())
		}
		return album.WorkResult{}, false, errors.New("album worker could not start")
	}
	text := output.String()
	return album.WorkResult{Completed: true, Detail: text}, strings.Contains(text, scan.BatchMoreMarker), nil
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
