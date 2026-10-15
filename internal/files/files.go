// Package files is the console's own file manager. It is written here rather
// than copied from another service. CasaOS file workflows are the functional
// reference; the frontend, containment and operations are implemented here.
//
// Two properties matter more than features, and both are structural rather than
// checked:
//
//   - **A request can never name a path outside a configured root.** Callers
//     address a storage ID plus a relative path. The relative path is resolved
//     through os.Root, which uses openat/statat under the kernel's own control,
//     so a symlink that points outside the root is refused by the kernel and
//     there is no window between "check the path" and "open the path" for
//     anything to change underneath us. A string-prefix check would have that
//     window; this does not.
//
//   - **No listing is unbounded.** Every read is one directory level, paged, with
//     a cap on entries examined. Search has depth, result and examination caps.
//     File writes require an opt-in root; publish/move/restore never overwrite.
package files

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/config"
)

// Errors returned by this package. They are sentinel values so that the HTTP
// layer can map them to status codes without matching on message text.
var (
	// ErrNoSuchRoot means the storage ID is not configured.
	ErrNoSuchRoot = errors.New("files: no such storage root")
	// ErrOutsideRoot means the path tried to leave the root. It is a refusal,
	// never a not-found: the caller asked for something it may not have.
	ErrOutsideRoot = errors.New("files: path is outside the storage root")
	// ErrNotExist means the entry is genuinely absent inside the root.
	ErrNotExist = errors.New("files: no such file or directory")
	// ErrNotRegular means the entry is not a regular file, so it cannot be
	// streamed as content.
	ErrNotRegular = errors.New("files: not a regular file")
	// ErrBadPath means the relative path is malformed (empty, absolute, NUL).
	ErrBadPath = errors.New("files: malformed path")
	// ErrRootUnavailable means the configured directory is missing or is not a
	// directory right now. A missing mount is reported as this, not as an empty
	// directory, because "the share is gone" and "the share is empty" must not
	// look the same.
	ErrRootUnavailable = errors.New("files: storage root is unavailable")
)

// Limits bounds every read this package performs.
type Limits struct {
	// MaxEntries caps how many directory entries are examined for one listing,
	// before paging. A directory with a million files is answered with its
	// first MaxEntries in sorted order plus an explicit truncation flag, rather
	// than with a million stat calls.
	MaxEntries int
	// MaxPageSize caps a single page.
	MaxPageSize int
	// DefaultPageSize is used when a request does not ask for one.
	DefaultPageSize int
	// SearchMaxResults caps a search, and SearchMaxExamined caps the work it may
	// do regardless of how many results it finds.
	SearchMaxResults  int
	SearchMaxExamined int
	// PreviewMaxBytes caps what the preview endpoint will pass through as an
	// image. The console does not decode images in a request; it hands the
	// browser bytes it can already render, and refuses to hand it something
	// enormous.
	PreviewMaxBytes int64
	UploadMaxBytes  int64
	TrashMaxEntries int
	CopyMaxEntries  int
	CopyMaxBytes    int64
	BatchMaxItems   int
}

// DefaultLimits returns the shipped bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxEntries:        20000,
		MaxPageSize:       200,
		DefaultPageSize:   50,
		SearchMaxResults:  200,
		SearchMaxExamined: 20000,
		PreviewMaxBytes:   32 << 20, // 32 MiB
		UploadMaxBytes:    8 << 30,
		TrashMaxEntries:   1000,
		CopyMaxEntries:    10000,
		CopyMaxBytes:      8 << 30,
		BatchMaxItems:     100,
	}
}

// Root is one configured storage root, opened once.
type Root struct {
	ID           string
	Label        string
	Path         string
	ExposeHidden bool
	Writable     bool
	writeMu      sync.Mutex

	dir *os.Root
}

// RootInfo describes a root without exposing its handle.
type RootInfo struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Path         string `json:"path"`
	ExposeHidden bool   `json:"expose_hidden"`
	Available    bool   `json:"available"`
	Writable     bool   `json:"writable"`
	// Note explains an unavailable root in words an operator can act on.
	Note          string    `json:"note,omitempty"`
	Capacity      *Capacity `json:"capacity,omitempty"`
	CapacityError string    `json:"capacity_error,omitempty"`
}

