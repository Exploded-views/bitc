//go:build linux

package proc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ReadStat parses /proc/<pid>/stat; the comm field ends at the last ')'.
func ReadStat(pid int) (Stat, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Stat{}, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return Stat{}, fmt.Errorf("/proc/%d/stat: malformed", pid)
	}
	f := strings.Fields(s[i+2:]) // f[0] is field 3 (state)
	if len(f) < 20 {
		return Stat{}, fmt.Errorf("/proc/%d/stat: short", pid)
	}
	ppid, _ := strconv.Atoi(f[1])
	pgrp, _ := strconv.Atoi(f[2])
	start, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return Stat{}, fmt.Errorf("/proc/%d/stat: start time: %w", pid, err)
	}
	return Stat{Pid: pid, PPid: ppid, Pgrp: pgrp, State: f[0][0], Start: start}, nil
}

// Pids lists the numeric entries of /proc.
func Pids() []int {
	ents, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range ents {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, n)
		}
	}
	return out
}
