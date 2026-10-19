package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// TextMaxBytes bounds what the console reads into a text view and writes back
// from the editor. A larger file is shown truncated and cannot be edited:
// holding it whole in a page and a request is the wrong tool for it.
const TextMaxBytes = 1 << 20

// ErrNotText refuses an edit of something that is not a plain UTF-8 file the
// editor showed whole: binary or truncated content, a symlink or a hard link.
var ErrNotText = errors.New("files: not an editable text file")

// Text is a text file as the viewer and editor see it.
type Text struct {
	Content   string    `json:"content"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
	Truncated bool      `json:"truncated"`
	// Binary is set when the bytes are not UTF-8 text; Content is then empty.
	Binary bool `json:"binary"`
	// Revision identifies the exact bytes shown (SHA-256). Saving requires it,
	// so an edit never silently replaces a newer version. It is empty when the
	// file cannot be edited.
	Revision string `json:"revision"`
}

func revision(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// looksText accepts UTF-8 without NUL bytes, the same rule editors use to
// decide between text and binary.
func looksText(b []byte) bool {
	return utf8.Valid(b) && bytes.IndexByte(b, 0) < 0
}

// ReadText reads up to TextMaxBytes of a regular file.
func (m *Manager) ReadText(rootID, rel string) (*Text, error) {
	f, info, err := m.Open(rootID, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, TextMaxBytes+1))
	if err != nil {
		return nil, mapErr(err)
	}
	t := &Text{Size: info.Size(), ModTime: info.ModTime()}
	if len(b) > TextMaxBytes {
		b, t.Truncated = b[:TextMaxBytes], true
		// Do not judge a character cut in half at the boundary.
		for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.Valid(b); i++ {
			b = b[:len(b)-1]
		}
	}
	if !looksText(b) {
		t.Binary = true
		return t, nil
	}
	t.Content = string(b)
	if !t.Truncated {
		t.Revision = revision(b)
	}
	return t, nil
}

// SaveText writes content to rel. With an empty revision it creates a new file
// and refuses to replace anything; otherwise the file must still hold exactly
// the bytes that revision names, and is replaced atomically: the new content
// is fsynced in staging and renamed over the old file, keeping its mode and,
// where the process may, its owner. A failed save leaves the old file intact.
func (m *Manager) SaveText(ctx context.Context, id, rel string, content []byte, rev string) (*Entry, error) {
	p, err := writablePath(rel)
	if err != nil {
		return nil, err
	}
	if len(content) > TextMaxBytes {
		return nil, ErrTooLarge
	}
	if !looksText(content) {
		return nil, ErrNotText
	}
	r, unlock, err := m.lockWrite(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var mode os.FileMode
	uid, gid := -1, -1
	info, err := safeLstat(r.dir, p)
	switch {
	case rev == "" && err == nil:
		return nil, ErrExists
	case rev == "" && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrNotExist):
		return nil, mapWriteErr(err)
	case rev != "" && err != nil:
		return nil, mapWriteErr(err)
	case rev != "":
		var links uint64
		switch st := info.Sys().(type) {
		case *unix.Stat_t:
			links, uid, gid = uint64(st.Nlink), int(st.Uid), int(st.Gid)
		case *syscall.Stat_t:
			links, uid, gid = uint64(st.Nlink), int(st.Uid), int(st.Gid)
		}
		// Renaming over a symlink would replace the link, and over a hard
		// link would split it from its other names.
		if !info.Mode().IsRegular() || links != 1 {
			return nil, ErrNotText
		}
		f, err := safeOpen(r.dir, p, os.O_RDONLY|syscall.O_NONBLOCK)
		if err != nil {
			return nil, mapWriteErr(err)
		}
		old, err := io.ReadAll(io.LimitReader(f, TextMaxBytes+1))
		_ = f.Close()
		if err != nil {
			return nil, mapWriteErr(err)
		}
		if len(old) > TextMaxBytes || revision(old) != rev {
			return nil, ErrSourceChanged
		}
		mode = info.Mode().Perm()
	}
	stage, err := internalRoot(r, stagingDirectory, true)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	if err = cleanStaging(stage); err != nil {
		return nil, err
	}
	name, err := randomFileID()
	if err != nil {
		return nil, err
	}
	// A new file takes the service's umask, like any file it creates; the
	// staging folder itself is private.
	f, err := stage.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o666)
	if err != nil {
		return nil, mapWriteErr(err)
	}
	defer func() { _ = f.Close(); _ = stage.Remove(name) }()
	if _, err = f.Write(content); err != nil {
		return nil, mapWriteErr(err)
	}
	if uid >= 0 {
		// Only root or the same owner may do this; another owner's file
		// becomes the service user's, as with any atomic save.
		_ = f.Chown(uid, gid)
	}
	if rev != "" {
		if err = f.Chmod(mode); err != nil {
			return nil, mapWriteErr(err)
		}
	}
	if err = f.Sync(); err != nil {
		return nil, mapWriteErr(err)
	}
	if err = f.Close(); err != nil {
		return nil, mapWriteErr(err)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if rev == "" {
		err = moveNoReplace(stage, name, r.dir, p)
	} else {
		err = replace(stage, name, r.dir, p)
	}
	if err != nil {
		return nil, err
	}
	return m.Stat(id, p)
}

// replace renames from over to, both inside their roots.
func replace(fromRoot *os.Root, from string, toRoot *os.Root, to string) error {
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
	if err = unix.Renameat(int(a.Fd()), path.Base(from), int(b.Fd()), path.Base(to)); err != nil {
		return mapWriteErr(err)
	}
	_ = a.Sync()
	_ = b.Sync()
	return nil
}
