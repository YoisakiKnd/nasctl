// Command nasctl is the OneCloud Hub console: a unified, read-only view of the
// machine, its files, the album index, the watch library, the backup's state and
// the console's own tasks.
//
// Album reading is in process. Explicit bounded processing is supervised in the
// separate albumctl component; web handlers cannot import its scanner/decoder.
// Writable storage roots support audited operator file operations.
package main

import (
	"os"

	"github.com/YoisakiKnd/nasctl/internal/nasctl"
)

func main() {
	os.Exit(nasctl.Main(os.Args[1:], nasctl.Env{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}))
}
