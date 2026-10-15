// Package statelease prevents offline restore from replacing a state directory
// while a participating writer still has a database open. A shared lease is held
// for each writer's lifetime; restore must hold an exclusive lease through verify,
// checkpoint creation, publication and rollback. This is separate from job locks.
package statelease

import (
	"errors"
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

var ErrBusy = errors.New("state: in use by a service, CLI writer or offline restore")
var ErrRecoveryRequired = errors.New("state: interrupted offline restore; run nasctl state recover before starting writers")

// PendingRestore is never removed by a writer. It survives a process crash so
// the coordinator can complete rollback before any database is reopened.
const PendingRestore = ".nasctl-restore.pending"
const RestoreJournal = ".nasctl-restore.json"

const filename = ".nasctl-state.lock"

type Lease struct {
	file *os.File
	once sync.Once
	err  error
}

// Acquire is nonblocking. Callers must create the configured directory first.
// Never unlink the lock file: another process could still hold its inode.
func Acquire(directory string, exclusive bool) (*Lease, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return AcquireRoot(root, exclusive)
}

func AcquireRoot(root *os.Root, exclusive bool) (*Lease, error) {
	return acquireRoot(root, exclusive, true)
}

// ObserveRoot does not create or modify lock files, including on read-only mounts.
func ObserveRoot(root *os.Root) (*Lease, error) { return acquireRoot(root, false, false) }
func acquireRoot(root *os.Root, exclusive, create bool) (*Lease, error) {
	flags := os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if create {
		flags = os.O_CREATE | os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	}
	f, err := root.OpenFile(filename, flags, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		f.Close()
		return nil, errors.New("state: invalid restore lock file")
	}
	operation := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		operation = unix.LOCK_EX | unix.LOCK_NB
	}
	if err = unix.Flock(int(f.Fd()), operation); err != nil {
		f.Close()
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	linked, err := root.Lstat(filename)
	if err != nil || !os.SameFile(info, linked) || !linked.Mode().IsRegular() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
		return nil, errors.New("state: restore lock identity changed")
	}
	if !exclusive {
		for _, marker := range []string{PendingRestore, RestoreJournal} {
			if _, err = root.Lstat(marker); err == nil || !os.IsNotExist(err) {
				_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
				f.Close()
				return nil, ErrRecoveryRequired
			}
		}
	}
	return &Lease{file: f}, nil
}

func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		l.err = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
		if err := l.file.Close(); l.err == nil {
			l.err = err
		}
	})
	return l.err
}
