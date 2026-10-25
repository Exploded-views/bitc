package run

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/proctest"
)

func spec(t *testing.T, argv ...string) ExecSpec {
	t.Helper()
	devnull, _ := os.Open(os.DevNull)
	t.Cleanup(func() { devnull.Close() })
	return ExecSpec{Argv: argv, Env: os.Environ(), Dir: t.TempDir(), Stdin: devnull, Stdout: os.Stdout, Stderr: os.Stderr,
		OnSignal: "terminate", Grace: 300 * time.Millisecond, MaxSignalWait: 2 * time.Second}
}

func TestExitCodesAndSignals(t *testing.T) {
	for script, want := range map[string]int{"exit 0": 0, "exit 9": 9, "kill -TERM $$": 143, "kill -KILL $$": 137} {
		r, err := Exec(context.Background(), spec(t, "sh", "-c", script))
		if err != nil || r.Code != want {
			t.Errorf("%q: %+v %v, want %d", script, r, err, want)
		}
	}
}

func TestNotFoundAndNotExecutable(t *testing.T) {
	r, _ := Exec(context.Background(), spec(t, "/nonexistent/bitc-test-cmd"))
	if r.Code != 127 || r.StartErr == nil {
		t.Fatalf("%+v", r)
	}
	f := filepath.Join(t.TempDir(), "noexec")
	_ = os.WriteFile(f, []byte("#!/bin/sh\n"), 0o644)
	r, _ = Exec(context.Background(), spec(t, f))
	if r.Code != 126 || r.StartErr == nil {
		t.Fatalf("%+v", r)
	}
}

func TestOwnGroupAndStartedCallback(t *testing.T) {
	s := spec(t, "sh", "-c", "sleep 0.2")
	var gotPid, gotPgid int
	s.Started = func(pid, pgid int, _ uint64) { gotPid, gotPgid = pid, pgid }
	if _, err := Exec(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if gotPid == 0 || gotPgid != gotPid {
		t.Fatalf("pid %d pgid %d", gotPid, gotPgid)
	}
}

func TestStragglersKilledAtEndOfHold(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "bg")
	s := spec(t, "sh", "-c", `sleep 30 & echo $! > "$1"`, "_", pidFile)
	t0 := time.Now()
	if r, _ := Exec(context.Background(), s); r.Code != 0 {
		t.Fatal(r)
	}
	if time.Since(t0) > 3*time.Second {
		t.Fatal("waited for the straggler")
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for deadline := time.Now().Add(3 * time.Second); !proctest.Gone(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("straggler %d still alive", pid)
		}
	}
}

func TestPendingSignalsAreTakenOver(t *testing.T) {
	pending := make(chan os.Signal, 4)
	signal.Notify(pending, syscall.SIGTERM)
	pending <- syscall.SIGTERM // queued during the hand-off, before Exec registered its own handler
	s := spec(t, "sleep", "30")
	s.Pending = pending
	r, err := Exec(context.Background(), s)
	if err != nil || r.Code != 143 || r.Interrupted != syscall.SIGTERM {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestCancelledContextDoesNotSpin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := filepath.Join(t.TempDir(), "ready")
	// ignores TERM, then says so: the cancel's TERM lands after the trap and the run ends at the grace SIGKILL
	s := spec(t, "sh", "-c", `trap "" TERM; : > "$1"; sleep 5`, "_", ready)
	s.Grace = time.Second
	var before, after syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &before)
	done := make(chan ExecResult, 1)
	go func() { r, _ := Exec(ctx, s); done <- r }()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("trap never installed")
		}
	}
	cancel()
	r := <-done
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &after)
	cpu := time.Duration(after.Utime.Nano() + after.Stime.Nano() - before.Utime.Nano() - before.Stime.Nano())
	if r.Code != 143 || cpu > 400*time.Millisecond {
		t.Fatalf("%+v, %v of CPU during a 1s grace", r, cpu)
	}
}

func TestTTYStdinStartedWithoutAGroup(t *testing.T) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no /dev/ptmx:", err)
	}
	defer m.Close()
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skip(err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skip(err)
	}
	sl, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip(err)
	}
	defer sl.Close()
	s := spec(t, "true")
	s.Stdin = sl
	gotPgid := -1
	s.Started = func(_, pgid int, _ uint64) { gotPgid = pgid }
	if _, err := Exec(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if gotPgid != 0 {
		t.Fatalf("pgid %d, want 0 for a TTY run", gotPgid)
	}
}
