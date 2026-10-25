// Command sshfakegate is the fake ssh host gate the conformance suite runs in place of ssh
// (providers delta C.11): `sshfakegate --root DIR -- <gate command line>`.
package main

import (
	"os"

	"github.com/Exploded-views/bitc/internal/provider/ssh/sshfake"
)

func main() { os.Exit(sshfake.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
