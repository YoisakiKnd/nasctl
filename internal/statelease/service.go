package statelease

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

var ErrServiceRunning = errors.New("state: another NAS console already owns this state directory")

// AcquireService is independent of the shared restore lease, so normal CLI
// operations remain possible. Callers keep the restore lease as well. Reject a
// second console before opening databases or reconciling the first one's jobs.
// The zero-length inode is never unlinked, including on clean shutdown.
func AcquireService(directory string) (*Lease, error) {
	root, e := os.OpenRoot(directory)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	f, e := root.OpenFile(".nasctl-service.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if e != nil {
		return nil, e
	}
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Size() != 0 || i.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, errors.New("state: invalid NAS service lock")
	}
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		if errors.Is(e, unix.EAGAIN) || errors.Is(e, unix.EWOULDBLOCK) {
			return nil, ErrServiceRunning
		}
		return nil, e
	}
	linked, e := root.Lstat(".nasctl-service.lock")
	if e != nil || !os.SameFile(i, linked) {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
		return nil, errors.New("state: NAS service lock changed")
	}
	return &Lease{file: f}, nil
}
