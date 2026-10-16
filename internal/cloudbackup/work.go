package cloudbackup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"filippo.io/age"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/klauspost/compress/zstd"
)

type manifestItem struct {
	Name       string `json:"name"`
	Directory  bool   `json:"directory"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256,omitempty"`
	ModifiedAt int64  `json:"modified_at,omitempty"`
}
type manifest struct {
	Version int            `json:"version"`
	Items   []manifestItem `json:"items"`
}

func passwordValid(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) >= 12 && len(value) <= 256
}
func remoteName(r Record) string {
	if r.Input.Mode == "encrypted" {
		return r.ID + ".tar.zst.age"
	}
	return r.ID
}
func (s *Store) Run(ctx context.Context, req RunRequest, progress func(Progress)) (record Record, err error) {
	c, e := s.begin()
	if e != nil {
		return record, e
	}
	defer s.end()
	if c.Revision != req.Revision || validateConnection(c) != nil {
		return record, ErrInvalid
	}
	if req.Mode == "encrypted" && !passwordValid(req.Password) || req.Mode == "direct" && req.Password != "" {
		return record, ErrInvalid
	}
	if req.Mode == "direct" {
		if v, ok := s.Vault.(interface{ HasPending() bool }); ok && v.HasPending() {
			return record, ErrPendingVault
		}
	}
	source, runtime, e := s.validateInput(req.Input)
	if e != nil {
		return record, e
	}
	ctx, stop := context.WithTimeout(ctx, runtime)
	defer stop()
	id, e := randomID()
	if e != nil {
		return record, e
	}
	record = Record{ID: id, Input: req.Input, SourceScope: source, ConnectionScope: scope(c), CreatedAt: time.Now().Unix(), State: "running"}
	if e = s.update(record); e != nil {
		return record, e
	}
	defer func() {
		if err != nil {
			record.State = "failed"
			record.Error = "备份未完成，原文件保留。请检查连接、空间和备份范围后重试。"
			record.Retryable = record.Input.Mode == "direct" || len(record.SHA256) == 64
			_ = s.update(record)
		}
	}()
	if req.Mode == "encrypted" {
		if err = s.pack(ctx, &record, req.Password, progress); err != nil {
			return record, err
		}
		req.Password = ""
		err = s.uploadArchive(ctx, c, &record, progress)
	} else {
		err = s.uploadDirect(ctx, c, &record, progress)
	}
	if err != nil {
		return record, err
	}
	record.State = "complete"
	record.Error = ""
	record.Retryable = false
	err = s.update(record)
	if err == nil && record.Input.Mode == "encrypted" {
		_ = s.root.Remove(record.ID + ".age")
	}
	return record, err
}
func (s *Store) pack(ctx context.Context, r *Record, password string, progress func(Progress)) (err error) {
	f, e := s.root.OpenFile(r.ID+".age", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	complete := false
	defer func() {
		f.Close()
		if !complete {
			_ = s.root.Remove(r.ID + ".age")
		}
	}()
	recipient, e := age.NewScryptRecipient(password)
	if e != nil {
		return ErrInvalid
	}
	recipient.SetWorkFactor(15)
	hash := sha256.New()
	bounded := &budgetWriter{check: func() error {
		var stat unix.Statfs_t
		if e := unix.Fstatfs(int(f.Fd()), &stat); e != nil {
			return files.ErrNoSpace
		}
		if uint64(stat.Bavail)*uint64(stat.Bsize) < 64<<20 {
			return files.ErrNoSpace
		}
		return nil
	}, writer: io.MultiWriter(f, hash), remaining: r.Input.MaxBytes + r.Input.MaxBytes/100 + int64(r.Input.Limit)*4096 + (16 << 20)}
	encrypted, e := age.Encrypt(bounded, recipient)
	if e != nil {
		return e
	}
	compressed, e := zstd.NewWriter(encrypted, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(1<<20), zstd.WithLowerEncoderMem(true))
	if e != nil {
		encrypted.Close()
		return e
	}
	export := func(ctx context.Context, w io.Writer, limit int, maxBytes int64, p files.CopyProgress) (files.ArchiveResult, error) {
		return s.files.Archive(ctx, r.Input.Root, r.Input.Path, w, limit, maxBytes, p)
	}
	if r.Input.Root == "@photo-vault" {
		export = s.Vault.ArchiveBackup
	}
	result, err := export(ctx, compressed, r.Input.Limit, r.Input.MaxBytes, func(entries, n int64, _ string) {
		if progress != nil {
			progress(Progress{"pack", entries, n, 0})
		}
	})
	closeErr := compressed.Close()
	encryptErr := encrypted.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if encryptErr != nil {
		return encryptErr
	}
	if err = f.Sync(); err != nil {
		return err
	}
	info, e := f.Stat()
	if e != nil {
		return e
	}
	r.Entries = result.Entries
	r.Bytes = result.Bytes
	r.Skipped = result.Skipped
	r.StoredBytes = info.Size()
	r.SHA256 = hex.EncodeToString(hash.Sum(nil))
	dir, e := s.root.Open(".")
	if e != nil {
		return e
	}
	e = dir.Sync()
	dir.Close()
	if e != nil {
		return e
	}
	if err = s.update(*r); err != nil {
		return err
	}
	complete = true
	return nil
}

type budgetWriter struct {
	check     func() error
	checked   int64
	writer    io.Writer
	remaining int64
}

func (w *budgetWriter) Write(b []byte) (int, error) {
	if w.check != nil && (w.checked == 0 || w.checked >= 1<<20) {
		if e := w.check(); e != nil {
			return 0, e
		}
		w.checked = 1
	}
	w.checked += int64(len(b))
	if int64(len(b)) > w.remaining {
		return 0, files.ErrTreeLimit
	}
	n, e := w.writer.Write(b)
	w.remaining -= int64(n)
	return n, e
}
func (s *Store) uploadArchive(ctx context.Context, c Connection, r *Record, progress func(Progress)) error {
	final := remoteName(*r)
	res, e := s.request(ctx, c, "GET", final, nil, 0, nil)
	if e != nil {
		return e
	}
	if res.StatusCode == 200 {
		res.Body.Close()
		return s.verify(ctx, c, final, r.StoredBytes, r.SHA256)
	}
	if e = finish(res, 404); e != nil {
		return e
	}
	file, e := s.root.OpenFile(r.ID+".age", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return e
	}
	defer file.Close()
	info, e := file.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Size() != r.StoredBytes {
		return ErrInvalid
	}
	random, e := randomID()
	if e != nil {
		return e
	}
	temporary := "." + r.ID + "-" + random + ".part"
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		s.removeTemporary(cleanup, c, temporary)
	}()
	sum, e := s.put(ctx, c, temporary, file, r.StoredBytes, func(n int64) {
		if progress != nil {
			progress(Progress{"upload", r.Entries, n, r.StoredBytes})
		}
	})
	if e != nil {
		return e
	}
	if sum != r.SHA256 {
		return ErrRemote
	}
	if progress != nil {
		progress(Progress{"publish", r.Entries, r.StoredBytes, r.StoredBytes})
	}
	return s.move(ctx, c, temporary, final)
}
func (s *Store) sourceArchive(ctx context.Context, in Input) (*io.PipeReader, <-chan struct {
	result files.ArchiveResult
	err    error
}) {
	reader, writer := io.Pipe()
	done := make(chan struct {
		result files.ArchiveResult
		err    error
	}, 1)
	go func() {
		r, e := s.files.Archive(ctx, in.Root, in.Path, writer, in.Limit, in.MaxBytes, nil)
		writer.CloseWithError(e)
		done <- struct {
			result files.ArchiveResult
			err    error
		}{r, e}
	}()
	return reader, done
}
func (s *Store) uploadDirect(ctx context.Context, c Connection, r *Record, progress func(Progress)) error {
	if len(r.SHA256) == 64 {
		published, e := s.directPublished(ctx, c, *r)
		if e != nil {
			return e
		}
		if published {
			return nil
		}
	}
	random, e := randomID()
	if e != nil {
		return e
	}
	temporary := "." + r.ID + "-" + random + ".part"
	if e = s.mkdir(ctx, c, temporary); e != nil {
		return e
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		s.removeTemporary(cleanup, c, temporary)
	}()
	reader, done := s.sourceArchive(ctx, r.Input)
	defer func() { reader.Close(); <-done }()
	tr := tar.NewReader(reader)
	m := manifest{Version: 1, Items: []manifestItem{}}
	used := 0
	r.Bytes = 0
	r.Entries = 0
	for {
		h, e := tr.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name != "data" && !strings.HasPrefix(name, "data/") {
			return ErrInvalid
		}
		r.Entries++
		used += len(name) + 160
		if used > 4<<20 {
			return files.ErrTreeLimit
		}
		item := manifestItem{Name: name, Directory: h.Typeflag == tar.TypeDir, Bytes: h.Size, ModifiedAt: h.ModTime.UnixNano()}
		remote := path.Join(temporary, name)
		if item.Directory {
			if e = s.mkdir(ctx, c, remote); e != nil {
				return e
			}
		} else {
			before := r.Bytes
			sum, e := s.put(ctx, c, remote, tr, h.Size, func(n int64) {
				if progress != nil {
					progress(Progress{"upload", r.Entries, before + n, 0})
				}
			})
			if e != nil {
				return e
			}
			r.Bytes += h.Size
			item.SHA256 = sum
		}
		m.Items = append(m.Items, item)
	}
	// The tar writer closes only after checking every source identity again.
	if _, e = io.Copy(io.Discard, reader); e != nil {
		return e
	}
	body, e := json.Marshal(m)
	if len(body) > 4<<20 {
		return files.ErrTreeLimit
	}
	if e != nil {
		return e
	}
	r.StoredBytes = int64(len(body))
	sum, e := s.put(ctx, c, path.Join(temporary, "manifest.json"), bytes.NewReader(body), int64(len(body)), nil)
	if e != nil {
		return e
	}
	r.SHA256 = sum
	if e = s.update(*r); e != nil {
		return e
	}
	if progress != nil {
		progress(Progress{"publish", r.Entries, r.Bytes, r.Bytes})
	}
	return s.move(ctx, c, temporary, r.ID)
}
func (s *Store) Retry(ctx context.Context, id string, progress func(Progress)) (record Record, err error) {
	c, e := s.begin()
	if e != nil {
		return record, e
	}
	defer s.end()
	record, e = s.find(id)
	if e != nil || record.ConnectionScope != scope(c) || !record.Retryable || record.State == "complete" {
		return record, ErrInvalid
	}
	runtime, e := time.ParseDuration(record.Input.MaxRuntime)
	if e != nil || runtime < time.Second || runtime > 2*time.Hour {
		return record, ErrInvalid
	}
	if record.Input.Mode == "direct" {
		if len(record.SHA256) == 64 {
			published, e := s.directPublished(ctx, c, record)
			if e != nil {
				return record, e
			}
			if published {
				record.State = "complete"
				record.Error = ""
				record.Retryable = false
				return record, s.update(record)
			}
		}
		source, _, e := s.validateInput(record.Input)
		if e != nil || source != record.SourceScope {
			return record, ErrInvalid
		}
	}
	ctx, stop := context.WithTimeout(ctx, runtime)
	defer stop()
	record.State = "running"
	record.Error = ""
	if err = s.update(record); err != nil {
		return record, err
	}
	if record.Input.Mode == "encrypted" {
		err = s.uploadArchive(ctx, c, &record, progress)
	} else {
		err = s.uploadDirect(ctx, c, &record, progress)
	}
	if err == nil {
		record.State = "complete"
		record.Retryable = false
	} else {
		record.State = "failed"
		record.Error = "重试未完成，请检查连接和备份范围。"
	}
	saveErr := s.update(record)
	if err == nil {
		err = saveErr
	}
	if err == nil && record.Input.Mode == "encrypted" {
		_ = s.root.Remove(record.ID + ".age")
	}
	return record, err
}

func (s *Store) directPublished(ctx context.Context, c Connection, r Record) (bool, error) {
	res, e := s.request(ctx, c, "GET", path.Join(r.ID, "manifest.json"), nil, 0, nil)
	if e != nil {
		return false, e
	}
	if res.StatusCode == 404 {
		return false, finish(res, 404)
	}
	if res.StatusCode != 200 {
		return false, finish(res, 200)
	}
	res.Body.Close()
	m, e := s.getManifest(ctx, c, r)
	if e != nil {
		return false, e
	}
	for _, item := range m.Items {
		if !item.Directory {
			if e = s.verify(ctx, c, path.Join(r.ID, item.Name), item.Bytes, item.SHA256); e != nil {
				return false, e
			}
		}
	}
	return true, nil
}
