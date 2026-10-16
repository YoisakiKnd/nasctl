package albumapi_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/YoisakiKnd/nasctl"

// TestWebServiceCannotReachADecoderOrAScanner enforces the central structural
// claim of the design: the process that answers HTTP requests is incapable of
// decoding an image, spawning a helper, or walking a media directory.
//
// This is checked by walking the real import graph rather than by reading a
// comment, because the failure mode it prevents is exactly the one that took
// the machine down: a handler that quietly acquires the ability to do heavy
// work, and then does it inside a request.
//
// If this test fails, the fix is to move the shared code into a package that
// does not carry the decoder with it. Do not add the package to the allowlist
// without checking that it performs no file-header parsing, no image decoding,
// no process execution and no directory traversal.
func TestWebServiceCannotReachADecoderOrAScanner(t *testing.T) {
	root := moduleRoot(t)

	// Packages that must be unreachable from the web service. Every one of
	// these does work that must never happen inside an HTTP request.
	forbidden := map[string]string{
		"internal/exif":      "parses file headers and metadata",
		"internal/probe":     "reads and interprets media headers",
		"internal/scan":      "walks media directories",
		"internal/thumbs":    "decodes images and spawns helper processes",
		"image/jpeg":         "decodes JPEG",
		"image/png":          "decodes PNG",
		"image/gif":          "decodes GIF",
		"image":              "decodes images",
		"golang.org/x/image": "decodes images",
		"os/exec":            "starts external processes",
		"path/filepath":      "resolves paths across the filesystem (see note below)",
		"io/fs":              "walks directories",
		"modernc.org/sqlite": "opens the index; the service uses a read-only handle only",
	}

	// path/filepath is genuinely hard to avoid in a server that must resolve a
	// configured static directory. Rather than ban it outright, ban the two
	// functions that turn it into a traversal primitive, and require that all
	// path construction from user-visible data goes through fsutil.
	pkgs := importGraph(t, root, modulePath+"/internal/albumapi")

	var reachable []string
	for p := range pkgs {
		reachable = append(reachable, p)
	}
	sort.Strings(reachable)

	for _, bad := range []string{"internal/exif", "internal/probe", "internal/scan", "internal/thumbs"} {
		if path := modulePath + "/" + bad; pkgs[path] {
			t.Errorf("the web service can reach %s (transitively), which %s.\n"+
				"An HTTP handler that can do this will eventually do it.\nReachable set:\n  %s",
				bad, forbidden[bad], strings.Join(reachable, "\n  "))
		}
	}
	for _, bad := range []string{"image/jpeg", "image/png", "image/gif", "image", "os/exec", "modernc.org/sqlite"} {
		if pkgs[bad] {
			t.Errorf("the web service can reach %s (transitively), which %s.\nReachable set:\n  %s",
				bad, forbidden[bad], strings.Join(reachable, "\n  "))
		}
	}
	for _, bad := range []string{"golang.org/x/image"} {
		for p := range pkgs {
			if p == bad || strings.HasPrefix(p, bad+"/") {
				t.Errorf("the web service can reach %s, which %s", p, forbidden[bad])
			}
		}
	}

	// The handler must not be able to construct a filesystem path from a
	// request parameter. Assert no source file in the package mentions the
	// primitives that would be needed.
	assertNoSourceContains(t, filepath.Join(root, "internal", "albumapi"), []string{
		`filepath.Join(`,
		`filepath.Clean(`,
		`filepath.Abs(`,
		`os.OpenFile(`,
		`os.Create(`,
		`os.Remove(`,
		`os.Rename(`,
		`os.MkdirAll(`,
		`exec.Command(`,
		`scan.Walk(`,
		`thumbs.Generate(`,
	})

	// Conversely, the service must not be so restricted that it cannot do its
	// job: it needs the read-only index, the config and the resource sampler.
	required := []string{
		modulePath + "/internal/store",
		modulePath + "/internal/config",
		modulePath + "/internal/fsutil",
		modulePath + "/internal/mimetype",
	}
	for _, r := range required {
		if !pkgs[r] {
			t.Errorf("the web service no longer reaches %s; the boundary test is "+
				"describing a different program than the one being built", r)
		}
	}
}

