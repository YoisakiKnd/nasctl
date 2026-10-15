// Command albumctl is a small, careful photo index for a low-resource ARM NAS.
//
// Nothing in this program moves, renames, deletes or rewrites an original file,
// and nothing starts an unbounded job. The heavy work — scanning a directory
// tree and decoding images — is always a bounded task with an explicit limit,
// a wall-clock budget and a resource guard that can stop it.
package main

import (
	"fmt"
	"os"

	"github.com/YoisakiKnd/nasctl/internal/cli"
	"github.com/YoisakiKnd/nasctl/internal/launchgate"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "exec-album-task" {
		if len(args) < 2 || args[1] != "thumbs" {
			fmt.Fprintln(os.Stderr, "album launch: only bounded thumbnail supervisors may use the private gate")
			os.Exit(2)
		}
		if e := launchgate.Wait(os.NewFile(3, "nasctl-admission")); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(3)
		}
		args = args[1:]
	}
	os.Exit(cli.Run(args, cli.Env{
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
		Stdin:    os.Stdin,
		Getenv:   os.Getenv,
		ProcRoot: "/proc",
	}))
}
