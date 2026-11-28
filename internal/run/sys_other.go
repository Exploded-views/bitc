//go:build !linux

package run

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const ioctlGetTermios = unix.TIOCGETA

// fdDir lists the open descriptors of this process.
const fdDir = "/dev/fd"

// selfExe is the path the exec gate re-executes.
func selfExe() (string, error) { return os.Executable() }

// childAttr off Linux: there is no parent-death signal, so a command outlives a bitc that was
// SIGKILLed (a clean exit, a signal or a deadline still ends it).
func childAttr(setpgid bool) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: setpgid}
}
