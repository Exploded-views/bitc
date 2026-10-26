//go:build linux

package plugin

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// pinFD is the descriptor the shim finds the pinned plugin on (fd 3 is the secrets pipe, A.3.3).
const pinFD = 4

// ExecShim is the body of `bitc __plugin-exec <path>` (providers delta A.2.4): Go's SysProcAttr
// has no PR_SET_NO_NEW_PRIVS and no execveat, so the adapter starts bitc's own binary here with
// the open, hashed plugin file as fd 4. The shim sets no-new-privs on this thread (inherited
// across the exec), marks fd 4 close-on-exec, and execs that same descriptor with
// execveat(AT_EMPTY_PATH): the bytes that were hashed are the bytes that run. It returns only on
// failure; the environment it was started with is the plugin's environment.
func ExecShim(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "bitc: __plugin-exec: usage: __plugin-exec <path>")
		return 64
	}
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, "bitc: __plugin-exec: no-new-privs:", err)
		return 69
	}
	unix.CloseOnExec(pinFD)
	argv, err := syscall.SlicePtrFromStrings([]string{args[0]})
	if err == nil {
		var envv []*byte
		if envv, err = syscall.SlicePtrFromStrings(os.Environ()); err == nil {
			empty, _ := syscall.BytePtrFromString("")
			// #nosec G103 -- raw execveat: no wrapper exists in x/sys/unix
			_, _, errno := unix.Syscall6(unix.SYS_EXECVEAT, pinFD, uintptr(unsafe.Pointer(empty)),
				uintptr(unsafe.Pointer(&argv[0])), uintptr(unsafe.Pointer(&envv[0])), unix.AT_EMPTY_PATH, 0)
			err = errno
		}
	}
	fmt.Fprintln(os.Stderr, "bitc: __plugin-exec: execveat:", err)
	return 69
}
