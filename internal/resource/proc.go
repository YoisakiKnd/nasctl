package resource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ProcStat is the small part of /proc/<pid>/stat we need.
type ProcStat struct {
	PID        int
	PPID       int
	PGID       int
	State      byte
	NumThreads int
	StartTicks uint64
}

// ReadProcStat parses /proc/<pid>/stat.
//
// The comm field is wrapped in parentheses and may itself contain spaces and
// parentheses, so the only safe split is on the last ')'.
func ReadProcStat(procRoot string, pid int) (ProcStat, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return ProcStat{}, err
	}
	s := string(data)
	close := strings.LastIndexByte(s, ')')
	open := strings.IndexByte(s, '(')
	if open < 0 || close < 0 || close < open {
		return ProcStat{}, fmt.Errorf("procfs: malformed stat for pid %d", pid)
	}
	rest := strings.Fields(s[close+1:])
	// After comm: state, ppid, pgrp, session, tty_nr, tpgid, flags, minflt,
	// cminflt, majflt, cmajflt, utime, stime, cutime, cstime, priority, nice,
	// num_threads, itrealvalue, starttime, ...
	if len(rest) < 20 {
		return ProcStat{}, fmt.Errorf("procfs: short stat for pid %d", pid)
	}
	p := ProcStat{PID: pid, State: rest[0][0]}
	p.PPID, _ = strconv.Atoi(rest[1])
	p.PGID, _ = strconv.Atoi(rest[2])
	p.NumThreads, _ = strconv.Atoi(rest[17])
	p.StartTicks, _ = strconv.ParseUint(rest[19], 10, 64)
	return p, nil
}

// ProcMemory is /proc/<pid>/status memory fields plus the cgroup of the pid.
type ProcMemory struct {
	PID       int
	VMRSSKB   int64
	VMHWMB    int64
	VMSwapKB  int64
	VMDataKB  int64
	VMStackKB int64
	Threads   int
	VGSizeKB  int64
}

// ReadProcMemory parses the memory lines of /proc/<pid>/status.
//
// VmHWM is the high-water mark since process start and is what makes a
// transient allocation spike visible after the fact; VmRSS alone can miss it.
func ReadProcMemory(procRoot string, pid int) (ProcMemory, error) {
	f, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return ProcMemory{}, err
	}
	defer f.Close()

	var m ProcMemory
	m.PID = pid
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 4096)
	for {
		n, err := f.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
		if len(buf) > 1<<20 {
			break
		}
	}
	for _, line := range strings.Split(string(buf), "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		key := line[:i]
		val := strings.TrimSpace(line[i+1:])
		switch key {
		case "VmRSS", "VmHWM", "VmSwap", "VmData", "VmStk", "VmSize":
			val = strings.TrimSuffix(val, " kB")
			n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
			if err != nil {
				continue
			}
			switch key {
			case "VmRSS":
				m.VMRSSKB = n
			case "VmHWM":
				m.VMHWMB = n
			case "VmSwap":
				m.VMSwapKB = n
			case "VmData":
				m.VMDataKB = n
			case "VmStk":
				m.VMStackKB = n
			case "VmSize":
				m.VGSizeKB = n
			}
		case "Threads":
			m.Threads, _ = strconv.Atoi(val)
		}
	}
	if m.VMRSSKB == 0 && m.VGSizeKB == 0 {
		return m, fmt.Errorf("procfs: no memory lines for pid %d", pid)
	}
	return m, nil
}

// RSSSample is the measured cost of one process tree.
type RSSSample struct {
	Root         int
	PIDs         []int
	RSSBytes     int64
	HWMBytes     int64
	SwapBytes    int64
	PeakRSSBytes int64
	PeakRSSPID   int
	Complete     bool   // false when a /proc entry vanished mid-walk
	Note         string // why Complete is false
}

// TreeRSS measures the resident set of pid and every descendant.
//
// The walk is done with a single pass over /proc to build the parent map, then
// a downward traversal. A single pass avoids N directory listings on a small
// machine and gives a consistent snapshot: entries that disappear between
// listing and reading are tolerated and reported rather than fatal.
func TreeRSS(procRoot string, pid int) (RSSSample, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return RSSSample{}, err
	}
	children := map[int][]int{}
	known := map[int]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		child, err := strconv.Atoi(e.Name())
		if err != nil || child <= 0 {
			continue
		}
		known[child] = true
		st, err := ReadProcStat(procRoot, child)
		if err != nil {
			continue
		}
		children[st.PPID] = append(children[st.PPID], child)
	}

	sample := RSSSample{Root: pid, Complete: true}
	seen := map[int]bool{}
	queue := []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		sample.PIDs = append(sample.PIDs, cur)
		if m, err := ReadProcMemory(procRoot, cur); err == nil {
			if cur == pid || m.VMRSSKB > 0 {
				rss := m.VMRSSKB << 10
				sample.RSSBytes += rss
				sample.HWMBytes += m.VMHWMB << 10
				sample.SwapBytes += m.VMSwapKB << 10
				if rss > sample.PeakRSSBytes {
					sample.PeakRSSBytes = rss
					sample.PeakRSSPID = cur
				}
			}
		} else if !os.IsNotExist(err) {
			sample.Complete = false
			sample.Note = fmt.Sprintf("pid %d unreadable: %v", cur, err)
		}
		queue = append(queue, children[cur]...)
	}
	sample.PIDs = append([]int(nil), sample.PIDs...)
	if !known[pid] && len(sample.PIDs) <= 1 {
		return sample, fmt.Errorf("procfs: pid %d not found", pid)
	}
	return sample, nil
}

