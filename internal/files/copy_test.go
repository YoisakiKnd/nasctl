package files

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/config"
)

func copyRig(t *testing.T, writable bool) (*Manager, string, string) {
	t.Helper()
	source, destination := t.TempDir(), t.TempDir()
	m, err := New([]config.HubStorage{{ID: "source", Path: source, Writable: writable}, {ID: "destination", Path: destination, Writable: true}}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, source, destination
}
func copyOptions(from, to string) CopyOptions {
	return CopyOptions{SourceRoot: "source", DestinationRoot: "destination", From: from, To: to, Limit: 100, MaxBytes: 1 << 20}
}

func TestConfiguredLargeBudgetStillRequiresLocalPolicy(t *testing.T) {
	m, src, dst := copyRig(t, false)
	mustWrite(t, filepath.Join(src, "small"), "bounded original")
	o := copyOptions("small", "copied")
	o.MaxBytes = 32 << 30
	if _, e := m.Copy(context.Background(), o, nil); !errors.Is(e, ErrTreeLimit) {
		t.Fatal("request raised local byte policy", e)
	}
	assertAbsent(t, filepath.Join(dst, "copied"))
	l := DefaultLimits()
	l.CopyMaxBytes = 32 << 30
	large, e := New([]config.HubStorage{{ID: "source", Path: src}, {ID: "destination", Path: dst, Writable: true}}, l)
	if e != nil {
		t.Fatal(e)
	}
	defer large.Close()
	result, e := large.Copy(context.Background(), o, nil)
	if e != nil || !result.Published || result.Bytes != 16 {
		t.Fatal("configured large budget failed on bounded stream", result, e)
	}
	assertContent(t, filepath.Join(dst, "copied"), "bounded original")
}
func mustWrite(t *testing.T, p, data string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}
}
func assertContent(t *testing.T, p, want string) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil || string(got) != want {
		t.Fatalf("%s: %q %v", p, got, err)
	}
}
func assertAbsent(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent %s: %v", p, err)
	}
}

func TestCopyReadonlyTreePreservesSourceAndNeverFollowsSymlinks(t *testing.T) {
	m, src, dst := copyRig(t, false)
	if err := os.Mkdir(filepath.Join(src, "tree"), 0750); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, "tree", "file"), "original")
	outside := filepath.Join(t.TempDir(), "secret")
	mustWrite(t, outside, "outside")
	if err := os.Symlink(outside, filepath.Join(src, "tree", "link")); err != nil {
		t.Fatal(err)
	}
	result, err := m.Copy(context.Background(), copyOptions("tree", "copied"), nil)
	if err != nil || !result.Published || result.Entries != 3 || result.Bytes != 8 {
		t.Fatalf("%+v %v", result, err)
	}
	assertContent(t, filepath.Join(src, "tree", "file"), "original")
	assertContent(t, filepath.Join(dst, "copied", "file"), "original")
	link, err := os.Readlink(filepath.Join(dst, "copied", "link"))
	if err != nil || link != outside {
		t.Fatal(link, err)
	}
	assertContent(t, outside, "outside")
	_, err = m.Copy(context.Background(), copyOptions("tree", "copied"), nil)
	if !errors.Is(err, ErrExists) {
		t.Fatal("overwrite", err)
	}
	o := copyOptions("tree", "moved")
	o.Move = true
	if _, err = m.Copy(context.Background(), o, nil); !errors.Is(err, ErrReadOnly) {
		t.Fatal("readonly move", err)
	}
}

