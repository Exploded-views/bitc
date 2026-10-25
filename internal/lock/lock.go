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

// HeldRO reports whether any process holds a flock on path, read from /proc/locks without
// taking the lock, so an observer never makes a free lock look busy to a concurrent
// non-blocking caller. It matches the device and inode /proc/locks prints. A missing file is
// not held; if /proc/locks is unreadable it falls back to Held. Locks of processes outside
// this pid namespace are not listed there.
func HeldRO(path string) bool {
	var st unix.Stat_t
	if unix.Lstat(path, &st) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/locks")
	if err != nil {
		return Held(path)
	}
	maj, mnr := unix.Major(st.Dev), unix.Minor(st.Dev)
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
