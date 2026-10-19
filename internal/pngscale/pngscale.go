// Package pngscale makes a thumbnail of a PNG without decoding it whole.
//
// image/png returns the full image, four or eight bytes a pixel: a 52 MP
// scan costs over 200 MiB, and an interlaced one twice that. Here each row
// is inflated and unfiltered with only the previous row kept, every pixel is
// composited over white and added to the output cell it falls in, and the
// cells are averaged at the end. Memory is the output grid and two rows,
// whatever the source size, and interlaced files cost the same.
package pngscale

import (
	"bufio"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"io"
)

// MaxWidth bounds a row buffer; MaxPixels a decode's work.
const (
	MaxWidth  = 1 << 20
	MaxPixels = 500_000_000
)

var (
	ErrFormat      = errors.New("pngscale: not a valid PNG")
	ErrUnsupported = errors.New("pngscale: unsupported PNG")
)

// Info is a PNG's header.
type Info struct {
	Width, Height      int
	BitDepth, Color    int
	Interlaced         bool
	channels, rowBytes int
}

// Size is the thumbnail size for a source within edge, the same rule the
// built-in thumbnailer uses: the longer side becomes edge, never enlarged.
func Size(w, h, edge int) (int, int) {
	tw, th := w, h
	if w > edge || h > edge {
		if w >= h {
			tw, th = edge, int(float64(h)*float64(edge)/float64(w)+0.5)
		} else {
			tw, th = int(float64(w)*float64(edge)/float64(h)+0.5), edge
		}
	}
	return max(tw, 1), max(th, 1)
}

// EstimateBytes bounds what Thumbnail allocates for a w x h source: the
// output cells (three uint64 sums and four bytes of image each), two rows at
// the widest sample format, the column map and the reader and inflater.
func EstimateBytes(w, h, edge int) int64 {
	tw, th := Size(w, h, edge)
	return int64(tw)*int64(th)*(3*8+4) + 2*(int64(w)*8+1) + int64(w)*4 + int64(th)*8 + 256<<10
}

type reader struct {
	r    *bufio.Reader
	info Info
	// palette as straight RGBA; transparent key for grey and RGB.
	palette     [][4]uint8
	key         [3]uint16
	keyed       bool
	idatLeft    uint32
	crc         uint32
	crcExpected bool
}

func (d *reader) chunkHeader() (uint32, string, error) {
	var h [8]byte
	if _, err := io.ReadFull(d.r, h[:]); err != nil {
		return 0, "", err
	}
	n := binary.BigEndian.Uint32(h[:4])
	if n > 1<<31-1 {
		return 0, "", ErrFormat
	}
	d.crc = crc32.Update(0, crc32.IEEETable, h[4:])
	return n, string(h[4:]), nil
}

func (d *reader) read(b []byte) error {
	if _, err := io.ReadFull(d.r, b); err != nil {
		return err
	}
	d.crc = crc32.Update(d.crc, crc32.IEEETable, b)
	return nil
}

func (d *reader) checkCRC() error {
	var b [4]byte
	if _, err := io.ReadFull(d.r, b[:]); err != nil {
		return err
	}
	if binary.BigEndian.Uint32(b[:]) != d.crc {
		return fmt.Errorf("%w: checksum mismatch", ErrFormat)
	}
	return nil
}

// skip reads a chunk's data to check its CRC without holding it.
func (d *reader) skip(n uint32) error {
	var buf [4096]byte
	for n > 0 {
		k := min(n, uint32(len(buf)))
		if err := d.read(buf[:k]); err != nil {
			return err
		}
		n -= k
	}
	return d.checkCRC()
}

