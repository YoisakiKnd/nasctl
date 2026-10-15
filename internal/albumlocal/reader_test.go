package albumlocal

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/store"
)

func TestLocalAlbumFreshInstallExistingIndexRangesAndReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Paths.StateDir = filepath.Join(dir, "state")
	cfg.Paths.CacheDir = filepath.Join(dir, "cache")
	cfg.Server.Token = "outer-console-credential"
	cfg.Collections = []config.Collection{{Code: "photos", Name: "Photos", Root: filepath.Join(dir, "photos"), Enabled: true, AllowSharedVolume: true}}
	if err := cfg.Finalize(); err != nil {
		t.Fatal(err)
	}
	reader := New(&cfg, nil)
	client, _ := album.NewHandler(reader)
	defer reader.Close()
	defer client.Close()
	ctx := context.Background()
	status, err := client.Status(ctx)
	if err != nil || status.IndexReady == nil || *status.IndexReady {
		t.Fatal(status, err)
	}
	if _, err := os.Stat(cfg.Paths.IndexFile); !os.IsNotExist(err) {
		t.Fatal("read created an index", err)
	}
	if err := os.MkdirAll(cfg.Collections[0].Root, 0750); err != nil {
		t.Fatal(err)
	}
	name := "<img src=x onerror=alert(1)>.jpg"
	if err := os.WriteFile(filepath.Join(cfg.Collections[0].Root, name), []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Paths.IndexFile, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	col, err := st.UpsertCollection(ctx, "photos", "Photos", cfg.Collections[0].Root, "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	up, err := st.UpsertMedia(ctx, store.MediaScan{CollectionID: col, CollectionCode: "photos", RelPath: name, BaseName: name, Ext: "jpg", Kind: "image", MIME: "image/jpeg", Size: 6, RunToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Pause(ctx, "fixture pause", "test"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	status, err = client.Status(ctx)
	if err != nil || status.IndexReady == nil || !*status.IndexReady || !status.Paused {
		t.Fatal(status, err)
	}
	if !reader.st.ReadOnly() {
		t.Fatal("reader opened writable store")
	}
	if _, err := reader.st.DB().Exec("DELETE FROM media"); err == nil {
		t.Fatal("SQLite permitted a write")
	}
	page, err := client.List(ctx, album.Query{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Name != name {
		t.Fatal(page, err)
	}
	id := strconv.FormatInt(up.ID, 10)
	resp, err := client.Stream(ctx, id, "raw", "GET", http.Header{"Range": {"bytes=1-3"}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 206 || string(data) != "bcd" || resp.ContentLength != 3 {
		t.Fatal(resp, string(data), err)
	}
	// An unread original must not serialize unrelated index requests.
	resp, err = client.Stream(ctx, id, "raw", "GET", nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.Status(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	resp.Body.Close()
	reader.Close()
	reopened := New(&cfg, nil)
	defer reopened.Close()
	other, _ := album.NewHandler(reopened)
	defer other.Close()
	if _, err := other.Details(ctx, id); err != nil {
		t.Fatal("reopen", err)
	}
	if _, err := client.Status(ctx); err == nil {
		t.Fatal("closed reader remained available")
	}
}

func TestCorruptLocalIndexReportsUnavailableWithoutLeakingPath(t *testing.T) {
	cfg := config.Default()
	cfg.Paths.IndexFile = filepath.Join(t.TempDir(), "private-index.sqlite")
	if err := os.WriteFile(cfg.Paths.IndexFile, []byte("invalid database"), 0600); err != nil {
		t.Fatal(err)
	}
	r := New(&cfg, nil)
	defer r.Close()
	c, _ := album.NewHandler(r)
	defer c.Close()
	_, err := c.Status(context.Background())
	if err == nil || strings.Contains(err.Error(), "private-index") {
		t.Fatal(err)
	}
	var problem *album.Error
	if !errors.As(err, &problem) || problem.Code != "album_index_unavailable" {
		t.Fatal("corrupt index mislabeled as busy", err)
	}
}
