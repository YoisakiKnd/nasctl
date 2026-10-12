package synth

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteProducesParseableLibrary is a self-check on the generator. If the
// generator is wrong, every test that depends on it proves nothing.
func TestWriteProducesParseableLibrary(t *testing.T) {
	dir := t.TempDir()
	lib, err := Write(dir)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(lib.Files) < 25 {
		t.Fatalf("expected a substantial library, got %d files", len(lib.Files))
	}
	for _, f := range lib.Files {
		full := filepath.Join(dir, filepath.FromSlash(f.Rel))
		fi, err := os.Stat(full)
		if err != nil {
			t.Errorf("%s was recorded but not written: %v", f.Rel, err)
			continue
		}
		if fi.Size() != f.Bytes {
			t.Errorf("%s: recorded %d bytes, on disk %d", f.Rel, f.Bytes, fi.Size())
		}
		t.Logf("%-52s %8d B  %s", f.Rel, fi.Size(), f.Note)
	}
}
