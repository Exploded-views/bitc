package remote

import (
	"os"

	"golang.org/x/sys/unix"
)

const stdinData = "the command's stdin has data waiting, which a remote job cannot read"

// StdinIdle: /dev/null, a TTY, an empty regular file, or a pipe/socket with nothing readable on
// a zero-timeout poll(2) may go remote (spec 8.4).
func StdinIdle(f *os.File) (bool, string) {
	if f == nil {
		return true, ""
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return true, ""
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		if st.Size == 0 {
			return true, ""
		}
		return false, stdinData
	case unix.S_IFIFO, unix.S_IFSOCK:
		fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 0)
		if err == nil && n > 0 && fds[0].Revents&unix.POLLIN != 0 {
			return false, stdinData
		}
	}
	return true, "" // /dev/null, a TTY, anything else
}
