package files

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/config"
)

// newTree builds a storage root plus an "outside" directory, and returns a
// manager over the root. The outside directory is where escape attempts aim.
func newTree(t *testing.T) (*Manager, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside, filepath.Join(root, "sub"), filepath.Join(root, "sub", "deep")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "a.txt"), "aaa")
	write(filepath.Join(root, "b.log"), "bbbb")
	write(filepath.Join(root, "sub", "c.txt"), "ccccc")
	write(filepath.Join(root, "sub", "deep", "d.txt"), "dddddd")
	write(filepath.Join(root, ".hidden"), "hidden")
	write(filepath.Join(outside, "secret.txt"), "SECRET-CONTENT")

	// Symlinks: one escaping the root, one staying inside it.
	if err := os.Symlink(outside, filepath.Join(root, "escape-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape-file")); err != nil {
		t.Fatal(err)
	}
	// A *relative* link that stays inside the root: the followed case.
	if err := os.Symlink("a.txt", filepath.Join(root, "inside-link")); err != nil {
		t.Fatal(err)
	}
	// An *absolute* link, even one pointing inside the root. os.Root refuses
	// these unconditionally, which is a documented Go restriction rather than a
	// choice made here.
	if err := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "abs-inside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..", filepath.Join(root, "sub", "updir")); err != nil {
		t.Fatal(err)
	}

	m, err := New([]config.HubStorage{{ID: "data", Label: "Data", Path: root}}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, root, outside
}

func TestAPlainListingShowsTheDirectory(t *testing.T) {
	m, _, _ := newTree(t)
	l, err := m.List("data", ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := map[string]bool{}
	for _, e := range l.Entries {
		names[e.Name] = true
	}
	for _, want := range []string{"a.txt", "b.log", "sub", "inside-link", "escape-dir"} {
		if !names[want] {
			t.Errorf("listing is missing %q; got %v", want, names)
		}
	}
	if names[".hidden"] {
		t.Error("a dot-prefixed entry was listed even though expose_hidden is off")
	}
	if l.Path != "/" {
		t.Errorf("root path renders as %q, want %q", l.Path, "/")
	}
}

func TestHiddenEntriesAppearOnlyWhenAskedFor(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".state"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := New([]config.HubStorage{{ID: "d", Path: root, ExposeHidden: true}}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	l, err := m.List("d", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) != 1 || l.Entries[0].Name != ".state" {
		t.Fatalf("expose_hidden did not include the hidden entry: %+v", l.Entries)
	}
}

// TestTraversalIsRefused is the core path-safety test. Every one of these shapes
// must be refused, and refused as a refusal (ErrOutsideRoot or ErrBadPath) rather
// than as a not-found, so that a caller cannot use error text to probe the
// filesystem.
func TestTraversalIsRefused(t *testing.T) {
	m, _, _ := newTree(t)
	cases := []struct {
		path string
		want error
	}{
		{"..", ErrOutsideRoot},
		{"../outside/secret.txt", ErrOutsideRoot},
		{"../outside", ErrOutsideRoot},
		{"sub/../../outside/secret.txt", ErrOutsideRoot},
		{"sub/../..", ErrOutsideRoot},
		{"/etc/passwd", ErrBadPath},
		{"/etc", ErrBadPath},
		{"//etc/passwd", ErrBadPath},
		// Note: "/" is *not* in this list. It means the root, and is covered by
		// TestTheRootPathIsAcceptedInTheFormsTheAPISends.
		{"with\x00nul", ErrBadPath},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if _, err := m.List("data", ListOptions{Path: c.path}); !errors.Is(err, c.want) {
				t.Errorf("List(%q) error = %v, want %v", c.path, err, c.want)
			}
			if _, err := m.Stat("data", c.path); !errors.Is(err, c.want) {
				t.Errorf("Stat(%q) error = %v, want %v", c.path, err, c.want)
			}
			f, _, err := m.Open("data", c.path)
			if f != nil {
				f.Close()
			}
			if !errors.Is(err, c.want) {
				t.Errorf("Open(%q) error = %v, want %v", c.path, err, c.want)
			}
		})
	}
}

