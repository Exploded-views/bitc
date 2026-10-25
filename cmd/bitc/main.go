// Command bitc is a daemonless job gate for a shared Linux host.
package main

import (
	"os"

	"github.com/Exploded-views/bitc/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], cli.Std{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}))
}
