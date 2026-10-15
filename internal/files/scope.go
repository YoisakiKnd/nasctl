package files

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path"
	"syscall"

	"golang.org/x/sys/unix"
)

// StorageScope binds a saved backup plan to its configured location and source
// filesystem. A missing mount must not be mistaken for a newly empty directory
// and allowed to replace the last good backup through retention.
func (m *Manager) StorageScope(rootID, rel string) (string, error) {
	r, err := m.root(rootID)
	if err != nil {
		return "", err
	}
	p, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	info, err := safeLstat(r.dir, p)
	if err != nil {
		return "", err
	}
	if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return "", ErrNotRegular
	}
	openPath := p
	if info.Mode()&os.ModeSymlink != 0 {
		openPath = path.Dir(p)
	}
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		flags |= syscall.O_DIRECTORY
	}
	f, err := safeOpen(r.dir, openPath, flags)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var fs unix.Statfs_t
	if err = unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		return "", err
	}
	var dev uint64
	if fs.Fsid.Val == [2]int32{} {
		var st unix.Stat_t
		if err = unix.Fstat(int(f.Fd()), &st); err != nil {
			return "", err
		}
		dev = uint64(st.Dev)
	}
	var directoryInode uint64
	if info.IsDir() {
		var st unix.Stat_t
		if err = unix.Fstat(int(f.Fd()), &st); err != nil {
			return "", err
		}
		directoryInode = uint64(st.Ino)
	} else {
		parent, err := safeOpen(r.dir, path.Dir(p), os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK)
		if err != nil {
			return "", err
		}
		var st unix.Stat_t
		err = unix.Fstat(int(parent.Fd()), &st)
		parent.Close()
		if err != nil {
			return "", err
		}
		directoryInode = uint64(st.Ino)
	}
	value := struct {
		Root, Path, Relative string
		FSID                 [2]int32
		Type                 uint32
		FallbackDevice       uint64
		DirectoryInode       uint64
	}{rootID, r.Path, p, fs.Fsid.Val, uint32(fs.Type), dev, directoryInode}
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
