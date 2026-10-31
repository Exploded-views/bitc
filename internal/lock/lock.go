// Package lock wraps flock(2) on O_CLOEXEC files (spec 6.2, 9).
package lock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// File is an open lock file. Locks belong to the open file description (flock semantics),
// so two Opens of one path exclude each other even within one process.
type File struct {
	f    *os.File
	Path string
}

// Open opens (creating 0600, parents 0700) path with O_CLOEXEC.
func Open(path string) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return &File{f: os.NewFile(uintptr(fd), path), Path: path}, nil
}

func (l *File) Fd() int { return int(l.f.Fd()) }

// TryLock takes an exclusive lock without blocking.
func (l *File) TryLock() (bool, error) {
	err := unix.Flock(l.Fd(), unix.LOCK_EX|unix.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EWOULDBLOCK):
		return false, nil
	default:
		return false, err
	}
}

// LockWait retries TryLock every 5 ms until d passes or ctx ends (no unbounded blocking).
func (l *File) LockWait(ctx context.Context, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		ok, err := l.TryLock()
		if err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for " + l.Path)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (l *File) Unlock() error { return unix.Flock(l.Fd(), unix.LOCK_UN) }
func (l *File) Close() error  { return l.f.Close() }

// Rewrite replaces the file's content in place (pwrite + ftruncate), keeping the inode and lock.
func (l *File) Rewrite(b []byte) error {
	if _, err := l.f.WriteAt(b, 0); err != nil {
		return err
	}
	return l.f.Truncate(int64(len(b)))
}

// Held reports whether another open file description holds path's lock. A missing file is
// not held. It never creates the file.
func Held(path string) bool {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.Is(err, unix.EWOULDBLOCK)
	}
	_ = unix.Flock(fd, unix.LOCK_UN)
	return false
}

func fcntlGetfd(fd int) (int, error) { return unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0) }

// inProbe is a test seam: HeldRO calls it while it inspects the lock, at the latest point
// where a lock taken by the probe itself would still be in force. It is a plain variable: a test
// that sets it must not run in parallel, and no HeldRO caller may outlive the test that started it.
var inProbe func()

// procLocksPath is where HeldRO reads the kernel's lock table; a test points it elsewhere to reach
// the fallback. The same rules as inProbe apply: only tests write it, never in parallel.
var procLocksPath = "/proc/locks"

// HeldRO reports whether any process holds a flock on path without making a free lock look busy
// to a concurrent non-blocking caller (#55). Linux reads /proc/locks, matching the device and
// inode it prints; darwin asks fcntl(F_GETLK), which sees flock locks there. Neither takes the
// lock. A missing file is not held. Without /proc/locks, Linux falls back to a brief try-lock,
// which can make a free lock look busy for that moment; darwin uses it only if F_GETLK fails.
// Locks of processes outside this pid namespace are not listed in /proc/locks.
func HeldRO(path string) bool {
	var st unix.Stat_t
	if unix.Lstat(path, &st) != nil {
		return false
	}
	b, err := os.ReadFile(procLocksPath)
	if err != nil {
		return heldNoProcLocks(path)
	}
	if inProbe != nil {
		inProbe()
	}
	maj, mnr := devMajorMinor(&st)
	for _, line := range strings.Split(string(b), "\n") {
		// "1: FLOCK  ADVISORY  WRITE 1234 00:2b:5678 0 EOF"; a blocked waiter has "->" in f[1]
		f := strings.Fields(line)
		if len(f) < 6 || f[1] != "FLOCK" {
			continue
		}
		id := strings.Split(f[5], ":")
		if len(id) != 3 {
			continue
		}
		ma, e1 := strconv.ParseUint(id[0], 16, 32)
		mi, e2 := strconv.ParseUint(id[1], 16, 32)
		ino, e3 := strconv.ParseUint(id[2], 10, 64)
		if e1 == nil && e2 == nil && e3 == nil && uint32(ma) == maj && uint32(mi) == mnr && ino == st.Ino {
			return true
		}
	}
	return false
}
