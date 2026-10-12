//go:build linux

package scan

import "golang.org/x/sys/unix"

// syscallStat is the concrete type behind os.FileInfo.Sys() on Linux. Naming it
// keeps the walker's device comparison out of the platform-specific code.
type syscallStat = unix.Stat_t
