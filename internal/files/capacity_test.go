package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/config"
)

func TestCapacityUsesFilesystemAndIdentifiesSharedVolume(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")
	for _, p := range []string{a, b} {
		if e := os.Mkdir(p, 0750); e != nil {
			t.Fatal(e)
		}
	}
	m, e := New([]config.HubStorage{{ID: "a", Path: a}, {ID: "b", Path: b}}, DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	roots := m.Roots()
	if len(roots) != 2 {
		t.Fatal(roots)
	}
	for _, r := range roots {
		if !r.Available || r.Capacity == nil || r.Capacity.TotalBytes <= 0 || r.Capacity.AvailableBytes < 0 || r.Capacity.FreeBytes < r.Capacity.AvailableBytes || r.Capacity.UsedBytes+r.Capacity.FreeBytes != r.Capacity.TotalBytes || r.Capacity.UsagePercent < 0 || r.Capacity.UsagePercent > 100 {
			t.Fatalf("real capacity invalid: %+v", r)
		}
	}
	if roots[0].Capacity.VolumeID != roots[1].Capacity.VolumeID {
		t.Fatal("same filesystem not recognized")
	}
	if _, e = capacityProduct(^uint64(0), 4096); e == nil {
		t.Fatal("overflow accepted")
	}
}
func TestReplacedRootIsUnavailableForReadsAndWrites(t *testing.T) {
	base := t.TempDir()
	p := filepath.Join(base, "data")
	if e := os.Mkdir(p, 0750); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(p, "old.txt"), []byte("original"), 0640); e != nil {
		t.Fatal(e)
	}
	m, e := New([]config.HubStorage{{ID: "data", Path: p, Writable: true}}, DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	if e = os.Rename(p, p+".old"); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(p, 0750); e != nil {
		t.Fatal(e)
	}
	root := m.Roots()[0]
	if root.Available || root.Capacity != nil {
		t.Fatal("replacement reported as original mounted root", root)
	}
	if _, e = m.List("data", ListOptions{}); !errors.Is(e, ErrRootUnavailable) {
		t.Fatal("read used stale root", e)
	}
	if e = m.Mkdir(context.Background(), "data", "new"); !errors.Is(e, ErrRootUnavailable) {
		t.Fatal("write used stale root", e)
	}
	b, e := os.ReadFile(filepath.Join(p+".old", "old.txt"))
	if e != nil || string(b) != "original" {
		t.Fatal("original damaged", e)
	}
}
