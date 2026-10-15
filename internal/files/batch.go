package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"strings"
)

var ErrBatchIncomplete = errors.New("files: batch partially completed; inspect item results")

type BatchItem struct {
	Root string `json:"root"`
	Path string `json:"path,omitempty"`
	ID   string `json:"id,omitempty"`
}
type BatchOptions struct {
	Action    string      `json:"action"`
	Items     []BatchItem `json:"items"`
	ToRoot    string      `json:"to_root,omitempty"`
	Directory string      `json:"directory,omitempty"`
	Limit     int         `json:"limit"`
	MaxBytes  int64       `json:"max_bytes"`
}
type BatchItemResult struct {
	BatchItem
	State       string `json:"state"`
	Code        string `json:"code,omitempty"`
	Destination string `json:"destination,omitempty"`
	TrashID     string `json:"trash_id,omitempty"`
	Published   bool   `json:"published,omitempty"`
}
type BatchResult struct {
	Items      []BatchItemResult `json:"items"`
	Completed  int               `json:"completed"`
	Failed     int               `json:"failed"`
	NotStarted int               `json:"not_started"`
	Entries    int64             `json:"entries"`
	Bytes      int64             `json:"bytes"`
}

// Validate the whole selection before any mutation. Paths cannot overlap, so a
// parent and its child cannot accidentally be moved twice by the same batch.
func (m *Manager) ValidateBatch(o BatchOptions) (BatchOptions, error) {
	if len(o.Items) < 1 || len(o.Items) > m.limits.BatchMaxItems {
		return o, ErrTreeLimit
	}
	if o.Limit == 0 {
		o.Limit = m.limits.CopyMaxEntries
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = m.limits.CopyMaxBytes
	}
	if o.Limit < 1 || o.Limit > m.limits.CopyMaxEntries || o.MaxBytes < 1 || o.MaxBytes > m.limits.CopyMaxBytes {
		return o, ErrTreeLimit
	}
	copying := o.Action == "copy" || o.Action == "move"
	if !copying && o.Action != "trash" && o.Action != "restore" {
		return o, ErrBadPath
	}
	if copying {
		p, err := cleanRel(o.Directory)
		if err != nil {
			return o, err
		}
		o.Directory = p
		r, err := m.root(o.ToRoot)
		if err != nil {
			return o, err
		}
		if !r.Writable {
			return o, ErrReadOnly
		}
		info, err := m.Stat(o.ToRoot, p)
		if err != nil {
			return o, err
		}
		if !info.IsDir {
			return o, ErrBadPath
		}
	} else if o.ToRoot != "" || o.Directory != "" {
		return o, ErrBadPath
	}
	// Copy the slice: normalization must not mutate a caller's selection.
	o.Items = append([]BatchItem(nil), o.Items...)
	for i, item := range o.Items {
		if len(item.Root) > 32 {
			return o, ErrBadPath
		}
		r, err := m.root(item.Root)
		if err != nil {
			return o, err
		}
		if o.Action != "copy" && !r.Writable {
			return o, ErrReadOnly
		}
		if o.Action == "restore" {
			if !validFileID(item.ID) || item.Path != "" {
				return o, ErrBadPath
			}
		} else {
			if item.ID != "" {
				return o, ErrBadPath
			}
			p, err := writablePath(item.Path)
			if err != nil {
				return o, err
			}
			item.Path = p
			o.Items[i] = item
			if copying {
				if _, err := ValidateCopy(CopyOptions{SourceRoot: item.Root, DestinationRoot: o.ToRoot, From: p, To: path.Join(o.Directory, path.Base(p)), Limit: o.Limit, MaxBytes: o.MaxBytes}); err != nil {
					return o, err
				}
			}
		}
		for _, prev := range o.Items[:i] {
			if prev.Root != item.Root {
				continue
			}
			if o.Action == "restore" {
				if prev.ID == item.ID {
					return o, ErrBadPath
				}
			} else if prev.Path == item.Path || strings.HasPrefix(prev.Path, item.Path+"/") || strings.HasPrefix(item.Path, prev.Path+"/") {
				return o, ErrBadPath
			}
		}
	}
	return o, nil
}

