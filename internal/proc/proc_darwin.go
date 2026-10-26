//go:build darwin

package proc

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ReadStat reads pid's struct kinfo_proc (sysctl kern.proc.pid): macOS has no /proc.
func ReadStat(pid int) (Stat, error) {
	if pid <= 0 {
		return Stat{}, fmt.Errorf("pid %d: no such process", pid)
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return Stat{}, fmt.Errorf("kern.proc.pid.%d: %w", pid, err)
	}
	if int(k.Proc.P_pid) != pid {
		return Stat{}, fmt.Errorf("kern.proc.pid.%d: no such process", pid)
	}
	return statFromKinfo(pid, int(k.Eproc.Ppid), int(k.Eproc.Pgid), int8(k.Proc.P_stat),
		int64(k.Proc.P_starttime.Sec), int32(k.Proc.P_starttime.Usec)), nil
}

// Pids lists every process (sysctl kern.proc.all).
func Pids() []int {
	ks, _ := unix.SysctlKinfoProcSlice("kern.proc.all")
	out := make([]int, 0, len(ks))
	for i := range ks {
		out = append(out, int(ks[i].Proc.P_pid))
	}
	return out
}
