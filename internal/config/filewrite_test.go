package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileOnlyConsoleAndWritableRootValidation(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "files")
	state := filepath.Join(base, "state")
	cache := filepath.Join(base, "cache")
	if err := os.Mkdir(root, 0750); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf("[paths]\nstate_dir=%q\ncache_dir=%q\n[[hub.storage]]\nid=\"data\"\npath=%q\nwritable=true\n", state, cache, root)
	cfg, err := Parse(doc)
	if err != nil || len(cfg.Collections) != 0 || !cfg.Hub.Storage[0].Writable {
		t.Fatalf("file-only console refused: %v", err)
	}
	bad := strings.Replace(doc, fmt.Sprintf("path=%q", root), fmt.Sprintf("path=%q", base), 1)
	if _, err := Parse(bad); err == nil {
		t.Fatal("writable root contains state/cache")
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	bad = strings.Replace(doc, fmt.Sprintf("state_dir=%q", state), fmt.Sprintf("state_dir=%q", filepath.Join(alias, "future-state")), 1)
	if _, err := Parse(bad); err == nil {
		t.Fatal("symlink ancestor evaded protected state boundary")
	}
	bad = doc + fmt.Sprintf("\n[[hub.storage]]\nid=\"nested\"\npath=%q\n", filepath.Join(root, "nested"))
	if _, err := Parse(bad); err == nil {
		t.Fatal("overlapping writable roots accepted")
	}
}
