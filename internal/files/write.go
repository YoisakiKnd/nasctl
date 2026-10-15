package files

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"golang.org/x/sys/unix"
)

var (
	ErrReadOnly     = errors.New("files: storage is read-only")
	ErrExists       = errors.New("files: destination already exists")
	ErrTooLarge     = errors.New("files: upload exceeds size limit")
	ErrBusy         = errors.New("files: another write is active")
	ErrUnsupported  = errors.New("files: filesystem does not support the required atomic operation")
	ErrNoSpace      = errors.New("files: insufficient space or quota")
	ErrSizeMismatch = errors.New("files: upload size differs from the declared size")
)

const trashDirectory = ".nasctl-trash"
const stagingDirectory = ".nasctl-staging"
const writeLockFile = ".nasctl-write.lock"
const backupDirectory = ".nasctl-backups"

func internalName(name string) bool {
	return config.StoragePrivateName(name)
}
func writablePath(value string) (string, error) {
	p, err := cleanRel(value)
	if err != nil {
		return "", err
	}
	if p == "." || len(p) > 1024 || len(strings.Split(p, "/")) > 64 {
		return "", ErrBadPath
	}
	for _, segment := range strings.Split(p, "/") {
		if len(segment) > 255 {
			return "", ErrBadPath
		}
	}
	return p, nil
}
func randomFileID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func validFileID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}
func mapWriteErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrExist), errors.Is(err, unix.ENOTEMPTY):
		return ErrExists
	case errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EDQUOT):
		return ErrNoSpace
	case errors.Is(err, unix.EROFS):
		return ErrReadOnly
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EXDEV), errors.Is(err, unix.EINVAL):
		return ErrUnsupported
	default:
		return mapErr(err)
	}
}

