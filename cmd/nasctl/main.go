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
	"strings"

	"github.com/YoisakiKnd/nasctl/internal/cli"
	"github.com/YoisakiKnd/nasctl/internal/launchgate"
	"github.com/YoisakiKnd/nasctl/internal/nasctl"
)

func main() {
	args := os.Args[1:]
	if rest, ok := albumCommand(args); ok {
		os.Exit(runAlbum(rest))
	}
	if len(args) > 0 {
		switch args[0] {
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

// albumCommand recognises `nasctl [--config PATH] [--json] album ...`, so the
// global flags may precede the verb as they do for every other command. They
// are moved after the album arguments, where the album command line reads them.
func albumCommand(args []string) ([]string, bool) {
	var globals []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "album":
			return append(append([]string{}, args[i+1:]...), globals...), true
		case (a == "--config" || a == "-c") && i+1 < len(args):
			globals = append(globals, "--config", args[i+1])
			i++
		case strings.HasPrefix(a, "--config=") || a == "--json":
			globals = append(globals, a)
		default:
			return nil, false
		}
	}
	return nil, false
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
