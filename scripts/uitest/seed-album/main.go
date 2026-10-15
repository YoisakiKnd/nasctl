// Generate a named disposable album service fixture. This is a development/CI
// utility; it is never linked into either NAS web service.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/cachepath"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/source"
	"github.com/YoisakiKnd/nasctl/internal/store"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func picture(index, width, height int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			v := color.RGBA{uint8(130 + x*60/width + index%20), uint8(170 + y*40/height), uint8(190 + index%30), 255}
			if y > height*3/5+(x*index%max(1, height/5)) {
				v = color.RGBA{uint8(40 + index%80), uint8(100 + y*60/height), uint8(75 + x*25/width), 255}
			}
			img.SetRGBA(x, y, v)
		}
	}
	var buffer bytes.Buffer
	must(jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 80}))
	return buffer.Bytes()
}
func main() {
	fixture := flag.Bool("fixture-only", false, "acknowledge disposable test data")
	base := flag.String("root", "", "empty or previously owned fixture directory")
	albumPort := flag.Int("album-port", 18095, "local album port")
	hubPort := flag.Int("hub-port", 18096, "local console port")
	flag.Parse()
	if !*fixture || *base == "" {
		panic("--fixture-only and --root are required")
	}
	abs, err := filepath.Abs(*base)
	must(err)
	marker := filepath.Join(abs, ".nasctl-album-fixture")
	if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
		data, err := os.ReadFile(marker)
		if err != nil || string(data) != "nasctl-album-fixture-v1\n" {
			panic("refusing to adopt a non-fixture directory")
		}
	}
	for _, dir := range []string{"library", "album-state", "cache", "hub-state", "hub-cache"} {
		must(os.MkdirAll(filepath.Join(abs, dir), 0750))
	}
	must(os.MkdirAll(filepath.Join(abs, "library", "Additional"), 0750))
	must(os.WriteFile(marker, []byte("nasctl-album-fixture-v1\n"), 0600))
	albumDoc := fmt.Sprintf("[server]\nbind=\"127.0.0.1:%d\"\ntoken=\"nasctl-disposable-album-token\"\n[paths]\nstate_dir=%q\ncache_dir=%q\n[[collections]]\ncode=\"photos\"\nname=\"Fixture Photos\"\nroot=%q\nenabled=true\nallow_shared_volume=true\n", *albumPort, filepath.Join(abs, "album-state"), filepath.Join(abs, "cache"), filepath.Join(abs, "library"))
	must(os.WriteFile(filepath.Join(abs, "album.toml"), []byte(albumDoc), 0600))
	cfg, err := config.Parse(albumDoc)
	must(err)
	hubDoc := fmt.Sprintf("[server]\nbind=\"127.0.0.1:%d\"\n[paths]\nstate_dir=%q\ncache_dir=%q\n[hub.album]\napi_base=\"http://127.0.0.1:%d\"\ntoken=\"nasctl-disposable-album-token\"\n", *hubPort, filepath.Join(abs, "hub-state"), filepath.Join(abs, "hub-cache"), *albumPort)
	must(os.WriteFile(filepath.Join(abs, "hub.toml"), []byte(hubDoc), 0600))
	// This uses the same actual index/cache, with no album HTTP service. The
	// processing component is discovered beside the nasctl binary when needed.
	nativeDoc := fmt.Sprintf("[server]\nbind=\"127.0.0.1:%d\"\n[paths]\nstate_dir=%q\ncache_dir=%q\n[[collections]]\ncode=\"photos\"\nname=\"Fixture Photos\"\nroot=%q\nenabled=true\nallow_shared_volume=true\n[[hub.storage]]\nid=\"photos\"\nlabel=\"Fixture Photos\"\npath=%q\n[hub.album]\nmode=\"local\"\n", *hubPort, filepath.Join(abs, "album-state"), filepath.Join(abs, "cache"), filepath.Join(abs, "library"), filepath.Join(abs, "library"))
	must(os.WriteFile(filepath.Join(abs, "native.toml"), []byte(nativeDoc), 0600))
	st, err := store.Open(cfg.Paths.IndexFile, store.Options{})
	must(err)
	defer st.Close()
	ctx := context.Background()
	id, err := st.UpsertCollection(ctx, "photos", "Fixture Photos", cfg.Collections[0].Root, "", true, false)
	must(err)
	for i := 0; i < 57; i++ {
		name := fmt.Sprintf("Photo-%03d.jpg", i)
		kind, mime, ext := "image", "image/jpeg", "jpg"
		body := picture(i, 640, 420)
		if i == 0 {
			name = "<img src=x onerror=alert(1)>.jpg"
		}
		if i == 55 {
			name = "Video-fixture.mp4"
			kind = "video"
			mime = "video/mp4"
			ext = "mp4"
			body = []byte("Disposable HTTP/DOM video fixture; codec test uses separate media sample")
		}
		if i == 56 {
			name = "Missing-photo.jpg"
		}
		p := filepath.Join(cfg.Collections[0].Root, name)
		stamp := time.Date(2026, 8, 1+i, 12, 0, 0, 0, time.UTC).Unix()
		if i != 56 {
			must(os.WriteFile(p, body, 0640))
			must(os.Chtimes(p, time.Unix(stamp, 0), time.Unix(stamp, 0)))
		}
		token := int64(1)
		if i == 56 {
			token = 0
		}
		up, err := st.UpsertMedia(ctx, store.MediaScan{CollectionID: id, CollectionCode: "photos", RelPath: name, BaseName: name, Ext: ext, Size: int64(len(body)), MtimeNS: stamp * 1e9, Kind: kind, MIME: mime, At: stamp, RunToken: token})
		must(err)
		takenSrc := store.TakenSrcExif
		if i%10 == 0 {
			takenSrc = store.TakenSrcFiletime
		}
		must(st.SetProbe(ctx, store.ProbeUpdate{ID: up.ID, State: store.ProbeOK, Kind: kind, MIME: mime, Format: ext, Width: 640, Height: 420, Pixels: 640 * 420, TakenAt: &stamp, TakenSrc: takenSrc, Make: "Fixture", Model: "Test Camera", Orientation: 1, ThumbState: store.ThumbPending, At: stamp, MetaExtra: `{"tags":[{"k":"Fixture","v":"Generated sample, not a user photo"}]}`}))
		if i%5 != 0 {
			thumb := picture(i, cfg.Decode.ThumbMaxEdge, cfg.Decode.ThumbMaxEdge*420/640)
			p, err := cachepath.Path(cfg.Paths.CacheDir, cachepath.Key("photos", name), cfg.Decode.ThumbMaxEdge, cfg.Cache.ShardDepth)
			must(err)
			must(os.MkdirAll(filepath.Dir(p), 0750))
			must(os.WriteFile(p, thumb, 0640))
			must(st.SetThumb(ctx, store.ThumbUpdate{ID: up.ID, State: store.ThumbOK, Bytes: int64(len(thumb)), At: stamp}))
		}
	}
	_, err = st.MarkMissing(ctx, id, 1)
	must(err)
	must(st.RefreshMediaCount(ctx, id))
	must(st.SetLastScan(ctx, id, time.Now().Unix()))
	health := source.Verify(cfg.Collections[0], cfg.Paths.StateDir, "")
	if !health.OK {
		panic(health.Fatal)
	}
	must(st.RecordMountCheck(ctx, id, true, "Disposable local fixture", health.FSID, health.Device))
	fmt.Println("Generated 57 indexed test records, including one missing original and cached/placeholder thumbnails.")
}
