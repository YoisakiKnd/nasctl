//go:build !linux

package scan

import "syscall"

type syscallStat = syscall.Stat_t
