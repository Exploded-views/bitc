// Linux-only: opens a pty with Linux ioctls (TIOCSPTLCK, TIOCGPTN), which do not exist on darwin.

package conformance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/proctest"
	"golang.org/x/sys/unix"
)

func TestRN1_TTYStdinKeepsTheCallersGroup(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()
	out := filepath.Join(h.Root, "pgid")
	cmd := exec.Command(harness.Bin, "run", "lint", "--", "sh", "-c", `cut -d" " -f5 /proc/$$/stat > "$1"`, "_", out)
	cmd.Env, cmd.Dir, cmd.Stdin = h.Env, h.Root, slave
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(must(os.ReadFile(out))))
	if got != strconv.Itoa(syscall.Getpgrp()) {
		t.Fatalf("command pgid %s, caller pgid %d", got, syscall.Getpgrp())
	}
}

// Cleanup rail: a TTY hold records only the command (pgid 0), so the reap after a dead
// supervisor never kills other members of the caller's process group.
func TestRN7_TTYHoldReapsOnlyTheCommandTree(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"5s\"\n")
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()
	pids := filepath.Join(h.Root, "pids")
	cmd := exec.Command(harness.Bin, "run", "lint", "--", "sh", "-c", `echo $$ > "$1"; sleep 60`, "_", pids)
	cmd.Env, cmd.Dir, cmd.Stdin = h.Env, h.Root, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // bitc leads the "caller's group"
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h.Track(cmd.Process.Pid)
	h.Until(func() bool { b, _ := os.ReadFile(pids); return len(strings.TrimSpace(string(b))) > 0 }, 10*time.Second)
	leader := h.Atoi(string(must(os.ReadFile(pids))))
	// bitc records the leader in the ticket after the command starts: kill it only once it has
	h.Until(func() bool {
		for _, n := range tickets(h, "lint") {
			b, _ := os.ReadFile(filepath.Join(h.State, "pools", "lint", "tickets", n))
			if strings.Contains(string(b), fmt.Sprintf(`"leader":%d`, leader)) {
				return true
			}
		}
		return false
	}, 10*time.Second)
	sib := exec.Command("sleep", "60") // joins the caller's group after the command started
	sib.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: cmd.Process.Pid}
	if err := sib.Start(); err != nil {
		t.Fatal(err)
	}
	h.Track(sib.Process.Pid)
	defer func() { _ = sib.Process.Kill(); _ = sib.Wait() }()
	_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
	h.Until(func() bool { return proctest.Gone(leader) }, 3*time.Second) // Pdeathsig
	r := h.Run("run", "lint", "--", "true")                              // prune reaps the dead hold
	if r.Code != 0 || !strings.Contains(r.Stderr, "bitc: reaped orphaned lint job") {
		t.Fatalf("%+v", r)
	}
	if proctest.Gone(sib.Process.Pid) {
		t.Fatal("the reap killed a process of the caller's group outside the command")
	}
}

func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no /dev/ptmx:", err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skip(err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skip(err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip(err)
	}
	return m, s
}
