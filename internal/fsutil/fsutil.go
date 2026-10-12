// Package fsutil holds filesystem primitives that the rest of albumctl relies
// on for safety: atomic replacement, path containment, and filesystem identity
// checks.
//
// Two invariants matter more than anything else in this package:
//
//   - Nothing is ever published by a non-atomic write. A cache entry or index
//     update either appears complete or does not appear at all, so an
//     interrupted run can never leave a half-written thumbnail that looks valid.
//   - No path derived from data is ever used without proving it is inside a
//     configured root. Media names come from the filesystem and could contain
//     "..", an absolute path, or a symlink pointing at /etc.
package fsutil

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ErrOutsideRoot is returned when a path escapes its configured root.
var ErrOutsideRoot = errors.New("path escapes the configured root")

// ErrSymlink is returned when a symbolic link is encountered and following it
// is not allowed.
var ErrSymlink = errors.New("symbolic link refused")

// ErrUnstable is returned for a file whose size or modification time changed
// between the moment it was discovered and the moment it was about to be
// indexed or decoded. Such a file is deferred rather than published: a
// half-written photo or a video still being recorded must never produce an
// index row or a thumbnail that claims to describe it.
var ErrUnstable = errors.New("file changed while it was being processed; deferred to a later run")

// SafeRelJoin joins rel onto root and rejects anything that escapes root.
//
// rel is expected to be a relative path produced by our own walker, but it is
// still validated: defence in depth costs nothing here, and a database row
// could in principle be edited by hand.
func SafeRelJoin(root, rel string) (string, error) {
	if root == "" {
		return "", errors.New("fsutil: empty root")
	}
	if rel == "" {
		return "", errors.New("fsutil: empty relative path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q is absolute", ErrOutsideRoot, rel)
	}
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: NUL byte in %q", ErrOutsideRoot, rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrOutsideRoot, rel)
	}
	joined := filepath.Join(root, clean)
	if !IsWithin(root, joined) {
		return "", fmt.Errorf("%w: %q", ErrOutsideRoot, rel)
	}
	return joined, nil
}

// IsWithin reports whether p is base or a descendant of base, comparing
// cleaned lexical paths. It does not resolve symlinks; combine with
// ResolveWithin when the filesystem is not trusted.
func IsWithin(base, p string) bool {
	base = filepath.Clean(base)
	p = filepath.Clean(p)
	if base == p {
		return true
	}
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ResolveWithin resolves symlinks for both root and target and verifies the
// real target is inside the real root.
//
// This is the check that actually stops a symlink inside a photo directory from
// exposing /etc/shadow through the streaming endpoint.
func ResolveWithin(root, target string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve root %s: %w", root, err)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve target %s: %w", target, err)
	}
	if !IsWithin(realRoot, realTarget) {
		return "", fmt.Errorf("%w: %s resolves to %s, outside %s", ErrOutsideRoot, target, realTarget, realRoot)
	}
	return realTarget, nil
}

