package cachepath

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestKeySeparatesItsInputs pins the delimiter. Concatenation without one makes
// Key("ab","c") and Key("a","bc") the same string, which would mean two
// different photos sharing one cache entry - the wrong picture shown, silently.
func TestKeySeparatesItsInputs(t *testing.T) {
	cases := [][2][2]string{
		{{"ab", "c"}, {"a", "bc"}},
		{{"a", "b"}, {"a", "b/"}},
		{{"a", "b"}, {"a/", "b"}},
		{{"a", "b/c"}, {"a/b", "c"}},
	}
	for _, pair := range cases {
		left := Key(pair[0][0], pair[0][1])
		right := Key(pair[1][0], pair[1][1])
		if left == right {
			t.Errorf("Key(%q,%q) == Key(%q,%q) == %q; the two inputs must not collide",
				pair[0][0], pair[0][1], pair[1][0], pair[1][1], left)
		}
	}
}

func TestKeyIsStableAndFixedWidth(t *testing.T) {
	first := Key("smoke", "photos/IMG_0001.jpg")
	for i := 0; i < 5; i++ {
		if got := Key("smoke", "photos/IMG_0001.jpg"); got != first {
			t.Fatalf("Key is not deterministic: %q then %q", first, got)
		}
	}
	if len(first) != 24 {
		t.Errorf("Key length is %d, want 24 hex characters (12 bytes)", len(first))
	}
	for _, r := range first {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("Key %q contains %q, which is not lowercase hex", first, r)
		}
	}
	if Key("smoke", "a") == Key("smoke", "b") {
		t.Error("Key ignores the relative path")
	}
	if Key("a", "x") == Key("b", "x") {
		t.Error("Key ignores the collection code")
	}
}

func TestRelPathIsShardedAndStayInside(t *testing.T) {
	key := Key("smoke", "photos/2019/paris/IMG_0007.jpg")
	for _, depth := range []int{1, 2, 3, 4} {
		rel := RelPath(key, 512, depth)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != depth+1 {
			t.Errorf("depth %d produced %q (%d components), want %d", depth, rel, len(parts), depth+1)
		}
		if parts[len(parts)-1] != Name(key, 512) {
			t.Errorf("depth %d: the last component is %q, want %q", depth, parts[len(parts)-1], Name(key, 512))
		}
		// Each shard component is two characters of the key, in order.
		for i := 0; i < depth; i++ {
			if parts[i] != key[i*2:i*2+2] {
				t.Errorf("depth %d: shard %d is %q, want %q", depth, i, parts[i], key[i*2:i*2+2])
			}
		}
	}
}

func TestShardDirIsPrefixOfRelPath(t *testing.T) {
	key := Key("smoke", "a/b.jpg")
	dir := ShardDir(key, 3)
	rel := RelPath(key, 512, 3)
	if !strings.HasPrefix(filepath.ToSlash(rel), filepath.ToSlash(dir)+"/") {
		t.Errorf("RelPath %q does not start with ShardDir %q", rel, dir)
	}
}

