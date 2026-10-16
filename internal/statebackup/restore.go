package statebackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/anime"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/dbsnapshot"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
	"github.com/YoisakiKnd/nasctl/internal/store"
)

type change struct {
	Key       string `json:"key"`
	Target    string `json:"target"`
	OldSHA256 string `json:"old_sha256"`
	NewSHA256 string `json:"new_sha256"`
}
type journal struct {
	Version      int      `json:"version"`
	ID           string   `json:"id"`
	LayoutSHA256 string   `json:"layout_sha256"`
	Committed    bool     `json:"committed"`
	Changes      []change `json:"changes"`
}

func (j journal) stage(c change) string {
	return filepath.Join(filepath.Dir(c.Target), ".nasctl-restore-"+j.ID+"-"+c.Key+".new")
}
func (j journal) old(c change) string {
	return filepath.Join(filepath.Dir(c.Target), ".nasctl-restore-"+j.ID+"-"+c.Key+".old")
}
func (l Layout) dirs() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, p := range []string{l.State, filepath.Dir(l.Hub), filepath.Dir(l.Album)} {
		if !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	sort.Strings(out)
	return out
}
func writeJournal(l Layout, j journal) error {
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	return fsutil.AtomicWrite(filepath.Join(l.State, journalName), 0600, func(w io.Writer) error { _, e = w.Write(b); return e })
}
func markPending(l Layout, j journal) error {
	for _, p := range l.dirs() {
		f, e := os.OpenFile(filepath.Join(p, statelease.PendingRestore), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if e != nil {
			return e
		}
		_, e = f.WriteString(j.ID + "\n")
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			return e
		}
		if e = syncDir(p); e != nil {
			return e
		}
	}
	return nil
}
func removePending(l Layout, j journal) error {
	for _, p := range l.dirs() {
		marker := filepath.Join(p, statelease.PendingRestore)
		b, e := readPrivate(marker, 64)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		if string(b) != j.ID+"\n" {
			return ErrInvalid
		}
		if e = os.Remove(marker); e != nil {
			return e
		}
		if e = syncDir(p); e != nil {
			return e
		}
	}
	if e := os.Remove(filepath.Join(l.State, journalName)); e != nil && !os.IsNotExist(e) {
		return e
	}
	return syncDir(l.State)
}

// digestPath also accepts current database permissions. It never follows links,
// and the sole directory target is the bounded, flat metadata-backup directory.
func digestPath(ctx context.Context, p string) (string, error) {
	f, e := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(e) {
		return "", nil
	}
	if e != nil {
		return "", e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return "", e
	}
	h := sha256.New()
	if info.IsDir() {
		rows, e := f.ReadDir(1001)
		if e != nil && e != io.EOF {
			return "", e
		}
		if len(rows) > 1000 {
			return "", ErrInvalid
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Name() < rows[j].Name() })
		var total int64
		for _, row := range rows {
			if !keyValid("metadata/" + row.Name()) {
				return "", ErrInvalid
			}
			child := filepath.Join(p, row.Name())
			i, e := os.Lstat(child)
			if e != nil || !i.Mode().IsRegular() {
				return "", ErrInvalid
			}
			total += i.Size()
			if total > metadataBudget {
				return "", ErrInvalid
			}
			d, e := digestPath(ctx, child)
			if e != nil {
				return "", e
			}
			_, _ = fmt.Fprintf(h, "%s\x00%d\x00%s\n", row.Name(), i.Size(), d)
		}
	} else {
		if !info.Mode().IsRegular() || info.Size() > dbBudget {
			return "", ErrInvalid
		}
		buf := make([]byte, 64<<10)
		var n int64
		for {
			if e = ctx.Err(); e != nil {
				return "", e
			}
			count, err := f.Read(buf)
			n += int64(count)
			if n > dbBudget {
				return "", ErrInvalid
			}
			_, _ = h.Write(buf[:count])
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
		}
	}
	after, e := os.Lstat(p)
	if e != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return "", ErrInvalid
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func removeOwned(ctx context.Context, p, expected string) error {
	got, e := digestPath(ctx, p)
	if e != nil {
		return e
	}
	if got == "" {
		return nil
	}
	if got != expected {
		return errors.New("state restore: publication path changed; recovery required")
	}
	i, e := os.Lstat(p)
	if e != nil {
		return e
	}
	if i.IsDir() {
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		rows, e := f.ReadDir(1001)
		_ = f.Close()
		if e != nil && e != io.EOF {
			return e
		}
		for _, row := range rows {
			if e = os.Remove(filepath.Join(p, row.Name())); e != nil {
				return e
			}
		}
	}
	return os.Remove(p)
}
func allowedChanges(l Layout) map[string]string {
	out := l.targets()
	delete(out, "config")
	for _, key := range []string{"hub", "anime", "album"} {
		for _, suffix := range []string{"wal", "shm", "journal"} {
			out[key+"-"+suffix] = out[key] + "-" + suffix
		}
	}
	return out
}
func validateJournal(l Layout, j journal) error {
	if j.Version != 1 || len(j.ID) != 32 || j.LayoutSHA256 != l.fingerprint() || len(j.Changes) < 1 || len(j.Changes) > 18 {
		return ErrInvalid
	}
	if _, e := hex.DecodeString(j.ID); e != nil {
		return ErrInvalid
	}
	seen := map[string]bool{}
	allowed := allowedChanges(l)
	for _, c := range j.Changes {
		if seen[c.Key] || allowed[c.Key] != c.Target || (c.OldSHA256 != "" && len(c.OldSHA256) != 64) || (c.NewSHA256 != "" && len(c.NewSHA256) != 64) {
			return ErrInvalid
		}
		seen[c.Key] = true
	}
	return nil
}

