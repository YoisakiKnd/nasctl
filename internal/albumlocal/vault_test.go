package albumlocal

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/cachepath"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/photovault"
	"github.com/YoisakiKnd/nasctl/internal/store"
)

func TestVaultMoveRemovesPlaintextCacheMetadataAndOldURLs(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	cfg := config.Default()
	cfg.Paths.StateDir = filepath.Join(base, "state")
	cfg.Paths.CacheDir = filepath.Join(base, "cache")
	root := filepath.Join(base, "photos")
	os.MkdirAll(root, 0700)
	os.MkdirAll(cfg.Paths.StateDir, 0700)
	cfg.Collections = []config.Collection{{Code: "photos", Name: "Photos", Root: root, Enabled: true, AllowSharedVolume: true}}
	cfg.Hub.Storage = []config.HubStorage{{ID: "photos", Path: root, Writable: true}}
	if e := cfg.Finalize(); e != nil {
		t.Fatal(e)
	}
	fm, e := files.New(cfg.Hub.Storage, files.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer fm.Close()
	control, e := NewControl(&cfg, fm, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer control.Close()
	client, _ := album.NewHandler(control.Reader)
	defer client.Close()
	name := "private-event.jpg"
	body := []byte("original private photo bytes")
	source := filepath.Join(root, name)
	if e = os.WriteFile(source, body, 0600); e != nil {
		t.Fatal(e)
	}
	info, _ := os.Stat(source)
	st, e := store.Open(cfg.Paths.IndexFile, store.Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	col, e := st.UpsertCollection(ctx, "photos", "Photos", root, "", true, false)
	if e != nil {
		t.Fatal(e)
	}
	up, e := st.UpsertMedia(ctx, store.MediaScan{CollectionID: col, CollectionCode: "photos", RelPath: name, BaseName: name, Ext: "jpg", Kind: "image", MIME: "image/jpeg", Size: int64(len(body)), MtimeNS: info.ModTime().UnixNano(), RunToken: 1})
	if e != nil {
		t.Fatal(e)
	}
	key := cachepath.Key("photos", name)
	cache, _ := cachepath.Path(cfg.Paths.CacheDir, key, cfg.Decode.ThumbMaxEdge, cfg.Cache.ShardDepth)
	os.MkdirAll(filepath.Dir(cache), 0700)
	os.WriteFile(cache, []byte("private thumbnail"), 0600)
	details, e := client.Details(ctx, strconv.FormatInt(up.ID, 10))
	if e != nil {
		t.Fatal(e)
	}
	vault, e := photovault.Open(cfg.Paths.StateDir)
	if e != nil {
		t.Fatal(e)
	}
	defer vault.Close()
	if e = vault.Create("generated vault test password"); e != nil {
		t.Fatal(e)
	}
	token, e := vault.Unlock("generated vault test password", "owner")
	if e != nil {
		t.Fatal(e)
	}
	// Simulate a cache cleanup failure after the verified original has been sealed.
	blocker := filepath.Join(filepath.Dir(cache), key+"_256.jpg")
	os.Mkdir(blocker, 0700)
	os.WriteFile(filepath.Join(blocker, "foreign"), []byte("keep"), 0600)
	id, e := control.MoveToVault(ctx, vault, token, "owner", up.ID, details)
	if e == nil || id == "" || !vault.Protected(up.ID) {
		t.Fatal("partial move incorrectly completed", e)
	}
	pending, _, e := vault.Details(ctx, token, "owner", id)
	if e != nil || !pending.Pending {
		t.Fatal("pending move not recoverable", e)
	}
	os.Remove(filepath.Join(blocker, "foreign"))
	os.Remove(blocker)
	retried, e := control.MoveToVault(ctx, vault, token, "owner", up.ID, details)
	if e != nil || retried != id {
		t.Fatal("retry failed or duplicated encrypted photo", retried, e)
	}

	if _, e = os.Stat(source); !os.IsNotExist(e) {
		t.Fatal("plaintext original remains", e)
	}
	if _, e = os.Stat(cache); !os.IsNotExist(e) {
		t.Fatal("plaintext thumbnail remains", e)
	}
	listing, e := client.List(ctx, album.Query{IncludeMissing: true})
	if e != nil || len(listing.Items) != 0 {
		t.Fatal("private row remains in include_missing", listing, e)
	}
	for _, part := range []string{"thumb", "raw", "download"} {
		response, e := client.Stream(ctx, strconv.FormatInt(up.ID, 10), part, "GET", nil)
		if e == nil {
			response.Body.Close()
			t.Fatal("old link accessible", part)
		}
	}
	if _, e = client.Details(ctx, strconv.FormatInt(up.ID, 10)); e == nil {
		t.Fatal("old details accessible")
	}
	var kind, rel, originalName, extra string
	st.DB().QueryRowContext(ctx, "SELECT kind,rel_path,base_name,meta_extra FROM media WHERE id=?", up.ID).Scan(&kind, &rel, &originalName, &extra)
	if kind != "private" || originalName == name || rel == name || extra != "" {
		t.Fatal("index metadata not redacted", kind, rel, originalName, extra)
	}
	reader, it, _, e := vault.OpenMedia(ctx, token, "owner", id, "raw")
	if e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(reader)
	reader.Close()
	if e != nil || !bytes.Equal(got, body) || it.Pending {
		t.Fatal("encrypted original not recoverable", e)
	}
	// An unrelated new public item cannot inherit a private item's old media ID.
	next, e := st.UpsertMedia(ctx, store.MediaScan{CollectionID: col, CollectionCode: "photos", RelPath: "next.jpg", BaseName: "next.jpg", Size: 2, Kind: "image", RunToken: 2})
	if e != nil || next.ID == up.ID {
		t.Fatal("private ID reused", next, e)
	}
}