// Entry is one directory entry.
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"` // root-relative, slash-separated
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	Mode    string    `json:"mode"`
	Symlink bool      `json:"symlink"`
	// Kind is a coarse classification used for icons and for deciding whether a
	// preview makes sense. It never decides permissions.
	Kind string `json:"kind"`
}

// Listing is one page of one directory.
type Listing struct {
	Root     string  `json:"root"`
	Path     string  `json:"path"`
	Entries  []Entry `json:"entries"`
	Page     int     `json:"page"`
	PageSize int     `json:"page_size"`
	// Total is how many entries the directory has, up to the examination cap.
	Total int `json:"total"`
	// Truncated is true when the directory holds more entries than were
	// examined. The page count is then a lower bound, and the UI says so
	// instead of implying the directory is smaller than it is.
	Truncated bool `json:"truncated"`
	// Sort and Order echo the applied ordering so a client never has to guess.
	Sort  string `json:"sort"`
	Order string `json:"order"`
}

// Manager owns the configured roots.
type Manager struct {
	roots  map[string]*Root
	order  []string
	limits Limits
}

// New opens every configured root.
//
// A root that is missing at startup is not fatal: the console must still start so
// that an operator can see *why* the data volume is gone. It is recorded as
// unavailable, and every operation against it fails with ErrRootUnavailable
// rather than reporting an empty directory.
func New(storages []config.HubStorage, limits Limits) (*Manager, error) {
	d := DefaultLimits()
	if limits.MaxEntries == 0 {
		limits.MaxEntries = d.MaxEntries
	}
	if limits.MaxPageSize == 0 {
		limits.MaxPageSize = d.MaxPageSize
	}
	if limits.DefaultPageSize == 0 {
		limits.DefaultPageSize = d.DefaultPageSize
	}
	if limits.SearchMaxResults == 0 {
		limits.SearchMaxResults = d.SearchMaxResults
	}
	if limits.SearchMaxExamined == 0 {
		limits.SearchMaxExamined = d.SearchMaxExamined
	}
	if limits.PreviewMaxBytes == 0 {
		limits.PreviewMaxBytes = d.PreviewMaxBytes
	}
	if limits.CopyMaxEntries == 0 {
		limits.CopyMaxEntries = DefaultLimits().CopyMaxEntries
	}
	if limits.CopyMaxBytes == 0 {
		limits.CopyMaxBytes = DefaultLimits().CopyMaxBytes
	}
	if limits.BatchMaxItems == 0 {
		limits.BatchMaxItems = DefaultLimits().BatchMaxItems
	}
	if limits.UploadMaxBytes <= 0 {
		limits.UploadMaxBytes = DefaultLimits().UploadMaxBytes
	}
	if limits.TrashMaxEntries <= 0 {
		limits.TrashMaxEntries = DefaultLimits().TrashMaxEntries
	}
	m := &Manager{roots: map[string]*Root{}, limits: limits}
	for _, s := range storages {
		r := &Root{
			ID:           s.ID,
			Label:        s.Label,
			Path:         s.Path,
			ExposeHidden: s.ExposeHidden,
			Writable:     s.Writable,
		}
		if r.Label == "" {
			r.Label = s.ID
		}
		// A root is opened lazily-but-eagerly: we try once here, and the
		// manager keeps the failure so that it can be reported per request.
		if dir, err := os.OpenRoot(s.Path); err == nil {
			r.dir = dir
		}
		m.roots[s.ID] = r
		m.order = append(m.order, s.ID)
	}
	sort.Strings(m.order)
	return m, nil
}

// Close releases every open root.
func (m *Manager) Close() error {
	var first error
	for _, r := range m.roots {
		if r.dir != nil {
			if err := r.dir.Close(); err != nil && first == nil {
				first = err
			}
			r.dir = nil
		}
	}
	return first
}

// Limits returns the manager's bounds.
func (m *Manager) Limits() Limits { return m.limits }

