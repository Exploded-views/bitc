//go:build darwin

package main

import (
	"os"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
)

// noNewPrivs: macOS has no no_new_privs bit to read, so the stub reports none.
func noNewPrivs() string { return "" }

// openFds lists "<fd> <path>" for the descriptors in /dev/fd, the path from fcntl(F_GETPATH); a
// descriptor with no path (a pipe, a kqueue) has an empty target.
func openFds() []string {
	ents, _ := os.ReadDir("/dev/fd")
	var fds []string
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		var buf [unix.PathMax]byte
		path := ""
		// #nosec G103 -- fcntl(F_GETPATH) takes a buffer pointer
		if _, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(n), uintptr(unix.F_GETPATH), uintptr(unsafe.Pointer(&buf[0]))); errno == 0 {
			for i, c := range buf {
				if c == 0 {
					path = string(buf[:i])
					break
				}
			}
		}
		fds = append(fds, e.Name()+" "+path)
	}
	return fds
}

// exePath is the running binary's path (os.Executable).
func exePath() string {
	p, _ := os.Executable()
	return p
}
