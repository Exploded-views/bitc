package run

import (
	"os/exec"
	"syscall"
	"testing"
	"unsafe"
)

// waitidNoReap blocks until the child pid has exited and leaves it a zombie: waitid(P_PID, pid,
// &info, WEXITED|WNOWAIT). x/sys has no darwin waitid, so it is the raw system call (173), with
// the constants of <sys/wait.h>; siginfo_t is 104 bytes there, and the buffer is larger.
func waitidNoReap(pid int) error {
	const sysWaitid, pPid, wExited, wNoWait = 173, 1, 0x4, 0x20
	var info [256]byte
	for {
		_, _, e := syscall.Syscall6(sysWaitid, pPid, uintptr(pid), uintptr(unsafe.Pointer(&info[0])), wExited|wNoWait, 0, 0)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		return nil
	}
}

// XNU's NOTE_EXIT on a process that has already exited but is not reaped (a zombie): waitExit must
// return for it, not wait for a reap that only this test does afterwards. The test blocks in
// waitid(WEXITED|WNOWAIT) until the child has exited, which leaves it a zombie, then calls
// waitExit. No timer: if the kqueue never reports the zombie, go test's -timeout reports the hang.
func TestWaitExitReturnsForAZombie(t *testing.T) {
	c := exec.Command("true")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pid := c.Process.Pid
	if err := waitidNoReap(pid); err != nil {
		_ = c.Wait()
		t.Fatalf("waitid: %v", err)
	}
	err := waitExit(pid) // the child is a zombie now: exited, not reaped
	if werr := c.Wait(); werr != nil {
		t.Errorf("reaping the child: %v", werr)
	}
	if err != nil {
		t.Errorf("waitExit on a zombie: %v", err)
	}
}
