package files

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"syscall"
)

var ErrTreeLimit = errors.New("files: tree entry/depth/byte budget reached")
var ErrSourceChanged = errors.New("files: source changed while processing")

type treeNode struct {
	path string
	info os.FileInfo
}

// Plan before destructive work. A symlink is a leaf, including one pointing
// outside the root. Paths remain relative to a capability-owned directory.
func planTree(ctx context.Context, root *os.Root, p string, limit int) ([]treeNode, error) {
	return planTreeDepth(ctx, root, p, limit, 32)
}
func planTreeDepth(ctx context.Context, root *os.Root, p string, limit, maxDepth int) ([]treeNode, error) {
	nodes := []treeNode{}
	var visit func(string, int) error
	visit = func(rel string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > maxDepth || len(nodes) >= limit {
			return ErrTreeLimit
		}
		info, err := root.Lstat(rel)
		if err != nil {
			return mapWriteErr(err)
		}
		nodes = append(nodes, treeNode{rel, info})
		if !info.IsDir() {
			return nil
		}
		dir, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return mapWriteErr(err)
		}
		defer dir.Close()
		opened, err := dir.Stat()
		if err != nil || !os.SameFile(info, opened) {
			return ErrSourceChanged
		}
		for {
			entries, readErr := dir.ReadDir(64)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return mapWriteErr(readErr)
			}
			for _, entry := range entries {
				if err := visit(path.Join(rel, entry.Name()), depth+1); err != nil {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
		}
		return nil
	}
	err := visit(p, 0)
	return nodes, err
}

// Only identity is checked during removal: deleting a child changes directory
// timestamps. External replacement is refused instead of deleting a new entry.
func removePlanned(ctx context.Context, root *os.Root, nodes []treeNode, progress func(int64)) error {
	for i := len(nodes) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		node := nodes[i]
		current, err := root.Lstat(node.path)
		if err != nil {
			return mapWriteErr(err)
		}
		if !os.SameFile(node.info, current) || node.info.Mode().Type() != current.Mode().Type() {
			return ErrSourceChanged
		}
		if err = root.Remove(node.path); err != nil {
			return mapWriteErr(err)
		}
		if progress != nil {
			progress(int64(len(nodes) - i))
		}
	}
	return nil
}

func removeBounded(ctx context.Context, root *os.Root, p string, limit int) error {
	if _, err := root.Lstat(p); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	nodes, err := planTree(ctx, root, p, limit)
	if err != nil {
		return err
	}
	return removePlanned(ctx, root, nodes, nil)
}
