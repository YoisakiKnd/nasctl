package anime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/fsutil"
)

const metadataBackupFiles = 1000
const metadataBackupBytes = 64 << 20

type MetadataBackup struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	SavedAt   int64  `json:"saved_at"`
	SavedAtMS int64  `json:"saved_at_ms"`
	Revision  string `json:"revision"`
}

func metadataPath(value string) (string, error) {
	clean, err := safeRelative(value)
	if err != nil || clean != value || (!strings.HasSuffix(clean, ".nfo") && path.Base(clean) != "poster.jpg") {
		return "", inputError("元数据目标路径无效")
	}
	return clean, nil
}

func (e *Engine) backupMetadata(dest string, data []byte) error {
	if len(data) > MaxFeedBytes {
		return inputError("已有元数据超过 2 MiB，未替换")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	sum := sha256.Sum256(append([]byte(dest), nonce[:]...))
	digest := sha256.Sum256(data)
	info := MetadataBackup{ID: hex.EncodeToString(sum[:]), Path: dest, Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), SavedAt: time.Now().Unix(), SavedAtMS: time.Now().UnixMilli(), Revision: hex.EncodeToString(nonce[:])}
	header, err := json.Marshal(info)
	if err != nil || len(header) > 4000 {
		return inputError("元数据备份信息过长")
	}
	if err := os.MkdirAll(e.metadataDir, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(e.metadataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(metadataBackupFiles + 1)
	dir.Close()
	if (err != nil && !errors.Is(err, io.EOF)) || len(entries) > metadataBackupFiles {
		return inputError("元数据备份已达到文件上限")
	}
	var total int64
	count := 0
	name := info.ID + ".backup"
	for _, entry := range entries {
		stat, err := entry.Info()
		if err != nil || !stat.Mode().IsRegular() {
			return inputError("元数据备份目录包含异常条目")
		}
		total += stat.Size()
		count++
	}
	if count >= metadataBackupFiles || total+int64(len(header)+1+len(data)) > metadataBackupBytes {
		return inputError("元数据备份达到 64 MiB／1000 文件限额，请先导出并清理备份")
	}
	return fsutil.AtomicWrite(filepath.Join(e.metadataDir, name), 0600, func(w io.Writer) error {
		if _, err := w.Write(append(header, '\n')); err != nil {
			return err
		}
		_, err := w.Write(data)
		return err
	})
}

func (e *Engine) updateMetadataFile(ctx context.Context, dest string, data []byte, replace bool) (bool, error) {
	if _, err := metadataPath(dest); err != nil {
		return false, err
	}
	if len(data) > MaxFeedBytes {
		return false, inputError("元数据超过 2 MiB 上限")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var oldInfo os.FileInfo
	if info, err := e.libraryRoot.Lstat(dest); err == nil {
		if !info.Mode().IsRegular() {
			return false, inputError("已有元数据不是普通文件，未替换")
		}
		if !replace {
			return false, nil
		}
		f, err := e.libraryRoot.OpenFile(dest, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return false, err
		}
		oldInfo, err = f.Stat()
		if err != nil {
			f.Close()
			return false, err
		}
		old, err := io.ReadAll(io.LimitReader(f, MaxFeedBytes+1))
		f.Close()
		if err != nil {
			return false, err
		}
		if len(old) > MaxFeedBytes {
			return false, inputError("已有元数据超过 2 MiB，未替换")
		}
		if bytes.Equal(old, data) {
			return false, nil
		}
		if !os.SameFile(info, oldInfo) || info.Size() != oldInfo.Size() || !info.ModTime().Equal(oldInfo.ModTime()) {
			return false, inputError("元数据已发生变化，请重试")
		}
		if err = e.backupMetadata(dest, old); err != nil {
			return false, err
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return false, err
	}
	tmp := path.Join(path.Dir(dest), ".nasctl-meta-"+hex.EncodeToString(token[:]))
	f, err := e.libraryRoot.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return false, err
	}
	defer e.libraryRoot.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	if oldInfo != nil {
		current, err := e.libraryRoot.Lstat(dest)
		if err != nil || !current.Mode().IsRegular() || !os.SameFile(current, oldInfo) || current.Size() != oldInfo.Size() || !current.ModTime().Equal(oldInfo.ModTime()) {
			return false, inputError("元数据已发生变化，请核对后重试")
		}
		err = e.libraryRoot.Rename(tmp, dest)
	} else {
		err = e.libraryRoot.Link(tmp, dest)
	}
	if err != nil {
		return false, err
	}
	dir, err := e.libraryRoot.Open(path.Dir(dest))
	if err != nil {
		return true, err
	}
	defer dir.Close()
	return true, dir.Sync()
}

func (e *Engine) readMetadataBackup(id string, withData bool) (MetadataBackup, []byte, error) {
	var info MetadataBackup
	if len(id) != 64 {
		return info, nil, inputError("备份编号无效")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return info, nil, inputError("备份编号无效")
	}
	root, err := os.OpenRoot(e.metadataDir)
	if err != nil {
		return info, nil, err
	}
	defer root.Close()
	f, err := root.OpenFile(id+".backup", os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return info, nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > MaxFeedBytes+4096 {
		return info, nil, inputError("元数据备份无效或过大")
	}
	r := bufio.NewReaderSize(f, 4096)
	header, err := r.ReadSlice('\n')
	if err != nil {
		return info, nil, inputError("元数据备份头无效")
	}
	if json.Unmarshal(header, &info) != nil || info.ID != id || info.Size < 0 || info.Size > MaxFeedBytes {
		return info, nil, inputError("元数据备份无效")
	}
	if _, err := metadataPath(info.Path); err != nil {
		return info, nil, err
	}
	nonce, err := hex.DecodeString(info.Revision)
	if err != nil || len(nonce) != 16 {
		return info, nil, inputError("元数据备份版本无效")
	}
	key := sha256.Sum256(append([]byte(info.Path), nonce...))
	if hex.EncodeToString(key[:]) != id {
		return info, nil, inputError("元数据备份路径不匹配")
	}
	if !withData {
		return info, nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxFeedBytes+1))
	if err != nil {
		return info, nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != info.Size || hex.EncodeToString(sum[:]) != info.SHA256 {
		return info, nil, inputError("元数据备份校验失败，未恢复")
	}
	return info, data, nil
}

func (e *Engine) MetadataBackups(ctx context.Context) ([]MetadataBackup, error) {
	if !e.mu.TryLock() {
		return nil, ErrBusy
	}
	defer e.mu.Unlock()
	out := []MetadataBackup{}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	f, err := os.Open(e.metadataDir)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	entries, err := f.ReadDir(metadataBackupFiles + 1)
	f.Close()
	if (err != nil && !errors.Is(err, io.EOF)) || len(entries) > metadataBackupFiles {
		return out, inputError("元数据备份目录超过上限")
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".backup") {
			info, _, err := e.readMetadataBackup(strings.TrimSuffix(entry.Name(), ".backup"), false)
			if err != nil {
				return out, err
			}
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SavedAtMS == out[j].SavedAtMS {
			return out[i].ID > out[j].ID
		}
		return out[i].SavedAtMS > out[j].SavedAtMS
	})
	return out, nil
}

func (e *Engine) RestoreMetadata(ctx context.Context, id string) error {
	if !e.mu.TryLock() {
		return ErrBusy
	}
	defer e.mu.Unlock()
	if err := e.admit(ctx); err != nil {
		return err
	}
	info, data, err := e.readMetadataBackup(id, true)
	if err != nil {
		return err
	}
	subs, err := e.Store.Subscriptions(ctx)
	if err != nil {
		return err
	}
	allowed := false
	for _, sub := range subs {
		if info.Path == path.Join(sub.Title, "tvshow.nfo") || info.Path == path.Join(sub.Title, "poster.jpg") {
			allowed = true
			break
		}
	}
	if !allowed && strings.HasSuffix(info.Path, ".nfo") {
		stem := strings.TrimSuffix(info.Path, ".nfo")
		args := []any{}
		marks := []string{}
		for _, ext := range []string{".mp4", ".mkv", ".avi", ".webm", ".m4v", ".ts", ".mov"} {
			args = append(args, stem+ext)
			marks = append(marks, "?")
		}
		err = e.Store.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM episodes WHERE state='archived' AND library_path IN (`+strings.Join(marks, ",")+`))`, args...).Scan(&allowed)
		if err != nil {
			return err
		}
	}
	if !allowed {
		return inputError("此备份不属于现有订阅或媒体库，未恢复")
	}
	_, err = e.updateMetadataFile(ctx, info.Path, data, true)
	if errors.Is(err, os.ErrNotExist) {
		return inputError("元数据所在目录不可用，未恢复")
	}
	return err
}

func (e *Engine) ExportMetadataBackup(ctx context.Context, id string) (MetadataBackup, []byte, error) {
	if !e.mu.TryLock() {
		return MetadataBackup{}, nil, ErrBusy
	}
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return MetadataBackup{}, nil, err
	}
	return e.readMetadataBackup(id, true)
}
func (e *Engine) DeleteMetadataBackup(ctx context.Context, id, expectedSHA string) error {
	if !e.mu.TryLock() {
		return ErrBusy
	}
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	info, _, err := e.readMetadataBackup(id, false)
	if err != nil {
		return err
	}
	if expectedSHA == "" || expectedSHA != info.SHA256 {
		return inputError("备份已变化，请刷新并重新确认清理")
	}
	root, err := os.OpenRoot(e.metadataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(id + ".backup")
}
