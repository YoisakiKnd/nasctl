// Package localrepair provides a finite private quarantine for offline repairs.
// It deliberately never opens the job database: that database may be damaged.
package localrepair

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
)

const Directory = "local-recovery"
const MaxRepairs = 16

type Result struct {
	Quarantine string `json:"quarantine"`
	Completed  bool   `json:"completed"`
}

func syncDir(root *os.Root, name string) error {
	f, e := root.Open(name)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func record(root *os.Root, name, action, status string) error {
	b, e := json.Marshal(struct {
		Action string `json:"action"`
		Status string `json:"status"`
		Time   int64  `json:"time"`
	}{action, status, time.Now().Unix()})
	if e != nil {
		return e
	}
	f, e := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	return errors.Join(e, f.Close())
}

// Run holds the exclusive state lease through intent, quarantine, repair and
// its result record. A crash leaves the original bytes and intent for inspection.
// Only the two fixed repair targets are accepted. Missing users are recoverable;
// missing settings are not a reset and consume no quarantine entry.
func Run(ctx context.Context, state, action string, repair func(*os.Root, string) error) (out Result, err error) {
	target := ""
	switch action {
	case "users.recover":
		target = accounts.FileName
	case "settings.reset":
		target = config.NASSettingsFile
	default:
		return out, errors.New("repair: unsupported action")
	}
	root, e := os.OpenRoot(state)
	if e != nil {
		return out, e
	}
	defer root.Close()
	lease, e := statelease.AcquireRoot(root, true)
	if e != nil {
		return out, e
	}
	defer lease.Close()
	for _, n := range []string{statelease.PendingRestore, statelease.RestoreJournal} {
		if _, e = root.Lstat(n); !os.IsNotExist(e) {
			return out, statelease.ErrRecoveryRequired
		}
	}
	if e = ctx.Err(); e != nil {
		return out, e
	}
	i, e := root.Lstat(target)
	exists := e == nil
	if e != nil && !os.IsNotExist(e) {
		return out, e
	}
	if exists && !i.Mode().IsRegular() {
		return out, errors.New("repair: target must be a regular file; inspect it locally")
	}
	if !exists && action == "settings.reset" {
		return out, os.ErrNotExist
	}
	if e = root.Mkdir(Directory, 0700); e != nil && !os.IsExist(e) {
		return out, e
	}
	i, e = root.Lstat(Directory)
	if e != nil || !i.IsDir() || i.Mode().Perm()&0077 != 0 {
		return out, errors.New("repair: quarantine must be a private real directory")
	}
	dir, e := root.OpenRoot(Directory)
	if e != nil {
		return out, e
	}
	defer dir.Close()
	f, e := dir.Open(".")
	if e != nil {
		return out, e
	}
	opened, e := f.Stat()
	if e != nil || !os.SameFile(i, opened) {
		f.Close()
		return out, errors.New("repair: quarantine directory changed")
	}
	rows, e := f.ReadDir(MaxRepairs + 1)
	f.Close()
	if e != nil && !errors.Is(e, io.EOF) {
		return out, e
	}
	if len(rows) >= MaxRepairs {
		return out, errors.New("repair: 16 recovery entries retained; inspect and move old quarantines to private offline storage")
	}
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return out, e
	}
	rel := filepath.Join(Directory, hex.EncodeToString(nonce[:]))
	if e = root.Mkdir(rel, 0700); e != nil {
		return out, e
	}
	out.Quarantine = filepath.Join(state, rel)
	if e = record(root, filepath.Join(rel, "intent.json"), action, "prepared"); e != nil {
		return out, e
	}
	if e = syncDir(root, rel); e != nil {
		return out, e
	}
	if e = syncDir(root, Directory); e != nil {
		return out, e
	}
	if e = syncDir(root, "."); e != nil {
		return out, e
	}
	// The repair may establish a fail-closed marker before moving its original.
	err = repair(root, rel)
	status := "failed"
	if err == nil {
		status = "completed"
	}
	logErr := record(root, filepath.Join(rel, "result.json"), action, status)
	err = errors.Join(err, logErr, syncDir(root, rel), syncDir(root, "."))
	out.Completed = err == nil
	return out, err
}

// Move preserves even malformed or oversized original bytes without decoding,
// copying, following links or changing their permissions.
func Move(root *os.Root, source, rel string) error {
	i, e := root.Lstat(source)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if !i.Mode().IsRegular() {
		return errors.New("repair: unsafe original")
	}
	if e = root.Rename(source, filepath.Join(rel, "original.raw")); e != nil {
		return e
	}
	return errors.Join(syncDir(root, rel), syncDir(root, "."))
}
