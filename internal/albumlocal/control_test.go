package albumlocal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/settings"
	"github.com/YoisakiKnd/nasctl/internal/settingslocal"
	"github.com/YoisakiKnd/nasctl/internal/store"
)

func controlRig(t *testing.T, script string) (*Control, *config.Config, *files.Manager) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Paths.StateDir = filepath.Join(dir, "state")
	cfg.Paths.CacheDir = filepath.Join(dir, "cache")
	root := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(root, "Photos"), 0750); err != nil {
		t.Fatal(err)
	}
	cfg.Server.Token = "must-not-enter-worker-snapshot"
	// This fixture is a shell stub for argument/snapshot and cancellation
	// checks, not an album decoder. Required kernel isolation is verified with
	// the real binaries by scripts/verify-native-album-linux.sh.
	cfg.Guard.RequireCgroupIsolation = false
	cfg.Hub.Storage = []config.HubStorage{{ID: "data", Path: root}}
	cfg.Hub.Agents = []config.Agent{{Name: "helper", Token: "agent-must-not-enter-snapshot"}}
	if err := cfg.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.Paths.StateDir, 0750); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "nasctl")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Hub.Album.WorkerBinary = binary
	fm, err := files.New(cfg.Hub.Storage, files.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fm.Close() })
	c, err := NewControl(&cfg, fm, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Reader.Close() })
	return c, &cfg, fm
}

