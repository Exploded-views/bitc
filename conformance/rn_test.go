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

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/proctest"
)

func TestS3_BackgroundedChildNeverKeepsTheSlot(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[host]\ngrace = \"300ms\"\n[pools.lint]\nwait = \"0s\"\n")
	pidFile := filepath.Join(h.Root, "bg")
	r := h.Run("run", "lint", "--", "sh", "-c", `sleep 30 & echo $! > "$1"`, "_", pidFile)
	if r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	if r := h.Run("run", "lint", "--", "true"); r.Code != 0 {
		t.Fatalf("slot still held: %+v", r)
	}
	pid := h.Atoi(string(must(os.ReadFile(pidFile))))
	h.Until(func() bool { return proctest.Gone(pid) }, 3*time.Second)
}

func TestY7_ForwardAndWaitKeepsTheHoldUntilCleanup(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[host]\ngrace = \"300ms\"\n[pools.e2e]\non_signal = \"forward-and-wait\"\nwait = \"0s\"\n[pools.lint]\nwait = \"0s\"\n")
	marker := filepath.Join(h.Root, "cleaned")
	ready := filepath.Join(h.Root, "ready")
	script := `trap 'sleep 0.8; : > "$1"; exit 0' TERM; : > "$2"; while :; do sleep 0.05; done`
	p := h.Start("run", "e2e", "--", "sh", "-c", script, "_", marker, ready)
	h.Until(func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second)
	_ = syscall.Kill(-p.Cmd.Process.Pid, syscall.SIGTERM) // a group signal, as a harness sends
	time.Sleep(300 * time.Millisecond)
	if r := h.Run("run", "e2e", "--", "true"); r.Code != 75 {
		t.Fatalf("the hold ended before the command's cleanup: %+v", r)
	}
	r := p.Wait()
	if _, err := os.Stat(marker); err != nil || r.Code != 0 {
		t.Fatalf("cleanup did not finish first: %+v", r)
	}
	// a terminate pool ends at once with 128+N
	q := h.Start("run", "lint", "--", "sh", "-c", `: > "$1"; sleep 30`, "_", ready+"2")
	h.Until(func() bool { _, err := os.Stat(ready + "2"); return err == nil }, 10*time.Second)
	t0 := time.Now()
	_ = syscall.Kill(-q.Cmd.Process.Pid, syscall.SIGTERM)
	if r := q.Wait(); r.Code != 143 || time.Since(t0) > 2*time.Second {
		t.Fatalf("%+v after %v", r, time.Since(t0))
	}
}

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

func TestRN2_SigkilledSupervisorLeavesNoProcess(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"5s\"\n")
	pids := filepath.Join(h.Root, "pids")
	p := h.Start("run", "lint", "--", "sh", "-c", `sleep 60 & echo "$$ $!" > "$1"; wait`, "_", pids)
	h.Until(func() bool { b, _ := os.ReadFile(pids); return strings.Contains(string(b), " ") }, 10*time.Second)
	f := strings.Fields(string(must(os.ReadFile(pids))))
	leader, bg := h.Atoi(f[0]), h.Atoi(f[1])
	_ = syscall.Kill(p.Cmd.Process.Pid, syscall.SIGKILL)
	h.Until(func() bool { return proctest.Gone(leader) }, 3*time.Second) // Pdeathsig
	if r := h.Run("run", "lint", "--", "true"); r.Code != 0 {            // prune reaps the rest
		t.Fatalf("%+v", r)
	}
	h.Until(func() bool { return proctest.Gone(bg) }, 3*time.Second)
}

func TestRN3_TerminateExits128PlusN(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[host]\ngrace = \"200ms\"\n[pools.lint]\n")
	ready := filepath.Join(h.Root, "ready")
	p := h.Start("run", "lint", "--", "sh", "-c", `trap "" INT; : > "$1"; sleep 30`, "_", ready)
	h.Until(func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second)
	p.Signal(syscall.SIGINT) // to bitc alone; the command ignores INT, so grace then SIGKILL
	if r := p.Wait(); r.Code != 130 {
		t.Fatalf("%+v", r)
	}
}

func TestRN4_ForwardAndWaitIsBounded(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[host]\ngrace = \"200ms\"\nmax_signal_wait = \"1s\"\n[pools.e2e]\non_signal = \"forward-and-wait\"\n")
	ready := filepath.Join(h.Root, "ready")
	p := h.Start("run", "e2e", "--", "sh", "-c", `trap "" TERM; : > "$1"; while :; do sleep 0.05; done`, "_", ready)
	h.Until(func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second)
	t0 := time.Now()
	p.Signal(syscall.SIGTERM)
	r := p.Wait()
	if r.Code != 143 || time.Since(t0) < time.Second || time.Since(t0) > 4*time.Second {
		t.Fatalf("%+v after %v", r, time.Since(t0))
	}
}

// Review Focus 4.
func TestRN5_ClosedPipeAndFlood(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"0s\"\n")
	sh := exec.Command("bash", "-c", `set -o pipefail; "$1" run lint -- yes | head -1; echo "rc=$?"`, "_", harness.Bin)
	sh.Env, sh.Dir = h.Env, h.Root
	out, _ := sh.Output()
	if string(out) != "y\nrc=141\n" {
		t.Fatalf("%q", out)
	}
	if r := h.Run("run", "lint", "--", "true"); r.Code != 0 {
		t.Fatalf("slot held after SIGPIPE: %+v", r)
	}
	flood := exec.Command("bash", "-c", `"$1" run lint -- head -c 50000000 /dev/zero | wc -c`, "_", harness.Bin)
	flood.Env, flood.Dir = h.Env, h.Root
	t0 := time.Now()
	out, err := flood.Output()
	if err != nil || strings.TrimSpace(string(out)) != "50000000" || time.Since(t0) > 10*time.Second {
		t.Fatalf("%q %v %v", out, err, time.Since(t0))
	}
}

func TestRN6_CommandNotFoundOrNotExecutable(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	r := h.Run("run", "lint", "--", "no-such-command-bitc")
	if r.Code != 127 || !strings.HasPrefix(r.Stderr, "bitc: no-such-command-bitc: ") {
		t.Fatalf("%+v", r)
	}
	f := filepath.Join(h.Root, "noexec.sh")
	h.Write(f, "#!/bin/sh\n", 0o644)
	if r := h.Run("run", "lint", "--", f); r.Code != 126 {
		t.Fatalf("%+v", r)
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

func TestRN8_RepeatedSignalsNeverPostponeTheKill(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[host]\ngrace = \"1500ms\"\n[pools.lint]\nwait = \"0s\"\n")
	ready := filepath.Join(h.Root, "ready")
	p := h.Start("run", "lint", "--", "sh", "-c", `trap "" TERM; : > "$1"; while :; do sleep 0.05; done`, "_", ready)
	h.Until(func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second)
	t0 := time.Now()
	for time.Since(t0) < 6*time.Second && !p.Exited() {
		p.Signal(syscall.SIGTERM)
		time.Sleep(300 * time.Millisecond)
	}
	if !p.Exited() {
		p.Signal(syscall.SIGKILL)
		t.Fatal("a TERM every 300 ms kept pushing the SIGKILL back past grace")
	}
	if r := p.Wait(); r.Code != 143 {
		t.Fatalf("%+v", r)
	}
}
