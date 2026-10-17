package thumbs

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/synth"
)

// writePhoto writes a w x h JPEG, optionally with an EXIF orientation tag, and
// returns its path and stat.
func writePhoto(t *testing.T, dir string, w, h, orientation int) (string, os.FileInfo) {
	t.Helper()
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(40)
			if x < w/2 {
				v = 220 // left half bright: shows which way the picture turned
			}
			img.Y[y*img.YStride+x] = v
		}
	}
	for i := range img.Cb {
		img.Cb[i], img.Cr[i] = 128, 128
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	if orientation > 1 {
		// APP1 Exif with a one-entry IFD0: Orientation (0x0112), SHORT.
		tiff := []byte("MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00")
		binary.BigEndian.PutUint16(tiff[18:], uint16(orientation))
		payload := append([]byte("Exif\x00\x00"), tiff...)
		seg := []byte{0xFF, 0xE1, 0, 0}
		binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
		data = append(append(append([]byte{}, data[:2]...), append(seg, payload...)...), data[2:]...)
	}
	path := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	return path, fi
}

func TestLargeJPEGIsDecodedAtReducedSize(t *testing.T) {
	dir := t.TempDir()
	// 24 MP: over the 12 MP limit at full size, a quarter decode is 1.5 MP.
	src, fi := writePhoto(t, dir, 6000, 4000, 1)
	opt := testOptions()
	opt.MaxDecodeBytes = 96 << 20
	dst := filepath.Join(dir, "thumb.jpg")
	got := Generate(src, dst, fi.Size(), fi.ModTime().UnixNano(), opt)
	if got.State != StateOK {
		t.Fatalf("state %q reason %q detail %q", got.State, got.Reason, got.Detail)
	}
	if got.OutWidth != 512 || got.OutHeight != 341 || !strings.Contains(strings.Join(got.Notes, ";"), "decoded at 1/8 size") {
		t.Fatalf("output %dx%d notes %v", got.OutWidth, got.OutHeight, got.Notes)
	}
	if cfg := decodeJPEG(t, dst); cfg.Width != 512 {
		t.Fatal("thumbnail width", cfg.Width)
	}
}

func TestThumbnailIsTurnedUprightFromItsOrientationTag(t *testing.T) {
	dir := t.TempDir()
	src, fi := writePhoto(t, dir, 800, 600, 6) // rotate 90 clockwise to view
	dst := filepath.Join(dir, "thumb.jpg")
	got := Generate(src, dst, fi.Size(), fi.ModTime().UnixNano(), testOptions())
	if got.State != StateOK || got.OutWidth != 384 || got.OutHeight != 512 {
		t.Fatalf("state %q %dx%d", got.State, got.OutWidth, got.OutHeight)
	}
	f, _ := os.Open(dst)
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	// After a clockwise turn the bright left half of the photo is on top.
	top := color.GrayModel.Convert(img.At(192, 40)).(color.Gray).Y
	bottom := color.GrayModel.Convert(img.At(192, 470)).(color.Gray).Y
	if top < 150 || bottom > 100 {
		t.Fatalf("not upright: top %d bottom %d", top, bottom)
	}
}

// fakeVips stands in for libvips: it copies a ready JPEG to the output path
// named in its second argument (minus the [options] suffix), or fails.
func fakeVips(t *testing.T, fail bool) string {
	t.Helper()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready.jpg")
	img := image.NewGray(image.Rect(0, 0, 300, 200))
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, nil)
	_ = os.WriteFile(ready, buf.Bytes(), 0o644)
	script := "#!/bin/sh\n[ \"$1\" = thumbnail ] || exit 9\nout=\"${3%%\\[*}\"\n"
	if fail {
		script += "echo 'VipsForeignLoad: file is not in a known format' >&2\nexit 1\n"
	} else {
		script += "/bin/cp '" + ready + "' \"$out\"\n"
	}
	path := filepath.Join(dir, "vips")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLibvipsMakesThumbnailsAndFallsBackWhenItFails(t *testing.T) {
	dir := t.TempDir()
	src, fi := writePhoto(t, dir, 800, 600, 1)
	opt := testOptions()
	opt.VipsJPEG = true

	opt.Vips = fakeVips(t, false)
	dst := filepath.Join(dir, "a.jpg")
	got := Generate(src, dst, fi.Size(), fi.ModTime().UnixNano(), opt)
	if got.State != StateOK || got.OutWidth != 300 || !strings.Contains(strings.Join(got.Notes, ";"), "libvips") {
		t.Fatalf("libvips path: %+v", got)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatal("temporary file left behind:", e.Name())
		}
	}

	opt.Vips = fakeVips(t, true)
	dst = filepath.Join(dir, "b.jpg")
	got = Generate(src, dst, fi.Size(), fi.ModTime().UnixNano(), opt)
	if got.State != StateOK || got.OutWidth != 512 || !strings.Contains(strings.Join(got.Notes, ";"), "built-in decoder") {
		t.Fatalf("fallback: %+v", got)
	}
}

func TestHEICNeedsLibvips(t *testing.T) {
	lib, err := synth.Write(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(lib.Root, "unsupported", "photo.heic")
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	opt := testOptions()
	if got := Generate(src, filepath.Join(out, "x.jpg"), fi.Size(), fi.ModTime().UnixNano(), opt); got.State != StateSkipped || got.Reason != "needs_vips" {
		t.Fatalf("without libvips: %+v", got)
	}
	opt.Vips = fakeVips(t, false)
	if got := Generate(src, filepath.Join(out, "x.jpg"), fi.Size(), fi.ModTime().UnixNano(), opt); got.State != StateOK {
		t.Fatalf("with libvips: %+v", got)
	}
}