// WaitForExit polls until pid is gone or the deadline passes. Returns true when
// the process is gone.
func WaitForExit(procRoot string, pid int, timeout, poll time.Duration) bool {
	deadline := time.Now().Add(timeout)
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	for {
		if !PIDAlive(procRoot, pid) {
			return true
		}
		if time.Now().After(deadline) {
			return !PIDAlive(procRoot, pid)
		}
		time.Sleep(poll)
	}
}

// PIDAlive reports whether /proc/<pid> exists and the process is still running.
//
// A zombie keeps its /proc entry, so existence alone is not liveness: waiting
// for "the pid to go away" would never finish for a child that has exited and
// not yet been reaped.
func PIDAlive(procRoot string, pid int) bool {
	st, err := ReadProcStat(procRoot, pid)
	if err != nil {
		return false
	}
	return st.State != 'Z' && st.State != 'X' && st.State != 'x'
}

// ProcessGroupAlive reports whether the process group still has a *running*
// process in it.
//
// Signal 0 is the cheap existence test: ESRCH means the group is empty, and it
// is the common case, so it answers in one syscall. It is not sufficient on its
// own, though, because a process that has exited but has not been reaped still
// occupies its slot in the process table, and signal 0 still succeeds for it.
// That matters here: the supervisor uses this to decide whether a worker really
// went away, and a zombie would make it report that a worker "ignored SIGTERM
// and was SIGKILLed after the grace period" when the worker exited promptly.
// So when something is still there, the group is scanned to see whether any of
// it is actually running.
//
// Groups 0 and below are not groups this program ever names - group 0 is the
// caller's own group - so they report false rather than a meaningless success.
func ProcessGroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	if err := syscall.Kill(-pgid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	live, err := groupHasLiveProcess("/proc", pgid)
	if err != nil {
		// Cannot tell. Report "still alive" so that the caller keeps trying and
		// escalates rather than abandoning a process it cannot see.
		return true
	}
	return live
}

// groupHasLiveProcess reports whether any process in pgid is in a state that
// still runs code. Entries that are unreadable or vanish mid-scan are skipped:
// they are not evidence of a live worker, and a stop path must not fail open
// into "the group is empty" either - that is why a read error is returned to
// the caller instead of being folded into the answer.
func groupHasLiveProcess(procRoot string, pgid int) (bool, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		st, err := ReadProcStat(procRoot, pid)
		if err != nil {
			continue
		}
		if st.PGID != pgid {
			continue
		}
		// 'Z' is a process that has exited and is waiting to be reaped, 'X' is
		// one that is gone. Neither can be decoding anything.
		if st.State == 'Z' || st.State == 'X' || st.State == 'x' {
			continue
		}
		return true, nil
	}
	return false, nil
}

// TerminateGroup stops an entire process group.
//
// SIGTERM is delivered to -pgid so that grandchildren (a shell, an ffmpeg
// child) are included, then after grace the group is SIGKILLed. Only this
// program's own worker group is ever touched: we never signal a pid we did not
// spawn, and we never touch another service.
func TerminateGroup(pgid int, grace time.Duration, poll time.Duration) (terminated bool, escalated bool, err error) {
	if pgid <= 1 {
		return false, false, fmt.Errorf("refusing to signal process group %d", pgid)
	}
	if !ProcessGroupAlive(pgid) {
		return true, false, nil
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false, false, fmt.Errorf("SIGTERM group %d: %w", pgid, err)
	}
	if poll <= 0 {
		poll = 25 * time.Millisecond
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !ProcessGroupAlive(pgid) {
			return true, false, nil
		}
		time.Sleep(poll)
	}
	if !ProcessGroupAlive(pgid) {
		return true, false, nil
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false, true, fmt.Errorf("SIGKILL group %d: %w", pgid, err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !ProcessGroupAlive(pgid) {
			return true, true, nil
		}
		time.Sleep(poll)
	}
	return !ProcessGroupAlive(pgid), true, nil
}