// Roots lists the configured roots with their current availability.
func (m *Manager) Roots() []RootInfo {
	out := make([]RootInfo, 0, len(m.order))
	for _, id := range m.order {
		r := m.roots[id]
		info := RootInfo{
			ID:           r.ID,
			Label:        r.Label,
			Path:         r.Path,
			ExposeHidden: r.ExposeHidden,
			Available:    r.dir != nil,
			Writable:     r.Writable,
		}
		if r.dir == nil {
			info.Note = describeUnavailable(r.Path)
		} else {
			capacity, err := capacityFor(r)
			if err == nil {
				info.Capacity = capacity
			} else if errors.Is(err, ErrRootUnavailable) {
				info.Available = false
				info.Note = "storage path or mount changed; reconnect the configured root"
			} else {
				info.CapacityError = "filesystem capacity could not be read"
			}
		}
		out = append(out, info)
	}
	return out
}

// describeUnavailable explains why a root cannot be opened, in one sentence.
func describeUnavailable(p string) string {
	fi, err := os.Stat(p)
	switch {
	case err == nil && !fi.IsDir():
		return "the configured path exists but is not a directory"
	case err != nil:
		return "the configured path cannot be opened: " + err.Error() +
			" (is the volume mounted?)"
	default:
		return "the configured path could not be opened"
	}
}

// root returns the named root, or ErrNoSuchRoot.
func (m *Manager) root(id string) (*Root, error) {
	r, ok := m.roots[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoSuchRoot, id)
	}
	if r.dir == nil {
		return nil, fmt.Errorf("%w: %s", ErrRootUnavailable, r.Path)
	}
	if f, err := connectedRoot(r); err != nil {
		return nil, err
	} else {
		_ = f.Close()
	}
	return r, nil
}

// cleanRel normalises a caller-supplied relative path.
//
// The rules are deliberately strict, because everything downstream depends on
// this value being a plain relative path:
//
//   - an absolute path is refused, since roots are addressed by ID;
//   - a backslash is *accepted* and treated as a literal character, because on
//     Linux that is what it is: `a\b` is one filename, not a directory `a`
//     containing `b`. Refusing it made every file with a backslash in its name
//     unreachable - listed by the browser, then refused when opened - which is a
//     worse outcome than the theoretical confusion it was meant to prevent. A
//     Windows-style traversal like `..\..\etc\passwd` is a single (usually
//     non-existent) filename here, and os.Root refuses it if it is not;
//   - NUL is refused;
//   - "." and "" mean the root itself.
//
// Traversal sequences are *not* stripped here. They are refused by os.Root at
// the kernel boundary, which is the check that actually holds; this function's
// job is to reject the shapes that have no meaning, not to be the safety net.
func cleanRel(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: contains a NUL byte", ErrBadPath)
	}
	// The root itself can be named several ways, and all of them mean the same
	// thing. "/" is not decoration: it is what displayPath renders the root as,
	// so it is what the API and the UI send back. It is accepted *before* the
	// absolute-path check below, which exists to stop a caller naming a path on
	// the machine - and "/" names no path beyond the root it is relative to.
	if p == "" || p == "." || p == "./" || p == "/" || p == "/." {
		return ".", nil
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: must be relative to a storage root", ErrBadPath)
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		// Refused here as well as by the kernel so that the error is
		// unambiguous in logs: this is a caller naming the parent, not a
		// symlink inside the root.
		return "", fmt.Errorf("%w: %q names a parent directory", ErrOutsideRoot, p)
	}
	if strings.HasPrefix(c, "/") {
		return "", fmt.Errorf("%w: must be relative", ErrBadPath)
	}
	for _, segment := range strings.Split(c, "/") {
		if internalName(segment) {
			return "", fmt.Errorf("%w: reserved internal directory", ErrOutsideRoot)
		}
	}
	return c, nil
}

// mapErr translates an os error from a Root operation into this package's
// vocabulary. os.Root reports escapes as a specific error; a plain ENOENT means
// the entry really is absent inside the root.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %v", ErrNotExist, err)
	}
	if errors.Is(err, syscall.ENOTDIR) {
		return fmt.Errorf("%w: not a directory", ErrBadPath)
	}
	// os.Root refuses escapes with this sentinel.
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w: %v", ErrOutsideRoot, err)
	}
	if strings.Contains(err.Error(), "path escapes from parent") {
		return fmt.Errorf("%w: %v", ErrOutsideRoot, err)
	}
	return err
}

