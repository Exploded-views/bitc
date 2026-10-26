//go:build linux

package jobs

import (
	"fmt"
	"io"
	"os"
)

// lastByte is the log's last byte, read through the job's own fd (/proc/self/fd), so a rerun
// that replaced the path never confuses it; '\n' for an empty or unopenable log, 0 when unreadable.
func lastByte(log *os.File) byte {
	last := []byte{'\n'}
	if f, err := os.Open(fmt.Sprintf("/proc/self/fd/%d", log.Fd())); err == nil {
		if st, err := f.Stat(); err == nil && st.Size() > 0 {
			if _, err := f.ReadAt(last, st.Size()-1); err != nil && err != io.EOF {
				last[0] = 0
			}
		}
		f.Close()
	}
	return last[0]
}
