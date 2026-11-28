package run

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// isGate reports whether the process at pid is still the exec gate (this test binary), not the
// command it execs: its p_comm is the test binary's base name, cut to the 16 bytes the kernel
// keeps. It is read once, with no poll: Go's cmd.Start returns only after the exec has succeeded,
// so without a gate the started process is already the command.
func isGate(pid int) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(k.Proc.P_pid) != pid {
		return false
	}
	var comm []byte
	for _, c := range k.Proc.P_comm {
		if c == 0 {
			break
		}
		comm = append(comm, byte(c))
	}
	want := filepath.Base(self)
	if len(want) > 16 {
		want = want[:16]
	}
	return string(comm) == want
}

// waitExit blocks until the process pid has exited, through a kqueue EVFILT_PROC NOTE_EXIT with no
// timeout. A process that is already gone is not an error.
func waitExit(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	change := []unix.Kevent_t{{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, change, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if events[0].Flags&unix.EV_ERROR != 0 {
			if errno := unix.Errno(events[0].Data); errno != unix.ESRCH {
				return errno
			}
		}
		return nil
	}
}
