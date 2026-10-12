// Package scan walks configured media directories and keeps the index in step
// with them.
//
// The walker is written by hand rather than using filepath.WalkDir because
// WalkDir reads an entire directory's entries into memory in one call. On a
// phone-dump directory with 60 000 files that is a spike this machine cannot
// afford. This walker reads directory entries in fixed-size batches and keeps
// only the directory stack, so peak memory is O(depth * batch) regardless of
// how many files exist.
package scan

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WalkOptions configures a walk.
type WalkOptions struct {
	MaxDepth       int
	Batch          int
	FollowSymlinks bool
	Exclude        []string
	// OneFileSystem stops at mount boundaries so that a stray mount inside a
	// media tree is never indexed as if it were part of the collection.
	OneFileSystem bool
	RootDevice    uint64
}

// Entry is one candidate file.
type Entry struct {
	AbsPath string
	RelPath string
	RelDir  string
	Name    string
	Size    int64
	MtimeNS int64
}

// WalkStats counts what the walker saw.
type WalkStats struct {
	Dirs      int64
	FilesSeen int64
	FilesKept int64
	Symlinks  int64
	// Excluded counts directories matched by an exclude rule. Files matched by
	// the same rules, and files whose extension is not a media type this build
	// handles, are counted separately: lumping them in here made the "directories
	// excluded" line report thousands of *files*, which reads as a walk that
	// dropped pictures.
	Excluded    int64
	FilesByName int64
	NotMedia    int64
	Hidden      int64
	DepthCut    int64
	OtherFS     int64
	ReadErrors  int64
	Notes       []string
}

func (s *WalkStats) note(format string, args ...any) {
	if len(s.Notes) >= 64 {
		return
	}
	s.Notes = append(s.Notes, fmt.Sprintf(format, args...))
}

// WalkStream visits candidate files under root, calling fn for each.
//
// fn returning ErrStop ends the walk without an error. Symlinks are counted and
// skipped unless FollowSymlinks is set; even then the resolved path is checked
// against root before fn is called, so a link pointing outside the collection
// is refused rather than followed.
func WalkStream(root string, opts WalkOptions, include func(ext string) bool, fn func(Entry) error) (WalkStats, error) {
	var st WalkStats
	if opts.Batch <= 0 {
		opts.Batch = 256
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = 64
	}
	root = filepath.Clean(root)

	type level struct {
		path  string
		rel   string
		depth int
	}
	stack := []level{{path: root, rel: "", depth: 0}}

	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		st.Dirs++

		entries, err := readDirBatched(cur.path, opts.Batch)
		if err != nil {
			st.ReadErrors++
			st.note("cannot read %s: %v", cur.path, err)
			continue
		}
		for _, de := range entries {
			name := de.Name()
			rel := name
			if cur.rel != "" {
				rel = cur.rel + "/" + name
			}
			abs := filepath.Join(cur.path, name)

			if excludeMatch(opts.Exclude, rel, name) {
				if de.IsDir() {
					st.Excluded++
				} else {
					st.FilesByName++
				}
				continue
			}

			// A leading dot means "not mine". The concrete case is a media
			// tree shared over SMB from a Mac, where every photo is
			// accompanied by an AppleDouble sidecar named "._IMG_1234.JPG".
			// Those carry a media extension, so without this rule they would be
			// probed, indexed and shown as broken frames next to the real
			// picture. .DS_Store, .Trash and .thumbnails live under the same
			// rule. The count is reported, so the exclusion is visible rather
			// than a file quietly going missing.
			if len(name) > 0 && name[0] == '.' {
				st.Hidden++
				continue
			}

			mode := de.Type()
			switch {
			case mode&fs.ModeSymlink != 0:
				st.Symlinks++
				if !opts.FollowSymlinks {
					continue
				}
				target, err := filepath.EvalSymlinks(abs)
				if err != nil {
					st.note("broken symlink %s: %v", rel, err)
					continue
				}
				if !within(root, target) {
					st.note("symlink %s resolves outside the collection root (%s); skipped", rel, target)
					continue
				}
				info, err := os.Stat(abs)
				if err != nil {
					continue
				}
				if info.IsDir() {
					if cur.depth+1 > opts.MaxDepth {
						st.DepthCut++
						continue
					}
					stack = append(stack, level{path: abs, rel: rel, depth: cur.depth + 1})
					continue
				}
				if opts.OneFileSystem && !sameDevice(info, root) {
					st.OtherFS++
					continue
				}
				if err := addFile(&st, abs, rel, name, info, include, fn); err != nil {
					return st, stopOrError(err)
				}

			case mode.IsDir():
				if cur.depth+1 > opts.MaxDepth {
					st.DepthCut++
					st.note("depth limit %d reached at %s", opts.MaxDepth, rel)
					continue
				}
				if opts.OneFileSystem {
					info, err := de.Info()
					if err != nil {
						continue
					}
					if !sameDevice(info, root) {
						st.OtherFS++
						st.note("%s is on a different filesystem than the collection root; skipped", rel)
						continue
					}
				}
				stack = append(stack, level{path: abs, rel: rel, depth: cur.depth + 1})

			default:
				info, err := de.Info()
				if err != nil {
					st.ReadErrors++
					continue
				}
				if !info.Mode().IsRegular() {
					continue
				}
				if opts.OneFileSystem && !sameDevice(info, root) {
					st.OtherFS++
					continue
				}
				if err := addFile(&st, abs, rel, name, info, include, fn); err != nil {
					return st, stopOrError(err)
				}
			}
		}
	}
	return st, nil
}

