package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestStorageCannotExposePrivateNamespacesThroughNestedRootsOrAliases(t *testing.T) {
	base := t.TempDir()
	private := filepath.Join(base, ".nasctl-backups", "version", "data")
	if err := os.MkdirAll(private, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(private, alias); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{private, alias, filepath.Join(base, ".nasctl-trash", "id", "entry")} {
		doc := fmt.Sprintf("[[hub.storage]]\nid=\"readonly\"\npath=%q\n", p)
		if _, err := Parse(doc); err == nil {
			t.Fatal("private storage exposed", p)
		}
	}
}
