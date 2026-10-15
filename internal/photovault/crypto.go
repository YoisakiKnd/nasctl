package photovault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

const chunkSize = 64 << 10
const MaxPhotoBytes int64 = 64 << 20

var ErrInvalid = errors.New("保险箱数据无效或已损坏")
var ErrLocked = errors.New("请先输入保险箱密码解锁")
var ErrSyncPending = errors.New("保险箱设置已发布，但磁盘同步未确认")
var ErrBusy = errors.New("保险箱正在处理，请稍后重试")

// Every chunk authenticates its position, file identity and complete length.
// Files can be streamed and ranged without plaintext staging or size-dependent RAM.
func aead(key []byte) (cipher.AEAD, error) {
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(b)
}

// A domain-separated key per random object/part prevents nonce reuse between files.
func fileAEAD(key []byte, identity string) (cipher.AEAD, error) {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("nasctl-vault-file-v1:" + identity))
	derived := mac.Sum(nil)
	defer clear(derived)
	return aead(derived)
}
func nonce(header []byte, index uint32) []byte {
	n := make([]byte, 12)
	copy(n, header[16:24])
	binary.BigEndian.PutUint32(n[8:], index)
	return n
}
func associated(identity string, header []byte, index uint32) []byte {
	b := append([]byte(identity), header...)
	return binary.BigEndian.AppendUint32(b, index)
}
func encrypt(ctx context.Context, f *os.File, key []byte, identity string, source io.Reader, size int64) (string, error) {
	if size < 0 || size > MaxPhotoBytes {
		return "", ErrInvalid
	}
	a, e := fileAEAD(key, identity)
	if e != nil {
		return "", e
	}
	header := make([]byte, 24)
	copy(header, "NASVLT01")
	binary.BigEndian.PutUint64(header[8:], uint64(size))
	if _, e = rand.Read(header[16:]); e != nil {
		return "", e
	}
	if _, e = f.Write(header); e != nil {
		return "", e
	}
	hash := sha256.New()
	buffer := make([]byte, chunkSize)
	remaining := size
	for index := uint32(0); remaining > 0 || index == 0; index++ {
		if e = ctx.Err(); e != nil {
			return "", e
		}
		want := min(int64(chunkSize), remaining)
		if _, e = io.ReadFull(source, buffer[:want]); e != nil {
			return "", e
		}
		hash.Write(buffer[:want])
		sealed := a.Seal(nil, nonce(header, index), buffer[:want], associated(identity, header, index))
		if _, e = f.Write(sealed); e != nil {
			return "", e
		}
		remaining -= want
		if remaining == 0 {
			break
		}
	}
	var extra [1]byte
	n, e := source.Read(extra[:])
	if n != 0 || e != io.EOF {
		return "", ErrInvalid
	}
	if e = f.Sync(); e != nil {
		return "", e
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type Reader struct {
	f         *os.File
	a         cipher.AEAD
	header    []byte
	identity  string
	ctx       context.Context
	valid     func() bool
	size, pos int64
	index     int64
	buffer    []byte
}

func openReader(ctx context.Context, f *os.File, key []byte, identity string, valid func() bool) (*Reader, error) {
	a, e := fileAEAD(key, identity)
	if e != nil {
		return nil, e
	}
	header := make([]byte, 24)
	if _, e = f.ReadAt(header, 0); e != nil || string(header[:8]) != "NASVLT01" {
		return nil, ErrInvalid
	}
	size := int64(binary.BigEndian.Uint64(header[8:]))
	if size < 0 || size > MaxPhotoBytes {
		return nil, ErrInvalid
	}
	info, e := f.Stat()
	chunks := max(int64(1), (size+chunkSize-1)/chunkSize)
	if e != nil || !info.Mode().IsRegular() || info.Size() != 24+size+chunks*int64(a.Overhead()) {
		return nil, ErrInvalid
	}
	if size == 0 {
		tag := make([]byte, a.Overhead())
		if _, e = f.ReadAt(tag, 24); e != nil {
			return nil, ErrInvalid
		}
		if _, e = a.Open(nil, nonce(header, 0), tag, associated(identity, header, 0)); e != nil {
			return nil, ErrInvalid
		}
	}
	return &Reader{f: f, a: a, header: header, identity: identity, ctx: ctx, valid: valid, size: size, index: -1}, nil
}
func (r *Reader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	if r.valid != nil && !r.valid() {
		return 0, ErrLocked
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos >= r.size {
		return 0, io.EOF
	}
	index := r.pos / chunkSize
	if index != r.index {
		length := min(int64(chunkSize), r.size-index*chunkSize)
		sealed := make([]byte, length+int64(r.a.Overhead()))
		if _, e := r.f.ReadAt(sealed, 24+index*int64(chunkSize+r.a.Overhead())); e != nil {
			return 0, e
		}
		plain, e := r.a.Open(r.buffer[:0], nonce(r.header, uint32(index)), sealed, associated(r.identity, r.header, uint32(index)))
		if e != nil {
			return 0, ErrInvalid
		}
		r.buffer = plain
		r.index = index
	}
	n := copy(p, r.buffer[r.pos%chunkSize:])
	r.pos += int64(n)
	return n, nil
}
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	base := int64(0)
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = r.pos
	case io.SeekEnd:
		base = r.size
	default:
		return 0, ErrInvalid
	}
	if offset < -base || offset > r.size-base {
		return 0, ErrInvalid
	}
	r.pos = base + offset
	return r.pos, nil
}
func (r *Reader) Close() error { clear(r.buffer); return r.f.Close() }