// Read serves the concatenated IDAT data to the inflater.
func (d *reader) Read(p []byte) (int, error) {
	for d.idatLeft == 0 {
		if d.crcExpected {
			if err := d.checkCRC(); err != nil {
				return 0, err
			}
		}
		n, kind, err := d.chunkHeader()
		if err != nil {
			return 0, err
		}
		if kind != "IDAT" {
			return 0, io.ErrUnexpectedEOF
		}
		d.idatLeft, d.crcExpected = n, true
	}
	p = p[:min(len(p), int(d.idatLeft))]
	n, err := d.r.Read(p)
	d.crc = crc32.Update(d.crc, crc32.IEEETable, p[:n])
	d.idatLeft -= uint32(n)
	return n, err
}

// header reads the signature and chunks up to the first IDAT.
func (d *reader) header() error {
	var sig [8]byte
	if _, err := io.ReadFull(d.r, sig[:]); err != nil || string(sig[:]) != "\x89PNG\r\n\x1a\n" {
		return ErrFormat
	}
	seenIHDR := false
	for {
		n, kind, err := d.chunkHeader()
		if err != nil {
			return ErrFormat
		}
		switch {
		case kind == "IHDR":
			if seenIHDR || n != 13 {
				return ErrFormat
			}
			var b [13]byte
			if err := d.read(b[:]); err != nil {
				return err
			}
			seenIHDR = true
			i := &d.info
			w, h := binary.BigEndian.Uint32(b[0:]), binary.BigEndian.Uint32(b[4:])
			i.BitDepth, i.Color, i.Interlaced = int(b[8]), int(b[9]), b[12] == 1
			if w > 1<<31-1 || h > 1<<31-1 {
				return ErrFormat
			}
			// The size is reported even when it is refused, for the index.
			i.Width, i.Height = int(w), int(h)
			if w == 0 || h == 0 || w > MaxWidth || uint64(w)*uint64(h) > MaxPixels || b[10] != 0 || b[11] != 0 || b[12] > 1 {
				return ErrUnsupported
			}
			depths := map[int][]int{0: {1, 2, 4, 8, 16}, 2: {8, 16}, 3: {1, 2, 4, 8}, 4: {8, 16}, 6: {8, 16}}
			ok := false
			for _, depth := range depths[i.Color] {
				ok = ok || depth == i.BitDepth
			}
			if !ok {
				return ErrFormat
			}
			i.channels = map[int]int{0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[i.Color]
			i.rowBytes = (i.Width*i.channels*i.BitDepth + 7) / 8
		case !seenIHDR:
			return ErrFormat
		case kind == "PLTE":
			if n%3 != 0 || n == 0 || n > 256*3 {
				return ErrFormat
			}
			b := make([]byte, n)
			if err := d.read(b); err != nil {
				return err
			}
			d.palette = make([][4]uint8, n/3)
			for k := range d.palette {
				d.palette[k] = [4]uint8{b[3*k], b[3*k+1], b[3*k+2], 255}
			}
		case kind == "tRNS":
			if n > 256 {
				return ErrFormat
			}
			b := make([]byte, n)
			if err := d.read(b); err != nil {
				return err
			}
			switch d.info.Color {
			case 3:
				if int(n) > len(d.palette) {
					return ErrFormat
				}
				for k, a := range b {
					d.palette[k][3] = a
				}
			case 0:
				if n != 2 {
					return ErrFormat
				}
				d.key[0], d.keyed = binary.BigEndian.Uint16(b), true
			case 2:
				if n != 6 {
					return ErrFormat
				}
				for k := range d.key {
					d.key[k] = binary.BigEndian.Uint16(b[2*k:])
				}
				d.keyed = true
			default:
				return ErrFormat
			}
		case kind == "IDAT":
			if d.info.Color == 3 && len(d.palette) == 0 {
				return ErrFormat
			}
			d.idatLeft, d.crcExpected = n, true
			return nil
		case kind == "IEND":
			return ErrFormat
		default:
			if kind[0]&0x20 == 0 {
				// An unknown critical chunk changes how the image decodes.
				return ErrUnsupported
			}
			if err := d.skip(n); err != nil {
				return err
			}
			continue
		}
		if err := d.checkCRC(); err != nil {
			return err
		}
	}
}

// Inspect reads the header only.
func Inspect(r io.Reader) (Info, error) {
	d := &reader{r: bufio.NewReader(r)}
	err := d.header()
	return d.info, err
}

// adam7 lists the interlace passes: x and y start, then step.
var adam7 = [7][4]int{{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4}, {0, 2, 2, 4}, {1, 0, 2, 2}, {0, 1, 1, 2}}

// Thumbnail returns the image area-averaged to fit within edge, composited
// over white and opaque.
func Thumbnail(r io.Reader, edge int) (*image.RGBA, Info, error) {
	d := &reader{r: bufio.NewReaderSize(r, 64<<10)}
	if err := d.header(); err != nil {
		return nil, d.info, err
	}
	i := d.info
	tw, th := Size(i.Width, i.Height, edge)
	// Which output column and row each source column and row adds to.
	// A very tall image would make a row map large, so rows are mapped as
	// they come; int64 keeps the products whole on 32-bit ARM.
	rowCell := func(y int) int { return int(int64(y) * int64(th) / int64(i.Height)) }
	colCell := make([]int32, i.Width)
	colCount, rowCount := make([]uint64, tw), make([]uint64, th)
	for x := range colCell {
		colCell[x] = int32(int64(x) * int64(tw) / int64(i.Width))
		colCount[colCell[x]]++
	}
	for ty := range rowCount {
		// Rows y with y*th/H == ty: from ceil(ty*H/th) up to the next cell's.
		first := (int64(ty)*int64(i.Height) + int64(th) - 1) / int64(th)
		next := (int64(ty+1)*int64(i.Height) + int64(th) - 1) / int64(th)
		rowCount[ty] = uint64(next - first)
	}
	sums := make([]uint64, tw*th*3)
	z, err := zlib.NewReader(d)
	if err != nil {
		return nil, i, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	defer z.Close()
	bpp := max(1, i.channels*i.BitDepth/8)
	cur, prev := make([]byte, i.rowBytes+1), make([]byte, i.rowBytes+1)
	passes := [][4]int{{0, 0, 1, 1}}
	if i.Interlaced {
		passes = adam7[:]
	}
	for _, p := range passes {
		pw := (i.Width - p[0] + p[2] - 1) / p[2]
		ph := (i.Height - p[1] + p[3] - 1) / p[3]
		if pw <= 0 || ph <= 0 {
			continue
		}
		n := (pw*i.channels*i.BitDepth+7)/8 + 1
		clear(prev[:n])
		for py := 0; py < ph; py++ {
			if _, err := io.ReadFull(z, cur[:n]); err != nil {
				return nil, i, fmt.Errorf("%w: image data ends early", ErrFormat)
			}
			if err := unfilter(cur[:n], prev[:n], bpp); err != nil {
				return nil, i, err
			}
			y := p[1] + py*p[3]
			row := sums[rowCell(y)*tw*3:]
			for px := 0; px < pw; px++ {
				rgb := d.pixel(cur[1:n], px)
				c := int(colCell[p[0]+px*p[2]]) * 3
				row[c] += uint64(rgb[0])
				row[c+1] += uint64(rgb[1])
				row[c+2] += uint64(rgb[2])
			}
			cur, prev = prev, cur
		}
	}
	// The stream must end here, with its checksum, and so must the chunk.
	if n, err := io.Copy(io.Discard, io.LimitReader(z, 1)); n != 0 || err != nil {
		return nil, i, fmt.Errorf("%w: image data is damaged or too long", ErrFormat)
	}
	if d.idatLeft == 0 && d.crcExpected {
		if err := d.checkCRC(); err != nil {
			return nil, i, err
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, tw, th))
	for ty := 0; ty < th; ty++ {
		for tx := 0; tx < tw; tx++ {
			count := colCount[tx] * rowCount[ty]
			s, o := sums[(ty*tw+tx)*3:], out.Pix[ty*out.Stride+tx*4:]
			for k := 0; k < 3; k++ {
				o[k] = uint8((s[k] + count/2) / count)
			}
			o[3] = 255
		}
	}
	return out, i, nil
}

// pixel returns sample x of an unfiltered row, composited over white.
func (d *reader) pixel(row []byte, x int) [3]uint8 {
	i := &d.info
	var s [4]uint16 // samples at the file's depth
	switch i.BitDepth {
	case 16:
		for k := 0; k < i.channels; k++ {
			s[k] = binary.BigEndian.Uint16(row[(x*i.channels+k)*2:])
		}
	case 8:
		for k := 0; k < i.channels; k++ {
			s[k] = uint16(row[x*i.channels+k])
		}
	default: // 1, 2 or 4 bits: grey or palette, one channel
		bit := x * i.BitDepth
		s[0] = uint16(row[bit/8]>>(8-i.BitDepth-bit%8)) & (1<<i.BitDepth - 1)
	}
	to8 := func(v uint16) uint8 {
		switch i.BitDepth {
		case 16:
			return uint8(v >> 8)
		case 8:
			return uint8(v)
		}
		return uint8(uint32(v) * 255 / (1<<i.BitDepth - 1))
	}
	var r, g, b, a uint8
	switch i.Color {
	case 0:
		r = to8(s[0])
		g, b, a = r, r, 255
		if d.keyed && s[0] == d.key[0] {
			a = 0
		}
	case 2:
		r, g, b, a = to8(s[0]), to8(s[1]), to8(s[2]), 255
		if d.keyed && s[0] == d.key[0] && s[1] == d.key[1] && s[2] == d.key[2] {
			a = 0
		}
	case 3:
		c := [4]uint8{0, 0, 0, 255} // an index past the palette reads as black
		if int(s[0]) < len(d.palette) {
			c = d.palette[s[0]]
		}
		r, g, b, a = c[0], c[1], c[2], c[3]
	case 4:
		r = to8(s[0])
		g, b, a = r, r, to8(s[1])
	case 6:
		r, g, b, a = to8(s[0]), to8(s[1]), to8(s[2]), to8(s[3])
	}
	if a == 255 {
		return [3]uint8{r, g, b}
	}
	over := func(c uint8) uint8 { return uint8((uint32(c)*uint32(a) + 255*uint32(255-a) + 127) / 255) }
	return [3]uint8{over(r), over(g), over(b)}
}

// unfilter reverses one row's filter in place; prev is the previous
// unfiltered row of the same pass, zero for the first.
func unfilter(cur, prev []byte, bpp int) error {
	c, p := cur[1:], prev[1:]
	switch cur[0] {
	case 0:
	case 1:
		for k := bpp; k < len(c); k++ {
			c[k] += c[k-bpp]
		}
	case 2:
		for k := range c {
			c[k] += p[k]
		}
	case 3:
		for k := 0; k < bpp && k < len(c); k++ {
			c[k] += p[k] / 2
		}
		for k := bpp; k < len(c); k++ {
			c[k] += uint8((int(c[k-bpp]) + int(p[k])) / 2)
		}
	case 4:
		for k := range c {
			var a, cc int
			if k >= bpp {
				a, cc = int(c[k-bpp]), int(p[k-bpp])
			}
			b := int(p[k])
			pa, pb, pc := abs(b-cc), abs(a-cc), abs(a+b-2*cc)
			switch {
			case pa <= pb && pa <= pc:
				c[k] += uint8(a)
			case pb <= pc:
				c[k] += uint8(b)
			default:
				c[k] += uint8(cc)
			}
		}
	default:
		return fmt.Errorf("%w: bad filter type %d", ErrFormat, cur[0])
	}
	return nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
