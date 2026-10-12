// Command cmdgen writes the synthetic library to a directory. It exists so the
// same fixtures used by the tests can be produced by hand, for example to run
// the ARMv7 binary under emulation against a known tree.
package main

import (
	"fmt"
	"os"

	"github.com/YoisakiKnd/nasctl/internal/synth"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: cmdgen DIR")
		os.Exit(2)
	}
	lib, err := synth.Write(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "cmdgen:", err)
		os.Exit(1)
	}
	var bytes int64
	for _, f := range lib.Files {
		bytes += f.Bytes
	}
	fmt.Printf("wrote %d files, %d bytes to %s\n", len(lib.Files), bytes, os.Args[1])
}