// AtomicWriteFile writes data to path via a temporary file in the same
// directory, fsyncs it, renames it into place and fsyncs the directory.
//
// The fsync before rename is what makes a sudden power loss leave either the
// old content or the new content, never a truncated file. The directory fsync
// is what makes the rename itself durable.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return AtomicWrite(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// AtomicWrite is AtomicWriteFile for streamed content.
func AtomicWrite(path string, perm os.FileMode, write func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("atomic write: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("atomic write: temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if perm != 0 {
		if err = tmp.Chmod(perm); err != nil {
			return fmt.Errorf("atomic write: chmod: %w", err)
		}
	}
	if err = write(tmp); err != nil {
		return fmt.Errorf("atomic write: %s: %w", tmpName, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("atomic write: fsync %s: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("atomic write: close %s: %w", tmpName, err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("atomic write: rename into %s: %w", path, err)
	}
	if d, derr := os.Open(dir); derr == nil {
		// Best effort: a filesystem that cannot fsync a directory still gets
		// the atomic rename, which is the important half.
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// TempPrefix is the marker used by AtomicWrite. Cleanup routines only ever
// remove entries with this prefix, inside albumctl's own directories.
const TempPrefix = "."

// IsTempArtifact reports whether a base name looks like an in-progress temp
// file created by AtomicWrite.
func IsTempArtifact(name string) bool {
	return strings.Contains(name, ".tmp-") && strings.HasPrefix(name, TempPrefix)
}

// RemoveTempArtifacts deletes stale temp files under dir. It refuses to run
// outside the caller-supplied root, which must be albumctl's own cache
// directory.
func RemoveTempArtifacts(root string, olderThan time.Duration) (removed int, freed int64, err error) {
	if root == "" || !filepath.IsAbs(root) {
		return 0, 0, errors.New("fsutil: cleanup root must be an absolute path")
	}
	cutoff := time.Now().Add(-olderThan)
	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // a vanished entry is not a failure
		}
		if d.IsDir() {
			return nil
		}
		if !IsTempArtifact(d.Name()) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if olderThan > 0 && info.ModTime().After(cutoff) {
			return nil
		}
		size := info.Size()
		if rerr := os.Remove(p); rerr == nil {
			removed++
			freed += size
		}
		return nil
	})
	return removed, freed, walkErr
}

// DeviceID returns st_dev for a path. Two paths on the same filesystem share a
// device id, which is how albumctl proves that the media tree is not the system
// disk.
func DeviceID(path string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}

// LDeviceID is DeviceID using lstat, so a symlinked mountpoint is not followed.
func LDeviceID(path string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}

// FSID returns a stable identifier for the filesystem holding path. It is
// recorded on the first successful check and compared afterwards, which catches
// "the RAID array came back as a different filesystem".
func FSID(path string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "", err
	}
	// Fsid is [2]int32 on every Linux architecture, but the underlying type of
	// the elements differs between 32-bit and 64-bit builds, so format through
	// explicit conversion instead of Printf on the struct.
	a := int64(int32(st.Fsid.Val[0]))
	b := int64(int32(st.Fsid.Val[1]))
	return fmt.Sprintf("%08x%08x", uint32(a), uint32(b)), nil
}

// FSKey groups device id and fsid, the pair albumctl pins per collection.
type FSKey struct {
	Device uint64
	FSID   string
}

// Identify returns the FSKey for a path.
func Identify(path string) (FSKey, error) {
	dev, err := DeviceID(path)
	if err != nil {
		return FSKey{}, err
	}
	fsid, err := FSID(path)
	if err != nil {
		return FSKey{}, err
	}
	return FSKey{Device: dev, FSID: fsid}, nil
}

// IsMountPoint reports whether path is a mount point, by comparing its device
// with its parent's. This misses a bind mount of the same filesystem, which is
// why /proc/self/mountinfo is consulted as well when available.
func IsMountPoint(path string) (bool, error) {
	dev, err := DeviceID(path)
	if err != nil {
		return false, err
	}
	parent := filepath.Dir(filepath.Clean(path))
	if parent == path {
		return true, nil // filesystem root is always a mount point
	}
	pdev, err := DeviceID(parent)
	if err != nil {
		return false, err
	}
	if dev != pdev {
		return true, nil
	}
	return mountinfoHas(path)
}

// mountinfoHas checks /proc/self/mountinfo for an exact mount point match.
func mountinfoHas(path string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, nil // not fatal: the device comparison already ran
	}
	want := filepath.Clean(path)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mp := fields[4]
		// mountinfo escapes space, tab, newline and backslash as octal.
		mp = unescapeMountInfo(mp)
		if filepath.Clean(mp) == want {
			return true, nil
		}
	}
	return false, nil
}

func unescapeMountInfo(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// DirUsage totals the size and count of regular files under root.
//
// It is used for the cache report. It is O(files) by design and is never called
// from an HTTP handler.
func DirUsage(root string) (files int, bytes int64, err error) {
	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			files++
			bytes += info.Size()
		}
		return nil
	})
	return files, bytes, walkErr
}

// FreeBytes reports free space on the filesystem holding path.
func FreeBytes(path string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	return st.Bavail * bsize, st.Blocks * bsize, nil
}

// EnsureDir creates a directory with mode 0750 if it does not exist.
func EnsureDir(path string) error {
	if path == "" {
		return errors.New("fsutil: empty directory path")
	}
	return os.MkdirAll(path, 0o750)
}

// FileUnchanged reports whether the file at path still has the recorded size
// and modification time.
//
// This is the cheap change detection the design requires: relative path, size
// and mtime. Hashing every original is explicitly not done.
func FileUnchanged(path string, size int64, mtimeNS int64) (bool, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() {
		return false, nil
	}
	return st.Size() == size && st.ModTime().UnixNano() == mtimeNS, nil
}
