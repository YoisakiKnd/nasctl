package files

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"syscall"
	"time"
)

var ErrTrashFull = errors.New("files: trash record limit reached")

type TrashItem struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	OriginalPath     string `json:"original_path"`
	DeletedAt        int64  `json:"deleted_at"`
	IsDir            bool   `json:"is_dir"`
	Size             int64  `json:"size"`
	RecoveryRequired bool   `json:"recovery_required"`
	PurgeStarted     bool   `json:"purge_started"`
}
type trashManifest struct {
	Version   int    `json:"version"`
	Path      string `json:"path"`
	DeletedAt int64  `json:"deleted_at"`
}
type TrashListing struct {
	Items        []TrashItem `json:"items"`
	Total        int         `json:"total"`
	Page         int         `json:"page"`
	PageSize     int         `json:"page_size"`
	Truncated    bool        `json:"truncated"`
	EmptyRecords int         `json:"empty_records"`
}

func readManifest(bucket *os.Root) (trashManifest, error) {
	var out trashManifest
	f, err := bucket.OpenFile("metadata.json", os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return out, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return out, ErrBadPath
	}
	d := json.NewDecoder(io.LimitReader(f, 4097))
	d.DisallowUnknownFields()
	if err = d.Decode(&out); err != nil {
		return out, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return out, ErrBadPath
	}
	p, err := writablePath(out.Path)
	if err != nil || p != out.Path || out.Version != 1 || out.DeletedAt <= 0 {
		return out, ErrBadPath
	}
	return out, nil
}
func trashEntries(trash *os.Root, maximum int) ([]os.DirEntry, bool, error) {
	f, err := trash.Open(".")
	if err != nil {
		return nil, false, mapWriteErr(err)
	}
	defer f.Close()
	entries, err := f.ReadDir(maximum + 2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, mapWriteErr(err)
	}
	filtered := make([]os.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() != ".owner" {
			filtered = append(filtered, entry)
		}
	}
	truncated := len(filtered) > maximum
	if truncated {
		filtered = filtered[:maximum]
	}
	return filtered, truncated, nil
}
func itemInTrash(trash *os.Root, id string) (TrashItem, bool, error) {
	item := TrashItem{ID: id, Name: "待恢复内容 " + id, RecoveryRequired: true}
	info, err := trash.Lstat(id)
	if err != nil {
		return item, false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return item, false, ErrBadPath
	}
	bucket, err := trash.OpenRoot(id)
	if err != nil {
		return item, false, err
	}
	defer bucket.Close()
	payload, err := bucket.Lstat("entry")
	if errors.Is(err, os.ErrNotExist) {
		return item, false, nil
	}
	if err != nil {
		return item, false, err
	}
	item.IsDir = payload.IsDir()
	if marker, err := bucket.Lstat(".deleting"); err == nil && marker.Mode().IsRegular() {
		item.PurgeStarted = true
	}
	item.Size = payload.Size()
	item.DeletedAt = payload.ModTime().Unix()
	metadata, err := readManifest(bucket)
	if err == nil {
		item.Name = path.Base(metadata.Path)
		item.OriginalPath = metadata.Path
		item.DeletedAt = metadata.DeletedAt
		item.RecoveryRequired = false
	}
	return item, true, nil
}

