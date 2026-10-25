// Package proctest tells tests whether a process is gone (standard library only). A zombie
// counts as gone: a killed orphan stays one until its new parent reaps it.
package proctest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// Gone reports whether pid no longer runs: kill(pid, 0) fails with ESRCH, or
// /proc/<pid>/stat shows state Z.
func Gone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return errors.Is(err, fs.ErrNotExist) // exited between the two checks
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // comm may contain ')'
	return i >= 0 && i+2 < len(s) && s[i+2] == 'Z'
}
