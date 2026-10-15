package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
	"time"
)

const copyDirectory = ".nasctl-copy"

type CopyOptions struct {
	allowRoot       bool
	verify          func(context.Context, *os.Root, string) error
	SourceRoot      string `json:"root"`
	DestinationRoot string `json:"to_root"`
	From            string `json:"from"`
	To              string `json:"to"`
	Limit           int    `json:"limit"`
	MaxBytes        int64  `json:"max_bytes"`
	Move            bool   `json:"move"`
}
type CopyResult struct {
	Entries         int64  `json:"entries"`
	Bytes           int64  `json:"bytes"`
	Skipped         int    `json:"skipped_internal"`
	Published       bool   `json:"published"`
	SourceTrashed   string `json:"source_trash_id,omitempty"`
	DestinationRoot string `json:"destination_root"`
	Path            string `json:"path"`
}
type CopyProgress func(entries, bytes int64, phase string)
type copyManifest struct {
	Version   int         `json:"version"`
	Options   CopyOptions `json:"options"`
	StartedAt int64       `json:"started_at"`
}

func ValidateCopy(o CopyOptions) (CopyOptions, error) {
	var err error
	if o.SourceRoot == "" || o.DestinationRoot == "" {
		return o, ErrBadPath
	}
	if o.allowRoot && !o.Move {
		o.From, err = cleanRel(o.From)
	} else {
		o.From, err = writablePath(o.From)
	}
	if err != nil {
		return o, err
	}
	if o.To, err = writablePath(o.To); err != nil {
		return o, err
	}
	if o.Limit < 1 || o.Limit > 10000 || o.MaxBytes < 1 || o.MaxBytes > MaxTransferBytes {
		return o, ErrTreeLimit
	}
	if o.SourceRoot == o.DestinationRoot && (o.From == o.To || strings.HasPrefix(o.To, o.From+"/")) {
		return o, ErrBadPath
	}
	return o, nil
}
func sameSource(a, b os.FileInfo) bool {
	return sameIdentity(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// Publish a complete copy with no replacement. Cross-root move is copy then
// recoverable trash, under ordered root locks; a trash failure keeps both copies.
func (m *Manager) Copy(ctx context.Context, options CopyOptions, progress CopyProgress) (CopyResult, error) {
	result := CopyResult{DestinationRoot: options.DestinationRoot, Path: options.To}
	o, err := ValidateCopy(options)
	if err != nil {
		return result, err
	}
	if o.Limit > m.limits.CopyMaxEntries || o.MaxBytes > m.limits.CopyMaxBytes {
		return result, ErrTreeLimit
	}
	if o.Move && o.SourceRoot == o.DestinationRoot {
		err = m.Rename(ctx, o.SourceRoot, o.From, o.To)
		result.Published = err == nil
		result.Entries = 1
		return result, err
	}
	src, err := m.root(o.SourceRoot)
	if err != nil {
		return result, err
	}
	dst, err := m.root(o.DestinationRoot)
	if err != nil {
		return result, err
	}
	if o.Move && !src.Writable {
		return result, ErrReadOnly
	}
	var release []func()
	defer func() {
		for i := len(release) - 1; i >= 0; i-- {
			release[i]()
		}
	}()
	ids := []string{o.DestinationRoot}
	if src.Writable && o.SourceRoot != o.DestinationRoot {
		ids = append(ids, o.SourceRoot)
		if ids[0] > ids[1] {
			ids[0], ids[1] = ids[1], ids[0]
		}
	}
	for _, id := range ids {
		_, unlock, err := m.lockWrite(ctx, id)
		if err != nil {
			return result, err
		}
		release = append(release, unlock)
	}
	if _, err = safeLstat(dst.dir, o.To); err == nil {
		return result, ErrExists
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrNotExist) {
		return result, mapWriteErr(err)
	}
	if _, err = openParentCheck(dst.dir, o.To); err != nil {
		return result, err
	}
	work, err := internalRoot(dst, copyDirectory, true)
	if err != nil {
		return result, err
	}
	defer work.Close()
	if err = cleanCopies(ctx, work); err != nil {
		return result, err
	}
	id, err := randomFileID()
	if err != nil {
		return result, err
	}
	if err = work.Mkdir(id, 0700); err != nil {
		return result, mapWriteErr(err)
	}
	bucket, err := work.OpenRoot(id)
	if err != nil {
		return result, mapWriteErr(err)
	}
	defer bucket.Close()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if removeBounded(cleanup, bucket, "entry", o.Limit) == nil {
			_ = bucket.Remove("metadata.json")
			_ = work.Remove(id)
		}
	}()
	f, err := bucket.OpenFile("metadata.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, mapWriteErr(err)
	}
	err = json.NewEncoder(f).Encode(copyManifest{1, o, time.Now().Unix()})
	if err == nil {
		err = f.Sync()
	}
	_ = f.Close()
	if err != nil {
		return result, mapWriteErr(err)
	}
	observations := []treeNode{}
	buffer := make([]byte, 64<<10)
	var copyNode func(string, string, int) error
	copyNode = func(from, to string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 32 || len(observations)+result.Skipped >= o.Limit {
			return ErrTreeLimit
		}
		info, err := safeLstat(src.dir, from)
		if err != nil {
			return mapWriteErr(err)
		}
		observations = append(observations, treeNode{from, info})
		result.Entries++
		switch {
		case info.IsDir():
			if err = bucket.Mkdir(to, 0750); err != nil {
				return mapWriteErr(err)
			}
			dir, err := safeOpen(src.dir, from, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK)
			if err != nil {
				return mapWriteErr(err)
			}
			defer dir.Close()
			opened, err := dir.Stat()
			if err != nil || !sameSource(info, opened) {
				return ErrSourceChanged
			}
			for {
				entries, readErr := dir.ReadDir(64)
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					return mapWriteErr(readErr)
				}
				for _, entry := range entries {
					if internalName(entry.Name()) {
						result.Skipped++
						if int(result.Entries)+result.Skipped >= o.Limit {
							return ErrTreeLimit
						}
						continue
					}
					if err = copyNode(path.Join(from, entry.Name()), path.Join(to, entry.Name()), depth+1); err != nil {
						return err
					}
				}
				if errors.Is(readErr, io.EOF) {
					break
				}
			}
		case info.Mode().IsRegular():
			if info.Size() > o.MaxBytes-result.Bytes {
				return ErrTreeLimit
			}
			in, err := safeOpen(src.dir, from, os.O_RDONLY|syscall.O_NONBLOCK)
			if err != nil {
				return mapWriteErr(err)
			}
			defer in.Close()
			opened, err := in.Stat()
			if err != nil || !sameSource(info, opened) {
				return ErrSourceChanged
			}
			out, err := bucket.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
			if err != nil {
				return mapWriteErr(err)
			}
			writer := copyWriter{out, func(n int) {
				result.Bytes += int64(n)
				if progress != nil {
					progress(result.Entries, result.Bytes, "copying")
				}
			}}
			n, copyErr := io.CopyBuffer(writer, uploadReader{ctx, io.LimitReader(in, info.Size())}, buffer)
			if copyErr == nil && n != info.Size() {
				copyErr = ErrSourceChanged
			}
			// Detect growth without writing an extra byte beyond the declared
			// shared budget into staging or counting it as copied data.
			if copyErr == nil {
				var probe [1]byte
				if err := ctx.Err(); err != nil {
					copyErr = err
				} else if extra, e := in.Read(probe[:]); extra != 0 || !errors.Is(e, io.EOF) {
					copyErr = ErrSourceChanged
				}
			}
			if copyErr == nil {
				copyErr = out.Sync()
			}
			closeErr := out.Close()
			if copyErr == nil {
				copyErr = closeErr
			}
			if copyErr != nil {
				return mapWriteErr(copyErr)
			}
			after, err := in.Stat()
			if err != nil || !sameSource(info, after) {
				return ErrSourceChanged
			}
			if err = bucket.Chtimes(to, info.ModTime(), info.ModTime()); err != nil {
				return mapWriteErr(err)
			}
		case info.Mode()&os.ModeSymlink != 0:
			target, err := safeReadlink(src.dir, from)
			if err != nil {
				return mapWriteErr(err)
			}
			if err = bucket.Symlink(target, to); err != nil {
				return mapWriteErr(err)
			}
		default:
			return ErrNotRegular
		}
		if progress != nil {
			progress(result.Entries, result.Bytes, "copying")
		}
		return nil
	}
	if err = copyNode(o.From, "entry", 0); err != nil {
		return result, err
	}
	if o.Move && result.Skipped > 0 {
		return result, ErrUnsupported
	}
	for _, node := range observations {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		current, err := safeLstat(src.dir, node.path)
		if err != nil || !sameSource(node.info, current) {
			return result, ErrSourceChanged
		}
	}
	if progress != nil {
		progress(result.Entries, result.Bytes, "publishing")
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if o.verify != nil {
		if err = o.verify(ctx, bucket, "entry"); err != nil {
			return result, err
		}
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = moveNoReplace(bucket, "entry", dst.dir, o.To); err != nil {
		return result, err
	}
	result.Published = true
	if o.Move {
		if err = ctx.Err(); err != nil {
			return result, fmt.Errorf("副本已保存，源内容仍保留：%w", err)
		}
		for _, node := range observations {
			current, err := safeLstat(src.dir, node.path)
			if err != nil || !sameSource(node.info, current) {
				return result, fmt.Errorf("副本已保存，源内容仍保留：%w", ErrSourceChanged)
			}
		}
		item, trashErr := m.trashLocked(ctx, src, o.From)
		if trashErr != nil {
			return result, fmt.Errorf("副本已保存，源内容仍保留：%w", trashErr)
		}
		result.SourceTrashed = item.ID
	}
	return result, nil
}

type copyWriter struct {
	w        io.Writer
	progress func(int)
}

func (w copyWriter) Write(b []byte) (int, error) {
	n, err := w.w.Write(b)
	if n > 0 {
		w.progress(n)
	}
	return n, err
}
func openParentCheck(root *os.Root, p string) (bool, error) {
	f, err := openParent(root, p)
	if err != nil {
		return false, err
	}
	f.Close()
	return true, nil
}
func cleanCopies(ctx context.Context, work *os.Root) error {
	f, err := work.Open(".")
	if err != nil {
		return mapWriteErr(err)
	}
	defer f.Close()
	entries, err := f.ReadDir(66)
	if err != nil && !errors.Is(err, io.EOF) {
		return mapWriteErr(err)
	}
	if len(entries) > 65 {
		return ErrNoSpace
	}
	for _, entry := range entries {
		if !validFileID(entry.Name()) || !entry.IsDir() {
			continue
		}
		bucket, err := work.OpenRoot(entry.Name())
		if err != nil {
			continue
		}
		meta, err := bucket.OpenFile("metadata.json", os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			bucket.Close()
			continue
		}
		var manifest copyManifest
		info, err := meta.Stat()
		if err == nil && info.Mode().IsRegular() && info.Size() < 8192 {
			decoder := json.NewDecoder(io.LimitReader(meta, 8192))
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&manifest)
			if err == nil && decoder.Decode(new(any)) != io.EOF {
				err = ErrBadPath
			}
		} else {
			err = ErrBadPath
		}
		meta.Close()
		if err == nil {
			if manifest.Version != 1 {
				err = ErrBadPath
			} else {
				manifest.Options.allowRoot = manifest.Options.DestinationRoot == "@vault" && !manifest.Options.Move
				_, err = ValidateCopy(manifest.Options)
				if err == nil {
					err = removeBounded(ctx, bucket, "entry", manifest.Options.Limit)
				}
			}
		}
		if err == nil {
			_ = bucket.Remove("metadata.json")
			_ = work.Remove(entry.Name())
		}
		bucket.Close()
	}
	return nil
}
