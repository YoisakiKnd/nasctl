// Package cachepath computes and validates thumbnail cache paths.
//
// Cache files are individual files in a sharded directory tree rather than
// blobs in the database. That choice is deliberate: the cache can be measured
// with du, pruned with rm, copied independently of the index, and its growth
// can be capped. A blob column would have made all four of those operations
// require database surgery, on a machine where the database is the thing that
// must never be disturbed.
//
// The file name is derived from the collection code and the relative path, not
// from the row id, so a thumbnail stays valid when the index is rebuilt from
// scratch and does not need to be regenerated.
package cachepath

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Key identifies one media file stably across index rebuilds.
func Key(collectionCode, relPath string) string {
	h := sha256.Sum256([]byte(collectionCode + "\x00" + relPath))
	return hex.EncodeToString(h[:12])
}

// Name returns the cache file name for a key and size, e.g.
// "ab12cd34ef56_512.jpg".
func Name(key string, edge int) string {
	return fmt.Sprintf("%s_%d.jpg", key, edge)
}

// RelPath returns the sharded relative path, e.g. "ab/cd/ab12cd34ef56_512.jpg"
// for depth 2.
//
// Sharding keeps any single directory small. A flat cache directory with tens
// of thousands of entries makes readdir slow on the target's filesystem and
// makes a partial cleanup awkward; a sharded tree lets cleanup work one branch
// at a time.
func RelPath(key string, edge, depth int) string {
	if depth < 1 {
		depth = 3
	}
	if depth > 4 {
		depth = 4
	}
	parts := make([]string, 0, depth+1)
	for i := 0; i < depth; i++ {
		parts = append(parts, key[i*2:i*2+2])
	}
	parts = append(parts, Name(key, edge))
	return filepath.Join(parts...)
}

// ShardDir is the deepest shard directory, used for batching cleanup.
func ShardDir(key string, depth int) string {
	if depth < 1 {
		depth = 3
	}
	parts := make([]string, 0, depth)
	for i := 0; i < depth; i++ {
		parts = append(parts, key[i*2:i*2+2])
	}
	return filepath.Join(parts...)
}

// Path joins a cache root with the sharded relative path.
func Path(root, key string, edge, depth int) (string, error) {
	if root == "" {
		return "", errors.New("cachepath: empty cache root")
	}
	if len(key) < 2*depth {
		return "", fmt.Errorf("cachepath: key %q is too short for depth %d", key, depth)
	}
	rel := RelPath(key, edge, depth)
	abs := filepath.Join(root, rel)
	if !strings.HasPrefix(abs, filepath.Clean(root)+string(filepath.Separator)) {
		return "", fmt.Errorf("cachepath: %q escapes %q", rel, root)
	}
	return abs, nil
}

// ParseSize extracts the edge length from a cache file name, or 0 if the name
// does not follow the convention.
func ParseSize(name string) int {
	base := strings.TrimSuffix(name, ".jpg")
	i := strings.LastIndexByte(base, '_')
	if i < 0 || i == len(base)-1 {
		return 0
	}
	n, err := strconv.Atoi(base[i+1:])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// IsCacheArtifact reports whether a name looks like a thumbnail this program
// wrote. Cleanup refuses to touch anything else, so a misconfigured cache root
// cannot turn into a delete-everything bug.
func IsCacheArtifact(name string) bool {
	if !strings.HasSuffix(name, ".jpg") {
		return false
	}
	base := strings.TrimSuffix(name, ".jpg")
	i := strings.LastIndexByte(base, '_')
	if i <= 0 {
		return false
	}
	key := base[:i]
	if len(key) < 8 {
		return false
	}
	for _, r := range key {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return ParseSize(name) > 0
}
