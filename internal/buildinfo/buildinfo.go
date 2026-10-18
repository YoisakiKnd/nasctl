// Package buildinfo carries version metadata stamped at link time.
package buildinfo

import (
	"fmt"
	"runtime"
)

// Set with -ldflags "-X .../internal/buildinfo.Version=... ".
var (
	Version   = "0.8.0-dev"
	Commit    = "unknown"
	BuildTime = "unknown"
	Target    = "unknown"
)

// Schema is the on-disk index schema understood by this binary. It is stamped
// here as well as in the database so `status` can warn about a mismatch even
// when the database cannot be opened.
//
// Must match store.SchemaVersion. v2 added media.seen_run.
const Schema = 2

// String returns a single line identifying the build.
func String() string {
	return fmt.Sprintf("nasctl %s (commit %s, built %s, target %s, runtime %s/%s)",
		Version, Commit, BuildTime, Target, runtime.GOOS, runtime.GOARCH)
}

// IsARM32 reports whether the binary was built for a 32-bit ARM target. Used
// only for reporting; nothing in the code depends on it.
func IsARM32() bool {
	return runtime.GOARCH == "arm" && (runtime.GOOS == "linux")
}
