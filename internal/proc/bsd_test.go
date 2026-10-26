package proc

import "testing"

// macOS has no /proc: the same Stat comes from sysctl kern.proc (struct kinfo_proc). The
// start time is the process's start in microseconds, an opaque identity compared for equality.
func TestStatFromKinfo(t *testing.T) {
	st := statFromKinfo(4242, 1, 4242, 3, 1_700_000_000, 123456)
	if st.Pid != 4242 || st.PPid != 1 || st.Pgrp != 4242 || st.State != 'S' || st.Start != 1_700_000_000_123_456 {
		t.Fatalf("%+v", st)
	}
	for stat, want := range map[int8]byte{1: 'R', 2: 'R', 3: 'S', 4: 'T', 5: 'Z', 0: '?', 9: '?'} {
		if got := statFromKinfo(1, 0, 1, stat, 1, 0).State; got != want {
			t.Errorf("p_stat %d: state %q, want %q", stat, got, want)
		}
	}
	// a later start in the same second still differs
	if statFromKinfo(1, 0, 1, 2, 5, 1).Start == statFromKinfo(1, 0, 1, 2, 5, 2).Start {
		t.Fatal("start times within one second collide")
	}
}
