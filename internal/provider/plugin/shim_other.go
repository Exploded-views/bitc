//go:build !linux && !darwin

package plugin

import (
	"fmt"
	"os"
	"runtime"

	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider"
)

// shimLaunch is unreached: a build for another OS refuses every [providers.*] table at load.
func (p *Provider) shimLaunch(_, _ *os.File, _ dirs) (shimSpec, error) {
	e := exit.LinuxOnly("a plugin provider", runtime.GOOS)
	return shimSpec{}, &provider.ErrUnavailable{Why: e.Line, Local: true}
}

// ExecShim is `bitc __plugin-exec`: it needs PR_SET_NO_NEW_PRIVS and execveat, so plugin
// providers run on Linux only; a non-Linux build prints one line and exits 69.
func ExecShim([]string) int {
	e := exit.LinuxOnly("a plugin provider", runtime.GOOS)
	fmt.Fprintln(os.Stderr, e.Line)
	return e.Code
}
