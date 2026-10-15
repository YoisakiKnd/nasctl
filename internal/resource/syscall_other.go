//go:build !linux

package resource

import "errors"

const (
	ioprioClassRT   = 1
	ioprioClassBE   = 2
	ioprioClassIdle = 3
)

func ioprioSet(pid, class int) error  { return errors.New("I/O priority requires Linux") }
func setpriority(pid, nice int) error { return errors.New("process priority control requires Linux") }