func TestManagedDirectoryRemovalHidesRowsAndOriginalReadsRemainRootAnchored(t *testing.T) {
	c, cfg, _ := controlRig(t, "exit 0\n")
	ctx := context.Background()
	d := album.Directory{Code: "photos", Name: "Photos", Root: "data", Path: "Photos", Enabled: true, AllowSharedVolume: true}
	if err := c.Configure(ctx, d); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Paths.IndexFile, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := filepath.Join(cfg.Hub.Storage[0].Path, "Photos")
	col, err := st.UpsertCollection(ctx, "photos", "Photos", root, "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	up, err := st.UpsertMedia(ctx, store.MediaScan{CollectionID: col, CollectionCode: "photos", RelPath: "photo.jpg", BaseName: "photo.jpg", Ext: "jpg", Kind: "image", Size: 6, RunToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "photo.jpg"), []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	client, _ := album.NewHandler(c.Reader)
	defer client.Close()
	page, err := client.List(ctx, album.Query{})
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	id := strconv.FormatInt(up.ID, 10)
	resp, err := client.Stream(ctx, id, "raw", "GET", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Saving a new configuration must not wait for an unread original body.
	d.Name = "Renamed"
	if err := c.Configure(ctx, d); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(data) != "abcdef" {
		t.Fatal(string(data), err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "photo.jpg"), []byte("SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	if response, err := client.Stream(ctx, id, "raw", "GET", nil); err == nil {
		response.Body.Close()
		t.Fatal("original escaped the configured NAS root")
	}
	if err := c.RemoveDirectory(ctx, "photos"); err != nil {
		t.Fatal(err)
	}
	page, err = client.List(ctx, album.Query{})
	if err != nil || len(page.Items) != 0 || page.TotalEstimate != 0 {
		t.Fatal("removed directory still listed", page, err)
	}
	if _, err := client.Details(ctx, id); err == nil {
		t.Fatal("removed metadata readable by id")
	}
	if response, err := client.Stream(ctx, id, "thumb", "GET", nil); err == nil {
		response.Body.Close()
		t.Fatal("removed thumbnail readable by id")
	}
	if _, err := st.GetMedia(ctx, up.ID); err != nil {
		t.Fatal("removing configuration deleted indexed row", err)
	}
}

func TestDirectoryConfigurationIsBoundedDurableAndKeepsOriginals(t *testing.T) {
	c, cfg, fm := controlRig(t, "exit 0\n")
	ctx := context.Background()
	d := album.Directory{Code: "photos", Name: "Family", Root: "data", Path: "Photos", Enabled: true, AllowSharedVolume: true}
	if err := c.Configure(ctx, d); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewControl(cfg, fm, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Reader.Close()
	state := reopened.Configuration()
	if len(state.Directories) != 1 || !state.WorkerAvailable || !state.Directories[0].Managed {
		t.Fatal(state)
	}
	info, err := os.Stat(c.catalog)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	if _, err := os.Stat(cfg.Paths.IndexFile); !os.IsNotExist(err) {
		t.Fatal("configuration created index", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cfg.Hub.Storage[0].Path, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []album.Directory{
		{Code: "escape", Name: "x", Root: "data", Path: "../../outside"},
		{Code: "escape", Name: "x", Root: "data", Path: "escape"},
		{Code: "photos", Name: "x", Root: "absent", Path: ""},
		{Code: "UPPERCASE", Name: "x", Root: "data", Path: "Photos"},
		{Code: "photos", Name: strings.Repeat("x", 129), Root: "data", Path: "Photos"},
	} {
		if err := c.Configure(ctx, bad); err == nil {
			t.Fatal("unsafe configuration accepted", bad)
		}
	}
	if len(c.Configuration().Directories) != 1 {
		t.Fatal("failed save mutated catalog")
	}
	if err := c.RemoveDirectory(ctx, "photos"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Hub.Storage[0].Path, "Photos")); err != nil {
		t.Fatal("configuration removal changed original", err)
	}
	if len(c.Configuration().Directories) != 0 {
		t.Fatal("remove not applied")
	}
}

func TestWorkerUsesPrivateCredentialFreeSnapshotAndBoundedOutput(t *testing.T) {
	c, cfg, _ := controlRig(t, `[ "$1" = "album" ] || exit 4
shift
op="$1"
shift
[ "$1" = "--config" ] || exit 5
conf="$2"
[ -f "$conf" ] || exit 6
if /usr/bin/grep -q 'must-not-enter' "$conf"; then exit 9; fi
case "$op" in
 scan) [ "$3" = "--collection" ] && [ "$4" = "photos" ] && [ "$5" = "--limit" ] && [ "$6" = "3" ] || exit 7;;
 thumbs) [ "$7" = "--max-runtime" ] && [ "$8" = "2s" ] || exit 8;;
 pause|resume) :;;
 *) exit 10;;
esac
i=0
while [ "$i" -lt 500 ]; do echo 'generated bounded worker report'; i=$((i+1)); done
`)
	if err := c.Configure(context.Background(), album.Directory{Code: "photos", Name: "Photos", Root: "data", Path: "Photos", Enabled: true, AllowSharedVolume: true}); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"scan", "thumbs", "pause", "resume"} {
		result, err := c.Run(context.Background(), album.Work{Operation: "album." + op, Collection: "photos", Limit: 3, MaxRuntime: "2s"}, nil)
		if err != nil || !result.Completed || len(result.Detail) > 4096 {
			t.Fatal(op, result, err)
		}
	}
	remaining, err := filepath.Glob(filepath.Join(cfg.Paths.StateDir, ".album-worker-*.toml"))
	if err != nil || len(remaining) != 0 {
		t.Fatal("worker snapshot not cleaned", remaining, err)
	}
	if c.Configuration().Busy {
		t.Fatal("busy after worker exit")
	}
}

func TestRequiredIsolationRefusesBeforeStartingHelper(t *testing.T) {
	c, cfg, _ := controlRig(t, "touch \"$0.ran\"\nexit 0\n")
	c.base.Guard.RequireCgroupIsolation = true
	c.launcher = &albumLauncher{unavailable: errors.New("generated missing delegation")}
	if e := c.Configure(context.Background(), album.Directory{Code: "photos", Name: "Photos", Root: "data", Path: "Photos", Enabled: true, AllowSharedVolume: true}); e != nil {
		t.Fatal(e)
	}
	out, e := c.Run(context.Background(), album.Work{Operation: "album.thumbs", Collection: "photos", Limit: 1, MaxRuntime: "2s"}, nil)
	if e == nil || out.Completed || out.StopReason != "isolation_unavailable" {
		t.Fatal("required isolation bypassed", out, e)
	}
	if _, e = os.Stat(cfg.Hub.Album.WorkerBinary + ".ran"); !os.IsNotExist(e) {
		t.Fatal("helper started before isolation", e)
	}
	remaining, e := filepath.Glob(filepath.Join(cfg.Paths.StateDir, ".album-worker-*.toml"))
	if e != nil || len(remaining) != 0 {
		t.Fatal("refusal leaked private snapshot", remaining, e)
	}
	if c.Configuration().Busy {
		t.Fatal("refusal left processing busy")
	}
}

func TestPendingStorageRemovalCannotAcquireNewAlbumReferences(t *testing.T) {
	c, cfg, _ := controlRig(t, "exit 0\n")
	ctx := context.Background()
	cfg.Hub.Setup = true
	source := filepath.Join(filepath.Dir(cfg.Paths.StateDir), "config.toml")
	var encoded bytes.Buffer
	if e := toml.NewEncoder(&encoded).Encode(cfg); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(source, encoded.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	cfg.Source = source
	c.base.Source = source
	control := settingslocal.New(cfg)
	snap, e := control.Get(ctx)
	if e != nil {
		t.Fatal(e)
	}
	req := settings.Request{Revision: snap.Revision, Settings: snap.Saved}
	req.Settings.Storage = []config.HubStorage{}
	if _, e = control.Save(ctx, req); e != nil {
		t.Fatal("initial pending removal", e)
	}
	d := album.Directory{Code: "photos", Name: "Photos", Root: "data", Path: "Photos", Enabled: true, AllowSharedVolume: true}
	if e = c.Configure(ctx, d); e == nil {
		t.Fatal("new directory referenced a removed pending root")
	}
	if len(c.Configuration().Directories) != 0 {
		t.Fatal("failed pending validation changed live directories")
	}
	snap, e = control.Get(ctx)
	if e != nil {
		t.Fatal(e)
	}
	req = settings.Request{Revision: snap.Revision, Settings: snap.Saved}
	req.Settings.Storage = cfg.Hub.Storage
	if _, e = control.Save(ctx, req); e != nil {
		t.Fatal(e)
	}
	if e = c.Configure(ctx, d); e != nil {
		t.Fatal("restored pending root blocked directory", e)
	}
	snap, e = control.Get(ctx)
	if e != nil {
		t.Fatal(e)
	}
	req = settings.Request{Revision: snap.Revision, Settings: snap.Saved}
	req.Settings.Storage = []config.HubStorage{}
	if _, e = control.Save(ctx, req); e == nil {
		t.Fatal("pending settings removed referenced directory root")
	}
	if e = c.RemoveDirectory(ctx, "photos"); e != nil {
		t.Fatal(e)
	}
	if _, e = control.Save(ctx, req); e != nil {
		t.Fatal("removing last reference should allow the reviewed pending removal", e)
	}
}

func TestWorkerCancellationAndBusyAdmission(t *testing.T) {
	c, _, _ := controlRig(t, "trap 'exit 0' INT TERM\nwhile :; do sleep 0.05; done\n")
	if err := c.Configure(context.Background(), album.Directory{Code: "photos", Name: "Photos", Root: "data", Path: "Photos", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := c.Run(ctx, album.Work{Operation: "album.scan", Collection: "photos", Limit: 1, MaxRuntime: "10s"}, func(p album.WorkProgress) {
			select {
			case started <- struct{}{}:
			default:
			}
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start")
	}
	if err := c.RemoveDirectory(context.Background(), "photos"); err == nil {
		t.Fatal("directory changed during task")
	}
	if _, err := c.Run(context.Background(), album.Work{Operation: "album.pause"}, nil); err == nil {
		t.Fatal("concurrent worker accepted")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop gracefully")
	}
	if c.Configuration().Busy {
		t.Fatal("cancel did not clear busy")
	}
}

func TestPrivateDirectoryIsReadOnlyThroughThePrivateScope(t *testing.T) {
	c, cfg, _ := controlRig(t, "exit 0\n")
	ctx := context.Background()
	for _, dir := range []string{"Photos/Trip", "Secret"} {
		if err := os.MkdirAll(filepath.Join(cfg.Hub.Storage[0].Path, dir), 0750); err != nil {
			t.Fatal(err)
		}
	}
	public := album.Directory{Code: "photos", Name: "Photos", Root: "data", Path: "Photos", Enabled: true, AllowSharedVolume: true}
	if err := c.Configure(ctx, public); err != nil {
		t.Fatal(err)
	}
	// A private directory inside an ordinary one would stay visible through
	// the ordinary one, so the overlap is refused in both directions.
	nested := album.Directory{Code: "trip", Name: "Trip", Root: "data", Path: "Photos/Trip", Enabled: true, AllowSharedVolume: true, Private: true}
	if err := c.Configure(ctx, nested); err == nil || !strings.HasPrefix(err.Error(), "私密目录") {
		t.Fatal("nested private directory accepted", err)
	}
	private := album.Directory{Code: "secret", Name: "Secret", Root: "data", Path: "Secret", Enabled: true, AllowSharedVolume: true, Private: true}
	if err := c.Configure(ctx, private); err != nil {
		t.Fatal(err)
	}
	if got := c.PrivateFolders(); len(got) != 1 || got[0] != (album.Folder{Root: "data", Path: "Secret"}) {
		t.Fatal("private folders", got)
	}
	st, err := store.Open(cfg.Paths.IndexFile, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := filepath.Join(cfg.Hub.Storage[0].Path, "Secret")
	col, err := st.UpsertCollection(ctx, "secret", "Secret", root, "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	up, err := st.UpsertMedia(ctx, store.MediaScan{CollectionID: col, CollectionCode: "secret", RelPath: "me.jpg", BaseName: "me.jpg", Ext: "jpg", Kind: "image", Size: 6, RunToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "me.jpg"), []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	client, _ := album.NewHandler(c.Reader)
	defer client.Close()
	id := strconv.FormatInt(up.ID, 10)
	if page, err := client.List(ctx, album.Query{}); err != nil || len(page.Items) != 0 || page.TotalEstimate != 0 {
		t.Fatal("private photo in the ordinary album", page, err)
	}
	if _, err := client.Details(ctx, id); err == nil {
		t.Fatal("private photo readable by id")
	}
	if response, err := client.Stream(ctx, id, "raw", "GET", nil); err == nil {
		response.Body.Close()
		t.Fatal("private original readable through the ordinary scope")
	}
	if _, err := client.List(ctx, album.Query{Collection: "secret"}); err == nil {
		t.Fatal("private directory listable by name")
	}
	scoped := client.Private()
	page, err := scoped.List(ctx, album.Query{})
	if err != nil || len(page.Items) != 1 || page.TotalEstimate != 1 || !strings.HasPrefix(page.Items[0].ThumbURL, "/api/v1/album/private/media/") {
		t.Fatal("private scope listing", page, err)
	}
	response, err := scoped.Stream(ctx, id, "raw", "GET", nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(data) != "abcdef" {
		t.Fatal("private original", string(data))
	}
}