// TestSymlinkEscapeIsRefused is the other half of containment, and the half a
// string-prefix check gets wrong. A link inside the root that points outside must
// not be followable through any of the three read paths.
func TestSymlinkEscapeIsRefused(t *testing.T) {
	m, _, _ := newTree(t)

	t.Run("directory link out of the root", func(t *testing.T) {
		if _, err := m.List("data", ListOptions{Path: "escape-dir"}); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("listing through an escaping directory link: error = %v, want ErrOutsideRoot", err)
		}
	})
	t.Run("file link out of the root", func(t *testing.T) {
		f, _, err := m.Open("data", "escape-file")
		if f != nil {
			f.Close()
		}
		if !errors.Is(err, ErrOutsideRoot) {
			t.Fatalf("opening an escaping file link: error = %v, want ErrOutsideRoot", err)
		}
		if err == nil {
			t.Fatal("the escaping link was opened; the secret outside the root is reachable")
		}
	})
	t.Run("stat through an escaping link", func(t *testing.T) {
		// Lstat answers about the link itself, which is inside the root and
		// therefore allowed - but it must not report the target's size.
		e, err := m.Stat("data", "escape-file")
		if err != nil {
			t.Fatalf("Stat of a link inside the root: %v", err)
		}
		if !e.Symlink {
			t.Error("the entry is not reported as a symlink, so a caller cannot tell")
		}
	})
	t.Run("a link to the parent directory stays inside", func(t *testing.T) {
		// sub/updir points at "..", which from sub/ resolves to the root
		// itself. The root is inside the root, so this is permitted, and the
		// listing is the root's listing. Asserting the real behaviour rather
		// than the intuitive one matters: a reader who assumes links to ".."
		// are always refused would misjudge what this module guarantees.
		l, err := m.List("data", ListOptions{Path: "sub/updir"})
		if err != nil {
			t.Fatalf("listing through a link to the parent: %v", err)
		}
		names := map[string]bool{}
		for _, e := range l.Entries {
			names[e.Name] = true
		}
		if !names["a.txt"] {
			t.Errorf("following sub/updir did not land on the root: %v", names)
		}
	})
	t.Run("a relative link inside the root is followed", func(t *testing.T) {
		f, _, err := m.Open("data", "inside-link")
		if err != nil {
			t.Fatalf("opening a relative link that stays inside the root: %v", err)
		}
		defer f.Close()
		buf := make([]byte, 8)
		n, _ := f.Read(buf)
		if got := string(buf[:n]); got != "aaa" {
			t.Errorf("content = %q, want %q", got, "aaa")
		}
	})
	t.Run("an absolute link is refused even when it points inside", func(t *testing.T) {
		// This is the behaviour an operator is most likely to be surprised by:
		// an absolute symlink to a file that is inside the root is still
		// refused. Documented, tested, and reported as a refusal rather than a
		// missing file so that the cause is visible.
		f, _, err := m.Open("data", "abs-inside-link")
		if f != nil {
			f.Close()
		}
		if !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("opening an absolute link: error = %v, want ErrOutsideRoot", err)
		}
	})
}

// TestTheRootPathIsAcceptedInTheFormsTheAPISends: displayPath renders the root as
// "/", so that is what the API returns and what the UI sends back on the next
// request. Every one of these spellings must mean the root, or the file manager's
// first listing fails while every other path works - a bug that a test which
// omits the parameter would never see.
func TestTheRootPathIsAcceptedInTheFormsTheAPISends(t *testing.T) {
	m, _, _ := newTree(t)
	for _, p := range []string{"", ".", "./", "/", "/."} {
		l, err := m.List("data", ListOptions{Path: p})
		if err != nil {
			t.Errorf("List(%q) = %v, want the root listing", p, err)
			continue
		}
		if l.Path != "/" {
			t.Errorf("List(%q) rendered as %q, want /", p, l.Path)
		}
		if len(l.Entries) == 0 {
			t.Errorf("List(%q) returned no entries; the root has files", p)
		}
		// The round trip must hold: the path the API returns must be accepted
		// back by the API.
		if _, err := m.List("data", ListOptions{Path: l.Path}); err != nil {
			t.Errorf("the path the API returned (%q) was refused when sent back: %v", l.Path, err)
		}
	}
}

