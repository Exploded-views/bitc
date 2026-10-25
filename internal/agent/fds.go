package agent

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// cloexecInherited marks every fd above 2 close-on-exec, so the job and its setup steps inherit
// only stdin, stdout and stderr (spec 8.4) even when whatever started the agent leaked fds into
// it: a CI runner's pipes, an init system's sockets. The agent keeps using its own fds; files
// passed on purpose go through exec.Cmd.ExtraFiles, which re-dups them for the child.
func cloexecInherited() {
	if unix.CloseRange(3, ^uint(0), unix.CLOSE_RANGE_CLOEXEC) == nil {
		return
	}
	// kernels before 5.11: walk the fd table
	d, err := os.Open("/proc/self/fd")
	if err != nil {
		return
	}
	names, _ := d.Readdirnames(-1)
	_ = d.Close()
	for _, n := range names {
		if fd, err := strconv.Atoi(n); err == nil && fd > 2 {
			unix.CloseOnExec(fd)
		}
	}
}
