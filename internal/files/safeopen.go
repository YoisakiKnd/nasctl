package files

import (
	"errors"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Resolve links component-by-component through pinned directory descriptors.
// Every kernel open is NOFOLLOW; link text is resolved here and checked against
// private names before another descriptor is opened. This preserves ordinary
// in-root links without permitting aliases into trash, staging or backup vaults.
func safeOpen(root *os.Root, rel string, flags int) (*os.File, error) {
	clean, err := cleanRel(rel)
	if err != nil {
		return nil, err
	}
	if len(clean) > 4096 {
		return nil, ErrBadPath
	}
	base, err := root.OpenFile(".", os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, mapErr(err)
	}
	current := base
	defer func() {
		if current != base {
			_ = current.Close()
		}
		_ = base.Close()
	}()
	resolved := []string{}
	queue := strings.Split(clean, "/")
	links := 0
	reset := func() {
		if current != base {
			_ = current.Close()
		}
		current = base
		resolved = nil
	}
	for len(queue) > 0 {
		part := queue[0]
		queue = queue[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if len(resolved) == 0 {
				return nil, ErrOutsideRoot
			}
			queue = append(append([]string{}, resolved[:len(resolved)-1]...), queue...)
			reset()
			continue
		}
		if internalName(part) {
			return nil, ErrOutsideRoot
		}
		last := len(queue) == 0
		openFlags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if last {
			openFlags = flags | unix.O_NOFOLLOW | unix.O_CLOEXEC
		}
		fd, e := unix.Openat(int(current.Fd()), part, openFlags, 0)
		if e == nil {
			f := os.NewFile(uintptr(fd), part)
			if last {
				return f, nil
			}
			if current != base {
				_ = current.Close()
			}
			current = f
			resolved = append(resolved, part)
			continue
		}
		var st unix.Stat_t
		if statErr := unix.Fstatat(int(current.Fd()), part, &st, unix.AT_SYMLINK_NOFOLLOW); statErr != nil {
			return nil, mapErr(e)
		}
		if uint32(st.Mode)&unix.S_IFMT != unix.S_IFLNK {
			return nil, mapErr(e)
		}
		links++
		if links > 40 {
			return nil, ErrOutsideRoot
		}
		var buffer [4097]byte
		n, e := unix.Readlinkat(int(current.Fd()), part, buffer[:])
		if e != nil {
			return nil, mapErr(e)
		}
		if n == 0 || n == len(buffer) {
			return nil, ErrBadPath
		}
		target := string(buffer[:n])
		if strings.HasPrefix(target, "/") {
			return nil, ErrOutsideRoot
		}
		next := append([]string{}, resolved...)
		next = append(next, strings.Split(target, "/")...)
		next = append(next, queue...)
		if len(next) > 4096 {
			return nil, ErrBadPath
		}
		queue = next
		reset()
	}
	fd, err := unix.Dup(int(current.Fd()))
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "."), nil
}

type statInfo struct {
	name string
	stat unix.Stat_t
}

func (s statInfo) Name() string { return s.name }
func (s statInfo) Size() int64  { return s.stat.Size }
func (s statInfo) ModTime() time.Time {
	return time.Unix(int64(s.stat.Mtim.Sec), int64(s.stat.Mtim.Nsec))
}
func (s statInfo) IsDir() bool { return s.Mode().IsDir() }
func (s statInfo) Sys() any    { return &s.stat }
func (s statInfo) Mode() os.FileMode {
	bits := uint32(s.stat.Mode)
	m := os.FileMode(bits & 0777)
	if bits&unix.S_ISUID != 0 {
		m |= os.ModeSetuid
	}
	if bits&unix.S_ISGID != 0 {
		m |= os.ModeSetgid
	}
	if bits&unix.S_ISVTX != 0 {
		m |= os.ModeSticky
	}
	switch bits & unix.S_IFMT {
	case unix.S_IFDIR:
		m |= os.ModeDir
	case unix.S_IFLNK:
		m |= os.ModeSymlink
	case unix.S_IFIFO:
		m |= os.ModeNamedPipe
	case unix.S_IFSOCK:
		m |= os.ModeSocket
	case unix.S_IFCHR:
		m |= os.ModeDevice | os.ModeCharDevice
	case unix.S_IFBLK:
		m |= os.ModeDevice
	}
	return m
}
func safeLstat(root *os.Root, rel string) (os.FileInfo, error) {
	clean, err := cleanRel(rel)
	if err != nil {
		return nil, err
	}
	if clean == "." {
		f, err := safeOpen(root, ".", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return f.Stat()
	}
	parent, err := safeOpen(root, path.Dir(clean), os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return infoAt(parent, path.Base(clean))
}
func infoAt(parent *os.File, name string) (os.FileInfo, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, mapErr(err)
	}
	return statInfo{name: name, stat: st}, nil
}

func safeMkdirAll(root *os.Root, rel string, mode uint32) error {
	p, err := cleanRel(rel)
	if err != nil {
		return err
	}
	if p == "." {
		return nil
	}
	parts := strings.Split(p, "/")
	for i, part := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		f, err := safeOpen(root, prefix, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK)
		if err == nil {
			f.Close()
			continue
		}
		if !errors.Is(err, ErrNotExist) && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent, err := safeOpen(root, path.Dir(prefix), os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK)
		if err != nil {
			return err
		}
		err = unix.Mkdirat(int(parent.Fd()), part, mode)
		parent.Close()
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return mapWriteErr(err)
		}
		f, err = safeOpen(root, prefix, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK)
		if err != nil {
			return err
		}
		f.Close()
	}
	return nil
}
func safeReadlink(root *os.Root, rel string) (string, error) {
	p, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	parent, err := safeOpen(root, path.Dir(p), os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	var buffer [4097]byte
	n, err := unix.Readlinkat(int(parent.Fd()), path.Base(p), buffer[:])
	if err != nil {
		return "", mapErr(err)
	}
	if n == len(buffer) {
		return "", ErrBadPath
	}
	return string(buffer[:n]), nil
}
func sameIdentity(a, b os.FileInfo) bool {
	if os.SameFile(a, b) {
		return true
	}
	identity := func(info os.FileInfo) (uint64, uint64, bool) {
		switch s := info.Sys().(type) {
		case *unix.Stat_t:
			return uint64(s.Dev), uint64(s.Ino), true
		case *syscall.Stat_t:
			return uint64(s.Dev), uint64(s.Ino), true
		}
		return 0, 0, false
	}
	ad, ai, ok := identity(a)
	bd, bi, other := identity(b)
	return ok && other && ad == bd && ai == bi
}
