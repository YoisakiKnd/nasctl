// Package jpegscale is Go's image/jpeg decoder (Go 1.27, BSD licence, see
// LICENSE) with one addition: DecodeScaled produces the image at 1/2, 1/4 or
// 1/8 of its size directly from the compressed blocks. A thumbnail of a 48 MP
// photo then decodes into about 1 MP instead of 48, which is what lets small
// NAS boards make thumbnails of large photos at all.
//
// Only reconstructBlock and the image allocation differ from the standard
// library: an eighth keeps each block's DC coefficient and skips the inverse
// DCT, a half or a quarter averages the inverse DCT's output. Parsing,
// Huffman decoding, progressive refinement and colour conversion are
// unchanged. The encoder is not included.
package jpegscale
