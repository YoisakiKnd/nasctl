package hubweb

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheWebLayerCannotReachADecoderOrTheScanner is the structural test behind the
// promise that an HTTP request cannot decode an image, launch a thumbnail run or
// start a library scan.
//
// It works on the import graph rather than on behaviour, and that is the point:
// a behavioural test can only show that the handlers written today do not do
// those things, whereas this fails the moment somebody adds the import that would
// let a future handler do them. On a machine with 981 MiB of memory, that import
// is the difference between a console and another outage.
func TestTheWebLayerCannotReachADecoderOrTheScanner(t *testing.T) {
	// Packages this layer must not reach, directly or through the module.
	forbidden := []string{
		"internal/thumbs",  // decodes images and supervises workers
		"internal/scan",    // walks media trees
		"internal/probe",   // reads media metadata, pulls in exif
		"internal/exif",    // parses untrusted metadata
		"internal/store",   // the album index; the console has its own
		"internal/taskctl", // album task locks
		"internal/backupgate",
		"internal/synth",
	}
	// Standard-library packages that would mean the same thing.
	forbiddenStd := []string{
		"image", "image/jpeg", "image/png", "image/gif",
		"os/exec", // launching ffmpeg or exiftool from a request
	}

	root := moduleRoot(t)
	seen := map[string]bool{}
	var walk func(pkgDir, importPath string)

	walk = func(pkgDir, importPath string) {
		if seen[importPath] {
			return
		}
		seen[importPath] = true

		pkg, err := build.ImportDir(pkgDir, 0)
		if err != nil {
			// A package that cannot be parsed is not this test's business; the
			// build already fails for that.
			return
		}
		for _, imp := range pkg.Imports {
			for _, f := range forbidden {
				if strings.HasSuffix(imp, f) {
					t.Errorf("hubweb reaches %s (via %s). An HTTP request must not be able "+
						"to decode an image, walk a media tree or touch the album index: "+
						"those belong to bounded, separately supervised tasks",
						imp, importPath)
				}
			}
			for _, f := range forbiddenStd {
				if imp == f {
					t.Errorf("hubweb reaches the standard package %q (via %s)", imp, importPath)
				}
			}
			// Follow module-internal imports only.
			if strings.HasPrefix(imp, modulePath) {
				sub := strings.TrimPrefix(imp, modulePath+"/")
				subDir := filepath.Join(root, sub)
				if fi, err := os.Stat(subDir); err == nil && fi.IsDir() {
					walk(subDir, imp)
				}
			}
		}
	}
	walk(filepath.Join(root, "internal", "hubweb"), modulePath+"/internal/hubweb")
}

const modulePath = "github.com/YoisakiKnd/nasctl"

// moduleRoot locates the checkout root by walking up to go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("no go.mod found above the test directory")
		}
		dir = parent
	}
}
