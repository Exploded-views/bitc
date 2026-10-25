package proc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

func TestCommWithParensAndSpaces(t *testing.T) {
	// A process whose comm imitates stat fields must not shift the parse. comm is the basename
	// of the path passed to execve, so run sleep through a symlink named "x) S 1 1".
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "x) S 1 1")
	if err := os.Symlink(sleep, link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(link, "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", cmd.Process.Pid))
	if err != nil || !strings.Contains(string(comm), ")") {
		t.Fatalf("comm %q (%v): the child's comm has no ')', so the test proves nothing", comm, err)
	}
	// A just-exec'd child can be in D (loading its binary) on a busy host; a mis-parse shows up
	// in PPid and Pgrp, not in the state.
	st, err := ReadStat(cmd.Process.Pid)
	if err != nil || st.Pid != cmd.Process.Pid || st.PPid != os.Getpid() ||
		st.Pgrp != syscall.Getpgrp() || !strings.ContainsRune("SRD", rune(st.State)) || st.Start == 0 {
		t.Fatalf("%+v %v (a parse at the first ')' reads state S, ppid 1, pgrp 1)", st, err)
	}
}
