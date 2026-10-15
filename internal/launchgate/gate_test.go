package launchgate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLaunchRequiresPrivatePipeAndExplicitParentAdmission(t *testing.T) {
	r, w, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	done := make(chan error, 1)
	go func() { done <- Wait(r) }()
	select {
	case e := <-done:
		t.Fatal("launched before parent admission", e)
	case <-time.After(20 * time.Millisecond):
	}
	if _, e = w.Write([]byte{1}); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"eof", "wrong-byte", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			r, w, e := os.Pipe()
			if e != nil {
				t.Fatal(e)
			}
			defer w.Close()
			if kind == "eof" {
				w.Close()
			}
			if kind == "wrong-byte" {
				w.Write([]byte{0})
			}
			if e = wait(r, 20*time.Millisecond); e == nil {
				t.Fatal("unadmitted launch", kind)
			}
		})
	}
	f, e := os.Create(filepath.Join(t.TempDir(), "not-a-gate"))
	if e != nil {
		t.Fatal(e)
	}
	f.Write([]byte{1})
	f.Seek(0, 0)
	if e = Wait(f); e == nil {
		t.Fatal("regular file acted as gate")
	}
}
