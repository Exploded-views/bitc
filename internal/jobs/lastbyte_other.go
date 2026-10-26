//go:build !linux

package jobs

import "os"

// lastByte off Linux cannot reopen the job's own write-only fd for reading (no /proc/self/fd),
// so it reports 0, "unknown": Terminate then always starts the marker on a fresh line (at worst
// after an empty one), never glued to the command's last line.
func lastByte(*os.File) byte { return 0 }
