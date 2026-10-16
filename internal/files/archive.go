package files

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
	"time"
)

type ArchiveResult struct {
	Entries int64 `json:"entries"`
	Bytes   int64 `json:"bytes"`
	Skipped int   `json:"skipped"`
}

// Archive streams regular files and directories without following any link. It
// observes all source identities again before declaring the archive complete.
func (m *Manager) Archive(ctx context.Context, rootID, rel string, output io.Writer, limit int, maxBytes int64, progress CopyProgress) (ArchiveResult, error) {
	var result ArchiveResult
	if limit < 1 || limit > m.limits.CopyMaxEntries || maxBytes < 1 || maxBytes > m.limits.CopyMaxBytes {
		return result, ErrTreeLimit
	}
	r, e := m.root(rootID)
	if e != nil {
		return result, e
	}
	p, e := cleanRel(rel)
	if e != nil {
		return result, e
	}
	var unlock func()
	if r.Writable {
		_, unlock, e = m.lockWrite(ctx, rootID)
		if e != nil {
			return result, e
		}
		defer unlock()
	}
	tw := tar.NewWriter(output)
	defer tw.Close()
	observations := []treeNode{}
	buffer := make([]byte, 64<<10)
	var visit func(string, string, int) error
	visit = func(from, name string, depth int) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if depth > 32 || int(result.Entries)+result.Skipped >= limit || len(name) > 1024 {
			return ErrTreeLimit
		}
		info, e := safeLstat(r.dir, from)
		if e != nil {
			return e
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			result.Skipped++
			return nil
		}
		observations = append(observations, treeNode{from, info})
		result.Entries++
		h, e := tar.FileInfoHeader(info, "")
		if e != nil {
			return e
		}
		h.Name = name
		h.Uid = 0
		h.Gid = 0
		h.Uname = ""
		h.Gname = ""
		h.AccessTime = time.Time{}
		h.ChangeTime = time.Time{}
		h.Mode &= 0777
		if info.IsDir() {
			h.Name += "/"
		} else if info.Size() > maxBytes-result.Bytes {
			return ErrTreeLimit
		}
		if e = tw.WriteHeader(h); e != nil {
			return e
		}
		f, e := safeOpen(r.dir, from, os.O_RDONLY|syscall.O_NONBLOCK)
		if e != nil {
			return e
		}
		defer f.Close()
		opened, e := f.Stat()
		if e != nil || !sameSource(info, opened) {
			return ErrSourceChanged
		}
		if info.IsDir() {
			for {
				entries, readErr := f.ReadDir(64)
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					return readErr
				}
				for _, entry := range entries {
					if internalName(entry.Name()) {
						result.Skipped++
						if int(result.Entries)+result.Skipped >= limit {
							return ErrTreeLimit
						}
						continue
					}
					if e = visit(path.Join(from, entry.Name()), path.Join(name, entry.Name()), depth+1); e != nil {
						return e
					}
				}
				if errors.Is(readErr, io.EOF) {
					break
				}
			}
		} else {
			n, e := io.CopyBuffer(copyWriter{tw, func(n int) {
				result.Bytes += int64(n)
				if progress != nil {
					progress(result.Entries, result.Bytes, "pack")
				}
			}}, uploadReader{ctx, io.LimitReader(f, info.Size())}, buffer)
			if e != nil {
				return e
			}
			if n != info.Size() {
				return ErrSourceChanged
			}
			var extra [1]byte
			if n, e := f.Read(extra[:]); n != 0 || !errors.Is(e, io.EOF) {
				return ErrSourceChanged
			}
		}
		return nil
	}
	info, e := safeLstat(r.dir, p)
	if e != nil {
		return result, e
	}
	name := "data"
	if !info.IsDir() {
		result.Entries++
		if e = tw.WriteHeader(&tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0700}); e != nil {
			return result, e
		}
		name = path.Join("data", path.Base(p))
	}
	if e = visit(p, name, 0); e != nil {
		return result, e
	}
	for _, node := range observations {
		now, e := safeLstat(r.dir, node.path)
		if e != nil || !sameSource(node.info, now) {
			return result, ErrSourceChanged
		}
	}
	return result, tw.Close()
}

