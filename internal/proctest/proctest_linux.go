//go:build linux

package proctest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// readStat is os.ReadFile; tests swap it to reach the read-error branches.
var readStat = os.ReadFile

// zombieOrExited reports whether /proc/<pid>/stat shows state Z or is gone (the process
// exited between Gone's kill check and this read).
func zombieOrExited(pid int) bool {
	b, err := readStat(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		// ESRCH: a zombie reaped between the open and the read of its stat file.
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ESRCH) {
			panic(fmt.Sprintf("proctest: read /proc/%d/stat: %v", pid, err))
		}
		return true
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // comm may contain ')'
	return i >= 0 && i+2 < len(s) && s[i+2] == 'Z'
}
