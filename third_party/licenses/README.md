# Dependency notices

The WebDAV backup and anime source discovery implementations use these modules. Full versions and checksums are pinned in go.mod and go.sum.

- filippo.io/age v1.3.2 — https://github.com/FiloSottile/age
- filippo.io/hpke v0.4.0 — https://github.com/FiloSottile/hpke
- github.com/klauspost/compress v1.20.3 — https://github.com/klauspost/compress
- golang.org/x/crypto v0.58.0 — https://go.googlesource.com/crypto

- golang.org/x/net v0.61.0 — https://go.googlesource.com/net
- Go standard library image/jpeg decoder (Go 1.27), adapted in internal/jpegscale to decode at reduced size — https://go.dev/src/image/jpeg (go-image-jpeg.txt)

Upstream license texts accompany this notice. This directory describes the backup, source discovery and thumbnail decoder dependencies, not the complete dependency inventory of nasctl.