// Kind classifies an entry for display. It is advisory only.
func Kind(name string, isDir bool) string {
	if isDir {
		return "dir"
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".avif":
		return "image"
	case ".mp4", ".m4v", ".mkv", ".webm", ".mov", ".avi", ".ts":
		return "video"
	case ".mp3", ".flac", ".m4a", ".ogg", ".opus", ".wav":
		return "audio"
	case ".zip", ".tar", ".gz", ".xz", ".7z", ".rar":
		return "archive"
	case ".nfo", ".srt", ".ass", ".sub", ".txt", ".md", ".json", ".toml", ".yaml", ".yml":
		return "text"
	default:
		return "other"
	}
}

// Pages returns how many pages this listing's total implies, at least one. The
// count is a lower bound when Truncated is set, and the UI says so.
func (l *Listing) Pages() int {
	if l.PageSize <= 0 {
		return 1
	}
	n := (l.Total + l.PageSize - 1) / l.PageSize
	if n < 1 {
		return 1
	}
	return n
}

// SortField names an accepted sort key.
type SortField string

const (
	SortName SortField = "name"
	SortSize SortField = "size"
	SortTime SortField = "time"
	SortKind SortField = "kind"
)

// ParseSort validates a requested sort key, defaulting to name.
func ParseSort(s string) SortField {
	switch SortField(strings.ToLower(strings.TrimSpace(s))) {
	case SortSize:
		return SortSize
	case SortTime:
		return SortTime
	case SortKind:
		return SortKind
	default:
		return SortName
	}
}

// ParseOrder validates a requested direction, defaulting to ascending.
func ParseOrder(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), "desc") {
		return "desc"
	}
	return "asc"
}

// ListOptions is a listing request, already parsed and bounded.
type ListOptions struct {
	Path     string
	Page     int
	PageSize int
	Sort     SortField
	Order    string
}

// clamp bounds a listing request against the configured limits.
func (m *Manager) clamp(o ListOptions) ListOptions {
	if o.Page < 1 {
		o.Page = 1
	}
	if o.PageSize <= 0 {
		o.PageSize = m.limits.DefaultPageSize
	}
	if o.PageSize > m.limits.MaxPageSize {
		o.PageSize = m.limits.MaxPageSize
	}
	if o.Sort == "" {
		o.Sort = SortName
	}
	if o.Order == "" {
		o.Order = "asc"
	}
	return o
}

// List returns one page of one directory level.
//
// The implementation reads at most MaxEntries entries, stats each one, sorts in
// memory (bounded by MaxEntries) and returns a slice. It never recurses and never
// returns more than a page.
func (m *Manager) List(rootID string, o ListOptions) (*Listing, error) {
	r, err := m.root(rootID)
	if err != nil {
		return nil, err
	}
	rel, err := cleanRel(o.Path)
	if err != nil {
		return nil, err
	}
	o = m.clamp(o)

	dir, err := safeOpen(r.dir, rel, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK)
	if err != nil {
		return nil, mapErr(err)
	}
	defer dir.Close()
	fi, err := dir.Stat()
	if err != nil {
		return nil, mapErr(err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrBadPath, rel)
	}

	entries, truncated, _, err := readDirBounded(dir, r.ExposeHidden, m.limits.MaxEntries)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].Path = joinRel(rel, entries[i].Name)
	}
	sortEntries(entries, o.Sort, o.Order)

	total := len(entries)
	start := (o.Page - 1) * o.PageSize
	if start > total {
		start = total
	}
	end := start + o.PageSize
	if end > total {
		end = total
	}

	return &Listing{
		Root:      rootID,
		Path:      displayPath(rel),
		Entries:   entries[start:end],
		Page:      o.Page,
		PageSize:  o.PageSize,
		Total:     total,
		Truncated: truncated,
		Sort:      string(o.Sort),
		Order:     o.Order,
	}, nil
}

