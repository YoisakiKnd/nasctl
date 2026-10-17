package jpegscale

import (
	"bytes"
	"image/jpeg"
	"testing"
)

func benchData(b *testing.B) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, photo(4032, 3024), &jpeg.Options{Quality: 88}); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

func BenchmarkStandardFull(b *testing.B) {
	data := benchData(b)
	b.ResetTimer()
	for range b.N {
		if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScaled(b *testing.B) {
	data := benchData(b)
	for _, scale := range []int{4, 2, 1} {
		b.Run(map[int]string{4: "half", 2: "quarter", 1: "eighth"}[scale], func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if _, err := DecodeScaled(bytes.NewReader(data), scale); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