// A root mutex plus an advisory filesystem lock serializes writes even across
// two nasctl processes. Reads remain available throughout a streaming upload.
func (m *Manager) lockWrite(ctx context.Context, id string) (*Root, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	r, err := m.root(id)
	if err != nil {
		return nil, nil, err
	}
	if !r.Writable {
		return nil, nil, ErrReadOnly
	}
	if !r.writeMu.TryLock() {
		return nil, nil, ErrBusy
	}
	f, err := r.dir.OpenFile(writeLockFile, os.O_RDWR|os.O_CREATE|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		r.writeMu.Unlock()
		return nil, nil, mapWriteErr(err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		f.Close()
		r.writeMu.Unlock()
		return nil, nil, ErrUnsupported
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		r.writeMu.Unlock()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, nil, ErrBusy
		}
		return nil, nil, mapWriteErr(err)
	}
	return r, func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close(); r.writeMu.Unlock() }, nil
}
func internalRoot(r *Root, name string, create bool) (*os.Root, error) {
	created := false
	info, err := r.dir.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && create {
		err = r.dir.Mkdir(name, 0700)
		created = err == nil
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, mapWriteErr(err)
		}
		info, err = r.dir.Lstat(name)
	}
	if err != nil {
		return nil, mapWriteErr(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnsupported
	}
	child, err := r.dir.OpenRoot(name)
	if err != nil {
		return nil, mapWriteErr(err)
	}
	opened, err := child.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		child.Close()
		return nil, ErrBusy
	}
	const marker = "nasctl-files-v1\n"
	if created {
		f, err := child.OpenFile(".owner", os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			child.Close()
			return nil, mapWriteErr(err)
		}
		_, err = f.WriteString(marker)
		if err == nil {
			err = f.Sync()
		}
		_ = f.Close()
		if err != nil {
			child.Close()
			return nil, mapWriteErr(err)
		}
	} else {
		f, err := child.OpenFile(".owner", os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			child.Close()
			return nil, ErrUnsupported
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			child.Close()
			return nil, ErrUnsupported
		}
		value, err := io.ReadAll(io.LimitReader(f, 100))
		_ = f.Close()
		if err != nil || string(value) != marker {
			child.Close()
			return nil, ErrUnsupported
		}
	}
	return child, nil
}
func openParent(root *os.Root, p string) (*os.File, error) {
	f, err := safeOpen(root, path.Dir(p), os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK)
	if err != nil {
		return nil, mapWriteErr(err)
	}
	return f, nil
}
func moveNoReplace(fromRoot *os.Root, from string, toRoot *os.Root, to string) error {
	a, err := openParent(fromRoot, from)
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := openParent(toRoot, to)
	if err != nil {
		return err
	}
	defer b.Close()
	if err = renameExclusive(int(a.Fd()), path.Base(from), int(b.Fd()), path.Base(to)); err != nil {
		return mapWriteErr(err)
	}
	// Directory fsync may be unsupported on a network filesystem. The atomic
	// operation has already succeeded; never report it as an absent result.
	_ = a.Sync()
	_ = b.Sync()
	return nil
}
func (m *Manager) Mkdir(ctx context.Context, id, rel string) error {
	p, err := writablePath(rel)
	if err != nil {
		return err
	}
	r, unlock, err := m.lockWrite(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	parent, err := openParent(r.dir, p)
	if err != nil {
		return err
	}
	defer parent.Close()
	return mapWriteErr(unix.Mkdirat(int(parent.Fd()), path.Base(p), 0750))
}
func (m *Manager) Rename(ctx context.Context, id, from, to string) error {
	source, err := writablePath(from)
	if err != nil {
		return err
	}
	dest, err := writablePath(to)
	if err != nil {
		return err
	}
	if source == dest {
		return ErrExists
	}
	if strings.HasPrefix(dest, source+"/") {
		return ErrBadPath
	}
	r, unlock, err := m.lockWrite(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	return moveNoReplace(r.dir, source, r.dir, dest)
}

type uploadReader struct {
	ctx context.Context
	r   io.Reader
}

func (r uploadReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

// Upload publishes only a complete, fsynced file. The destination is never
// opened for truncation; a failed/interrupted stream cannot damage an old file.
func (m *Manager) Upload(ctx context.Context, id, rel string, source io.Reader, declaredSize int64) (int64, error) {
	p, err := writablePath(rel)
	if err != nil {
		return 0, err
	}
	if declaredSize > m.limits.UploadMaxBytes {
		return 0, ErrTooLarge
	}
	r, unlock, err := m.lockWrite(ctx, id)
	if err != nil {
		return 0, err
	}
	defer unlock()
	if _, err = safeLstat(r.dir, p); err == nil {
		return 0, ErrExists
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrNotExist) {
		return 0, mapWriteErr(err)
	}
	stage, err := internalRoot(r, stagingDirectory, true)
	if err != nil {
		return 0, err
	}
	defer stage.Close()
	if err = cleanStaging(stage); err != nil {
		return 0, err
	}
	idValue, err := randomFileID()
	if err != nil {
		return 0, err
	}
	f, err := stage.OpenFile(idValue, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0640)
	if err != nil {
		return 0, mapWriteErr(err)
	}
	defer func() { _ = f.Close(); _ = stage.Remove(idValue) }()
	n, err := io.CopyBuffer(f, uploadReader{ctx, io.LimitReader(source, m.limits.UploadMaxBytes+1)}, make([]byte, 64<<10))
	if err != nil {
		return n, mapWriteErr(err)
	}
	if n > m.limits.UploadMaxBytes {
		return n, ErrTooLarge
	}
	if declaredSize >= 0 && n != declaredSize {
		return n, ErrSizeMismatch
	}
	if err = f.Sync(); err != nil {
		return n, mapWriteErr(err)
	}
	if err = f.Close(); err != nil {
		return n, mapWriteErr(err)
	}
	if err = ctx.Err(); err != nil {
		return n, err
	}
	return n, moveNoReplace(stage, idValue, r.dir, p)
}
func cleanStaging(stage *os.Root) error {
	dir, err := stage.Open(".")
	if err != nil {
		return mapWriteErr(err)
	}
	defer dir.Close()
	entries, err := dir.ReadDir(257)
	if err != nil && !errors.Is(err, io.EOF) {
		return mapWriteErr(err)
	}
	if len(entries) > 256 {
		return ErrNoSpace
	}
	for _, entry := range entries {
		if validFileID(entry.Name()) {
			info, err := entry.Info()
			if err == nil && info.Mode().IsRegular() {
				if err = stage.Remove(entry.Name()); err != nil {
					return mapWriteErr(err)
				}
			}
		}
	}
	return nil
}