// readDirBounded reads one directory, skipping hidden names unless asked, and
// stops after max entries. It stats every entry it keeps, because a listing that
// shows sizes and times must know them; the cap is what bounds that cost.
func readDirBounded(dir *os.File, exposeHidden bool, max int) ([]Entry, bool, int, error) {
	out := make([]Entry, 0, 64)
	truncated := false
	examined := 0
	for {
		batch, err := dir.ReadDir(256)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, false, examined, err
		}
		for _, de := range batch {
			if examined >= max {
				return out, true, examined, nil
			}
			examined++
			name := de.Name()
			if internalName(name) {
				continue
			}
			if !exposeHidden && strings.HasPrefix(name, ".") {
				continue
			}
			e := Entry{Name: name}
			// de.Info() on a directory read from an os.Root is an lstat-like
			// result: it does not follow a symlink out of the root, so an
			// escaping link is listed as a link, never as its target.
			if info, ierr := infoAt(dir, name); ierr == nil {
				e.Size = info.Size()
				e.ModTime = info.ModTime()
				e.Mode = info.Mode().String()
				e.IsDir = info.IsDir()
				e.Symlink = info.Mode()&os.ModeSymlink != 0
			}
			e.Kind = Kind(name, e.IsDir)
			out = append(out, e)
		}
		if errors.Is(err, io.EOF) {
			return out, truncated, examined, nil
		}
	}
}

// sortEntries orders a listing. Ties fall back to name so that paging is stable:
// without that, two entries with the same size could swap places between pages
// and an entry could be missed entirely.
func sortEntries(es []Entry, f SortField, order string) {
	less := func(i, j int) bool {
		a, b := es[i], es[j]
		// Directories first, always: it is what a file manager is expected to
		// do and it keeps navigation predictable.
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		switch f {
		case SortSize:
			if a.Size != b.Size {
				return a.Size < b.Size
			}
		case SortTime:
			if !a.ModTime.Equal(b.ModTime) {
				return a.ModTime.Before(b.ModTime)
			}
		case SortKind:
			if a.Kind != b.Kind {
				return a.Kind < b.Kind
			}
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	}
	sort.SliceStable(es, func(i, j int) bool {
		if order == "desc" {
			// Reverse only the comparator's non-directory part by swapping the
			// comparison, keeping directories first.
			a, b := es[i], es[j]
			if a.IsDir != b.IsDir {
				return a.IsDir
			}
			return less(j, i)
		}
		return less(i, j)
	})
}

// joinRel joins a directory-relative path with a name, using slashes.
func joinRel(dir, name string) string {
	if dir == "." || dir == "" {
		return name
	}
	return dir + "/" + name
}

// displayPath renders a root-relative path for the API: the root itself is "/".
func displayPath(rel string) string {
	if rel == "." || rel == "" {
		return "/"
	}
	return "/" + rel
}

// Stat returns one entry's metadata.
func (m *Manager) Stat(rootID, rel string) (*Entry, error) {
	r, err := m.root(rootID)
	if err != nil {
		return nil, err
	}
	clean, err := cleanRel(rel)
	if err != nil {
		return nil, err
	}
	// Lstat first: whether the entry is a symlink is part of what the caller is
	// asking, and following it before answering would hide that.
	info, err := safeLstat(r.dir, clean)
	if err != nil {
		return nil, mapErr(err)
	}
	name := path.Base(clean)
	if clean == "." {
		name = "/"
	}
	e := &Entry{
		Name:    name,
		Path:    displayPath(clean),
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Mode:    info.Mode().String(),
		Symlink: info.Mode()&os.ModeSymlink != 0,
	}
	e.Kind = Kind(name, e.IsDir)
	return e, nil
}

// Open opens a regular file for streaming.
//
// The returned file is inside the root; the caller closes it. A symlink is
// followed only if its target stays inside the root, which os.Root enforces.
// Anything that is not a regular file is refused, so a request cannot stream a
// device node, a FIFO (which would block a worker forever) or a directory.
func (m *Manager) Open(rootID, rel string) (*os.File, os.FileInfo, error) {
	r, err := m.root(rootID)
	if err != nil {
		return nil, nil, err
	}
	clean, err := cleanRel(rel)
	if err != nil {
		return nil, nil, err
	}
	// O_NONBLOCK matters more than it looks. Opening a FIFO for reading blocks
	// until a writer appears, so a request for a named pipe would park an HTTP
	// handler - and its goroutine and file descriptor - indefinitely. With
	// O_NONBLOCK the open returns at once and the mode check below refuses the
	// FIFO; for regular files the flag has no effect on subsequent reads.
	f, err := safeOpen(r.dir, clean, os.O_RDONLY|syscall.O_NONBLOCK)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, mapErr(err)
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%w: %s", ErrNotRegular, displayPath(clean))
	}
	return f, fi, nil
}

