package proc

import (
	"os"
	"slices"
	"testing"
)

func TestDarwinReadStatSelf(t *testing.T) {
	st, err := ReadStat(os.Getpid())
	if err != nil || st.Pid != os.Getpid() || st.PPid != os.Getppid() || st.Start == 0 {
		t.Fatalf("%+v %v", st, err)
	}
	if again, _ := ReadStat(os.Getpid()); again.Start != st.Start {
		t.Fatalf("start time not stable: %d then %d", st.Start, again.Start)
	}
	if !IsAncestor(os.Getpid(), st.Start) || !slices.Contains(Pids(), os.Getpid()) {
		t.Fatal("self is not found")
	}
	if _, err := ReadStat(1 << 30); err == nil {
		t.Fatal("a pid that does not exist has a stat")
	}
}
