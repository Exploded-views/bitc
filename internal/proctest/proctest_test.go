package proctest

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestRunningIsNotGone(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if Gone(cmd.Process.Pid) || Gone(os.Getpid()) {
		t.Fatal("a running process reported gone")
	}
}

func TestZombieIsGone(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() }) // reaped only at the end: a zombie until then
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !Gone(pid) {
		if time.Now().After(deadline) {
			t.Fatal("a killed, unreaped child is not reported gone")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("kill(pid, 0) = %v; the child must still be an unreaped zombie here", err)
	}
}

func TestReapedIsGone(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if !Gone(cmd.Process.Pid) {
		t.Fatal("a reaped process reported alive")
	}
}

// The darwin guard (#48): macOS has no /proc, and a missing /proc/<pid>/stat once made every
// live process read as gone. The name carries "Darwin" so CI's macOS step selects it.
func TestALiveProcessIsNotGoneDarwinGuard(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for name, pid := range map[string]int{"child": cmd.Process.Pid, "self": os.Getpid(), "parent": os.Getppid()} {
		if Gone(pid) {
			t.Errorf("live %s process %d reported gone", name, pid)
		}
	}
}
