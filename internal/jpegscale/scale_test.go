package jpegscale

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// photo makes a smooth, slightly noisy picture so averaging is meaningful.
func photo(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				uint8(128 + 90*math.Sin(float64(x)/37)),
				uint8(128 + 90*math.Cos(float64(y)/23)),
				uint8(128 + 60*math.Sin(float64(x+y)/29)),
				255,
			})
		}
	}
	return img
}

func encode(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// meanDiff compares a scaled decode with the full decode averaged into the
// same grid, in RGB.
func meanDiff(full, small image.Image, f int) float64 {
	fb, sb := full.Bounds(), small.Bounds()
	var sum float64
	var count int
	for y := 0; y < sb.Dy(); y++ {
		for x := 0; x < sb.Dx(); x++ {
			var r, g, b, n float64
			for yy := y * f; yy < (y+1)*f && yy < fb.Dy(); yy++ {
				for xx := x * f; xx < (x+1)*f && xx < fb.Dx(); xx++ {
					cr, cg, cb, _ := full.At(xx, yy).RGBA()
					r, g, b, n = r+float64(cr>>8), g+float64(cg>>8), b+float64(cb>>8), n+1
				}
			}
			sr, sg, sbb, _ := small.At(sb.Min.X+x, sb.Min.Y+y).RGBA()
			sum += math.Abs(r/n-float64(sr>>8)) + math.Abs(g/n-float64(sg>>8)) + math.Abs(b/n-float64(sbb>>8))
			count += 3
		}
	}
	return sum / float64(count)
}

func TestScaledDecodesMatchAveragedFullDecodes(t *testing.T) {
	src := photo(203, 141) // not a multiple of 8 or 16
	gray := image.NewGray(src.Bounds())
	draw.Draw(gray, gray.Bounds(), src, image.Point{}, draw.Src)
	for name, data := range map[string][]byte{"color": encode(t, src), "gray": encode(t, gray)} {
		full, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		for _, scale := range []int{8, 4, 2, 1} {
			small, err := DecodeScaled(bytes.NewReader(data), scale)
			if err != nil {
				t.Fatal(name, scale, err)
			}
			want := image.Rect(0, 0, (203*scale+7)/8, (141*scale+7)/8)
			if small.Bounds() != want {
				t.Fatalf("%s 1/%d: bounds %v, want %v", name, 8/scale, small.Bounds(), want)
			}
			// Chroma subsampling and rounding allow a small difference.
			// Luma matches the averaged full decode closely; colour also carries
			// the chroma subsampling offset, which grows with the scale.
			limit := 1.5
			if name != "gray" {
				limit = 8
			}
			if d := meanDiff(full, small, 8/scale); d > limit {
				t.Fatalf("%s 1/%d: mean difference %.2f", name, 8/scale, d)
			}
		}
	}
}

// The fixtures were made with ImageMagick from the same synthetic picture:
// progressive (colour and grey), 4:4:4, 4:2:2 and Adobe CMYK.
func TestScaledDecodesOfOtherEncodings(t *testing.T) {
	for _, name := range []string{"progressive", "progressive-gray", "444", "422", "cmyk"} {
		data, err := os.ReadFile("testdata/" + name + ".jpg")
		if err != nil {
			t.Fatal(err)
		}
		full, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(name, err)
		}
		progressive, _ := Progressive(bytes.NewReader(data))
		if progressive != strings.HasPrefix(name, "progressive") {
			t.Fatal(name, "progressive flag", progressive)
		}
		for _, scale := range []int{8, 4, 2, 1} {
			small, err := DecodeScaled(bytes.NewReader(data), scale)
			if err != nil {
				t.Fatal(name, scale, err)
			}
			if want := image.Rect(0, 0, (203*scale+7)/8, (141*scale+7)/8); small.Bounds() != want {
				t.Fatalf("%s 1/%d: bounds %v, want %v", name, 8/scale, small.Bounds(), want)
			}
			if d := meanDiff(full, small, 8/scale); d > 8 {
				t.Fatalf("%s 1/%d: mean difference %.2f", name, 8/scale, d)
			}
		}
	}
}

func TestFullScaleIsTheStandardDecode(t *testing.T) {
	data := encode(t, photo(64, 48))
	want, _ := jpeg.Decode(bytes.NewReader(data))
	got, err := DecodeScaled(bytes.NewReader(data), 8)
	if err != nil {
		t.Fatal(err)
	}
	a, b := want.(*image.YCbCr), got.(*image.YCbCr)
	if !bytes.Equal(a.Y, b.Y) || !bytes.Equal(a.Cb, b.Cb) || !bytes.Equal(a.Cr, b.Cr) {
		t.Fatal("full-scale decode differs from image/jpeg")
	}
}

func TestConfigAndProgressive(t *testing.T) {
	data := encode(t, photo(40, 30))
	cfg, err := DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != 40 || cfg.Height != 30 {
		t.Fatal(cfg, err)
	}
	if p, err := Progressive(bytes.NewReader(data)); err != nil || p {
		t.Fatal("baseline reported as progressive", p, err)
	}
	if _, err := DecodeScaled(bytes.NewReader(data), 3); err == nil {
		t.Fatal("unsupported scale accepted")
	}
}

// TestProgressiveKeepsOnlyWhatItReads: a progressive decode at a reduced
// scale drops the coefficients the reduced IDCT never reads, keeping only
// whether they are non-zero for the refinement scans. The pixels must be the
// same as when every coefficient is kept.
func TestProgressiveKeepsOnlyWhatItReads(t *testing.T) {
	files, _ := filepath.Glob("testdata/progressive*.jpg")
	more, _ := filepath.Glob(filepath.Join(runtime.GOROOT(), "src", "image", "testdata", "*progressive*.jpeg"))
	files = append(files, more...)
	if len(files) < 2 {
		t.Fatal("no progressive test images")
	}
	decode := func(f string, scale int) image.Image {
		r, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		m, err := DecodeScaled(r, scale)
		if err != nil {
			t.Fatalf("%s at %d: %v", f, scale, err)
		}
		return m
	}
	for _, f := range files {
		for _, scale := range []int{1, 2, 4} {
			keepAllCoefficients = true
			want := decode(f, scale)
			keepAllCoefficients = false
			got := decode(f, scale)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s at scale %d: compact coefficients changed the pixels", filepath.Base(f), scale)
			}
		}
	}
	if CoefficientBlockBytes(1) != 10 || CoefficientBlockBytes(8) != 128 {
		t.Errorf("block costs %d and %d", CoefficientBlockBytes(1), CoefficientBlockBytes(8))
	}
}
