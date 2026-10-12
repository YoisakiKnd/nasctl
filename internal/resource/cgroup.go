package resource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// CgroupManager places media workers in their own cgroup v2 child so that the
// kernel — not just this program — enforces a memory ceiling, and so that a
// runaway decoder is killed without taking the supervisor with it.
//
// Layout under the unit's own cgroup (systemd Delegate=yes):
//
//	/sys/fs/cgroup/system.slice/albumctl-thumbs.service/   <- unit cgroup, no processes
//	    sup/        supervisor + its own threads
//	    worker/     one media worker at a time, memory.max set, oom.group=1
//
// cgroup v2 forbids a cgroup from both hosting processes and distributing
// controllers to children, which is why the supervisor moves itself into `sup`
// before `+memory` is enabled on the unit cgroup.
//
// Everything here degrades explicitly. If delegation is unavailable the manager
// reports Inactive with the reason, and the caller decides whether to refuse to
// run heavy tasks (config: guard.require_cgroup_isolation) or to fall back to
// RSS-only supervision with that limitation recorded in the task record.
type CgroupManager struct {
	Root    string // mount point of cgroup v2, normally /sys/fs/cgroup
	OwnFS   string // filesystem path of our own cgroup
	SupDir  string
	WorkDir string

	Active bool
	Reason string // why Active is false, or a note when true

	mu       sync.Mutex
	prepared bool
	created  bool
}

// CgroupLayout describes the requested isolation without touching the system.
type CgroupLayout struct {
	WorkerMemHighBytes int64
	WorkerMemMaxBytes  int64
	WorkerSwapMax      int64 // 0 means "no swap for workers"
	OOMGroup           bool
}

// NewCgroupManager resolves our own cgroup. It performs no writes.
func NewCgroupManager(root string, procRoot string) (*CgroupManager, error) {
	m := &CgroupManager{Root: filepath.Clean(root)}
	rel, err := ownCgroupPath(procRoot)
	if err != nil {
		m.Reason = err.Error()
		return m, nil
	}
	m.OwnFS = filepath.Join(m.Root, strings.TrimPrefix(rel, "/"))
	m.SupDir = filepath.Join(m.OwnFS, "sup")
	m.WorkDir = filepath.Join(m.OwnFS, "worker")
	if st, err := os.Stat(m.OwnFS); err != nil || !st.IsDir() {
		m.Reason = fmt.Sprintf("own cgroup %s is not present", m.OwnFS)
		return m, nil
	}
	if m.OwnFS == filepath.Clean(root) {
		m.Reason = "process runs in the cgroup root; no delegation to create child cgroups"
		return m, nil
	}
	return m, nil
}

// ownCgroupPath parses the "0::<path>" line of /proc/<self>/cgroup.
func ownCgroupPath(procRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, "self", "cgroup"))
	if err != nil {
		return "", fmt.Errorf("read self cgroup: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "0::"))
		if p == "" {
			p = "/"
		}
		return p, nil
	}
	return "", errors.New("no cgroup v2 entry in /proc/self/cgroup")
}

