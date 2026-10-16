package cloudbackup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/klauspost/compress/zstd"
)

func (s *Store) getManifest(ctx context.Context, c Connection, r Record) (manifest, error) {
	var m manifest
	res, e := s.request(ctx, c, "GET", path.Join(r.ID, "manifest.json"), nil, 0, nil)
	if e != nil {
		return m, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return m, failure("读取备份清单", "GET", path.Join(r.ID, "manifest.json"), res, "")
	}
	if r.StoredBytes < 1 || r.StoredBytes > 4<<20 {
		return m, ErrRemote
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, r.StoredBytes+1))
	sum := sha256.Sum256(b)
	if e != nil || int64(len(b)) != r.StoredBytes || hex.EncodeToString(sum[:]) != r.SHA256 {
		return m, &RemoteError{Step: "读取备份清单", Method: "GET", Path: path.Join(r.ID, "manifest.json"), Status: 200, Reason: "远端清单与本机记录不一致，备份可能被修改或替换"}
	}
	if json.Unmarshal(b, &m) != nil || m.Version != 1 || len(m.Items) > r.Input.Limit {
		return m, ErrRemote
	}
	seen := map[string]bool{}
	var total int64
	for _, item := range m.Items {
		if item.Name != "data" && !strings.HasPrefix(item.Name, "data/") || path.Clean(item.Name) != item.Name || len(item.Name) > 1024 || strings.ContainsAny(item.Name, "\\\x00") || seen[item.Name] || item.Bytes < 0 || item.Bytes > r.Input.MaxBytes-total || item.Directory && item.Bytes != 0 || !item.Directory && len(item.SHA256) != 64 {
			return m, ErrRemote
		}
		seen[item.Name] = true
		total += item.Bytes
	}
	if !seen["data"] || total != r.Bytes {
		return m, ErrRemote
	}
	return m, nil
}
func (s *Store) streamDirect(ctx context.Context, c Connection, r Record, m manifest, w *io.PipeWriter, progress func(Progress)) {
	tw := tar.NewWriter(w)
	var err error
	var count, n int64
	defer func() {
		closeErr := tw.Close()
		if err == nil {
			err = closeErr
		}
		w.CloseWithError(err)
	}()
	for _, item := range m.Items {
		if err = ctx.Err(); err != nil {
			return
		}
		count++
		h := &tar.Header{Name: item.Name, Size: item.Bytes, Mode: 0600, Typeflag: tar.TypeReg}
		if item.ModifiedAt != 0 {
			h.ModTime = time.Unix(0, item.ModifiedAt)
		}
		if item.Directory {
			h.Name += "/"
			h.Mode = 0700
			h.Typeflag = tar.TypeDir
		}
		if err = tw.WriteHeader(h); err != nil {
			return
		}
		if item.Directory {
			continue
		}
		var resBody io.ReadCloser
		var responseErr error
		res, responseErr := s.request(ctx, c, "GET", path.Join(r.ID, item.Name), nil, 0, nil)
		if responseErr != nil {
			err = responseErr
			return
		}
		resBody = res.Body
		if res.StatusCode != 200 {
			err = failure("下载备份文件", "GET", path.Join(r.ID, item.Name), res, "")
			resBody.Close()
			return
		}
		hash := sha256.New()
		copied, e := io.CopyBuffer(io.MultiWriter(tw, hash), io.LimitReader(resBody, item.Bytes+1), make([]byte, 64<<10))
		resBody.Close()
		if e != nil || copied != item.Bytes || hex.EncodeToString(hash.Sum(nil)) != item.SHA256 {
			err = &RemoteError{Step: "下载备份文件", Method: "GET", Path: path.Join(r.ID, item.Name), Status: 200, Reason: "下载内容与备份清单不一致，远端文件可能被修改或损坏"}
			return
		}
		n += copied
		if progress != nil {
			progress(Progress{"restore", count, n, r.Bytes})
		}
	}
}
func (s *Store) Restore(ctx context.Context, id, toRoot, to, password string, progress func(Progress)) (files.CopyResult, error) {
	c, e := s.begin()
	if e != nil {
		return files.CopyResult{}, e
	}
	defer s.end()
	r, e := s.find(id)
	if e != nil || r.State != "complete" || r.ConnectionScope != scope(c) {
		return files.CopyResult{}, ErrInvalid
	}
	ctx, stop := context.WithTimeout(ctx, 2*time.Hour)
	defer stop()
	callback := func(entries, n int64, _ string) {
		if progress != nil {
			progress(Progress{"restore", entries, n, r.Bytes})
		}
	}
	if r.Input.Mode == "direct" {
		if password != "" {
			return files.CopyResult{}, ErrInvalid
		}
		m, e := s.getManifest(ctx, c, r)
		if e != nil {
			return files.CopyResult{}, e
		}
		reader, writer := io.Pipe()
		done := make(chan struct{})
		go func() { defer close(done); s.streamDirect(ctx, c, r, m, writer, progress) }()
		defer func() { reader.Close(); <-done }()
		return s.files.ImportArchive(ctx, toRoot, to, reader, r.Input.Limit, r.Input.MaxBytes, nil, callback)
	}
	if !passwordValid(password) {
		return files.CopyResult{}, ErrPassword
	}
	res, e := s.request(ctx, c, "GET", remoteName(r), nil, 0, nil)
	if e != nil {
		return files.CopyResult{}, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return files.CopyResult{}, failure("下载加密备份", "GET", remoteName(r), res, "")
	}
	digest := files.NewDigestReader(io.LimitReader(res.Body, r.StoredBytes+1))
	identity, e := age.NewScryptIdentity(password)
	if e != nil {
		return files.CopyResult{}, ErrPassword
	}
	identity.SetMaxWorkFactor(16)
	plaintext, e := age.Decrypt(digest, identity)
	password = ""
	if e != nil {
		return files.CopyResult{}, ErrPassword
	}
	decoder, e := zstd.NewReader(plaintext, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(16<<20), zstd.WithDecoderMaxWindow(1<<20))
	if e != nil {
		return files.CopyResult{}, ErrPassword
	}
	defer decoder.Close()
	verify := func() error {
		if n, e := io.Copy(io.Discard, io.LimitReader(plaintext, 1)); e != nil || n != 0 {
			return ErrPassword
		}
		if n, e := io.Copy(io.Discard, io.LimitReader(digest, 1)); e != nil || n != 0 || digest.Bytes != r.StoredBytes || digest.SHA256() != r.SHA256 {
			return ErrRemote
		}
		return nil
	}
	result, e := s.files.ImportArchive(ctx, toRoot, to, decoder, r.Input.Limit, r.Input.MaxBytes, verify, callback)
	if e != nil && !errors.Is(e, context.Canceled) && !errors.Is(e, context.DeadlineExceeded) && !errors.Is(e, files.ErrExists) && !errors.Is(e, files.ErrReadOnly) && !errors.Is(e, files.ErrBadPath) && !errors.Is(e, files.ErrTreeLimit) {
		return result, ErrPassword
	}
	return result, e
}
func (s *Store) Download(ctx context.Context, id string) (io.ReadCloser, int64, string, error) {
	r, e := s.find(id)
	s.mu.Lock()
	c := s.connection
	s.mu.Unlock()
	if e != nil || r.State != "complete" || r.Input.Mode != "encrypted" || r.ConnectionScope != scope(c) {
		return nil, 0, "", ErrInvalid
	}
	res, e := s.request(ctx, c, "GET", remoteName(r), nil, 0, nil)
	if e != nil {
		return nil, 0, "", e
	}
	if res.StatusCode != 200 {
		e := failure("下载加密备份", "GET", remoteName(r), res, "")
		res.Body.Close()
		return nil, 0, "", e
	}
	return &verifiedBody{ReadCloser: res.Body, reader: files.NewDigestReader(io.LimitReader(res.Body, r.StoredBytes+1)), size: r.StoredBytes, sum: r.SHA256}, r.StoredBytes, remoteName(r), nil
}

type verifiedBody struct {
	io.ReadCloser
	reader *files.DigestReader
	size   int64
	sum    string
}

func (b *verifiedBody) Read(p []byte) (int, error) {
	n, e := b.reader.Read(p)
	if e == io.EOF && (b.reader.Bytes != b.size || b.reader.SHA256() != b.sum) {
		e = ErrRemote
	}
	return n, e
}
