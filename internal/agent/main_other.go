//go:build !linux

package agent

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/Exploded-views/bitc/internal/exit"
)

// Main is `bitc __agent`. The remote agent (namespaces, mounts, seccomp, memfd re-exec) runs on
// Linux only: a non-Linux client build prints one line and exits 69 (docs/exit-codes.md).
func Main(_ []string, _ io.Writer) int {
	e := exit.LinuxOnly("the remote agent", runtime.GOOS)
	fmt.Fprintln(os.Stderr, e.Line)
	return e.Code
}