func TestCopyBudgetsCancelAndSourceChangeDoNotPublish(t *testing.T) {
	for _, scenario := range []string{"bytes", "entries", "cancel", "changed", "collision", "fifo"} {
		t.Run(scenario, func(t *testing.T) {
			m, src, dst := copyRig(t, true)
			if err := os.Mkdir(filepath.Join(src, "tree"), 0750); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(src, "tree", "file")
			mustWrite(t, p, strings.Repeat("x", 130000))
			o := copyOptions("tree", "target")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callback CopyProgress
			want := ErrTreeLimit
			switch scenario {
			case "bytes":
				o.MaxBytes = 5
			case "entries":
				o.Limit = 1
			case "cancel":
				want = context.Canceled
				callback = func(_, bytes int64, _ string) {
					if bytes > 0 {
						cancel()
					}
				}
			case "changed":
				want = ErrSourceChanged
				callback = func(_, bytes int64, _ string) {
					if bytes > 0 {
						mustWrite(t, p, "changed")
					}
				}
			case "collision":
				want = ErrExists
				callback = func(_, _ int64, phase string) {
					if phase == "publishing" {
						mustWrite(t, filepath.Join(dst, "target"), "existing")
					}
				}
			case "fifo":
				want = ErrNotRegular
				if err := syscall.Mkfifo(filepath.Join(src, "tree", "fifo"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := m.Copy(ctx, o, callback)
			if !errors.Is(err, want) || result.Published {
				t.Fatalf("%+v %v want %v", result, err, want)
			}
			if scenario == "collision" {
				assertContent(t, filepath.Join(dst, "target"), "existing")
			} else {
				assertAbsent(t, filepath.Join(dst, "target"))
			}
			if _, err := os.Stat(p); err != nil {
				t.Fatal("source lost", err)
			}
			entries, err := os.ReadDir(filepath.Join(dst, copyDirectory))
			if err != nil || len(entries) != 1 || entries[0].Name() != ".owner" {
				t.Fatal("temporary copy leaked", entries, err)
			}
		})
	}
}

func TestCrossRootMoveTrashesSourceAndRetainsBothIfTrashFails(t *testing.T) {
	for _, failTrash := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "trash-fails"}[failTrash], func(t *testing.T) {
			m, src, dst := copyRig(t, true)
			mustWrite(t, filepath.Join(src, "file"), "keep me")
			if failTrash {
				if err := os.Mkdir(filepath.Join(src, trashDirectory), 0750); err != nil {
					t.Fatal(err)
				}
			}
			o := copyOptions("file", "target")
			o.Move = true
			result, err := m.Copy(context.Background(), o, nil)
			if !result.Published {
				t.Fatal("target missing", result, err)
			}
			assertContent(t, filepath.Join(dst, "target"), "keep me")
			if failTrash {
				if err == nil {
					t.Fatal("trash error concealed")
				}
				assertContent(t, filepath.Join(src, "file"), "keep me")
			} else {
				if err != nil || result.SourceTrashed == "" {
					t.Fatal(result, err)
				}
				assertAbsent(t, filepath.Join(src, "file"))
				if _, err = m.Restore(context.Background(), "source", result.SourceTrashed, ""); err != nil {
					t.Fatal(err)
				}
				assertContent(t, filepath.Join(src, "file"), "keep me")
			}
		})
	}
}

func TestCopyPrivateNamespacesAndDepthAreBounded(t *testing.T) {
	m, src, dst := copyRig(t, true)
	if err := os.MkdirAll(filepath.Join(src, "tree", stagingDirectory), 0750); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, "tree", stagingDirectory, "foreign"), "private")
	mustWrite(t, filepath.Join(src, "tree", "file"), "public")
	o := copyOptions("tree", "target")
	o.Move = true
	if result, err := m.Copy(context.Background(), o, nil); !errors.Is(err, ErrUnsupported) || result.Published {
		t.Fatal(result, err)
	}
	o.Move = false
	result, err := m.Copy(context.Background(), o, nil)
	if err != nil || result.Skipped != 1 {
		t.Fatal(result, err)
	}
	assertAbsent(t, filepath.Join(dst, "target", stagingDirectory))
	assertContent(t, filepath.Join(src, "tree", stagingDirectory, "foreign"), "private")
	deep := "deep"
	for i := 0; i < 34; i++ {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(filepath.Join(src, deep), 0750); err != nil {
		t.Fatal(err)
	}
	if result, err = m.Copy(context.Background(), copyOptions("deep", "too-deep"), nil); !errors.Is(err, ErrTreeLimit) || result.Published {
		t.Fatal(result, err)
	}
	assertAbsent(t, filepath.Join(dst, "too-deep"))
}

func TestCopyCleanupPreservesUnknownManifestAndRemovesOwnedInterruptedCopy(t *testing.T) {
	m, src, dst := copyRig(t, true)
	mustWrite(t, filepath.Join(src, "source"), "ready")
	o := copyOptions("source", "first")
	if _, err := m.Copy(context.Background(), o, nil); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{1, 99} {
		id, err := randomFileID()
		if err != nil {
			t.Fatal(err)
		}
		bucket := filepath.Join(dst, copyDirectory, id)
		if err = os.Mkdir(bucket, 0700); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(copyManifest{Version: version, Options: o, StartedAt: 1})
		mustWrite(t, filepath.Join(bucket, "metadata.json"), string(data))
		mustWrite(t, filepath.Join(bucket, "entry"), "interrupted")
		o.To = id + "-published"
		if _, err = m.Copy(context.Background(), o, nil); err != nil {
			t.Fatal(err)
		}
		if version == 1 {
			assertAbsent(t, bucket)
		} else {
			assertContent(t, filepath.Join(bucket, "metadata.json"), string(data))
			assertContent(t, filepath.Join(bucket, "entry"), "interrupted")
		}
	}
}
