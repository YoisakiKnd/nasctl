package backuplocal

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"syscall"

	"github.com/YoisakiKnd/nasctl/internal/backups"
	"github.com/YoisakiKnd/nasctl/internal/files"
)

const manifestBudget = 8 << 20

type manifestEntry struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Type   string `json:"type"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256,omitempty"`
	Link   string `json:"link,omitempty"`
}
type manifestHeader struct {
	Kind    string       `json:"kind"`
	Version int          `json:"version"`
	Plan    backups.Plan `json:"plan"`
}
type manifestEnd struct {
	Kind     string `json:"kind"`
	Entries  int64  `json:"entries"`
	Bytes    int64  `json:"bytes"`
	Complete bool   `json:"complete"`
}

// Sorted traversal keeps the manifest stream verifiable without an index in RAM.
// Each file is hashed through a 64 KiB buffer; directory slices share the overall
// entry budget and do not grow with the rest of the filesystem.
func walkPayload(ctx context.Context, root *os.Root, base string, limit int, maxBytes int64, visit func(manifestEntry) error, progress func(backups.Progress)) (int64, int64, error) {
	var entries, bytes int64
	buffer := make([]byte, 64<<10)
	var walk func(string, int) error
	walk = func(rel string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 32 || entries >= int64(limit) || len(rel) > 1024 {
			return files.ErrTreeLimit
		}
		p := path.Join(base, rel)
		before, err := root.Lstat(p)
		if err != nil {
			return err
		}
		entries++
		row := manifestEntry{Kind: "entry", Path: rel}
		switch {
		case before.IsDir():
			row.Type = "directory"
		case before.Mode().IsRegular():
			row.Type = "file"
			row.Bytes = before.Size()
			if row.Bytes > maxBytes-bytes {
				return files.ErrTreeLimit
			}
			f, err := root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			opened, err := f.Stat()
			if err != nil || !os.SameFile(before, opened) {
				f.Close()
				return files.ErrSourceChanged
			}
			hash := sha256.New()
			n, err := io.CopyBuffer(hash, contextReader{ctx, io.LimitReader(f, before.Size()+1)}, buffer)
			after, e := f.Stat()
			f.Close()
			if err != nil {
				return err
			}
			if e != nil || n != before.Size() || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
				return files.ErrSourceChanged
			}
			row.SHA256 = hex.EncodeToString(hash.Sum(nil))
			bytes += n
		case before.Mode()&os.ModeSymlink != 0:
			row.Type = "symlink"
			row.Link, err = root.Readlink(p)
			if err != nil {
				return err
			}
			if len(row.Link) > 4096 {
				return files.ErrTreeLimit
			}
		default:
			return files.ErrNotRegular
		}
		if err = visit(row); err != nil {
			return err
		}
		if progress != nil {
			progress(backups.Progress{Phase: "verify", Entries: entries, Bytes: bytes})
		}
		if row.Type != "directory" {
			return nil
		}
		dir, err := root.OpenFile(p, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer dir.Close()
		opened, err := dir.Stat()
		if err != nil || !os.SameFile(before, opened) {
			return files.ErrSourceChanged
		}
		children, err := dir.ReadDir(limit - int(entries) + 1)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if int64(len(children)) > int64(limit)-entries {
			return files.ErrTreeLimit
		}
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, child := range children {
			if err = walk(path.Join(rel, child.Name()), depth+1); err != nil {
				return err
			}
		}
		after, err := root.Lstat(p)
		if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
			return files.ErrSourceChanged
		}
		return nil
	}
	err := walk(".", 0)
	return entries, bytes, err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

type limitedWriter struct {
	w io.Writer
	n int64
}

func (w *limitedWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > manifestBudget-w.n {
		return 0, ErrFull
	}
	n, err := w.w.Write(b)
	w.n += int64(n)
	return n, err
}

func writeManifest(ctx context.Context, bucket *os.Root, plan backups.Plan, report func(backups.Progress)) (int64, int64, string, error) {
	f, err := bucket.OpenFile("manifest.ndjson", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 0, 0, "", err
	}
	defer f.Close()
	hash := sha256.New()
	writer := &limitedWriter{w: io.MultiWriter(f, hash)}
	encoder := json.NewEncoder(writer)
	if err = encoder.Encode(manifestHeader{"header", 1, plan}); err != nil {
		return 0, 0, "", err
	}
	entries, bytes, err := walkPayload(ctx, bucket, "data", plan.Limit, plan.MaxBytes, func(row manifestEntry) error { return encoder.Encode(row) }, report)
	if err != nil {
		return entries, bytes, "", err
	}
	if err = encoder.Encode(manifestEnd{"end", entries, bytes, true}); err != nil {
		return entries, bytes, "", err
	}
	if err = f.Sync(); err != nil {
		return entries, bytes, "", err
	}
	return entries, bytes, hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyManifest(ctx context.Context, bucket *os.Root, data *os.Root, base string, snapshot backups.Snapshot, report func(backups.Progress)) error {
	f, err := bucket.OpenFile("manifest.ndjson", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ErrCorrupt
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > manifestBudget {
		return ErrCorrupt
	}
	hash := sha256.New()
	reader := bufio.NewReaderSize(io.TeeReader(io.LimitReader(f, manifestBudget+1), hash), 32<<10)
	next := func(out any) error {
		line, e := reader.ReadSlice('\n')
		if e != nil || len(line) > 32<<10 {
			return ErrCorrupt
		}
		d := json.NewDecoder(bytes.NewReader(line))
		d.DisallowUnknownFields()
		if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
			return ErrCorrupt
		}
		return nil
	}
	var header manifestHeader
	if next(&header) != nil || header.Kind != "header" || header.Version != 1 || header.Plan.ID != snapshot.PlanID || header.Plan.Name != snapshot.Name || header.Plan.Root != snapshot.Root || header.Plan.Path != snapshot.Path {
		return ErrCorrupt
	}
	if header.Plan.Limit < 1 || header.Plan.Limit > 10000 || header.Plan.MaxBytes < 1 || header.Plan.MaxBytes > files.MaxTransferBytes {
		return ErrCorrupt
	}
	entries, bytes, err := walkPayload(ctx, data, base, header.Plan.Limit, header.Plan.MaxBytes, func(actual manifestEntry) error {
		var expected manifestEntry
		if next(&expected) != nil || expected != actual {
			return ErrCorrupt
		}
		return nil
	}, report)
	if err != nil {
		return err
	}
	var end manifestEnd
	if next(&end) != nil || end.Kind != "end" || !end.Complete || end.Entries != entries || end.Bytes != bytes || entries != snapshot.Entries || bytes != snapshot.Bytes {
		return ErrCorrupt
	}
	if _, err = reader.ReadByte(); err != io.EOF {
		return ErrCorrupt
	}
	if hex.EncodeToString(hash.Sum(nil)) != snapshot.ManifestSHA256 {
		return ErrCorrupt
	}
	return nil
}