// The digest binds consent/audit to the normalized complete selection, without
// placing a large path array into the operation registry's scalar arguments.
func (o BatchOptions) Digest() string {
	b, _ := json.Marshal(o)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func BatchErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrBatchIncomplete):
		return "partial"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "interrupted"
	case errors.Is(err, ErrExists):
		return "exists"
	case errors.Is(err, ErrNotExist):
		return "not_found"
	case errors.Is(err, ErrTreeLimit):
		return "budget_exhausted"
	case errors.Is(err, ErrReadOnly):
		return "read_only"
	case errors.Is(err, ErrNoSpace):
		return "no_space"
	case errors.Is(err, ErrBusy):
		return "busy"
	case errors.Is(err, ErrTrashFull):
		return "trash_full"
	case errors.Is(err, ErrSourceChanged):
		return "source_changed"
	case errors.Is(err, ErrOutsideRoot):
		return "outside_root"
	case errors.Is(err, ErrRootUnavailable):
		return "root_unavailable"
	default:
		return "failed"
	}
}

// A batch is sequential, with one shared byte/entry budget. Completed entries
// stay committed; conflicts continue to the next item. Cancellation/budget/space
// failures stop further work and explicitly report every unstarted item.
func (m *Manager) Batch(ctx context.Context, options BatchOptions, progress func(BatchResult)) (BatchResult, error) {
	result := BatchResult{Items: make([]BatchItemResult, 0, len(options.Items))}
	o, err := m.ValidateBatch(options)
	if err != nil {
		return result, err
	}
	var stopped error
	for _, item := range o.Items {
		row := BatchItemResult{BatchItem: item, State: "not_started"}
		if stopped == nil {
			stopped = ctx.Err()
		}
		if stopped == nil && result.Entries >= int64(o.Limit) {
			stopped = ErrTreeLimit
		}
		if stopped == nil && (o.Action == "copy" || o.Action == "move") && result.Bytes >= o.MaxBytes {
			stopped = ErrTreeLimit
		}
		if stopped != nil {
			row.Code = BatchErrorCode(stopped)
			result.NotStarted++
			result.Items = append(result.Items, row)
			continue
		}
		var workErr error
		switch o.Action {
		case "copy", "move":
			row.Destination = path.Join(o.Directory, path.Base(item.Path))
			remaining := o.MaxBytes - result.Bytes
			if remaining <= 0 {
				workErr = ErrTreeLimit
				break
			}
			baseEntries, baseBytes := result.Entries, result.Bytes
			copied, e := m.Copy(ctx, CopyOptions{SourceRoot: item.Root, DestinationRoot: o.ToRoot, From: item.Path, To: row.Destination, Move: o.Action == "move", Limit: o.Limit - int(result.Entries), MaxBytes: remaining}, func(n, b int64, _ string) {
				if progress != nil {
					p := result
					p.Entries = baseEntries + n
					p.Bytes = baseBytes + b
					progress(p)
				}
			})
			result.Entries += copied.Entries + int64(copied.Skipped)
			result.Bytes += copied.Bytes
			row.Published = copied.Published
			row.TrashID = copied.SourceTrashed
			workErr = e
		case "trash":
			value, e := m.Trash(ctx, item.Root, item.Path)
			row.TrashID = value.ID
			workErr = e
			result.Entries++
		case "restore":
			value, e := m.Restore(ctx, item.Root, item.ID, "")
			row.Destination = value
			workErr = e
			result.Entries++
		}
		if workErr == nil {
			row.State = "completed"
			result.Completed++
		} else {
			row.State = "failed"
			row.Code = BatchErrorCode(workErr)
			result.Failed++
			if errors.Is(workErr, ErrTreeLimit) || errors.Is(workErr, ErrNoSpace) || errors.Is(workErr, ErrBusy) || ctx.Err() != nil {
				stopped = workErr
			}
		}
		result.Items = append(result.Items, row)
		if progress != nil {
			progress(result)
		}
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if result.Failed > 0 || result.NotStarted > 0 {
		return result, ErrBatchIncomplete
	}
	return result, nil
}