// Restore publishes only known service-state targets selected by CURRENT
// configuration. Saved config is included in the package but never activated
// automatically; a recovered token or obsolete storage root cannot silently
// change access. The exact manifest digest binds the operator's confirmation.
func Restore(ctx context.Context, l Layout, dir, digest string, report func(string)) (Result, error) {
	return restore(ctx, l, dir, digest, report, nil)
}
func restore(ctx context.Context, l Layout, dir, digest string, report func(string), afterPublish func(int) error) (result Result, workErr error) {
	ctx, stop := context.WithTimeout(ctx, 30*time.Minute)
	defer stop()
	close, e := l.lock(false)
	if e != nil {
		return result, e
	}
	defer close()
	if e = audit(l, "restore.start", Result{}, nil); e != nil {
		return result, e
	}
	defer func() {
		if e := audit(l, "restore", result, workErr); e != nil {
			workErr = errors.Join(workErr, e)
		}
	}()
	var m Manifest
	result, m, e = verify(ctx, dir)
	if e != nil {
		return result, e
	}
	if len(digest) != 64 || digest != result.ManifestSHA256 || m.LayoutSHA256 != l.fingerprint() {
		return result, errors.New("state restore: confirmation or configured layout changed")
	}
	// State packages do not include photo payloads. Reinstating an older album
	// index could expose pre-vault names/metadata, so refuse this mixed restore.
	if _, err := os.Lstat(filepath.Join(l.State, ".nasctl-photo-vault", "vault.json")); err == nil {
		return result, errors.New("state restore: an encrypted photo vault is configured; restore the complete state and vault together from an offline filesystem backup")
	} else if !os.IsNotExist(err) {
		return result, err
	}
	entries := map[string]Entry{}
	for _, v := range m.Entries {
		entries[v.Key] = v
	}
	if _, hasAccounts := entries["users"]; hasAccounts {
		// The package's TOML is deliberately not activated. Its setup flag
		// cannot protect accounts published under a different current policy.
		current, err := config.Load(l.Config)
		if err != nil {
			return result, err
		}
		if !current.Hub.Setup && current.Server.Token == "" {
			return result, errors.New("state restore: enable hub.setup or server.token in the current base configuration before restoring accounts")
		}
	}
	targets := l.targets()
	for _, key := range []string{"anime", "album", "users", "users-enabled"} {
		if _, exists := entries[key]; exists {
			continue
		}
		if _, err := os.Lstat(targets[key]); err == nil {
			return result, errors.New("state restore: package lacks current database or account protection; refusing to erase it")
		} else if !os.IsNotExist(err) {
			return result, err
		}
	}
	id, e := nonce()
	if e != nil {
		return result, e
	}
	checkpoints := filepath.Join(l.State, "restore-checkpoints")
	if e = os.Mkdir(checkpoints, 0700); e != nil && !os.IsExist(e) {
		return result, e
	}
	if e = privateDir(checkpoints); e != nil {
		return result, e
	}
	f, e := os.Open(checkpoints)
	if e != nil {
		return result, e
	}
	rows, e := f.ReadDir(17)
	_ = f.Close()
	if e != nil && e != io.EOF {
		return result, e
	}
	if len(rows) >= 16 {
		return result, errors.New("state restore: 16 checkpoints retained; export and review before another restore")
	}
	checkpoint := filepath.Join(checkpoints, id)
	if report != nil {
		report("保存恢复前检查点")
	}
	if _, e = capture(ctx, l, checkpoint, true, report); e != nil {
		return result, e
	}
	result.Checkpoint = checkpoint
	j := journal{Version: 1, ID: id, LayoutSHA256: l.fingerprint(), Changes: []change{}}
	cleanupStages := true
	createdStages := []string{}
	defer func() {
		if cleanupStages {
			for _, p := range createdStages {
				_ = os.RemoveAll(p)
			}
		}
	}()
	for _, key := range []string{"hub", "anime", "album", "album-directories", "backup-plans", "desktop-apps", "webdav-history", "metadata", "settings", "users", "users-enabled"} {
		target := targets[key]
		old, e := digestPath(ctx, target)
		if e != nil {
			return result, e
		}
		c := change{Key: key, Target: target, OldSHA256: old}
		j.Changes = append(j.Changes, c)
		stage := j.stage(c)
		if key == "metadata" {
			if e = os.Mkdir(stage, 0700); e != nil {
				return result, e
			}
			createdStages = append(createdStages, stage)
			for _, entry := range m.Entries {
				if strings.HasPrefix(entry.Key, "metadata/") {
					got, err := copyFile(ctx, payload(dir, entry.Key), filepath.Join(stage, strings.TrimPrefix(entry.Key, "metadata/")), metadataBudget)
					if err != nil {
						return result, err
					}
					if got.Bytes != entry.Bytes || got.SHA256 != entry.SHA256 {
						return result, ErrInvalid
					}
				}
			}
			if e = syncDir(stage); e != nil {
				return result, e
			}
		} else if entry, exists := entries[key]; exists {
			got, err := copyFile(ctx, payload(dir, key), stage, entry.Bytes)
			if err != nil {
				return result, err
			}
			createdStages = append(createdStages, stage)
			if got.Bytes != entry.Bytes || got.SHA256 != entry.SHA256 {
				return result, ErrInvalid
			}
			// SQLite migration and sequence preparation happen inside a private staging
			// directory; the live target remains untouched through all validation.
			if key == "hub" || key == "anime" || key == "album" {
				scratch := stage + ".prepare"
				if e = os.Mkdir(scratch, 0700); e != nil {
					return result, e
				}
				prepared := filepath.Join(scratch, "database")
				if e = os.Rename(stage, prepared); e != nil {
					_ = os.Remove(scratch)
					return result, e
				}
				e = prepare(ctx, key, target, prepared, old != "")
				if e == nil {
					e = dbsnapshot.Seal(ctx, prepared)
				}
				if e == nil {
					e = os.Rename(prepared, stage)
				}
				_ = os.RemoveAll(scratch)
				if e != nil {
					return result, e
				}
				if _, e = database(ctx, key, stage); e != nil {
					return result, e
				}
			}
		} else if old != "" && (key == "anime" || key == "album" || key == "users" || key == "users-enabled") {
			return result, errors.New("state restore: package lacks a currently existing database; refusing to erase it")
		}
		newHash, e := digestPath(ctx, stage)
		if e != nil {
			return result, e
		}
		j.Changes[len(j.Changes)-1].NewSHA256 = newHash
		if old == "" && newHash == "" {
			j.Changes = j.Changes[:len(j.Changes)-1]
		}
	}
	// WAL/SHM/journal are part of rollback. Never let old sidecars attach to a
	// restored main database. Their bytes are retained verbatim until commit.
	allowed := allowedChanges(l)
	for _, key := range []string{"hub", "anime", "album"} {
		for _, suffix := range []string{"wal", "shm", "journal"} {
			k := key + "-" + suffix
			p := allowed[k]
			old, e := digestPath(ctx, p)
			if e != nil {
				return result, e
			}
			if old != "" {
				j.Changes = append(j.Changes, change{Key: k, Target: p, OldSHA256: old})
			}
		}
	}
	if e = validateJournal(l, j); e != nil {
		return result, e
	}
	for _, c := range j.Changes {
		if _, e = os.Lstat(j.old(c)); !os.IsNotExist(e) {
			return result, ErrInvalid
		}
		if e = syncDir(filepath.Dir(c.Target)); e != nil {
			return result, e
		}
	}
	if e = ctx.Err(); e != nil {
		return result, e
	}
	if e = writeJournal(l, j); e != nil {
		return result, e
	}
	cleanupStages = false
	rollback := func(cause error) (Result, error) {
		recoveryCtx, stop := context.WithTimeout(context.Background(), 10*time.Minute)
		defer stop()
		if re := recoverLocked(recoveryCtx, l, j); re != nil {
			return result, fmt.Errorf("state restore failed (%v); automatic rollback failed: %w; run nasctl state recover", cause, re)
		}
		return result, fmt.Errorf("state restore rolled back: %w", cause)
	}
	if e = markPending(l, j); e != nil {
		return rollback(e)
	}
	for i, c := range j.Changes {
		if e = ctx.Err(); e != nil {
			return rollback(e)
		}
		if report != nil {
			report("发布 " + c.Key)
		}
		if c.OldSHA256 != "" {
			current, er := digestPath(ctx, c.Target)
			if er != nil || current != c.OldSHA256 {
				return rollback(ErrInvalid)
			}
			if e = os.Rename(c.Target, j.old(c)); e != nil {
				return rollback(e)
			}
			if e = syncDir(filepath.Dir(c.Target)); e != nil {
				return rollback(e)
			}
		}
		if c.NewSHA256 != "" {
			if _, e = os.Lstat(c.Target); !os.IsNotExist(e) {
				return rollback(ErrInvalid)
			}
			if e = os.Rename(j.stage(c), c.Target); e != nil {
				return rollback(e)
			}
			if e = syncDir(filepath.Dir(c.Target)); e != nil {
				return rollback(e)
			}
		}
		if afterPublish != nil {
			if e = afterPublish(i); e != nil {
				return rollback(e)
			}
		}
	}
	j.Committed = true
	if e = writeJournal(l, j); e != nil {
		// AtomicWrite can report a directory fsync failure AFTER its rename.
		// Never guess whether the committed journal reached storage and start
		// undoing it in memory. Recovery reads the actual durable phase and
		// either rolls back or finishes cleanup with all writers still blocked.
		return result, fmt.Errorf("state restore: commit durability uncertain; retained checkpoint and journal; run nasctl state recover: %w", e)
	}
	if report != nil {
		report("完成发布，清理恢复暂存")
	}
	finishCtx, done := context.WithTimeout(context.Background(), 10*time.Minute)
	defer done()
	if e = recoverLocked(finishCtx, l, j); e != nil {
		return result, fmt.Errorf("state restore committed; cleanup requires nasctl state recover: %w", e)
	}
	return result, nil
}
func prepare(ctx context.Context, key, current, staged string, hasCurrent bool) error {
	switch key {
	case "anime":
		if hasCurrent {
			_, e := anime.PreserveRestoreHighwater(ctx, current, staged)
			return e
		}
		st, e := anime.OpenStore(staged)
		if e != nil {
			return e
		}
		return st.Close()
	case "hub":
		st, e := hubstore.Open(staged, hubstore.DefaultLimits())
		if e != nil {
			return e
		}
		return st.Close()
	case "album":
		st, e := store.Open(staged, store.Options{})
		if e != nil {
			return e
		}
		return st.Close()
	}
	return ErrInvalid
}

