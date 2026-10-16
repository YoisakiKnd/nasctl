package files

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
)

// ZipPlan is a directory checked against the download budget before any byte
// is sent, so an oversized folder is refused with an error instead of ending
// as a truncated archive.
type ZipPlan struct {
	Name    string // the folder's own name, used as the archive's top level
	Entries int
	Bytes   int64
	root    *Root
	nodes   []zipNode
}

type zipNode struct {
	from, name string
	info       os.FileInfo
}

// PlanZip walks a directory without following links. Hidden files follow the
// root's listing policy, internal names are never included, and special files
// are skipped. It takes no write lock: a browser download can be slow, and the
// root must stay usable meanwhile; files that change during the download stop
// it rather than producing a mixed archive.
func (m *Manager) PlanZip(ctx context.Context, rootID, rel string, limit int, maxBytes int64) (*ZipPlan, error) {
	if limit < 1 || limit > m.limits.CopyMaxEntries || maxBytes < 1 || maxBytes > m.limits.CopyMaxBytes {
		return nil, ErrTreeLimit
	}
	r, e := m.root(rootID)
	if e != nil {
		return nil, e
	}
	p, e := cleanRel(rel)
	if e != nil {
		return nil, e
	}
	info, e := safeLstat(r.dir, p)
	if e != nil {
		return nil, e
	}
	if !info.IsDir() {
		return nil, ErrBadPath
	}
	name := path.Base(p)
	if p == "." || p == "" || name == "." || name == "/" {
		name = r.Label
		if name == "" {
			name = r.ID
		}
	}
	plan := &ZipPlan{Name: name, root: r}
	var visit func(from, zipName string, info os.FileInfo, depth int) error
	visit = func(from, zipName string, info os.FileInfo, depth int) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if depth > 32 || plan.Entries >= limit || len(zipName) > 1024 {
			return ErrTreeLimit
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		if !info.IsDir() {
			if info.Size() > maxBytes-plan.Bytes {
				return ErrTreeLimit
			}
			plan.Bytes += info.Size()
		}
		plan.Entries++
		plan.nodes = append(plan.nodes, zipNode{from, zipName, info})
		if !info.IsDir() {
			return nil
		}
		dir, e := safeOpen(r.dir, from, os.O_RDONLY|syscall.O_NONBLOCK)
		if e != nil {
			return e
		}
		defer dir.Close()
		if opened, e := dir.Stat(); e != nil || !sameIdentity(info, opened) {
			return ErrSourceChanged
		}
		for {
			entries, readErr := dir.ReadDir(64)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			for _, entry := range entries {
				child := entry.Name()
				if internalName(child) || (!r.ExposeHidden && strings.HasPrefix(child, ".")) {
					continue
				}
				childInfo, e := safeLstat(r.dir, path.Join(from, child))
				if e != nil {
					return e
				}
				if e = visit(path.Join(from, child), path.Join(zipName, child), childInfo, depth+1); e != nil {
					return e
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
		}
		return nil
	}
	if e = visit(p, name, info, 0); e != nil {
		return nil, e
	}
	return plan, nil
}

// Write streams the planned folder as a ZIP. Files are stored without
// compression: media rarely shrinks, and a NAS CPU is better spent elsewhere.
func (z *ZipPlan) Write(ctx context.Context, w io.Writer) error {
	zw := zip.NewWriter(w)
	buffer := make([]byte, 64<<10)
	for _, node := range z.nodes {
		if e := ctx.Err(); e != nil {
			return e
		}
		h := &zip.FileHeader{Name: node.name, Method: zip.Store, Modified: node.info.ModTime()}
		if node.info.IsDir() {
			h.Name += "/"
			h.SetMode(os.ModeDir | 0o755)
			if _, e := zw.CreateHeader(h); e != nil {
				return e
			}
			continue
		}
		h.SetMode(0o644)
		h.UncompressedSize64 = uint64(node.info.Size())
		out, e := zw.CreateHeader(h)
		if e != nil {
			return e
		}
		if e = z.copyFile(ctx, node, out, buffer); e != nil {
			return e
		}
	}
	return zw.Close()
}

func (z *ZipPlan) copyFile(ctx context.Context, node zipNode, out io.Writer, buffer []byte) error {
	f, e := safeOpen(z.root.dir, node.from, os.O_RDONLY|syscall.O_NONBLOCK)
	if e != nil {
		return e
	}
	defer f.Close()
	if opened, e := f.Stat(); e != nil || !sameSource(node.info, opened) {
		return ErrSourceChanged
	}
	n, e := io.CopyBuffer(out, uploadReader{ctx, io.LimitReader(f, node.info.Size())}, buffer)
	if e != nil {
		return e
	}
	if n != node.info.Size() {
		return ErrSourceChanged
	}
	return nil
}