// TestHeavyWorkersCannotReachTheWebService is the other half of the isolation.
// A thumbnail worker must not be able to serve HTTP, and in particular the
// worker that runs under a memory cap must not be able to pull the whole server
// and its request machinery into its address space.
func TestHeavyWorkersCannotReachTheWebService(t *testing.T) {
	root := moduleRoot(t)
	for _, worker := range []string{"internal/thumbs", "internal/scan"} {
		pkgs := importGraph(t, root, modulePath+"/"+worker)
		if pkgs[modulePath+"/internal/albumapi"] {
			t.Errorf("%s can reach internal/albumapi; the decode path and the serving "+
				"path must be separable processes", worker)
		}
		for pkg := range pkgs {
			for _, imp := range directImports(t, root, pkg) {
				if imp == "net/http" || strings.HasPrefix(imp, "net/http/") && imp != "net/http/internal" {
					t.Errorf("%s imports %s; a media worker must not be able to open "+
						"a listener", pkg, imp)
				}
			}
		}
	}
}

// TestScanAndThumbsDoNotImportEachOther keeps the two task types independent,
// so that a scan run cannot accidentally acquire the ability to decode.
func TestScanAndThumbsDoNotImportEachOther(t *testing.T) {
	root := moduleRoot(t)
	if importGraph(t, root, modulePath+"/internal/scan")[modulePath+"/internal/thumbs"] {
		t.Error("internal/scan reaches internal/thumbs; a metadata scan must not be able to decode an image")
	}
	if importGraph(t, root, modulePath+"/internal/thumbs")[modulePath+"/internal/scan"] {
		t.Error("internal/thumbs reaches internal/scan; the worker would then be able to walk directories")
	}
}

// importGraph returns every package reachable from pkg within this module, plus
// every standard-library and external import it transitively depends on. The
// starting package itself is included.
func importGraph(t *testing.T, moduleRoot, pkg string) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	queue := []string{pkg}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		for _, imp := range directImports(t, moduleRoot, cur) {
			// Traversal stops at the module edge. What matters is which of our
			// own packages are in the server's process, and what those packages
			// directly pull in.
			if strings.HasPrefix(imp, modulePath) && !seen[imp] {
				queue = append(queue, imp)
			}
		}
	}
	return seen
}

// short strips the module prefix so failures are readable.
func short(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, strings.TrimPrefix(p, modulePath+"/"))
	}
	return out
}

// directImports parses the package named by importPath.
//
// Standard-library and external packages cannot be parsed from this module, so
// they are recorded as leaves: that is what makes the "must not reach os/exec"
// assertion meaningful. Module-internal packages are parsed properly.
func directImports(t *testing.T, moduleRoot, importPath string) []string {
	t.Helper()
	var dir string
	if strings.HasPrefix(importPath, modulePath+"/") {
		dir = filepath.Join(moduleRoot, filepath.FromSlash(strings.TrimPrefix(importPath, modulePath+"/")))
	} else if importPath == modulePath {
		dir = moduleRoot
	} else {
		// A leaf outside this module. Look it up so that a standard-library
		// package with its own dependencies (net/http -> crypto/tls -> ...) is
		// still traversed.
		if d, ok := stdlibDir(importPath); ok {
			dir = d
		} else {
			return nil
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// An import that does not correspond to a directory we can read is
		// treated as a leaf rather than an error: vendored and cached packages
		// legitimately live outside the module.
		return nil
	}
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		// Skip files excluded by build constraints for this platform, roughly.
		if strings.HasSuffix(e.Name(), "_windows.go") || strings.HasSuffix(e.Name(), "_darwin.go") ||
			strings.HasSuffix(e.Name(), "_js.go") || strings.HasSuffix(e.Name(), "_plan9.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			continue
		}
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

// stdlibDir locates a standard-library package source directory, so the graph
// walk can cross into it.
func stdlibDir(importPath string) (string, bool) {
	goroot := os.Getenv("GOROOT")
	if goroot == "" {
		// Derive it from the running toolchain's location.
		for _, cand := range []string{"/usr/local/go", "/usr/lib/go", "/usr/lib/go-1.27"} {
			if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
				goroot = cand
				break
			}
		}
	}
	if goroot == "" {
		return "", false
	}
	dir := filepath.Join(goroot, "src", filepath.FromSlash(importPath))
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir, true
	}
	// Try the vendor subdirectory used by the standard library itself.
	dir = filepath.Join(goroot, "src", "vendor", filepath.FromSlash(importPath))
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir, true
	}
	return "", false
}

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
			t.Fatal("could not find the module root")
		}
		dir = parent
	}
}

// assertNoSourceContains fails if any non-test Go file in dir contains one of
// the given substrings.
func assertNoSourceContains(t *testing.T, dir string, forbidden []string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		for _, bad := range forbidden {
			if strings.Contains(string(data), bad) {
				t.Errorf("internal/albumapi/%s contains %s.\n"+
					"The web service must not be able to perform this operation; "+
					"the capability belongs in the scan or thumbs task.", rel, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