// TestPathRefusesKeysTooShortForTheDepth matters because Path is what turns a
// database row into a file path. A key shorter than the shard depth would be
// sliced out of range.
func TestPathRefusesKeysTooShortForTheDepth(t *testing.T) {
	root := t.TempDir()
	if _, err := Path(root, "ab", 512, 3); err == nil {
		t.Error("Path accepted a key shorter than the shard depth")
	}
	if _, err := Path("", Key("a", "b"), 512, 3); err == nil {
		t.Error("Path accepted an empty cache root")
	}
	good, err := Path(root, Key("a", "b"), 512, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(good, filepath.Clean(root)+string(filepath.Separator)) {
		t.Errorf("Path returned %q, which is not inside %q", good, root)
	}
}

func TestPathIsDeterministicAndDistinctPerKey(t *testing.T) {
	root := t.TempDir()
	seen := map[string]string{}
	for i := 0; i < 500; i++ {
		rel := "photos/file_" + string(rune('a'+i%26)) + string(rune('0'+i%10)) + string(rune('0'+i/10)) + ".jpg"
		key := Key("smoke", rel)
		p, err := Path(root, key, 512, 3)
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[p]; dup {
			t.Fatalf("cache path collision: %q is used by both %q and %q", p, prev, rel)
		}
		seen[p] = rel
		if p2, _ := Path(root, key, 512, 3); p2 != p {
			t.Fatalf("Path is not deterministic for %q", rel)
		}
	}
}

// TestEdgeIsPartOfTheName is what lets an operator change thumb_max_edge
// without silently serving stale thumbnails at the old size.
func TestEdgeIsPartOfTheName(t *testing.T) {
	key := Key("smoke", "a.jpg")
	if Name(key, 256) == Name(key, 512) {
		t.Error("the edge length is not part of the cache file name")
	}
	if RelPath(key, 256, 3) == RelPath(key, 512, 3) {
		t.Error("the edge length is not part of the sharded relative path")
	}
}

func TestParseSizeRoundTrips(t *testing.T) {
	key := Key("smoke", "a.jpg")
	for _, edge := range []int{64, 128, 256, 512, 1024} {
		name := Name(key, edge)
		if got := ParseSize(name); got != edge {
			t.Errorf("ParseSize(%q) = %d, want %d", name, got, edge)
		}
	}
	// ParseSize only reports the size suffix; it makes no claim about the key,
	// so "_512.jpg" legitimately parses. IsCacheArtifact is the function that
	// requires a plausible key.
	for _, bad := range []string{"plain.jpg", "no_extension", "abc_.jpg", "abc_0.jpg",
		"abc_-4.jpg", "abc_512.png", "", ".jpg", "abc_512x.jpg"} {
		if got := ParseSize(bad); got != 0 {
			t.Errorf("ParseSize(%q) = %d, want 0", bad, got)
		}
	}
}

// TestIsCacheArtifactOnlyAcceptsOurOwnNames is the guard that stops cache-gc
// from ever deleting something that is not a thumbnail. If it were too
// permissive, a misconfigured cache root pointed at a photo directory would
// turn a cleanup into data loss.
func TestIsCacheArtifactOnlyAcceptsOurOwnNames(t *testing.T) {
	key := Key("smoke", "photos/2019/IMG_0007.jpg")
	ours := []string{Name(key, 512), Name(key, 256), Name(Key("x", "y"), 1024)}
	for _, n := range ours {
		if !IsCacheArtifact(n) {
			t.Errorf("IsCacheArtifact(%q) = false; a real cache file must be recognised", n)
		}
	}

	notOurs := []string{
		"IMG_0007.jpg",         // a real photograph
		"notes.txt",            // not a thumbnail at all
		"",                     // empty
		".jpg",                 // nothing before the extension
		"_512.jpg",             // no key
		"abcdefgh.jpg",         // a plausible key but no size
		"zzzzzzzzzzzz_512.jpg", // not hex
		"ABCDEF123456_512.jpg", // uppercase: we always write lowercase
		"ab12cd34_512.png",     // wrong extension
		"ab12cd34_512x.jpg",    // size is not a number
		"../../etc/passwd",     // a traversal attempt
		"short_a_512.jpg.bak",  // a backup copy of one of ours
		"ab12cd34_512.jpg.tmp", // a temp file mid-write
	}
	for _, n := range notOurs {
		if IsCacheArtifact(n) {
			t.Errorf("IsCacheArtifact(%q) = true; cache-gc could then delete a file that "+
				"albumctl did not create", n)
		}
	}
}

// TestIsCacheArtifactRejectsAShortKey keeps the key-length floor honest: a short
// key is more likely to be somebody else's naming convention than ours.
func TestIsCacheArtifactRejectsAShortKey(t *testing.T) {
	// Keys are lowercase hex, so the short examples have to be hex too:
	// "abcdefgh" would be rejected for containing g and h, which proves nothing
	// about the length rule.
	for _, n := range []string{"a1_512.jpg", "a1b2_512.jpg", "a1b2c3_512.jpg", "a1b2c3d_512.jpg"} {
		if IsCacheArtifact(n) {
			t.Errorf("IsCacheArtifact(%q) = true; keys shorter than 8 characters are not ours", n)
		}
	}
	if !IsCacheArtifact("a1b2c3d4_512.jpg") {
		t.Error("IsCacheArtifact rejected an 8 character hex key, the shortest we produce")
	}
}
