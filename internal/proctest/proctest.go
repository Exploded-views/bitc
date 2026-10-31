// Package proctest tells tests whether a process is gone (standard library only). A zombie
// counts as gone: a killed orphan stays one until its new parent reaps it.
package proctest

import (
	"errors"
	"syscall"
)

// Gone reports whether pid no longer runs: kill(pid, 0) fails with ESRCH, or the OS reports
// the process as a zombie or already exited (/proc/<pid>/stat on Linux, ps on darwin).
func Gone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	return zombieOrExited(pid)
}
