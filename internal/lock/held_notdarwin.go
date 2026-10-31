//go:build !darwin

package lock

import (
	"errors"

	"golang.org/x/sys/unix"
)

// heldNoProcLocks is HeldRO without /proc/locks: a brief try-lock (Linux cannot see a flock
// through F_GETLK).
func heldNoProcLocks(path string) bool {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.Is(err, unix.EWOULDBLOCK)
	}
	if inProbe != nil {
		inProbe()
	}
	_ = unix.Flock(fd, unix.LOCK_UN)
	return false
}
