package proc

// statFromKinfo is a Stat from the fields of a BSD struct kinfo_proc: p_stat (SIDL 1, SRUN 2,
// SSLEEP 3, SSTOP 4, SZOMB 5) mapped to the /proc state letters, and p_starttime folded into
// microseconds. Start is an identity compared for equality, never a clock-tick count.
func statFromKinfo(pid, ppid, pgid int, pstat int8, sec int64, usec int32) Stat {
	state := byte('?')
	switch pstat {
	case 1, 2:
		state = 'R'
	case 3:
		state = 'S'
	case 4:
		state = 'T'
	case 5:
		state = 'Z'
	}
	return Stat{Pid: pid, PPid: ppid, Pgrp: pgid, State: state, Start: uint64(sec)*1_000_000 + uint64(usec)}
}