// TestABackslashIsALiteralFilenameCharacter: on Linux a backslash is an ordinary
// character in a filename. Refusing it made such files unreachable - the browser
// listed them and every attempt to open one failed - so the rule is that it is
// literal, and containment is left to os.Root, which is what actually enforces it.
func TestABackslashIsALiteralFilenameCharacter(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A file whose name contains a backslash, and one containing a quote.
	odd := `quote"and\backslash.txt`
	if err := os.WriteFile(filepath.Join(root, odd), []byte("odd-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := New([]config.HubStorage{{ID: "d", Path: root}}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// It is listed, and it can be opened: the two must agree.
	l, err := m.List("d", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var listed string
	for _, e := range l.Entries {
		if strings.Contains(e.Name, "backslash") {
			listed = e.Name
		}
	}
	if listed == "" {
		t.Fatalf("the file with a backslash in its name was not listed: %+v", l.Entries)
	}
	f, _, err := m.Open("d", listed)
	if err != nil {
		t.Fatalf("the file was listed but cannot be opened: %v", err)
	}
	buf := make([]byte, 32)
	n, _ := f.Read(buf)
	f.Close()
	if got := string(buf[:n]); got != "odd-content" {
		t.Errorf("content = %q, want %q", got, "odd-content")
	}

	// A Windows-style traversal is a literal name here, and it does not escape:
	// the secret outside the root is not reachable through it.
	f2, _, err := m.Open("d", `..\..\outside\secret.txt`)
	if f2 != nil {
		f2.Close()
	}
	if err == nil {
		t.Fatal("a Windows-style traversal was opened")
	}
	if !errors.Is(err, ErrNotExist) && !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("error = %v, want not-found or a refusal", err)
	}
}

func TestAnUnknownRootIsRefused(t *testing.T) {
	m, _, _ := newTree(t)
	if _, err := m.List("nope", ListOptions{}); !errors.Is(err, ErrNoSuchRoot) {
		t.Errorf("error = %v, want ErrNoSuchRoot", err)
	}
}

// TestAMissingRootIsNotAnEmptyDirectory: a data volume that is not mounted must
// be reported as unavailable. Reporting an empty listing would tell the operator
// their files are gone.
func TestAMissingRootIsNotAnEmptyDirectory(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "not-mounted")
	m, err := New([]config.HubStorage{{ID: "gone", Path: missing}}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	roots := m.Roots()
	if len(roots) != 1 {
		t.Fatalf("expected one root, got %d", len(roots))
	}
	if roots[0].Available {
		t.Error("a root that does not exist is reported as available")
	}
	if roots[0].Note == "" {
		t.Error("an unavailable root carries no explanation")
	}
	if _, err := m.List("gone", ListOptions{}); !errors.Is(err, ErrRootUnavailable) {
		t.Errorf("List error = %v, want ErrRootUnavailable", err)
	}
	if _, err := m.Stat("gone", "."); !errors.Is(err, ErrRootUnavailable) {
		t.Errorf("Stat error = %v, want ErrRootUnavailable", err)
	}
}

func TestPagingIsBoundedAndStable(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// 25 files, several sharing a size so that a tie in the comparator would
	// show up as a duplicated or missing entry across pages.
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("f%02d.txt", i)
		content := strings.Repeat("x", 1+i%3)
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lim := DefaultLimits()
	lim.DefaultPageSize = 10
	lim.MaxPageSize = 10
	m, err := New([]config.HubStorage{{ID: "d", Path: root}}, lim)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	seen := map[string]int{}
	for page := 1; page <= 3; page++ {
		l, err := m.List("d", ListOptions{Page: page, Sort: SortSize})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if l.Total != 25 {
			t.Errorf("page %d: total = %d, want 25", page, l.Total)
		}
		if len(l.Entries) > 10 {
			t.Errorf("page %d returned %d entries, above the page cap of 10", page, len(l.Entries))
		}
		for _, e := range l.Entries {
			seen[e.Name]++
		}
	}
	if len(seen) != 25 {
		t.Errorf("paging saw %d distinct entries, want 25: entries were duplicated or skipped", len(seen))
	}
	for n, c := range seen {
		if c != 1 {
			t.Errorf("%s appeared %d times across pages, want exactly once", n, c)
		}
	}

	// A page size above the cap is clamped, not honoured.
	l, err := m.List("d", ListOptions{Page: 1, PageSize: 100000})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) > 10 {
		t.Errorf("an oversized page_size was not clamped: %d entries", len(l.Entries))
	}
}

func TestSortingOrdersAsAsked(t *testing.T) {
	m, _, _ := newTree(t)
	l, err := m.List("data", ListOptions{Sort: SortSize, Order: "desc"})
	if err != nil {
		t.Fatal(err)
	}
	// Directories come first regardless of the requested order.
	if !l.Entries[0].IsDir {
		t.Errorf("the first entry is not a directory: %+v", l.Entries[0])
	}
	// Among files, size must be descending.
	var last int64 = -1
	for _, e := range l.Entries {
		if e.IsDir {
			continue
		}
		if last >= 0 && e.Size > last {
			t.Errorf("size order is not descending: %d after %d", e.Size, last)
		}
		last = e.Size
	}
}

func TestATruncatedDirectorySaysSo(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lim := DefaultLimits()
	lim.MaxEntries = 5
	lim.MaxPageSize = 100
	m, err := New([]config.HubStorage{{ID: "d", Path: root}}, lim)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	l, err := m.List("d", ListOptions{PageSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !l.Truncated {
		t.Error("a directory larger than the examination cap is not reported as truncated")
	}
	if len(l.Entries) > 5 {
		t.Errorf("listing returned %d entries despite a cap of 5", len(l.Entries))
	}
}

func TestOpenRefusesNonRegularFiles(t *testing.T) {
	m, root, _ := newTree(t)

	if f, _, err := m.Open("data", "sub"); !errors.Is(err, ErrNotRegular) {
		if f != nil {
			f.Close()
		}
		t.Errorf("opening a directory: error = %v, want ErrNotRegular", err)
	}

	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	f, _, err := m.Open("data", "pipe")
	if f != nil {
		f.Close()
	}
	if !errors.Is(err, ErrNotRegular) {
		t.Errorf("opening a FIFO: error = %v, want ErrNotRegular (a FIFO would block a "+
			"streaming handler indefinitely)", err)
	}
}

func TestSearchIsBoundedAndReportsWhyItStopped(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("match-%02d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lim := DefaultLimits()
	lim.SearchMaxResults = 10
	m, err := New([]config.HubStorage{{ID: "d", Path: root}}, lim)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	res, err := m.Search("d", SearchOptions{Query: "match", MaxDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 10 {
		t.Errorf("results = %d, want the cap of 10", len(res.Entries))
	}
	if !res.Truncated || res.Reason != "result cap" {
		t.Errorf("Truncated=%v Reason=%q, want true/%q: a capped search must not look complete",
			res.Truncated, res.Reason, "result cap")
	}
	if res.Examined == 0 {
		t.Error("Examined is zero, so a caller cannot see the work done")
	}
}

func TestSearchHonoursTheDepthCap(t *testing.T) {
	m, _, _ := newTree(t)
	// d.txt lives at sub/deep, two levels down.
	res, err := m.Search("data", SearchOptions{Query: "d.txt", MaxDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 0 {
		t.Errorf("a depth-1 search found %d entries two levels down", len(res.Entries))
	}
	res, err = m.Search("data", SearchOptions{Query: "d.txt", MaxDepth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 {
		t.Errorf("a depth-3 search found %d entries, want 1", len(res.Entries))
	}
}

func TestSearchRejectsAnEmptyQuery(t *testing.T) {
	m, _, _ := newTree(t)
	if _, err := m.Search("data", SearchOptions{Query: "   "}); !errors.Is(err, ErrBadPath) {
		t.Errorf("error = %v, want ErrBadPath for an empty query", err)
	}
}

func TestSearchSurvivesAnUnreadableSubdirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions are not enforced, so this cannot be tested")
	}
	m, root, _ := newTree(t)
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "hidden.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	res, err := m.Search("data", SearchOptions{Query: "a.txt", MaxDepth: 3})
	if err != nil {
		t.Fatalf("an unreadable subdirectory failed the whole search: %v", err)
	}
	if len(res.Entries) == 0 {
		t.Error("the search found nothing even though a.txt exists in a readable directory")
	}
}

func TestLimitsAreClampedNotTrusted(t *testing.T) {
	m, _, _ := newTree(t)
	l, err := m.List("data", ListOptions{Page: -5, PageSize: -1})
	if err != nil {
		t.Fatal(err)
	}
	if l.Page != 1 {
		t.Errorf("page = %d, want 1", l.Page)
	}
	if l.PageSize <= 0 || l.PageSize > m.Limits().MaxPageSize {
		t.Errorf("page size = %d, want it inside (0, %d]", l.PageSize, m.Limits().MaxPageSize)
	}
}

func TestKindClassifiesWithoutDecidingAnything(t *testing.T) {
	cases := map[string]string{
		"a.jpg":  "image",
		"a.MP4":  "video",
		"a.flac": "audio",
		"a.zip":  "archive",
		"a.nfo":  "text",
		"a.bin":  "other",
	}
	for name, want := range cases {
		if got := Kind(name, false); got != want {
			t.Errorf("Kind(%q) = %q, want %q", name, got, want)
		}
	}
	if got := Kind("anything", true); got != "dir" {
		t.Errorf("Kind(dir) = %q, want dir", got)
	}
}

func TestRootsReportsEveryConfiguredRoot(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New([]config.HubStorage{
		{ID: "alpha", Path: a},
		{ID: "beta", Path: b, Label: "Beta"},
		{ID: "gamma", Path: filepath.Join(base, "missing")},
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	roots := m.Roots()
	if len(roots) != 3 {
		t.Fatalf("roots = %d, want 3", len(roots))
	}
	// Sorted by ID for stable output.
	if roots[0].ID != "alpha" || roots[1].ID != "beta" || roots[2].ID != "gamma" {
		t.Errorf("roots are not ordered by ID: %+v", roots)
	}
	if roots[1].Label != "Beta" {
		t.Errorf("label = %q, want Beta", roots[1].Label)
	}
	if !roots[0].Available || !roots[1].Available {
		t.Error("an existing root is reported as unavailable")
	}
	if roots[2].Available {
		t.Error("a missing root is reported as available")
	}
	// A root without a label falls back to its ID, so the UI never shows blank.
	if roots[0].Label != "alpha" {
		t.Errorf("label = %q, want the ID as a fallback", roots[0].Label)
	}
}

func TestModifiedTimeIsReported(t *testing.T) {
	m, root, _ := newTree(t)
	when := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "a.txt"), when, when); err != nil {
		t.Fatal(err)
	}
	e, err := m.Stat("data", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !e.ModTime.Equal(when) {
		t.Errorf("mod time = %s, want %s", e.ModTime, when)
	}
	if e.Kind != "text" {
		t.Errorf("kind = %q, want text", e.Kind)
	}
}
