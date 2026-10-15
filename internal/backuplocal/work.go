package backuplocal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/backups"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
)

type snapshotRecord struct {
	Version  int              `json:"version"`
	Snapshot backups.Snapshot `json:"snapshot"`
}
type workRecord struct {
	Version   int          `json:"version"`
	ID        string       `json:"id"`
	Plan      backups.Plan `json:"plan"`
	StartedAt int64        `json:"started_at"`
}
type deleteRecord struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Limit   int    `json:"limit"`
}

func readJSON(root *os.Root, name string, max int64, out any) error {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > max {
		return ErrCorrupt
	}
	d := json.NewDecoder(io.LimitReader(f, max+1))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return ErrCorrupt
	}
	return nil
}
func writeJSON(root *os.Root, name string, value any) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = json.NewEncoder(f).Encode(value); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return syncRoot(root)
}
func openBucket(vault *os.Root, name string) (*os.Root, error) {
	info, err := vault.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrCorrupt
	}
	b, err := vault.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := b.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		b.Close()
		return nil, ErrCorrupt
	}
	return b, nil
}
func readSnapshot(vault *os.Root, id string) (backups.Snapshot, *os.Root, error) {
	var record snapshotRecord
	if !versionID.MatchString(id) {
		return record.Snapshot, nil, ErrInvalid
	}
	bucket, err := openBucket(vault, id)
	if err != nil {
		return record.Snapshot, nil, err
	}
	if err = readJSON(bucket, "snapshot.json", 8192, &record); err != nil {
		bucket.Close()
		return record.Snapshot, nil, ErrCorrupt
	}
	s := record.Snapshot
	if record.Version != 1 || s.ID != id || !identifier.MatchString(s.PlanID) || !identifier.MatchString(s.Root) || !identifier.MatchString(s.ToRoot) || s.CreatedAt < 1 || s.Entries < 1 || s.Entries > 10000 || s.Bytes < 0 || s.Bytes > files.MaxTransferBytes || len(s.ManifestSHA256) != 64 || len(s.Name) > 128 || len(s.Path) > 1024 {
		bucket.Close()
		return s, nil, ErrCorrupt
	}
	if _, err = vault.Lstat(".delete-" + id + ".json"); err == nil {
		s.Deleting = true
	} else if !os.IsNotExist(err) {
		bucket.Close()
		return s, nil, err
	}
	return s, bucket, nil
}

func (c *Control) vault(ctx context.Context, root string, create, write bool) (*os.Root, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if write {
		found := false
		for _, r := range c.files.Roots() {
			if r.ID != root {
				continue
			}
			found = true
			if !r.Available {
				return nil, nil, files.ErrRootUnavailable
			}
			if !r.Writable {
				return nil, nil, files.ErrReadOnly
			}
		}
		if !found {
			return nil, nil, files.ErrNoSuchRoot
		}
	}
	vault, err := c.files.BackupVault(ctx, root, create)
	if err != nil {
		return nil, nil, err
	}
	var lease *statelease.Lease
	if write {
		lease, err = statelease.AcquireRoot(vault, true)
	} else {
		lease, err = statelease.ObserveRoot(vault)
	}
	if err != nil {
		vault.Close()
		if errors.Is(err, statelease.ErrBusy) {
			return nil, nil, ErrBusy
		}
		return nil, nil, err
	}
	return vault, func() { lease.Close(); vault.Close() }, nil
}

