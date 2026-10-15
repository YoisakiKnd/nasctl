// Package launchgate holds an album supervisor until its owning NAS process has
// placed it in the delegated task cgroup. EOF or timeout never permits work.
package launchgate

import (
	"context"
	"errors"
	"os"
	"time"
)

func Wait(f *os.File) error {
	return wait(f, 10*time.Second)
}
func wait(f *os.File, timeout time.Duration) error {
	if f == nil {
		return errors.New("album launch: missing private gate")
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil || i.Mode()&os.ModeNamedPipe == 0 {
		return errors.New("album launch: gate must be an inherited pipe")
	}
	ctx, stop := context.WithTimeout(context.Background(), timeout)
	defer stop()
	done := make(chan error, 1)
	go func() {
		var b [1]byte
		n, e := f.Read(b[:])
		if e != nil || n != 1 || b[0] != 1 {
			done <- errors.New("album launch: parent did not admit the task")
			return
		}
		done <- nil
	}()
	select {
	case e := <-done:
		return e
	case <-ctx.Done():
		return errors.New("album launch: admission timed out")
	}
}
