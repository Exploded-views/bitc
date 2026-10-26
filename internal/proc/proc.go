// Package proc reads process ancestry and groups (spec 6.2, 6.6): /proc on Linux, sysctl
// kern.proc on darwin.
package proc

import "os"

// Stat is the part of /proc/<pid>/stat bitc uses (on darwin, the same fields of kinfo_proc).
type Stat struct {
	Pid, PPid, Pgrp int
	State           byte
	Start           uint64 // field 22, clock ticks since boot
}

// IsAncestor reports whether (pid, start) is this process or one of its ancestors.
func IsAncestor(pid int, start uint64) bool {
	cur := os.Getpid()
	for depth := 0; depth <= 256 && cur > 0; depth++ {
		st, err := ReadStat(cur)
		if err != nil {
			return false
		}
		if st.Pid == pid && st.Start == start {
			return true
		}
		cur = st.PPid
	}
	return false
}