// ImportArchive stages a bounded tar tree and publishes a new directory only
// after the stream and its transport digest have both passed verification.
func (m *Manager) ImportArchive(ctx context.Context, rootID, destination string, input io.Reader, limit int, maxBytes int64, verify func() error, progress CopyProgress) (CopyResult, error) {
	result := CopyResult{DestinationRoot: rootID, Path: destination}
	to, e := writablePath(destination)
	if e != nil {
		return result, e
	}
	if limit < 1 || limit > m.limits.CopyMaxEntries || maxBytes < 1 || maxBytes > m.limits.CopyMaxBytes {
		return result, ErrTreeLimit
	}
	dst, unlock, e := m.lockWrite(ctx, rootID)
	if e != nil {
		return result, e
	}
	defer unlock()
	if _, e = safeLstat(dst.dir, to); e == nil {
		return result, ErrExists
	} else if !errors.Is(e, ErrNotExist) && !errors.Is(e, os.ErrNotExist) {
		return result, e
	}
	if _, e = openParentCheck(dst.dir, to); e != nil {
		return result, e
	}
	work, e := internalRoot(dst, copyDirectory, true)
	if e != nil {
		return result, e
	}
	defer work.Close()
	if e = cleanCopies(ctx, work); e != nil {
		return result, e
	}
	id, e := randomFileID()
	if e != nil {
		return result, e
	}
	if e = work.Mkdir(id, 0700); e != nil {
		return result, e
	}
	bucket, e := work.OpenRoot(id)
	if e != nil {
		return result, e
	}
	defer bucket.Close()
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if removeBounded(cleanup, bucket, "entry", limit+1) == nil {
			_ = bucket.Remove("metadata.json")
			_ = work.Remove(id)
		}
	}()
	meta, e := bucket.OpenFile("metadata.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return result, e
	}
	e = json.NewEncoder(meta).Encode(copyManifest{1, CopyOptions{SourceRoot: "@archive", DestinationRoot: rootID, From: "data", To: to, Limit: limit, MaxBytes: maxBytes}, time.Now().Unix()})
	if e == nil {
		e = meta.Sync()
	}
	meta.Close()
	if e != nil {
		return result, e
	}
	tr := tar.NewReader(input)
	directories := []string{}
	directoryTimes := map[string]time.Time{}
	seen := map[string]bool{}
	buffer := make([]byte, 64<<10)
	for {
		if e = ctx.Err(); e != nil {
			return result, e
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, ErrSourceChanged
		}
		name := strings.TrimSuffix(h.Name, "/")
		if path.Clean(name) != name || name != "data" && !strings.HasPrefix(name, "data/") || strings.ContainsAny(name, "\\\x00") || len(name) > 1024 || len(strings.Split(name, "/")) > 34 || seen[name] {
			return result, ErrBadPath
		}
		for _, part := range strings.Split(name, "/") {
			if internalName(part) {
				return result, ErrOutsideRoot
			}
		}
		if result.Entries >= int64(limit) || h.Size < 0 || h.Size > maxBytes-result.Bytes {
			return result, ErrTreeLimit
		}
		seen[name] = true
		result.Entries++
		target := "entry" + strings.TrimPrefix(name, "data")
		switch h.Typeflag {
		case tar.TypeDir:
			if h.Size != 0 {
				return result, ErrBadPath
			}
			if e = bucket.Mkdir(target, 0700); e != nil {
				return result, e
			}
			directories = append(directories, target)
			directoryTimes[target] = h.ModTime
		case tar.TypeReg, tar.TypeRegA:
			if name == "data" {
				return result, ErrBadPath
			}
			f, e := bucket.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
			if e != nil {
				return result, e
			}
			_, e = io.CopyBuffer(copyWriter{f, func(n int) {
				result.Bytes += int64(n)
				if progress != nil {
					progress(result.Entries, result.Bytes, "restore")
				}
			}}, uploadReader{ctx, tr}, buffer)
			if e == nil && !h.ModTime.IsZero() {
				e = bucket.Chtimes(target, h.ModTime, h.ModTime)
			}
			if e == nil {
				e = f.Sync()
			}
			f.Close()
			if e != nil {
				return result, e
			}
		default:
			return result, ErrBadPath
		}
	}
	if !seen["data"] {
		return result, ErrBadPath
	}
	if n, e := io.Copy(io.Discard, io.LimitReader(uploadReader{ctx, input}, 1)); e != nil || n != 0 {
		return result, ErrSourceChanged
	}
	if verify != nil {
		if e = verify(); e != nil {
			return result, e
		}
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if modified := directoryTimes[directories[i]]; !modified.IsZero() {
			if e = bucket.Chtimes(directories[i], modified, modified); e != nil {
				return result, e
			}
		}
		f, e := bucket.OpenFile(directories[i], os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if e != nil {
			return result, e
		}
		e = f.Sync()
		f.Close()
		if e != nil {
			return result, e
		}
	}
	parent, e := openParent(dst.dir, to)
	if e != nil {
		return result, e
	}
	defer parent.Close()
	if e = moveNoReplace(bucket, "entry", dst.dir, to); e != nil {
		return result, e
	}
	result.Published = true
	return result, parent.Sync()
}

// DigestReader verifies a remote stream as it is consumed without buffering it.
type DigestReader struct {
	Reader io.Reader
	Hash   interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}
	Bytes int64
}

func NewDigestReader(r io.Reader) *DigestReader { return &DigestReader{Reader: r, Hash: sha256.New()} }
func (r *DigestReader) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	if n > 0 {
		r.Hash.Write(p[:n])
		r.Bytes += int64(n)
	}
	return n, e
}
func (r *DigestReader) SHA256() string { return hex.EncodeToString(r.Hash.Sum(nil)) }