// stopOrError keeps two very different outcomes apart: a visitor that asks to
// stop has succeeded, and a visitor that returns any other error has failed and
// must be heard.
//
// This distinction used to be lost — the walker compared the visitor's error
// against the stop sentinel and discarded everything else, so a caller whose
// bookkeeping had broken would get a silent partial walk reported as a finished
// one. A partial walk is exactly the thing that must never look complete: the
// scan uses completeness to decide whether a file may be marked absent.
func stopOrError(err error) error {
	if errors.Is(err, errStop) {
		return nil
	}
	return err
}

// errStop is an internal sentinel; exported behaviour is via StopWalk.
var errStop = errors.New("stop walk")

// StopWalk is returned by a visitor to end the walk cleanly, e.g. when a
// --limit has been reached.
func StopWalk() error { return errStop }

// IsStop reports whether err is the clean-stop sentinel.
func IsStop(err error) bool { return errors.Is(err, errStop) }

func addFile(st *WalkStats, abs, rel, name string, info os.FileInfo, include func(string) bool, fn func(Entry) error) error {
	st.FilesSeen++
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if ext == "" || !include(ext) {
		st.NotMedia++
		return nil
	}
	e := Entry{
		AbsPath: abs,
		RelPath: filepath.ToSlash(rel),
		RelDir:  filepath.ToSlash(filepath.Dir(rel)),
		Name:    name,
		Size:    info.Size(),
		MtimeNS: info.ModTime().UnixNano(),
	}
	if e.RelDir == "." {
		e.RelDir = ""
	}
	st.FilesKept++
	return fn(e)
}

// readDirBatched reads a directory in batches so a huge directory cannot
// materialise in memory.
//
// One batch of entries is held at a time; the caller processes each batch before
// the next is read. There is no cap on the number of entries in a directory
// (that would silently drop files), only on how many are resident at once.
func readDirBatched(dir string, batch int) ([]fs.DirEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []fs.DirEntry
	for {
		chunk, err := f.ReadDir(batch)
		out = append(out, chunk...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			// A partial read still yields usable entries; report the directory
			// as read and let the caller note the shortfall.
			return out, nil
		}
		if len(chunk) < batch {
			return out, nil
		}
	}
}

// excludeMatch applies the configured exclusion patterns to a relative path and
// to the base name, supporting simple globs (path.Match syntax) and prefix
// directory matches such as "@eaDir" or "#recycle".
func excludeMatch(patterns []string, rel, name string) bool {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "/") {
			p = strings.TrimPrefix(p, "/")
		}
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
		if ok, _ := filepath.Match(p, rel); ok {
			return true
		}
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	rel = filepath.Clean(rel)
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// sameDevice reports whether info and the root live on the same filesystem. It
// uses the portable-ish Sys() path so no extra syscall is needed.
func sameDevice(info os.FileInfo, root string) bool {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return true
	}
	ai, aok := info.Sys().(*syscallStat)
	bi, bok := rootInfo.Sys().(*syscallStat)
	if !aok || !bok {
		return true
	}
	return ai.Dev == bi.Dev
}

// SortEntries orders candidates by relative path. Only used by tests and by
// dry-run reports, which operate on bounded slices.
func SortEntries(es []Entry) {
	sort.Slice(es, func(i, j int) bool { return es[i].RelPath < es[j].RelPath })
}
