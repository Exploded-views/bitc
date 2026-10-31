package proc

import (
	"os"
	"testing"
)

func TestReadStatSelfAndParent(t *testing.T) {
	st, err := ReadStat(os.Getpid())
	if err != nil || st.Pid != os.Getpid() || st.PPid != os.Getppid() || st.Start == 0 {
		t.Fatalf("%+v %v", st, err)
	}
	pst, _ := ReadStat(os.Getppid())
	if !IsAncestor(os.Getppid(), pst.Start) {
		t.Fatal("parent is not an ancestor")
	}
	if IsAncestor(os.Getppid(), pst.Start+1) {
		t.Fatal("wrong start time accepted (a reused pid)")
	}
}
