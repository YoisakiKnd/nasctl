package albumlocal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/resource"
)

type albumLauncher struct {
	manager     *resource.CgroupManager
	layout      resource.CgroupLayout
	unavailable error
}
type admittedLaunch struct {
	manager     *resource.CgroupManager
	read, write *os.File
}

func newAlbumLauncher(cfg *config.Config) *albumLauncher {
	l := &albumLauncher{}
	// Other platforms retain albumctl's explicit resource refusal. There is no
	// browser switch to disable the required Linux cgroup policy.
	if runtime.GOOS != "linux" || !cfg.Guard.RequireCgroupIsolation {
		return l
	}
	if !cfg.Guard.UseCgroup {
		l.unavailable = errors.New("album launch: required cgroup isolation is disabled in the base configuration")
		return l
	}
	if cfg.Guard.WorkerMemMaxMB > (int64(^uint64(0)>>1)>>20)-64 {
		l.unavailable = errors.New("album launch: memory budget overflows")
		return l
	}
	l.manager, l.unavailable = resource.NewCgroupManager(cfg.Guard.CgroupRoot, "/proc")
	// The outer task ceiling includes the bounded album supervisor. Its own
	// decoder retains the original, smaller worker ceiling inside this group.
	l.layout = resource.CgroupLayout{WorkerMemHighBytes: (cfg.Guard.WorkerMemHighMB + 32) << 20, WorkerMemMaxBytes: (cfg.Guard.WorkerMemMaxMB + 64) << 20, WorkerSwapMax: 0, OOMGroup: true}
	return l
}
func (l *albumLauncher) prepare(cmd *exec.Cmd) (*admittedLaunch, error) {
	if l == nil {
		return nil, nil
	}
	if l.unavailable != nil {
		return nil, l.unavailable
	}
	if l.manager == nil {
		return nil, nil
	}
	if e := l.manager.Prepare(l.layout); e != nil {
		return nil, fmt.Errorf("album launch: delegated NAS cgroup unavailable: %w", e)
	}
	if e := resetAlbumGroup(l.manager); e != nil {
		return nil, e
	}
	r, w, e := os.Pipe()
	if e != nil {
		return nil, e
	}
	cmd.Args = append([]string{cmd.Args[0], "exec-album-task"}, cmd.Args[1:]...)
	cmd.ExtraFiles = []*os.File{r}
	return &admittedLaunch{manager: l.manager, read: r, write: w}, nil
}
func (l *admittedLaunch) admit(pid int) error {
	if l == nil {
		return nil
	}
	_ = l.read.Close()
	if e := l.manager.AttachWorker(pid); e != nil {
		return e
	}
	_, e := l.write.Write([]byte{1})
	return errors.Join(e, l.write.Close())
}
func (l *admittedLaunch) finish() error {
	if l == nil {
		return nil
	}
	_ = l.read.Close()
	_ = l.write.Close()
	return resetAlbumGroup(l.manager)
}
func resetAlbumGroup(m *resource.CgroupManager) error {
	b, e := os.ReadFile(filepath.Join(m.WorkDir, "cgroup.procs"))
	if e != nil {
		return e
	}
	if strings.TrimSpace(string(b)) != "" {
		return errors.New("album launch: task cgroup still has live processes; refusing reuse")
	}
	// albumctl leaves its empty supervisor directory. Remove only its two
	// known child directories; never recursively remove or kill unknown tasks.
	for _, n := range []string{"worker", "sup"} {
		if e = os.Remove(filepath.Join(m.WorkDir, n)); e != nil && !os.IsNotExist(e) {
			return fmt.Errorf("album launch: task subtree could not be cleaned: %w", e)
		}
	}
	f, e := os.OpenFile(filepath.Join(m.WorkDir, "cgroup.subtree_control"), os.O_WRONLY, 0)
	if e != nil {
		return e
	}
	_, e = f.WriteString("-memory")
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	return nil
}
func (c *Control) Close() error {
	e := c.Reader.Close()
	if c.launcher != nil && c.launcher.manager != nil {
		c.launcher.manager.Cleanup()
	}
	return e
}