// A manifest is fsynced before moving the source. If the process stops between
// steps, a payload always remains recoverable; an empty bucket contains no data.
func (m *Manager) Trash(ctx context.Context, rootID, rel string) (TrashItem, error) {
	var item TrashItem
	p, err := writablePath(rel)
	if err != nil {
		return item, err
	}
	r, unlock, err := m.lockWrite(ctx, rootID)
	if err != nil {
		return item, err
	}
	defer unlock()
	return m.trashLocked(ctx, r, p)
}
func (m *Manager) trashLocked(ctx context.Context, r *Root, p string) (TrashItem, error) {
	var item TrashItem
	var err error
	if _, err = r.dir.Lstat(p); err != nil {
		return item, mapWriteErr(err)
	}
	trash, err := internalRoot(r, trashDirectory, true)
	if err != nil {
		return item, err
	}
	defer trash.Close()
	entries, truncated, err := trashEntries(trash, m.limits.TrashMaxEntries)
	if err != nil {
		return item, err
	}
	if truncated {
		return item, ErrTrashFull
	}
	count := len(entries)
	// Reclaim only validated empty bookkeeping, under the cross-process lock.
	// Unknown directories or any bucket containing a payload are preserved.
	for _, entry := range entries {
		if !validFileID(entry.Name()) || !entry.IsDir() {
			continue
		}
		bucket, err := trash.OpenRoot(entry.Name())
		if err != nil {
			continue
		}
		_, payloadErr := bucket.Lstat("entry")
		_, metaErr := readManifest(bucket)
		if errors.Is(payloadErr, os.ErrNotExist) && metaErr == nil {
			_ = bucket.Remove(".deleting")
			_ = bucket.Remove("metadata.json")
			bucket.Close()
			if trash.Remove(entry.Name()) == nil {
				count--
			}
		} else {
			bucket.Close()
			if errors.Is(payloadErr, os.ErrNotExist) && metaErr != nil {
				if trash.Remove(entry.Name()) == nil {
					count--
				}
			}
		}
	}
	if count >= m.limits.TrashMaxEntries {
		return item, ErrTrashFull
	}
	id, err := randomFileID()
	if err != nil {
		return item, err
	}
	if err = trash.Mkdir(id, 0700); err != nil {
		return item, mapWriteErr(err)
	}
	bucket, err := trash.OpenRoot(id)
	if err != nil {
		return item, mapWriteErr(err)
	}
	defer bucket.Close()
	moved := false
	defer func() {
		if !moved {
			if _, err := bucket.Lstat("entry"); errors.Is(err, os.ErrNotExist) {
				_ = bucket.Remove("metadata.json")
				_ = trash.Remove(id)
			}
		}
	}()
	metadata := trashManifest{Version: 1, Path: p, DeletedAt: time.Now().Unix()}
	f, err := bucket.OpenFile("metadata.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return item, mapWriteErr(err)
	}
	err = json.NewEncoder(f).Encode(metadata)
	if err == nil {
		err = f.Sync()
	}
	_ = f.Close()
	if err != nil {
		return item, mapWriteErr(err)
	}
	if err = ctx.Err(); err != nil {
		return item, err
	}
	if err = moveNoReplace(r.dir, p, bucket, "entry"); err != nil {
		return item, err
	}
	moved = true
	item, _, err = itemInTrash(trash, id)
	return item, mapWriteErr(err)
}
func (m *Manager) ListTrash(rootID string, page, pageSize int) (TrashListing, error) {
	result := TrashListing{Items: []TrashItem{}}
	if page < 1 || page > 10000 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = m.limits.DefaultPageSize
	}
	if pageSize > m.limits.MaxPageSize {
		pageSize = m.limits.MaxPageSize
	}
	result.Page = page
	result.PageSize = pageSize
	r, err := m.root(rootID)
	if err != nil {
		return result, err
	}
	trash, err := internalRoot(r, trashDirectory, false)
	if errors.Is(err, ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer trash.Close()
	entries, truncated, err := trashEntries(trash, m.limits.TrashMaxEntries)
	if err != nil {
		return result, err
	}
	result.Truncated = truncated
	items := []TrashItem{}
	for _, entry := range entries {
		if !validFileID(entry.Name()) || !entry.IsDir() {
			continue
		}
		item, present, err := itemInTrash(trash, entry.Name())
		if err != nil {
			continue
		}
		if present {
			items = append(items, item)
		} else {
			result.EmptyRecords++
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].DeletedAt != items[j].DeletedAt {
			return items[i].DeletedAt > items[j].DeletedAt
		}
		return items[i].ID > items[j].ID
	})
	result.Total = len(items)
	start := (page - 1) * pageSize
	if start > len(items) {
		start = len(items)
	}
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}
	result.Items = items[start:end]
	return result, nil
}
func (m *Manager) Restore(ctx context.Context, rootID, id, destination string) (string, error) {
	if !validFileID(id) {
		return "", ErrBadPath
	}
	r, unlock, err := m.lockWrite(ctx, rootID)
	if err != nil {
		return "", err
	}
	defer unlock()
	trash, err := internalRoot(r, trashDirectory, false)
	if err != nil {
		return "", err
	}
	defer trash.Close()
	info, err := trash.Lstat(id)
	if err != nil {
		return "", mapWriteErr(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrBadPath
	}
	bucket, err := trash.OpenRoot(id)
	if err != nil {
		return "", mapWriteErr(err)
	}
	defer bucket.Close()
	if _, err = bucket.Lstat("entry"); err != nil {
		return "", mapWriteErr(err)
	}
	if destination == "" {
		metadata, err := readManifest(bucket)
		if err != nil {
			return "", ErrBadPath
		}
		destination = metadata.Path
	}
	p, err := writablePath(destination)
	if err != nil {
		return "", err
	}
	if err = safeMkdirAll(r.dir, path.Dir(p), 0750); err != nil {
		return "", mapWriteErr(err)
	}
	if err = moveNoReplace(bucket, "entry", r.dir, p); err != nil {
		return "", err
	}
	_ = bucket.Remove("metadata.json")
	_ = bucket.Remove(".deleting")
	_ = trash.Remove(id)
	return p, nil
}
