//go:build linux

package resource

import "golang.org/x/sys/unix"

// I/O priority classes, mirroring linux/ioprio.h. They are defined here rather
// than imported because the x/sys/unix package exposes the syscall number but
// not these constants.
const (
	ioprioWhoProcess = 1
	ioprioClassShift = 13
	ioprioClassRT    = 1
	ioprioClassBE    = 2
	ioprioClassIdle  = 3
)

// ioprioSet applies an I/O priority class to a process.
//
// This is an advisory extra on top of memory limits, never a replacement for
// them: ionice does nothing about a decoder that allocates its way past the
// machine's headroom.
func ioprioSet(pid int, class int) error {
	prio := class << ioprioClassShift
	_, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET,
		uintptr(ioprioWhoProcess), uintptr(pid), uintptr(prio))
	if errno != 0 {
		return errno
	}
	return nil
}

// setpriority lowers a process's CPU scheduling priority.
func setpriority(pid int, nice int) error {
	return unix.Setpriority(unix.PRIO_PROCESS, pid, nice)
}
