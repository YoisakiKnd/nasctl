package jpegscale

import mathbits "math/bits"

// coeffs holds a progressive image's coefficients between scans, compactly.
// A decode at scale n reads only the top-left n*n coefficients of a block,
// so only those are kept, as int16 (an 8-bit JPEG's quantized coefficients
// fit). The rest matter to later scans only as zero or not, because a
// refinement scan reads a correction bit for each non-zero coefficient, so
// a mask keeps that. At an eighth this is 10 bytes a block instead of 256.
type coeffs struct {
	n    int
	vals []int16
	// nz marks non-zero coefficients outside the kept corner; nil at n == 8.
	nz []uint64
}

// CoefficientBlockBytes is what coeffs holds for one block at a scale.
func CoefficientBlockBytes(scale int) int64 {
	if scale >= 8 {
		return blockSize * 2
	}
	return int64(scale*scale)*2 + 8
}

// keepAllCoefficients makes every decode keep whole blocks; tests compare.
var keepAllCoefficients = false

func keptEdge(scale int) int {
	if keepAllCoefficients {
		return 8
	}
	return scale
}

func newCoeffs(blocks, n int) *coeffs {
	c := &coeffs{n: n, vals: make([]int16, blocks*n*n)}
	if n < 8 {
		c.nz = make([]uint64, blocks)
	}
	return c
}

// load expands block i into b. A dropped non-zero coefficient comes back as
// 1: its value is never used, only that it is non-zero.
func (c *coeffs) load(i int, b *block) {
	*b = block{}
	n, v := c.n, c.vals[i*c.n*c.n:]
	if n == 8 {
		for k := range b {
			b[k] = int32(v[k])
		}
		return
	}
	for m, k := c.nz[i], 0; m != 0; m &= m - 1 {
		k = mathbits.TrailingZeros64(m)
		b[k] = 1
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			b[y*8+x] = int32(v[y*n+x])
		}
	}
}

func (c *coeffs) save(i int, b *block) {
	n, v := c.n, c.vals[i*c.n*c.n:]
	if n == 8 {
		for k := range b {
			v[k] = int16(b[k])
		}
		return
	}
	var m uint64
	for k, value := range b {
		if value != 0 && (k&7 >= n || k>>3 >= n) {
			m |= 1 << k
		}
	}
	c.nz[i] = m
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			v[y*n+x] = int16(b[y*8+x])
		}
	}
}