// SearchOptions is a bounded search request.
type SearchOptions struct {
	// Path restricts the search to a subtree.
	Path string
	// Query is matched case-insensitively against entry names. It is never
	// interpreted as a glob or a regular expression: a search box that accepts
	// patterns invites a caller to write one that is quadratic.
	Query string
	// MaxDepth bounds how deep the search may descend. Zero means one level.
	MaxDepth int
}

// SearchResult is a bounded search answer.
type SearchResult struct {
	Root string `json:"root"`
	// Entries found, capped.
	Entries []Entry `json:"entries"`
	// Examined is how many entries were looked at, so a caller can see the work
	// that was done rather than only the results.
	Examined int `json:"examined"`
	// Truncated is true when the search stopped early because it hit the
	// examined or result cap. A truncated search is reported as truncated, never
	// as "no more matches".
	Truncated bool `json:"truncated"`
	// Reason says why it stopped, in words: "result cap", "examined cap",
	// "depth cap", "time cap".
	Reason string `json:"reason,omitempty"`
	// ElapsedMS is the wall time spent.
	ElapsedMS int64 `json:"elapsed_ms"`
}

// Search walks a bounded subtree looking for names containing a substring.
//
// Every bound is enforced: the number of entries examined, the number of
// results, the depth, and a wall-clock cap. The first bound reached stops the
// walk and is reported, so the caller knows the answer is partial.
func (m *Manager) Search(rootID string, o SearchOptions) (*SearchResult, error) {
	r, err := m.root(rootID)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	rel, err := cleanRel(o.Path)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(o.Query))
	if q == "" {
		return nil, fmt.Errorf("%w: empty search query", ErrBadPath)
	}
	if len(q) > 128 {
		q = q[:128]
	}
	depth := o.MaxDepth
	if depth <= 0 {
		depth = 1
	}
	if depth > 6 {
		depth = 6
	}

	res := &SearchResult{Root: rootID}
	deadline := start.Add(2 * time.Second)

	var walk func(dirRel string, level int) bool
	walk = func(dirRel string, level int) bool {
		if time.Now().After(deadline) {
			res.Truncated, res.Reason = true, "time cap"
			return false
		}
		if level > depth {
			res.Truncated, res.Reason = true, "depth cap"
			return false
		}
		d, err := safeOpen(r.dir, dirRel, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK)
		if err != nil {
			// An unreadable subdirectory is skipped, not fatal: one bad
			// directory must not fail the whole search.
			return true
		}
		defer d.Close()
		remaining := m.limits.SearchMaxExamined - res.Examined
		if remaining <= 0 {
			res.Truncated, res.Reason = true, "examined cap"
			return false
		}
		entries, truncated, examined, err := readDirBounded(d, r.ExposeHidden, remaining)
		res.Examined += examined
		if err != nil {
			return true
		}
		if truncated {
			res.Truncated, res.Reason = true, "examined cap"
		}
		var subdirs []string
		for _, e := range entries {
			e.Path = displayPath(joinRel(dirRel, e.Name))
			if strings.Contains(strings.ToLower(e.Name), q) {
				if len(res.Entries) >= m.limits.SearchMaxResults {
					res.Truncated, res.Reason = true, "result cap"
					return false
				}
				res.Entries = append(res.Entries, e)
			}
			if e.IsDir && level < depth {
				subdirs = append(subdirs, joinRel(dirRel, e.Name))
			}
		}
		for _, sd := range subdirs {
			if !walk(sd, level+1) {
				return false
			}
		}
		return true
	}
	walk(rel, 1)
	res.ElapsedMS = time.Since(start).Milliseconds()
	return res, nil
}

// PreviewKind reports whether a preview makes sense for a name, and why not when
// it does not. The console passes image bytes through untouched; it never
// decodes them in a request, so this only decides whether the browser is likely
// to render the result.
func PreviewKind(name string) (string, bool) {
	switch Kind(name, false) {
	case "image":
		return "image", true
	case "video":
		return "video", true
	case "audio":
		return "audio", true
	default:
		return "none", false
	}
}
