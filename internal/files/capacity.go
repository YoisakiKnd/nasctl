package files

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/bits"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Capacity describes the underlying filesystem, not the sum of files below a
// root. Multiple roots on one volume share VolumeID and must not be added up.
type Capacity struct {
	TotalBytes     int64   `json:"total_bytes"`
	FreeBytes      int64   `json:"free_bytes"`
	AvailableBytes int64   `json:"available_bytes"`
	UsedBytes      int64   `json:"used_bytes"`
	UsagePercent   float64 `json:"usage_percent"`
	VolumeID       string  `json:"volume_id"`
}

func capacityProduct(blocks, blockSize uint64) (int64, error) {
	hi, lo := bits.Mul64(blocks, blockSize)
	if hi != 0 || lo > 9007199254740991 {
		return 0, errors.New("filesystem capacity exceeds representable range")
	}
	return int64(lo), nil
}
func connectedRoot(r *Root) (*os.File, error) {
	f, e := r.dir.OpenFile(".", os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	opened, e := f.Stat()
	if e != nil {
		f.Close()
		return nil, e
	}
	linked, e := os.Stat(r.Path)
	if e != nil || !sameIdentity(opened, linked) {
		f.Close()
		return nil, ErrRootUnavailable
	}
	return f, nil
}
func capacityFor(r *Root) (*Capacity, error) {
	f, e := connectedRoot(r)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	var st unix.Statfs_t
	if e = unix.Fstatfs(int(f.Fd()), &st); e != nil {
		return nil, e
	}
	if st.Bsize <= 0 || st.Bfree > st.Blocks || st.Bavail > st.Bfree {
		return nil, errors.New("invalid filesystem capacity counters")
	}
	total, e := capacityProduct(st.Blocks, uint64(st.Bsize))
	if e != nil {
		return nil, e
	}
	free, e := capacityProduct(st.Bfree, uint64(st.Bsize))
	if e != nil {
		return nil, e
	}
	available, e := capacityProduct(st.Bavail, uint64(st.Bsize))
	if e != nil {
		return nil, e
	}
	if total <= 0 {
		return nil, errors.New("filesystem capacity unavailable")
	}
	var stat unix.Stat_t
	if e = unix.Fstat(int(f.Fd()), &stat); e != nil {
		return nil, e
	}
	v := struct {
		FSID   [2]int32
		Device uint64
		Type   uint32
	}{st.Fsid.Val, uint64(stat.Dev), uint32(st.Type)}
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return &Capacity{TotalBytes: total, FreeBytes: free, AvailableBytes: available, UsedBytes: total - free, UsagePercent: float64(total-free) * 100 / float64(total), VolumeID: hex.EncodeToString(sum[:])}, nil
}
