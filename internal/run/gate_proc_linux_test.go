package run

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"

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

// dieWithTest makes a helper process that a test starts from this test binary die with the test
// process (Pdeathsig SIGKILL), so a hang that ends in a -timeout panic does not leave it running.
// Pdeathsig is tied to the thread that starts the helper, which the Go runtime keeps for a test
// goroutine unless it locks it and exits.
func dieWithTest(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// openFds lists this process's open descriptors, without the one the listing itself uses.
func openFds(t *testing.T) map[int]fdInfo { return listFds(t, "/proc/self/fd") }

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
