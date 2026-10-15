package files

import (
	"context"
	"errors"
	"os"
	"syscall"
)

var ErrConfirmation = errors.New("files: exact item confirmation is required")

type PurgeResult struct {
	Removed int64 `json:"removed"`
	Total   int64 `json:"total"`
}

// Purge accepts a trash ID only. Preflight refuses an oversized tree without
// deleting anything; cancellation after deletion starts leaves a visible marker.
func (m *Manager) Purge(ctx context.Context, rootID, id, confirmName string, limit int, progress func(int64, int64)) (PurgeResult, error) {
	var result PurgeResult
	if !validFileID(id) || limit < 1 || limit > m.limits.CopyMaxEntries {
		return result, ErrBadPath
	}
	r, unlock, err := m.lockWrite(ctx, rootID)
	if err != nil {
		return result, err
	}
	defer unlock()
	trash, err := internalRoot(r, trashDirectory, false)
	if err != nil {
		return result, err
	}
	defer trash.Close()
	item, present, err := itemInTrash(trash, id)
	if err != nil {
		return result, mapWriteErr(err)
	}
	if !present {
		return result, ErrNotExist
	}
	if confirmName == "" || confirmName != item.Name {
		return result, ErrConfirmation
	}
	info, err := trash.Lstat(id)
	if err != nil {
		return result, mapWriteErr(err)
	}
	bucket, err := trash.OpenRoot(id)
	if err != nil {
		return result, mapWriteErr(err)
	}
	defer bucket.Close()
	opened, err := bucket.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return result, ErrSourceChanged
	}
	nodes, err := planTree(ctx, bucket, "entry", limit)
	if err != nil {
		return result, err
	}
	result.Total = int64(len(nodes))
	marker, err := bucket.OpenFile(".deleting", os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if errors.Is(err, os.ErrExist) {
		marker, err = bucket.OpenFile(".deleting", os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err == nil {
			fi, statErr := marker.Stat()
			if statErr != nil || !fi.Mode().IsRegular() {
				marker.Close()
				return result, ErrBadPath
			}
		}
	} else if err == nil {
		_, err = marker.WriteString("nasctl-purge-v1\n")
		if err == nil {
			err = marker.Sync()
		}
	}
	if marker != nil {
		marker.Close()
	}
	if err != nil {
		return result, mapWriteErr(err)
	}
	if progress != nil {
		progress(0, result.Total)
	}
	err = removePlanned(ctx, bucket, nodes, func(done int64) {
		result.Removed = done
		if progress != nil {
			progress(done, result.Total)
		}
	})
	if err != nil {
		return result, err
	}
	_ = bucket.Remove(".deleting")
	_ = bucket.Remove("metadata.json")
	_ = trash.Remove(id)
	return result, nil
}
