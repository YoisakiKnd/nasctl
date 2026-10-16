// Command nasctl is the OneCloud Hub console: a unified view of the machine, its
// files, the album index, the watch library, the backup's state and the
// console's own tasks.
//
// Album reading is in process. Explicit bounded album processing re-executes
// this same binary as `nasctl album ...` in a separate, supervised process; web
// handlers cannot import its scanner/decoder. Writable storage roots support
// audited operator file operations.
package main

import (
	"fmt"
	"os"

	"github.com/YoisakiKnd/nasctl/internal/cli"
	"github.com/YoisakiKnd/nasctl/internal/launchgate"
	"github.com/YoisakiKnd/nasctl/internal/nasctl"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "album":
			os.Exit(runAlbum(args[1:]))
		case "exec-album-task":
			// The console starts a bounded thumbnail supervisor here and holds it
			// at the private gate until it is inside its task cgroup.
			if len(args) < 3 || args[1] != "album" || args[2] != "thumbs" {
				fmt.Fprintln(os.Stderr, "album launch: only bounded thumbnail supervisors may use the private gate")
				os.Exit(2)
			}
			if e := launchgate.Wait(os.NewFile(3, "nasctl-admission")); e != nil {
				fmt.Fprintln(os.Stderr, e)
				os.Exit(3)
			}
			os.Exit(runAlbum(args[2:]))
		case "exec-worker":
			// One isolated decoder, spawned by the thumbnail supervisor.
			os.Exit(runAlbum(args))
		}
	}
	os.Exit(nasctl.Main(args, nasctl.Env{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}))
}

func runAlbum(args []string) int {
	return cli.Run(args, cli.Env{
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
		Stdin:    os.Stdin,
		Getenv:   os.Getenv,
		ProcRoot: "/proc",
	})
}
