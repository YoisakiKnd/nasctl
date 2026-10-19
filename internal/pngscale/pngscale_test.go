package pngscale

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// reference makes the same thumbnail from image/png's full decode: straight
// 8-bit samples composited over white, averaged per output cell. Only the
// arithmetic is shared; parsing, unfiltering, interlacing, palettes,
// transparency keys and bit depths are image/png's.
func reference(t *testing.T, b []byte, edge int) *image.RGBA {
	t.Helper()
	m, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	r := m.Bounds()
	w, h := r.Dx(), r.Dy()
	tw, th := Size(w, h, edge)
	sums := make([]uint64, tw*th*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var c [4]uint8
			switch v := m.At(r.Min.X+x, r.Min.Y+y).(type) {
			case color.NRGBA:
				c = [4]uint8{v.R, v.G, v.B, v.A}
			case color.NRGBA64:
				c = [4]uint8{uint8(v.R >> 8), uint8(v.G >> 8), uint8(v.B >> 8), uint8(v.A >> 8)}
			case color.Gray:
				c = [4]uint8{v.Y, v.Y, v.Y, 255}
			case color.Gray16:
				c = [4]uint8{uint8(v.Y >> 8), uint8(v.Y >> 8), uint8(v.Y >> 8), 255}
			case color.RGBA:
				if v.A != 255 {
					t.Fatalf("premultiplied reference colour %v", v)
				}
				c = [4]uint8{v.R, v.G, v.B, 255}
			case color.RGBA64:
				if v.A != 0xffff {
					t.Fatalf("premultiplied reference colour %v", v)
				}
				c = [4]uint8{uint8(v.R >> 8), uint8(v.G >> 8), uint8(v.B >> 8), 255}
			default:
				t.Fatalf("reference colour %T", v)
			}
			cell := (y*th/h*tw + x*tw/w) * 4
			for k := 0; k < 3; k++ {
				v := uint32(c[k])
				if c[3] != 255 {
					v = (v*uint32(c[3]) + 255*uint32(255-c[3]) + 127) / 255
				}
				sums[cell+k] += uint64(v)
			}
			sums[cell+3]++
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, tw, th))
	for i := 0; i < tw*th; i++ {
		n := sums[i*4+3]
		for k := 0; k < 3; k++ {
			out.Pix[i*4+k] = uint8((sums[i*4+k] + n/2) / n)
		}
		out.Pix[i*4+3] = 255
	}
	return out
}

func same(t *testing.T, name string, b []byte, edge int) {
	t.Helper()
	got, _, err := Thumbnail(bytes.NewReader(b), edge)
	if err != nil {
		t.Errorf("%s at %d: %v", name, edge, err)
		return
	}
	want := reference(t, b, edge)
	if got.Bounds() != want.Bounds() {
		t.Errorf("%s at %d: size %v, want %v", name, edge, got.Bounds(), want.Bounds())
		return
	}
	if !bytes.Equal(got.Pix, want.Pix) {
		t.Errorf("%s at %d: pixels differ from image/png", name, edge)
	}
}

var edges = []int{1, 5, 16, 33, 512}

func TestMatchesImagePNGOnEveryColourTypeDepthAndInterlace(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.png")
	suite, _ := filepath.Glob(filepath.Join(runtime.GOROOT(), "src", "image", "png", "testdata", "pngsuite", "*.png"))
	if len(suite) == 0 {
		t.Log("GOROOT PngSuite not found; checking the committed interlaced files only")
	}
	files = append(files, suite...)
	files = append(files, filepath.Join(runtime.GOROOT(), "src", "image", "png", "testdata", "benchRGB-interlace.png"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, edge := range edges {
			same(t, filepath.Base(f), b, edge)
		}
	}
}

func TestMatchesImagePNGOnGeneratedImages(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	pal := color.Palette{}
	for i := 0; i < 200; i++ {
		pal = append(pal, color.NRGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256))})
	}
	for round := 0; round < 12; round++ {
		w, h := 1+rng.Intn(140), 1+rng.Intn(90)
		r := image.Rect(0, 0, w, h)
		for _, m := range []image.Image{image.NewGray(r), image.NewGray16(r), image.NewNRGBA(r), image.NewNRGBA64(r), image.NewPaletted(r, pal), image.NewRGBA(r)} {
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					c := color.NRGBA64{uint16(rng.Intn(65536)), uint16(rng.Intn(65536)), uint16(rng.Intn(65536)), uint16(rng.Intn(65536))}
					if _, ok := m.(*image.RGBA); ok {
						c.A = 0xffff
					}
					m.(interface{ Set(int, int, color.Color) }).Set(x, y, c)
				}
			}
			var b bytes.Buffer
			level := []png.CompressionLevel{png.DefaultCompression, png.NoCompression, png.BestSpeed}[round%3]
			if err := (&png.Encoder{CompressionLevel: level}).Encode(&b, m); err != nil {
				t.Fatal(err)
			}
			for _, edge := range edges {
				same(t, "generated", b.Bytes(), edge)
			}
		}
	}
}

func TestRefusesDamagedFiles(t *testing.T) {
	dir := filepath.Join(runtime.GOROOT(), "src", "image", "png", "testdata")
	// invalid-trunc.png only loses its IEND chunk; every image byte is there
	// and checked, so a thumbnail does not need it.
	for _, name := range []string{"invalid-crc32.png", "invalid-zlib.png"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Skip("GOROOT test files not found")
		}
		if _, _, err := Thumbnail(bytes.NewReader(b), 64); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	whole, _ := os.ReadFile("testdata/rgba8-i.png")
	for _, cut := range []int{len(whole) / 3, len(whole) - 20} {
		if _, _, err := Thumbnail(bytes.NewReader(whole[:cut]), 64); err == nil {
			t.Errorf("a file cut at %d of %d bytes was accepted", cut, len(whole))
		}
	}
	if _, _, err := Thumbnail(bytes.NewReader([]byte("GIF89a")), 64); !errors.Is(err, ErrFormat) {
		t.Errorf("not a PNG: %v", err)
	}
}

func TestMemoryDoesNotGrowWithTheSource(t *testing.T) {
	// 6000 x 6000 RGBA (144 MB decoded) against the estimate.
	if est := EstimateBytes(6000, 6000, 512); est > 16<<20 {
		t.Fatalf("estimate %d bytes for a 36 MP source", est)
	}
	if est := EstimateBytes(1, MaxPixels, 512); est > 16<<20 {
		t.Fatalf("estimate %d bytes for a very tall source", est)
	}
}
