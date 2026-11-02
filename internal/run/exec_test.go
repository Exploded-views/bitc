package run

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/exit"
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

// A command that exits at once is still recorded: Started reads the leader's stat before the
// command is reaped. The seams hold the record back until the reap, the worst order a loaded
// host can produce.
func TestStartedRecordsACommandReapedBeforeTheRecord(t *testing.T) {
	reaped := make(chan struct{})
	afterWait = func() { close(reaped) }
	beforeRecord = func() { <-reaped }
	t.Cleanup(func() { afterWait, beforeRecord = nil, nil })
	s := spec(t, "true")
	called := false
	s.Started = func(pid, pgid int, start uint64) { called = pid > 0 && pgid == pid && start > 0 }
	if _, err := Exec(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("Started was not called with the leader's pid, pgid and start")
	}
}

func TestTimeoutTerminatesTheGroupAndExits124(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "bg")
	s := spec(t, "sh", "-c", `sleep 30 & echo $! > "$1"; wait`, "_", pidFile)
	s.Timeout = 600 * time.Millisecond
	r, err := Exec(context.Background(), s)
	if err != nil || r.Code != exit.TimedOut || !r.TimedOut || r.Interrupted != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid == 0 || !proctest.Gone(pid) { // Exec reaps the group before it returns
		t.Fatalf("the command's child %d outlived the time limit", pid)
	}
}

func TestTimeoutSigkillsAfterGraceAndStillExits124(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	s := spec(t, "sh", "-c", `trap "" TERM; : > "$1"; while :; do sleep 0.05; done`, "_", ready)
	s.Timeout = 600 * time.Millisecond
	r, _ := Exec(context.Background(), s)
	if r.Code != exit.TimedOut || !r.TimedOut {
		t.Fatalf("%+v", r)
	}
}

func TestNoTimeoutAndASlowFinishAreNotTimedOut(t *testing.T) {
	s := spec(t, "sh", "-c", "exit 3")
	s.Timeout = time.Hour
	if r, _ := Exec(context.Background(), s); r.Code != 3 || r.TimedOut {
		t.Fatalf("%+v", r)
	}
}
