//go:build darwin

package lock

import "golang.org/x/sys/unix"

// inspectNoProcLocks asks fcntl(F_GETLK) on a read-only fd. On darwin flock(2) and fcntl locks
// share the vnode's lock list, so a held flock (exclusive or shared, any process) is
// reported as a conflict with l_pid -1, and nothing is locked: a concurrent non-blocking
// caller never sees a free lock made busy (#55). If F_GETLK fails, use the try-lock.
func inspectNoProcLocks(path string) State {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return StateFree
	}
	defer unix.Close(fd)
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0} // whole file
	if err := unix.FcntlFlock(uintptr(fd), unix.F_GETLK, &lk); err != nil {
		if Held(path) {
			return StateHeld
		}
		return StateFree
	}
	if inProbe != nil {
		inProbe()
	}
	if lk.Type != unix.F_UNLCK {
		return StateHeld
	}
	return StateFree
}

// HoldersVisible is always true on darwin: F_GETLK needs no procfs.
func HoldersVisible() bool { return true }
