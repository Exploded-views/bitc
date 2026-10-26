package conformance

import (
	"testing"

	"github.com/Exploded-views/bitc/internal/cli"
	"github.com/Exploded-views/bitc/internal/exit"
)

// Linux, and macOS as a client build (local pools; Linux-only features refused); every other
// OS exits 69.
func TestZ1_LinuxAndADarwinClientOnly(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		if err := cli.PlatformCheck(goos); err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
	}
	for _, goos := range []string{"windows", "freebsd", "openbsd", "netbsd", "illumos", ""} {
		if err := cli.PlatformCheck(goos); exit.Code(err) != 69 {
			t.Fatalf("%s: %v", goos, err)
		}
	}
}
