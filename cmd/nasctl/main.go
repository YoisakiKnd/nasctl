// Command nasctl is the OneCloud Hub console: a unified, read-only view of the
// machine, its files, the album index, the watch library, the backup's state and
// the console's own tasks.
//
// It is a separate binary from albumctl on purpose. The album service scans and
// decodes; the console reads and reports. Keeping them separate means the console
// can be restarted, upgraded or left running while album work is paused, and it
// means the console's import graph contains no decoder - which is what makes "an
// HTTP request cannot start a full-library scan" a property of the build rather
// than a promise in a document.
//
// Nothing here writes to a media tree. File writes are not implemented yet, and
// the configuration validator refuses the flag that would claim otherwise.
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
