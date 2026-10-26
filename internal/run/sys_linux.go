//go:build linux

package run

import (
	"syscall"

	"golang.org/x/sys/unix"
)

const ioctlGetTermios = unix.TCGETS

// childAttr: the command's own group unless it shares the caller's terminal, and SIGKILL if
// bitc dies first.
func childAttr(setpgid bool) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: setpgid, Pdeathsig: syscall.SIGKILL}
}
