//go:build !linux

package run

import (
	"syscall"

	"golang.org/x/sys/unix"
)

const ioctlGetTermios = unix.TIOCGETA

// childAttr off Linux: there is no parent-death signal, so a command outlives a bitc that was
// SIGKILLed (a clean exit, a signal or a deadline still ends it).
func childAttr(setpgid bool) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: setpgid}
}
