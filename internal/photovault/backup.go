package photovault

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/YoisakiKnd/nasctl/internal/files"
)

func (s *Store) HasPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.doc.Objects {
		if o.Pending {
			return true
		}
	}
	return false
}

// BackupScope binds the export to this private store, without exposing paths or
// password material. Pending moves must finish before a restorable snapshot.
func (s *Store) BackupScope() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.doc.Version != 1 {
		return "", ErrInvalid
	}
	for _, o := range s.doc.Objects {
		if o.Pending {
			return "", ErrInvalid
		}
	}
	i, e := s.root.Stat(".")
	if e != nil {
		return "", e
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ErrInvalid
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", s.root.Name(), st.Dev, st.Ino)))
	return hex.EncodeToString(h[:]), nil
}

// ArchiveBackup exports a consistent document plus immutable ciphertext only.
// No password is required, and no original, thumbnail or filename is decrypted.
func (s *Store) ArchiveBackup(ctx context.Context, w io.Writer, limit int, maxBytes int64, progress files.CopyProgress) (files.ArchiveResult, error) {
	var result files.ArchiveResult
	s.mu.Lock()
	doc := s.doc
	doc.Objects = append([]object{}, s.doc.Objects...)
	s.mu.Unlock()
	if doc.Version != 1 {
		return result, ErrInvalid
	}
	for _, o := range doc.Objects {
		if o.Pending {
			return result, ErrInvalid
		}
	}
	if 2+3*len(doc.Objects) > limit {
		return result, files.ErrTreeLimit
	}
	body, e := json.Marshal(doc)
	if e != nil {
		return result, e
	}
	tw := tar.NewWriter(w)
	defer tw.Close()
	if e = tw.WriteHeader(&tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0700}); e != nil {
		return result, e
	}
	result.Entries = 1
	write := func(name string, size int64, r io.Reader) error {
		if size > maxBytes-result.Bytes {
			return files.ErrTreeLimit
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := tw.WriteHeader(&tar.Header{Name: "data/" + name, Typeflag: tar.TypeReg, Mode: 0600, Size: size}); e != nil {
			return e
		}
		n, e := io.CopyBuffer(tw, r, make([]byte, 64<<10))
		if e != nil {
			return e
		}
		if n != size {
			return ErrInvalid
		}
		result.Entries++
		result.Bytes += n
		if progress != nil {
			progress(result.Entries, result.Bytes, "pack")
		}
		return nil
	}
	if e = write("vault.json", int64(len(body)), bytes.NewReader(body)); e != nil {
		return result, e
	}
	for _, o := range doc.Objects {
		for _, suffix := range []string{"original", "thumb", "meta"} {
			if e = ctx.Err(); e != nil {
				return result, e
			}
			name := o.ID + "." + suffix
			f, e := s.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if e != nil {
				return result, e
			}
			i, e := f.Stat()
			if e != nil || !i.Mode().IsRegular() {
				f.Close()
				return result, ErrInvalid
			}
			e = write(name, i.Size(), f)
			after, se := f.Stat()
			f.Close()
			if e != nil {
				return result, e
			}
			if se != nil || after.Size() != i.Size() || after.ModTime() != i.ModTime() {
				return result, ErrInvalid
			}
		}
	}
	return result, tw.Close()
}
