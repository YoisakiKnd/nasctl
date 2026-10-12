// Command albumctl is a small, careful photo index for a low-resource ARM NAS.
//
// Nothing in this program moves, renames, deletes or rewrites an original file,
// and nothing starts an unbounded job. The heavy work — scanning a directory
// tree and decoding images — is always a bounded task with an explicit limit,
// a wall-clock budget and a resource guard that can stop it.
package main

import (
	"os"

	"github.com/YoisakiKnd/nasctl/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.Env{
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
		Stdin:    os.Stdin,
		Getenv:   os.Getenv,
		ProcRoot: "/proc",
	}))
}
