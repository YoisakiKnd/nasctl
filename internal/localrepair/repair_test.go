package localrepair

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/config"
)

func TestFiniteQuarantineAndFailureEvidence(t *testing.T) {
	state := t.TempDir()
	ctx := context.Background()
	if _, e := Run(ctx, state, "settings.reset", func(*os.Root, string) error { return nil }); !os.IsNotExist(e) {
		t.Fatal("missing reset should not create evidence", e)
	}
	if _, e := os.Stat(filepath.Join(state, Directory)); !os.IsNotExist(e) {
		t.Fatal("empty reset consumed an entry")
	}
	if e := os.WriteFile(filepath.Join(state, accounts.FileName), []byte("original"), 0600); e != nil {
		t.Fatal(e)
	}
	crash := errors.New("simulated failure after quarantine")
	out, e := Run(ctx, state, "users.recover", func(root *os.Root, rel string) error {
		if e := Move(root, accounts.FileName, rel); e != nil {
			return e
		}
		return crash
	})
	if !errors.Is(e, crash) || out.Completed {
		t.Fatal(out, e)
	}
	b, e := os.ReadFile(filepath.Join(out.Quarantine, "original.raw"))
	if e != nil || string(b) != "original" {
		t.Fatal("failed repair lost bytes", e)
	}
	if _, e = os.Stat(filepath.Join(out.Quarantine, "result.json")); e != nil {
		t.Fatal("failed repair has no result", e)
	}
	for n := 1; n < MaxRepairs; n++ {
		if e = os.Mkdir(filepath.Join(state, Directory, fmt.Sprint(n)), 0700); e != nil {
			t.Fatal(e)
		}
	}
	if e = os.WriteFile(filepath.Join(state, config.NASSettingsFile), []byte("do not move"), 0600); e != nil {
		t.Fatal(e)
	}
	called := false
	if _, e = Run(ctx, state, "settings.reset", func(*os.Root, string) error { called = true; return nil }); e == nil || called {
		t.Fatal("quarantine cap was bypassed", e)
	}
	b, _ = os.ReadFile(filepath.Join(state, config.NASSettingsFile))
	if string(b) != "do not move" {
		t.Fatal("full quarantine moved settings")
	}
}
