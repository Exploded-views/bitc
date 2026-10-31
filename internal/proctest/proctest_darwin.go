//go:build darwin

package proctest

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// zombieOrExited asks ps(1) for pid's state: macOS has no /proc, and the standard library has
// no kinfo_proc reader. ps exits 1 with no output at all for a pid that does not exist; a state
// starting with Z is a zombie. Any other ps diagnostic leaves the process counted as running;
// a ps that cannot run panics.
func zombieOrExited(pid int) bool {
	out, err := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			// ps could not run: neither answer is honest, and a wrong "running" would let an
			// expected-alive check pass silently.
			panic(fmt.Sprintf("proctest: /bin/ps -p %d could not run: %v", pid, err))
		}
		return strings.TrimSpace(string(out)) == "" && len(ee.Stderr) == 0
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
