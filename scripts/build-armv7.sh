#!/bin/sh
# Reproducible ARMv7 build of nasctl.
#
# One binary. The console serves the web UI; explicit album work re-executes
# the same binary as `nasctl album ...` in a separate, resource-guarded process.
# Import-graph tests keep the decoders out of the web handlers' packages, so an
# HTTP request cannot start a full-library scan.
#
# Produces:
#   dist/nasctl-linux-armv7        static 32-bit ARM binary, GOARM=7
#   dist/SHA256SUMS                checksums
#   dist/deps.txt                  what the binaries need at runtime
#   dist/README.txt                the exact commands and their output
#
# Requirements on the BUILD machine: Go 1.27.1 or newer. Nothing else. No cgo
# toolchain, no cross-compiler, no Docker, and no Node/Python/PHP for Go builds.
# Generated Svelte assets are committed in internal/hubweb/web. To change the
# UI, rebuild with npm in frontend before running this script.
#
# Reproducibility: this script sets a fixed build time, a fixed module
# environment and a fixed target, so two runs from the same commit produce the
# same bytes. It does not set a fixed PATH or Go toolchain version: use the same
# Go version to get the same output, and record the version printed below with
# the artefact.
set -eu

cd "$(dirname "$0")/.."

OUT=dist
BIN=$OUT/nasctl-linux-armv7
GO=${GO:-go}
command -v "$GO" >/dev/null 2>&1 || {
    # A common case when Go was unpacked rather than installed.
    [ -x /usr/local/go/bin/go ] && GO=/usr/local/go/bin/go
}
command -v "$GO" >/dev/null 2>&1 || { echo "build: no go in PATH (set GO=...)" >&2; exit 1; }

# The one byte string that would otherwise differ between two builds.
if [ "${SOURCE_DATE_EPOCH:-}" != "" ]; then
    BUILD_TIME=$(date -u -d "@$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
        || date -u -r "$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ)
else
    BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)
fi
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
VERSION=$(cat VERSION 2>/dev/null || echo 0.4.0)

LDFLAGS="-s -w \
 -X github.com/YoisakiKnd/nasctl/internal/buildinfo.Version=$VERSION \
 -X github.com/YoisakiKnd/nasctl/internal/buildinfo.Commit=$COMMIT \
 -X github.com/YoisakiKnd/nasctl/internal/buildinfo.BuildTime=$BUILD_TIME \
 -X github.com/YoisakiKnd/nasctl/internal/buildinfo.Target=linux/arm/GOARM7"

mkdir -p "$OUT"

# ---- preconditions the binary depends on -----------------------------------
# GOOS=linux GOARCH=arm GOARM=7  : the target ABI, armv7l.
# CGO_ENABLED=0                  : a static binary. The index uses
#                                  modernc.org/sqlite, a pure Go SQLite, so
#                                  there is no cgo and no libsqlite3 to link
#                                  against. This is what makes a cross-build
#                                  with no cross-compiler possible, and it is
#                                  also why the binary has no shared-library
#                                  dependency to get wrong on the target.
# -trimpath                      : no build directory in the binary, so the
#                                  output does not depend on where it was built.
GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 GOFLAGS=-trimpath \
    "$GO" build -trimpath -buildvcs=false -ldflags "$LDFLAGS" -o "$BIN" ./cmd/nasctl

# ---- record what was built and what it needs -------------------------------
{
    echo "nasctl ARMv7 build"
    echo "go:      $("$GO" version)"
    echo "commit:  $COMMIT"
    echo "version: $VERSION"
    echo "built:   $BUILD_TIME"
    echo "target:  GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0"
    for b in "$BIN"; do
        echo
        echo "artefact:  $b"
        echo "size:      $(wc -c <"$b") bytes"
        echo
        echo "--- file(1) ---"
        file "$b" 2>/dev/null || echo "(file(1) not installed)"
        echo
        echo "--- dynamic dependencies ---"
        if command -v readelf >/dev/null 2>&1; then
            if readelf -d "$b" 2>/dev/null | grep -q NEEDED; then
                readelf -d "$b" | grep NEEDED
                echo "UNEXPECTED: a static build should need no shared library"
            else
                echo "none (static)"
            fi
        else
            echo "(readelf not installed)"
        fi
        echo
        echo "--- interpreter ---"
        if command -v readelf >/dev/null 2>&1; then
            readelf -l "$b" | sed -n 's/.*interpreter: \(.*\)\]/\1/p' \
                | { read -r i && echo "$i" || echo "none (static)"; }
        else
            echo "(readelf not installed)"
        fi
    done
} >"$OUT/README.txt" 2>&1

# ---- deps.txt: what the binary needs at run time ---------------------------
{
    cat "$OUT/README.txt"
    echo
    echo "--- runtime dependencies on the target machine ---"
    echo "shared libraries:  none. The binary is static and CGO_ENABLED=0, so it"
    echo "                   needs no libc, no musl, no dynamic linker and no"
    echo "                   separately installed runtime."
    echo "kernel:            Linux. Needs /proc (memory, PSI, process RSS) and a"
    echo "                   writable state and cache directory. cgroup v2 is"
    echo "                   required only for the isolated heavy path; without it"
    echo "                   heavy tasks refuse to run unless an operator"
    echo "                   explicitly accepts RSS-only supervision."
    echo "filesystems:       state requires reliable local SQLite WAL locking."
    echo "                   Media and backup roots need separate validation on"
    echo "                   their actual filesystem; NFS/SMB is not verified here."
    echo "external programs: nasctl re-executes itself as \`nasctl album\` on demand"
    echo "                   for explicit album tasks; no separate album binary or"
    echo "                   HTTP service is needed. ffmpeg and ExifTool are not required."
    echo "                   Video posters are off and their adapter is not"
    echo "                   compiled in; a missing adapter skips a format rather"
    echo "                   than failing a library."
    echo "memory needed:     see docs/RESOURCE-REPORT.md. The ceilings the"
    echo "                   operator sets (systemd MemoryHigh/MemoryMax and the"
    echo "                   guard thresholds) are policy, not requirements of the"
    echo "                   binary. Required cgroup isolation still refuses heavy"
    echo "                   tasks when unavailable; browsing remains available."
    echo
    echo "--- what this artefact has NOT been verified against ---"
    echo "This build script does not run QEMU or execute the target binary."
    echo "It has NOT verified the target onecloud. Memory numbers taken under"
    echo "emulation are not the target's numbers and must not be presented as"
    echo "measurements of it."
} >"$OUT/deps.txt" 2>&1

sha256sum "$BIN" > "$OUT/SHA256SUMS"

{
    echo
    echo "--- commands ---"
    echo "GO=\$GO ./scripts/build-armv7.sh"
    echo "sha256sum -c $OUT/SHA256SUMS    # run from the repository root"
} >> "$OUT/README.txt"

cat "$OUT/deps.txt"
echo
cat "$OUT/SHA256SUMS"
echo
echo "Wrote $BIN"
