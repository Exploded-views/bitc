//go:build darwin

package lock

import "golang.org/x/sys/unix"

// heldNoProcLocks asks fcntl(F_GETLK) on a read-only fd. On darwin flock(2) and fcntl locks
// share the vnode's lock list, so a held flock (exclusive or shared, any process) is
// reported as a conflict with l_pid -1, and nothing is locked: a concurrent non-blocking
// caller never sees a free lock made busy (#55). If F_GETLK fails, use the try-lock.
func heldNoProcLocks(path string) bool {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0} // whole file
	if err := unix.FcntlFlock(uintptr(fd), unix.F_GETLK, &lk); err != nil {
		return Held(path)
	}
	if inProbe != nil {
		inProbe()
	}
	return lk.Type != unix.F_UNLCK
}
