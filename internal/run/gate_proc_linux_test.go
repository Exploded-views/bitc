package run

import (
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// isGate reports whether the process at pid is still the exec gate (this test binary), not the
// command it execs. It is read once, with no poll: Go's cmd.Start returns only after the exec has
// succeeded, so without a gate the started process is already the command.
func isGate(pid int) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	return err == nil && exe == self
}

// waitExit blocks until the process pid has exited (it may still be a zombie), through a pidfd and
// a poll with no timeout. A process that is already gone is not an error.
func waitExit(pid int) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(fds, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}