// Recover uses only a strict, private journal matching the current layout. An
// uncommitted operation rolls back; a durably committed operation only finishes
// cleanup. It never guesses which of two conflicting files the user intended.
func Recover(ctx context.Context, l Layout) (workErr error) {
	close, e := l.lock(true)
	if e != nil {
		return e
	}
	defer close()
	if e = audit(l, "recover.start", Result{}, nil); e != nil {
		return e
	}
	defer func() {
		if e := audit(l, "recover", Result{}, workErr); e != nil {
			workErr = errors.Join(workErr, e)
		}
	}()
	b, e := readPrivate(filepath.Join(l.State, journalName), 65536)
	if e != nil {
		return e
	}
	var j journal
	if e = decode(b, &j); e != nil {
		return e
	}
	if e = validateJournal(l, j); e != nil {
		return e
	}
	return recoverLocked(ctx, l, j)
}
func recoverLocked(ctx context.Context, l Layout, j journal) error {
	if e := validateJournal(l, j); e != nil {
		return e
	}
	for i := len(j.Changes) - 1; i >= 0; i-- {
		c := j.Changes[i]
		if e := ctx.Err(); e != nil {
			return e
		}
		old, e := digestPath(ctx, j.old(c))
		if e != nil {
			return e
		}
		current, e := digestPath(ctx, c.Target)
		if e != nil {
			return e
		}
		if j.Committed {
			if current != c.NewSHA256 {
				return errors.New("state restore: committed target changed; retained checkpoint and journal")
			}
			if old != "" {
				if old != c.OldSHA256 {
					return ErrInvalid
				}
				if e = removeOwned(ctx, j.old(c), c.OldSHA256); e != nil {
					return e
				}
			}
		} else if old != "" {
			if old != c.OldSHA256 {
				return ErrInvalid
			}
			if current != "" {
				if e = removeOwned(ctx, c.Target, c.NewSHA256); e != nil {
					return e
				}
			}
			if e = os.Rename(j.old(c), c.Target); e != nil {
				return e
			}
		} else if c.OldSHA256 == "" {
			if current != "" {
				if e = removeOwned(ctx, c.Target, c.NewSHA256); e != nil {
					return e
				}
			}
		} else if current != c.OldSHA256 {
			return errors.New("state restore: original target missing or changed; retained checkpoint and journal")
		}
		if c.NewSHA256 != "" {
			if e = removeOwned(ctx, j.stage(c), c.NewSHA256); e != nil {
				return e
			}
		}
		if e = syncDir(filepath.Dir(c.Target)); e != nil {
			return e
		}
	}
	return removePending(l, j)
}
