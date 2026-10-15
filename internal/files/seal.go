package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"syscall"

	"golang.org/x/sys/unix"
)

// ConsumeSealed removes exactly the opened regular source, only after seal has
// durably published and verified ciphertext. No trash or plaintext staging copy
// is made. Hard links are refused because another name would retain plaintext.
func (m *Manager) ConsumeSealed(ctx context.Context, rootID, rel string, size, mtime int64, seal func(*os.File) (string, error), retryDigest string) error {
	p, e := writablePath(rel)
	if e != nil {
		return e
	}
	r, unlock, e := m.lockWrite(ctx, rootID)
	if e != nil {
		return e
	}
	defer unlock()
	parent, e := openParent(r.dir, p)
	if e != nil {
		return e
	}
	defer parent.Close()
	name := path.Base(p)
	fd, e := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		if errors.Is(e, unix.ENOENT) && retryDigest != "" {
			return nil
		}
		return mapWriteErr(e)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	before, e := f.Stat()
	if e != nil {
		return e
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !before.Mode().IsRegular() || !ok || stat.Nlink != 1 || before.Size() != size || before.ModTime().UnixNano() != mtime {
		return ErrSourceChanged
	}
	digest := retryDigest
	if digest == "" {
		digest, e = seal(f)
		if e != nil {
			return e
		}
	}
	if len(digest) != 64 {
		return ErrSourceChanged
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		return e
	}
	hash := sha256.New()
	if _, e = io.CopyBuffer(hash, uploadReader{ctx, f}, make([]byte, 64<<10)); e != nil {
		return e
	}
	after, e := f.Stat()
	if e != nil {
		return e
	}
	afterStat, statOK := after.Sys().(*syscall.Stat_t)
	if !statOK || afterStat.Nlink != 1 || hex.EncodeToString(hash.Sum(nil)) != digest || after.Size() != before.Size() || after.ModTime() != before.ModTime() {
		return ErrSourceChanged
	}
	// Reopen through the pinned parent and compare identity immediately before unlink.
	checkFD, e := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return mapWriteErr(e)
	}
	check := os.NewFile(uintptr(checkFD), name)
	current, e := check.Stat()
	check.Close()
	if e != nil || !os.SameFile(before, current) || current.Size() != before.Size() || current.ModTime() != before.ModTime() {
		return ErrSourceChanged
	}
	currentStat, statOK := current.Sys().(*syscall.Stat_t)
	if !statOK || currentStat.Nlink != 1 {
		return ErrSourceChanged
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = unix.Unlinkat(int(parent.Fd()), name, 0); e != nil {
		return mapWriteErr(e)
	}
	return parent.Sync()
}
