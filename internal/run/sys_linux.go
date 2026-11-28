//go:build linux

package run

import (
	"syscall"

	"golang.org/x/sys/unix"
)

const ioctlGetTermios = unix.TCGETS

// fdDir lists the open descriptors of this process.
const fdDir = "/proc/self/fd"

// selfExe is the path the exec gate re-executes: /proc/self/exe, which names this binary even if
// its file has been replaced since it started. /proc is required here anyway: the leader's start
// time is read from /proc/<pid>/stat.
func selfExe() (string, error) { return "/proc/self/exe", nil }

// childAttr: the command's own group unless it shares the caller's terminal, and SIGKILL if
// bitc dies first.
func childAttr(setpgid bool) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: setpgid, Pdeathsig: syscall.SIGKILL}
}