func history(vault *os.Root, root string) (backups.History, error) {
	h := backups.History{Snapshots: []backups.Snapshot{}, Incomplete: []string{}}
	dir, err := vault.Open(".")
	if err != nil {
		return h, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(257)
	if err != nil && !errors.Is(err, io.EOF) {
		return h, err
	}
	if len(entries) > 256 {
		h.Truncated = true
		entries = entries[:256]
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(name, "work-") && versionID.MatchString(strings.TrimPrefix(name, "work-")) {
			h.Incomplete = append(h.Incomplete, name)
			continue
		}
		if !versionID.MatchString(name) {
			continue
		}
		s, bucket, err := readSnapshot(vault, name)
		if err != nil {
			h.Incomplete = append(h.Incomplete, name)
			continue
		}
		bucket.Close()
		s.ToRoot = root
		h.Snapshots = append(h.Snapshots, s)
	}
	sort.Slice(h.Snapshots, func(i, j int) bool {
		if h.Snapshots[i].CreatedAt != h.Snapshots[j].CreatedAt {
			return h.Snapshots[i].CreatedAt > h.Snapshots[j].CreatedAt
		}
		return h.Snapshots[i].ID > h.Snapshots[j].ID
	})
	sort.Strings(h.Incomplete)
	return h, nil
}
func (c *Control) History(ctx context.Context, root string) (backups.History, error) {
	if !c.begin() {
		return backups.History{}, ErrBusy
	}
	defer c.end()
	vault, close, err := c.vault(ctx, root, false, false)
	if errors.Is(err, files.ErrNotExist) {
		return backups.History{Snapshots: []backups.Snapshot{}, Incomplete: []string{}}, nil
	}
	if err != nil {
		return backups.History{}, err
	}
	defer close()
	return history(vault, root)
}
func (c *Control) Run(ctx context.Context, id, digest string, report func(backups.Progress)) (backups.Result, error) {
	var result backups.Result
	if !c.begin() {
		return result, ErrBusy
	}
	defer c.end()
	p, err := c.plan(id)
	if err != nil {
		return result, err
	}
	if digest != p.Digest() {
		return result, ErrInvalid
	}
	duration, _ := time.ParseDuration(p.MaxRuntime)
	ctx, stop := context.WithTimeout(ctx, duration)
	defer stop()
	vault, close, err := c.vault(ctx, p.ToRoot, true, true)
	if err != nil {
		return result, err
	}
	defer close()
	h, err := history(vault, p.ToRoot)
	if err != nil {
		return result, err
	}
	if h.Truncated || len(h.Snapshots)+len(h.Incomplete) >= 64 {
		return result, ErrFull
	}
	version, err := randomID()
	if err != nil {
		return result, err
	}
	work := "work-" + version
	ownerName := ".work-" + version + ".json"
	owner := workRecord{1, version, p, time.Now().Unix()}
	if err = writeJSON(vault, ownerName, owner); err != nil {
		_ = vault.Remove(ownerName)
		return result, err
	}
	if err = vault.Mkdir(work, 0700); err != nil {
		_ = vault.Remove(ownerName)
		return result, err
	}
	bucket, err := openBucket(vault, work)
	if err != nil {
		return result, err
	}
	defer bucket.Close()
	if err = writeJSON(bucket, "work.json", owner); err != nil {
		return result, err
	}
	if report != nil {
		report(backups.Progress{Phase: "copy", Detail: "正在顺序复制，完成校验后才发布备份"})
	}
	copied, err := c.files.CopyToVault(ctx, vault, p.Root, p.Path, work+"/data", p.Limit, p.MaxBytes, func(n, b int64, _ string) {
		if report != nil {
			report(backups.Progress{Phase: "copy", Entries: n, Bytes: b})
		}
	})
	if err != nil {
		return result, err
	}
	entries, bytes, sha, err := writeManifest(ctx, bucket, p, report)
	if err != nil {
		return result, err
	}
	snapshot := backups.Snapshot{ID: version, PlanID: p.ID, Name: p.Name, Root: p.Root, Path: p.Path, ToRoot: p.ToRoot, CreatedAt: time.Now().Unix(), Entries: entries, Bytes: bytes, Skipped: copied.Skipped, ManifestSHA256: sha}
	if err = verifyManifest(ctx, bucket, bucket, "data", snapshot, report); err != nil {
		return result, err
	}
	if err = writeJSON(bucket, "snapshot.json", snapshotRecord{1, snapshot}); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = files.PublishVault(vault, work, version); err != nil {
		return result, err
	}
	result.Snapshot = &snapshot
	result.Published = true
	_ = vault.Remove(ownerName)
	if err = syncRoot(vault); err != nil {
		result.Warning = "备份已发布，但目录同步失败，请重新校验"
		return result, err
	}
	// The new version is always retained, even when runs share a timestamp.
	kept := 1
	for _, old := range h.Snapshots {
		if old.PlanID != p.ID {
			continue
		}
		if kept < p.Keep && !old.Deleting {
			kept++
			continue
		}
		if err = c.deleteVersion(ctx, vault, old, report); err != nil {
			result.Warning = "新备份已保存，旧版本清理未完成，可从历史中继续清理"
			return result, err
		}
		result.Pruned++
	}
	return result, nil
}

func (c *Control) Verify(ctx context.Context, root, id string, report func(backups.Progress)) (backups.Result, error) {
	var result backups.Result
	if !c.begin() {
		return result, ErrBusy
	}
	defer c.end()
	ctx, stop := context.WithTimeout(ctx, 2*time.Hour)
	defer stop()
	vault, close, err := c.vault(ctx, root, false, false)
	if err != nil {
		return result, err
	}
	defer close()
	snapshot, bucket, err := readSnapshot(vault, id)
	if err != nil {
		return result, err
	}
	defer bucket.Close()
	if snapshot.Deleting {
		return result, ErrCorrupt
	}
	err = verifyManifest(ctx, bucket, bucket, "data", snapshot, report)
	snapshot.ToRoot = root
	result.Snapshot = &snapshot
	return result, err
}
func (c *Control) Restore(ctx context.Context, input backups.Restore, report func(backups.Progress)) (backups.Result, error) {
	var result backups.Result
	if !c.begin() {
		return result, ErrBusy
	}
	defer c.end()
	ctx, stop := context.WithTimeout(ctx, 2*time.Hour)
	defer stop()
	to, err := normalDestination(input.To)
	if err != nil {
		return result, err
	}
	found := false
	for _, r := range c.files.Roots() {
		if r.ID != input.ToRoot {
			continue
		}
		found = true
		if !r.Available {
			return result, files.ErrRootUnavailable
		}
		if !r.Writable {
			return result, files.ErrReadOnly
		}
	}
	if !found {
		return result, files.ErrNoSuchRoot
	}
	if _, err = c.files.Stat(input.ToRoot, to); err == nil {
		return result, files.ErrExists
	} else if !errors.Is(err, files.ErrNotExist) {
		return result, err
	}
	vault, close, err := c.vault(ctx, input.Root, false, false)
	if err != nil {
		return result, err
	}
	defer close()
	snapshot, bucket, err := readSnapshot(vault, input.ID)
	if err != nil {
		return result, err
	}
	defer bucket.Close()
	if snapshot.Deleting || input.ManifestSHA256 != snapshot.ManifestSHA256 {
		return result, ErrCorrupt
	}
	if err = verifyManifest(ctx, bucket, bucket, "data", snapshot, report); err != nil {
		return result, err
	}
	l := c.files.Limits()
	if snapshot.Entries > int64(l.CopyMaxEntries) || snapshot.Bytes > l.CopyMaxBytes {
		return result, files.ErrTreeLimit
	}
	copied, err := c.files.RestoreFromVault(ctx, vault, input.ID+"/data", input.ToRoot, to, int(snapshot.Entries), max(1, snapshot.Bytes), func(ctx context.Context, staged *os.Root, base string) error {
		return verifyManifest(ctx, bucket, staged, base, snapshot, report)
	}, func(n, b int64, _ string) {
		if report != nil {
			report(backups.Progress{Phase: "restore", Entries: n, Bytes: b})
		}
	})
	result.Published = copied.Published
	result.DestinationRoot = input.ToRoot
	result.Path = to
	snapshot.ToRoot = input.Root
	result.Snapshot = &snapshot
	return result, err
}
func (c *Control) deleteVersion(ctx context.Context, vault *os.Root, s backups.Snapshot, report func(backups.Progress)) error {
	marker := ".delete-" + s.ID + ".json"
	if _, err := vault.Lstat(marker); os.IsNotExist(err) {
		if err = writeJSON(vault, marker, deleteRecord{1, s.ID, int(s.Entries) + 16}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if report != nil {
		report(backups.Progress{Phase: "delete", Detail: "正在清理所选备份版本；取消会保留剩余部分"})
	}
	if err := files.RemoveVault(ctx, vault, s.ID, c.files.Limits().CopyMaxEntries+16); err != nil {
		return err
	}
	_ = vault.Remove(marker)
	return syncRoot(vault)
}
func (c *Control) Delete(ctx context.Context, root, id, sha string, report func(backups.Progress)) (backups.Result, error) {
	var result backups.Result
	if !c.begin() {
		return result, ErrBusy
	}
	defer c.end()
	ctx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	vault, close, err := c.vault(ctx, root, false, true)
	if err != nil {
		return result, err
	}
	defer close()
	s, bucket, err := readSnapshot(vault, id)
	if err != nil {
		return result, err
	}
	bucket.Close()
	if sha == "" || sha != s.ManifestSHA256 {
		return result, ErrInvalid
	}
	err = c.deleteVersion(ctx, vault, s, report)
	s.ToRoot = root
	result.Snapshot = &s
	if err == nil {
		result.Pruned = 1
	}
	return result, err
}
func (c *Control) Cleanup(ctx context.Context, root, name string, report func(backups.Progress)) (backups.Result, error) {
	var result backups.Result
	if !c.begin() {
		return result, ErrBusy
	}
	defer c.end()
	ctx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	id := strings.TrimPrefix(name, "work-")
	if !versionID.MatchString(id) {
		return result, ErrInvalid
	}
	vault, close, err := c.vault(ctx, root, false, true)
	if err != nil {
		return result, err
	}
	defer close()
	var deletion deleteRecord
	deleting := readJSON(vault, ".delete-"+id+".json", 1024, &deletion) == nil && deletion.Version == 1 && deletion.ID == id
	if !deleting {
		bucket, err := openBucket(vault, name)
		if err != nil {
			return result, err
		}
		defer bucket.Close()
		var marker workRecord
		if readJSON(bucket, "work.json", 8192, &marker) != nil {
			if readJSON(vault, ".work-"+id+".json", 8192, &marker) != nil {
				return result, ErrCorrupt
			}
		}
		if marker.Version != 1 || marker.ID != id {
			return result, ErrCorrupt
		}
		if !strings.HasPrefix(name, "work-") {
			if _, b, e := readSnapshot(vault, id); e == nil {
				b.Close()
				return result, ErrInvalid
			}
		}
	}
	if report != nil {
		report(backups.Progress{Phase: "delete", Detail: "正在清理已确认的不完整备份"})
	}
	err = files.RemoveVault(ctx, vault, name, c.files.Limits().CopyMaxEntries+16)
	if err == nil {
		_ = vault.Remove(".delete-" + id + ".json")
		_ = vault.Remove(".work-" + id + ".json")
		result.Pruned = 1
		err = syncRoot(vault)
	}
	return result, err
}

var _ backups.Control = (*Control)(nil)
