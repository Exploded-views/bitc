package run

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/execgate"
	"github.com/Exploded-views/bitc/internal/exit"
)

// T8: a record that fails ends the run with 69 and the command never runs; nothing is left in
// its group.
func TestFailClosedWhenTheRecordFails(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	s := spec(t, "sh", "-c", `echo x > "$1"`, "sh", marker)
	var gotPgid int
	var gotStart uint64
	s.Started = func(pid, pgid int, start uint64) error {
		gotPgid, gotStart = pgid, start
		return errors.New("the ticket is gone")
	}
	r, err := Exec(context.Background(), s)
	if err != nil || r.Code != exit.Unavailable || r.StartErr == nil || r.StartErr.Error() != "could not record the command" {
		t.Fatalf("%+v %v, want code 69 and the record error", r, err)
	}
	if got := outcomeOf(r); got != "unavailable" {
		t.Errorf("outcome %q, want unavailable", got)
	}
	if got := originOf(r); got != "bitc" {
		t.Errorf("origin %q, want bitc", got)
	}
	if appears(marker, 300*time.Millisecond) {
		t.Error("the command ran although it was not recorded")
	}
	if gotPgid <= 0 {
		t.Fatalf("pgid %d", gotPgid)
	}
	var left []int
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if left = groupSurvivors(gotPgid, gotStart); len(left) == 0 || time.Now().After(deadline) {
			break
		}
	}
	for _, p := range left {
		_ = syscall.Kill(p, syscall.SIGKILL) // exact pids
	}
	if len(left) != 0 {
		t.Errorf("processes of group %d remain: %v", gotPgid, left)
	}
}

// A gate binary that cannot be exec'd is bitc's failure: code 69, naming the exec gate and its
// path, not the user's command.
func TestUnrunnableGateBinaryIsASetupFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-bitc")
	selfExeFn = func() (string, error) { return missing, nil }
	t.Cleanup(func() { selfExeFn = selfExe })
	r, err := Exec(context.Background(), spec(t, "sh", "-c", "true"))
	if err != nil || r.Code != exit.Unavailable || r.StartErr == nil {
		t.Fatalf("%+v %v, want code 69", r, err)
	}
	msg := r.StartErr.Error()
	if !strings.Contains(msg, "the exec gate") || !strings.Contains(msg, missing) || strings.Contains(msg, "/sh") {
		t.Errorf("StartErr %q should name the exec gate and %s, not the command", msg, missing)
	}
	if got, want := originOf(r), "bitc"; got != want {
		t.Errorf("origin %q, want %q", got, want)
	}
}

// A context that is done before the gate says hello ends the run as a cancel would.
func TestCancelBeforeTheHelloEndsTheRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan ExecResult, 1)
	s := spec(t, "sleep", "30")
	go func() { r, _ := Exec(ctx, s); done <- r }()
	select {
	case r := <-done:
		if r.Code != 143 || r.Interrupted != syscall.SIGTERM {
			t.Errorf("%+v, want 143 by SIGTERM", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Exec did not return")
	}
}

func TestOutcomeOf(t *testing.T) {
	for _, c := range []struct {
		res  ExecResult
		want string
	}{
		{ExecResult{}, "ok"},
		{ExecResult{Code: 3}, "command-failed"},
		{ExecResult{Code: 69}, "command-failed"}, // a command that itself exits 69
		{ExecResult{Code: 69, StartErr: errors.New("x")}, "unavailable"},
		{ExecResult{Code: 127, StartErr: errors.New("x")}, "command-failed"},
		{ExecResult{Code: 143, Interrupted: syscall.SIGTERM}, "interrupted"},
		{ExecResult{Code: exit.TimedOut, TimedOut: true}, "timed-out"},
	} {
		if got := outcomeOf(c.res); got != c.want {
			t.Errorf("%+v: %q, want %q", c.res, got, c.want)
		}
	}
}

// A run whose status pipe never ended would hang: a command that exits at once returns.
func TestExecOfAQuickCommandReturns(t *testing.T) {
	done := make(chan ExecResult, 1)
	s := spec(t, "true")
	go func() { r, _ := Exec(context.Background(), s); done <- r }()
	select {
	case r := <-done:
		if r.Code != 0 {
			t.Errorf("%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Exec did not return")
	}
}

// T9: a signal received before the exec is forwarded only after it. The gate waits 1s between
// its release and the exec; Started signals this process and waits until the signal has been
// seen, so it is queued in Exec before the release. onForward reports parent state only.
func TestForwardsWaitForTheExec(t *testing.T) {
	probe := make(chan os.Signal, 1)
	signal.Notify(probe, syscall.SIGTERM)
	defer signal.Stop(probe)
	var sigsSeen []syscall.Signal
	var execDone []bool
	onForward = func(sig syscall.Signal, done bool) {
		sigsSeen = append(sigsSeen, sig)
		execDone = append(execDone, done)
	}
	t.Cleanup(func() { onForward = nil })
	s := spec(t, "sleep", "30")
	s.Env = append(os.Environ(), execgate.DelayEnv+"=1000")
	s.Grace = 10 * time.Second // the SIGKILL, which is never held, must not fire in the window
	s.Started = func(pid, pgid int, start uint64) error {
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			return err
		}
		select {
		case <-probe:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("the signal never arrived")
		}
	}
	r, err := Exec(context.Background(), s)
	if err != nil || r.Code != 143 || r.Interrupted != syscall.SIGTERM {
		t.Fatalf("%+v %v", r, err)
	}
	if len(sigsSeen) == 0 || sigsSeen[0] != syscall.SIGTERM {
		t.Fatalf("forwards %v, want SIGTERM", sigsSeen)
	}
	for i, d := range execDone {
		if !d {
			t.Errorf("forward %d (%v) went out before the exec", i, sigsSeen[i])
		}
	}
}