// Prepare creates `sup` and `worker`, moves this process into `sup` and applies
// the worker's memory limits. It is safe to call once per process.
//
// A returned error means isolation was not established; Reason carries the
// operator-facing explanation.
func (m *CgroupManager) Prepare(layout CgroupLayout) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.prepared {
		return nil
	}
	if m.Reason != "" && !m.Active {
		return fmt.Errorf("cgroup isolation unavailable: %s", m.Reason)
	}

	self := strconv.Itoa(os.Getpid())

	// Step 1: move ourselves out of the unit cgroup so controllers may be
	// distributed to children.
	if err := os.Mkdir(m.SupDir, 0o755); err != nil && !os.IsExist(err) {
		m.Reason = fmt.Sprintf("mkdir %s: %v", m.SupDir, err)
		return errors.New(m.Reason)
	}
	if err := writeFile(m.SupDir+"/cgroup.procs", self); err != nil {
		m.Reason = fmt.Sprintf("move supervisor into %s: %v", m.SupDir, err)
		return errors.New(m.Reason)
	}

	// Step 2: enable the memory controller for children of the unit cgroup.
	// Writing "+memory" when already enabled is a no-op success.
	if err := writeFile(m.OwnFS+"/cgroup.subtree_control", "+memory"); err != nil {
		m.Reason = fmt.Sprintf("enable memory controller on %s: %v", m.OwnFS, err)
		return errors.New(m.Reason)
	}

	// Step 3: create the worker cgroup and set hard limits.
	if err := os.Mkdir(m.WorkDir, 0o755); err != nil && !os.IsExist(err) {
		m.Reason = fmt.Sprintf("mkdir %s: %v", m.WorkDir, err)
		return errors.New(m.Reason)
	}
	m.created = true

	type setting struct {
		file  string
		value string
		must  bool
	}
	sets := []setting{
		// memory.max is the only setting we insist on: without it the kernel
		// enforces nothing and the whole point of isolation is lost.
		{"memory.max", strconv.FormatInt(layout.WorkerMemMaxBytes, 10), true},
		{"memory.high", strconv.FormatInt(layout.WorkerMemHighBytes, 10), false},
		{"memory.swap.max", strconv.FormatInt(layout.WorkerSwapMax, 10), false},
	}
	if layout.OOMGroup {
		// Kill the whole worker subtree together instead of one arbitrary
		// process; the supervisor is outside this cgroup and survives.
		sets = append(sets, setting{"memory.oom.group", "1", false})
	}
	var soft []string
	for _, s := range sets {
		if err := writeFile(filepath.Join(m.WorkDir, s.file), s.value); err != nil {
			if s.must {
				m.Reason = fmt.Sprintf("set %s: %v", s.file, err)
				return errors.New(m.Reason)
			}
			soft = append(soft, fmt.Sprintf("%s not set: %v", s.file, err))
		}
	}

	m.Active = true
	m.prepared = true
	m.Reason = fmt.Sprintf("worker cgroup %s memory.max=%.0f MiB memory.high=%.0f MiB",
		m.WorkDir,
		float64(layout.WorkerMemMaxBytes)/(1<<20),
		float64(layout.WorkerMemHighBytes)/(1<<20))
	if len(soft) > 0 {
		m.Reason += "; partial: " + strings.Join(soft, ", ")
	}
	return nil
}

// AttachWorker moves a freshly spawned worker into the worker cgroup. It must
// be called before the worker is allowed to do real work; the supervisor waits
// for the child's readiness handshake, which the child only sends after
// confirming it is in a cgroup with an effective memory ceiling.
func (m *CgroupManager) AttachWorker(pid int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.Active {
		return fmt.Errorf("cgroup isolation not active: %s", m.Reason)
	}
	return writeFile(m.WorkDir+"/cgroup.procs", strconv.Itoa(pid))
}

// Read returns the worker cgroup's accounting.
func (m *CgroupManager) Read() (Cgroup, error) {
	if !m.Active {
		return Cgroup{}, nil
	}
	return ReadCgroup(m.WorkDir)
}

// Cleanup removes the worker cgroup. The `sup` directory is left in place: it
// is empty once we exit and is reused on the next run.
func (m *CgroupManager) Cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.created {
		return
	}
	// The cgroup can only be removed once it holds no processes.
	_ = os.Remove(m.WorkDir)
	m.created = false
}

// EffectiveWorkerLimitBytes reports the ceiling the kernel will actually apply,
// which is the minimum of every ancestor's memory.max that is set.
func (m *CgroupManager) EffectiveWorkerLimitBytes() (int64, error) {
	if !m.Active {
		return 0, fmt.Errorf("cgroup isolation not active")
	}
	dir := m.WorkDir
	var eff int64 = -1
	for {
		c, err := ReadCgroup(dir)
		if err != nil {
			return 0, err
		}
		if c.Max > 0 && (eff < 0 || c.Max < eff) {
			eff = c.Max
		}
		if dir == m.Root || dir == "/" || dir == "." {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if eff < 0 {
		return 0, errors.New("no memory.max found anywhere in the cgroup ancestry")
	}
	return eff, nil
}

func writeFile(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(value); err != nil {
		return err
	}
	return nil
}

// ReniceProcess applies best-effort nice and ionice to a pid. These are
// additional measures only; they never substitute for a memory ceiling.
func ReniceProcess(pid int, nice int, ioniceClass string) error {
	var firstErr error
	if nice != 0 {
		if err := setpriority(pid, nice); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("setpriority: %w", err)
		}
	}
	var class int
	switch ioniceClass {
	case "", "none":
		return firstErr
	case "idle":
		class = ioprioClassIdle
	case "best-effort":
		class = ioprioClassBE
	case "realtime":
		class = ioprioClassRT
	default:
		return fmt.Errorf("unknown ionice class %q", ioniceClass)
	}
	if err := ioprioSet(pid, class); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("ioprio_set: %w", err)
	}
	return firstErr
}
