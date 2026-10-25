//go:build liveprobe

// Command bitc-sprites-probe runs the Sprites probe kit against the live Sprites API.
// It is not part of make ci or the release builds; maintainers run it live.
package main

import (
	"os"

	"github.com/Exploded-views/bitc/internal/provider/sprites/liveprobe"
)

func main() { os.Exit(liveprobe.Main(os.Args[1:], os.Stdout, os.Stderr)) }
