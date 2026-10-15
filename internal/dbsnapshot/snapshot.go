// Package dbsnapshot creates a consistent, bounded SQLite online snapshot.
// Paths are supplied by startup configuration, never by an HTTP caller. The
// destination must be in a caller-owned private scratch directory.
package dbsnapshot

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"modernc.org/sqlite"
)

var ErrLimit = errors.New("snapshot: database exceeds snapshot budget")

type Result struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Schema int64  `json:"schema"`
	Pages  int64  `json:"pages"`
}

func openReadOnly(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(1000)", "query_only(1)", "mmap_size(0)", "cache_size(-1024)", "temp_store(FILE)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

// Create pins a source read transaction, then copies 32 pages per step through
// SQLite's backup API. WAL changes remain available to other writers, and the
// snapshot never mixes rows from different commits. The destination is created
// exclusively at 0600, is checked and hashed, and is removed on any failure.
func Create(ctx context.Context, source, destination string, maxBytes int64, progress func(done, total int64)) (result Result, err error) {
	ctx, stop := context.WithTimeout(ctx, 5*time.Minute)
	defer stop()
	if maxBytes < 4096 || maxBytes > 1<<30 {
		return result, ErrLimit
	}
	if !filepath.IsAbs(source) || !filepath.IsAbs(destination) || source == destination || strings.ContainsAny(source+destination, "\x00\r\n") {
		return result, errors.New("snapshot: invalid configured path")
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	in, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return result, err
	}
	info, err := in.Stat()
	_ = in.Close()
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, errors.New("snapshot: source is not a regular database")
	}
	private, err := os.Stat(filepath.Dir(destination))
	if err != nil {
		return result, err
	}
	if !private.IsDir() || private.Mode().Perm()&0077 != 0 {
		return result, errors.New("snapshot: destination directory must be private")
	}
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return result, err
	}
	_ = f.Close()
	complete := false
	defer func() {
		if !complete {
			for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
				_ = os.Remove(destination + suffix)
			}
		}
	}()
	db, err := openReadOnly(source)
	if err != nil {
		return result, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return result, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var count int64
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&count); err != nil {
		return result, err
	}
	var pages, pageSize int64
	for _, q := range []struct {
		sql   string
		value *int64
	}{{"PRAGMA page_count", &pages}, {"PRAGMA page_size", &pageSize}, {"PRAGMA user_version", &result.Schema}} {
		if err = tx.QueryRowContext(ctx, q.sql).Scan(q.value); err != nil {
			return result, err
		}
	}
	if pageSize < 512 || pageSize > 65536 || pages < 1 || pages > maxBytes/pageSize {
		return result, ErrLimit
	}
	current, err := os.Lstat(source)
	if err != nil || !os.SameFile(info, current) {
		return result, errors.New("snapshot: source database identity changed")
	}
	u := url.URL{Scheme: "file", Path: destination}
	u.RawQuery = url.Values{"mode": {"rw"}, "_pragma": {"mmap_size(0)", "cache_size(-1024)", "journal_mode(DELETE)", "synchronous(FULL)", "page_size(" + strconv.FormatInt(pageSize, 10) + ")", "max_page_count(" + strconv.FormatInt(maxBytes/pageSize, 10) + ")"}}.Encode()
	err = conn.Raw(func(raw any) (workErr error) {
		type backuper interface {
			NewBackup(string) (*sqlite.Backup, error)
		}
		driver, ok := raw.(backuper)
		if !ok {
			return errors.New("snapshot: backup driver unavailable")
		}
		backup, e := driver.NewBackup(u.String())
		if e != nil {
			return e
		}
		defer func() {
			finish := backup.Finish()
			if workErr == nil {
				workErr = finish
			}
		}()
		for {
			if e = ctx.Err(); e != nil {
				return e
			}
			more, e := backup.Step(32)
			if e != nil {
				return e
			}
			total := int64(backup.PageCount())
			remaining := int64(backup.Remaining())
			if total > maxBytes/pageSize {
				return ErrLimit
			}
			if progress != nil {
				progress(total-remaining, total)
			}
			if !more {
				return ctx.Err()
			}
		}
	})
	if err != nil {
		return result, err
	}
	// Backup copies the source header, including WAL journal mode. Seal only the
	// private destination so reopening it read-only cannot create empty sidecars
	// or depend on a WAL that was never part of the portable snapshot.
	if err = Seal(ctx, destination); err != nil {
		return result, err
	}
	result, err = Verify(ctx, destination, maxBytes)
	if err != nil {
		return result, err
	}
	if result.Pages != pages {
		return result, errors.New("snapshot: page count changed despite pinned source transaction")
	}
	f, err = os.OpenFile(destination, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return result, err
	}
	err = f.Sync()
	_ = f.Close()
	if err != nil {
		return result, err
	}
	parent, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return result, err
	}
	err = parent.Sync()
	_ = parent.Close()
	if err != nil {
		return result, err
	}
	complete = true
	return result, nil
}

// Seal converts a caller-owned private staged database into a self-contained
// DELETE-journal file. It must never be used on an active service database.
func Seal(ctx context.Context, destination string) error {
	info, err := os.Lstat(destination)
	if err != nil {
		return err
	}
	parent, err := os.Stat(filepath.Dir(destination))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return errors.New("snapshot: seal requires a private staged database")
	}
	u := url.URL{Scheme: "file", Path: destination}
	u.RawQuery = url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(1000)", "mmap_size(0)", "cache_size(-1024)"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var mode string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		return err
	}
	if mode != "delete" {
		return errors.New("snapshot: could not seal journal mode")
	}
	return db.Close()
}

// Verify is also used before a restore is published. It limits work to the
// configured bytes, verifies SQLite structure and computes a streaming digest.
func Verify(ctx context.Context, path string, maxBytes int64) (Result, error) {
	ctx, stop := context.WithTimeout(ctx, 5*time.Minute)
	defer stop()
	var result Result
	if maxBytes < 4096 || maxBytes > 1<<30 {
		return result, ErrLimit
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return result, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() || info.Size() < 512 || info.Size() > maxBytes {
		return result, ErrLimit
	}
	db, err := openReadOnly(path)
	if err != nil {
		return result, err
	}
	defer db.Close()
	var check string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return result, err
	}
	if check != "ok" {
		return result, errors.New("snapshot: database integrity check failed")
	}
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&result.Schema); err != nil {
		return result, err
	}
	if err = db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&result.Pages); err != nil {
		return result, err
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	for {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		n, e := f.Read(buffer)
		if n > 0 {
			result.Bytes += int64(n)
			if result.Bytes > maxBytes {
				return result, ErrLimit
			}
			_, _ = hash.Write(buffer[:n])
		}
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return result, e
		}
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return result, fmt.Errorf("snapshot: database changed during verification")
	}
	linked, err := os.Lstat(path)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(info, linked) || info.Size() != linked.Size() || !info.ModTime().Equal(linked.ModTime()) {
		return result, errors.New("snapshot: verified database path was replaced")
	}
	result.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return result, nil
}
