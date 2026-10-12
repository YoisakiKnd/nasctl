// Package taskctl provides mutual exclusion and process-identity helpers for
// albumctl's bounded tasks.
//
// Mutual exclusion uses flock(2) rather than a pid file. The kernel releases an
// flock when the holding process dies for any reason, including SIGKILL and a
// power cut, so a crashed run can never leave a stale lock behind. A pid file
// would need a stale-detection heuristic, and pid reuse makes that heuristic
// wrong exactly when it matters.
package taskctl

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ErrLocked is returned when another process holds the lock.
var ErrLocked = errors.New("another albumctl task already holds this lock")

// Lock is a held flock.
type Lock struct {
	path string
	file *os.File
}

// Acquire takes an exclusive, non-blocking flock on dir/name.lock.
//
// It returns ErrLocked when another process holds it; the caller must not spin.
// Two scanners running at once would double the I/O on a weak box, and two
// thumbnail runs would double the memory peak, which is precisely the failure
// the design exists to prevent.
func Acquire(dir, name string) (*Lock, error) {
	if dir == "" {
		return nil, errors.New("taskctl: empty lock directory")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("taskctl: create lock directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, name+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("taskctl: open lock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("taskctl: flock %s: %w", path, err)
	}
	// Record who holds it, for `status`. Best effort only: the flock is the
	// actual mechanism, this text is a hint.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("pid=%d at=%s argv=%s\n",
		os.Getpid(), time.Now().UTC().Format(time.RFC3339), Argv())), 0)
	return &Lock{path: path, file: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	// The holder line is advisory, so it is cleared as the lock is dropped;
	// otherwise `status` would keep reporting a process that has already
	// finished. The file itself is left in place so the next run does not have
	// to recreate it.
	_ = l.file.Truncate(0)
	// Closing the descriptor releases the flock.
	err := l.file.Close()
	l.file = nil
	return err
}

// Path returns the lock file path.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Holder reads the informational holder line, reporting it only when the
// process it names is still alive.
//
// A process that is SIGKILLed never runs its release, so a bare file read would
// confidently name a dead pid. The liveness check costs one stat and keeps
// `status` honest.
func Holder(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	if line == "" {
		return ""
	}
	pid := parsePidField(line)
	if pid <= 0 {
		return line
	}
	if err := unix.Kill(pid, 0); err != nil {
		return "" // the holder is gone; the flock is already released
	}
	return line
}

// parsePidField pulls the pid out of the informational "pid=NNN at=... argv=..."
// line.
func parsePidField(line string) int {
	for _, f := range strings.Fields(line) {
		if strings.HasPrefix(f, "pid=") {
			n, err := strconv.Atoi(strings.TrimPrefix(f, "pid="))
			if err == nil {
				return n
			}
		}
	}
	return 0
}

// Argv renders the command line for the task record.
//
// It is truncated and stripped of anything that looks like a credential. The
// task table must not become a place where a token leaks into a backup.
func Argv() string {
	parts := make([]string, 0, len(os.Args))
	for _, a := range os.Args {
		la := strings.ToLower(a)
		switch {
		case strings.HasPrefix(la, "--token"):
			parts = append(parts, "--token=[redacted]")
		case strings.HasPrefix(la, "token="):
			parts = append(parts, "token=[redacted]")
		default:
			parts = append(parts, a)
		}
	}
	s := strings.Join(parts, " ")
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}

// SelfExecPath returns the absolute path of the running binary so that a
// supervisor can re-exec itself without relying on PATH.
func SelfExecPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p, nil
	}
	return resolved, nil
}

// Process holds a started child.
type Process struct {
	Cmd    *exec.Cmd
	PID    int
	PGID   int
	Stdin  *os.File
	Stdout *os.File
}

// SpawnWorker starts a child in its own process group.
//
// A separate process group is what makes it possible to terminate the worker
// and everything it spawned — a shell, an ffmpeg process — without touching any
// other service on the machine. The child is given a pipe pair for the
// readiness handshake.
func SpawnWorker(binary string, args []string, stderrPath string) (*Process, error) {
	cmd := exec.Command(binary, args...)
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return nil, err
	}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	if stderrPath != "" {
		f, ferr := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if ferr == nil {
			cmd.Stderr = f
			defer f.Close()
		}
	}
	if err := cmd.Start(); err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, err
	}
	// The parent's copies of the child's ends must be closed or the pipes never
	// report EOF.
	_ = stdinR.Close()
	_ = stdoutW.Close()

	pgid, err := unix.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}
	return &Process{
		Cmd:    cmd,
		PID:    cmd.Process.Pid,
		PGID:   pgid,
		Stdin:  stdinW,
		Stdout: stdoutR,
	}, nil
}

// Close releases the pipes.
func (p *Process) Close() {
	if p == nil {
		return
	}
	if p.Stdin != nil {
		_ = p.Stdin.Close()
	}
	if p.Stdout != nil {
		_ = p.Stdout.Close()
	}
}

// ParsePIDFile reads a pid from a file. Used only for diagnostics.
func ParsePIDFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}
