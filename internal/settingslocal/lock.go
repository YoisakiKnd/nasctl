package settingslocal

import (
	"errors"
	"os"
	"syscall"

	"github.com/YoisakiKnd/nasctl/internal/settings"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
	"golang.org/x/sys/unix"
)

// LockConfiguration serializes pending settings and managed album references.
// Keep the same private inode across shutdown; offline repair uses the enclosing
// exclusive state lease and cannot race this shared lease.
func LockConfiguration(state string) (func(), error) {
	root, e := os.OpenRoot(state)
	if e != nil {
		return nil, e
	}
	lease, e := statelease.AcquireRoot(root, false)
	if e != nil {
		root.Close()
		return nil, e
	}
	f, e := root.OpenFile(".nasctl-settings.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if e != nil {
		lease.Close()
		root.Close()
		return nil, e
	}
	close := func() { f.Close(); lease.Close(); root.Close() }
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Size() != 0 || i.Mode().Perm()&0077 != 0 {
		close()
		return nil, errors.New("settings: invalid configuration lock")
	}
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		close()
		return nil, settings.ErrConflict
	}
	linked, e := root.Lstat(".nasctl-settings.lock")
	if e != nil || !os.SameFile(i, linked) {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		close()
		return nil, settings.ErrConflict
	}
	return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); close() }, nil
}
