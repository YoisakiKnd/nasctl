package dbsnapshot

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnlineSnapshotPinsOneCommitAndPreservesWALWriter(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.sqlite")
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(source)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE data(id INTEGER PRIMARY KEY,payload BLOB); PRAGMA user_version=5"); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err = tx.Exec("INSERT INTO data(payload) VALUES(?)", strings.Repeat("x", 32768)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(private, "snapshot.sqlite")
	wrote := false
	result, err := Create(context.Background(), source, destination, 8<<20, func(done, total int64) {
		if !wrote && done < total {
			wrote = true
			if _, err := db.Exec("INSERT INTO data(payload) VALUES('later commit')"); err != nil {
				t.Error(err)
			}
		}
	})
	if err != nil || !wrote || result.Schema != 5 || len(result.SHA256) != 64 || result.Bytes < 3<<20 {
		t.Fatal(result, wrote, err)
	}
	copy, err := openReadOnly(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var rows int
	if err = copy.QueryRow("SELECT count(*) FROM data").Scan(&rows); err != nil || rows != 100 {
		t.Fatal(rows, err)
	}
	if err = db.QueryRow("SELECT count(*) FROM data").Scan(&rows); err != nil || rows != 101 {
		t.Fatal(rows, err)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	check, err := Verify(context.Background(), destination, 8<<20)
	if err != nil || check.SHA256 != result.SHA256 {
		t.Fatal(check, err)
	}
}

func TestSnapshotBudgetCancelAndNoOverwrite(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.sqlite")
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE data(payload BLOB); INSERT INTO data VALUES(zeroblob(1048576))"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"budget", "cancel", "exists", "permissions"} {
		t.Run(mode, func(t *testing.T) {
			destination := filepath.Join(private, mode+".sqlite")
			limit := int64(2 << 20)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var report func(int64, int64)
			switch mode {
			case "budget":
				limit = 4096
			case "cancel":
				report = func(int64, int64) { cancel() }
			case "exists":
				if err := os.WriteFile(destination, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				destination = filepath.Join(root, "public.sqlite")
				if err := os.Chmod(root, 0750); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Create(ctx, source, destination, limit, report)
			if err == nil {
				t.Fatal("unsafe snapshot succeeded")
			}
			if mode == "exists" {
				b, e := os.ReadFile(destination)
				if e != nil || string(b) != "preserve" {
					t.Fatal(string(b), e)
				}
			} else {
				if _, e := os.Stat(destination); !os.IsNotExist(e) {
					t.Fatal("failed snapshot left payload", e)
				}
			}
			if mode == "budget" && !errors.Is(err, ErrLimit) {
				t.Fatal(err)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestSnapshotClosedWALDatabaseWithoutSideFiles(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "closed.sqlite")
	private := filepath.Join(base, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(source)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE data(value TEXT); INSERT INTO data VALUES('checkpointed')"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err = os.Stat(source + suffix); !os.IsNotExist(err) {
			t.Fatalf("expected closed source without %s: %v", suffix, err)
		}
	}
	destination := filepath.Join(private, "snapshot.sqlite")
	result, err := Create(context.Background(), source, destination, 1<<20, nil)
	if err != nil || result.Bytes == 0 {
		t.Fatal(result, err)
	}
	copy, err := openReadOnly(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var value string
	if err = copy.QueryRow("SELECT value FROM data").Scan(&value); err != nil || value != "checkpointed" {
		t.Fatal(value, err)
	}
}
