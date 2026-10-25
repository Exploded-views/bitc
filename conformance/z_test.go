package conformance

import (
	"testing"

	"github.com/Exploded-views/bitc/internal/cli"
	"github.com/Exploded-views/bitc/internal/exit"
)

func TestZ1_LinuxOnly(t *testing.T) {
	if err := cli.PlatformCheck("darwin"); exit.Code(err) != 69 {
		t.Fatalf("%v", err)
	}
	if err := cli.PlatformCheck("linux"); err != nil {
		t.Fatal(err)
	}
}
