package run

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/execgate"
	"github.com/Exploded-views/bitc/internal/exit"
)

// T8: a record that fails ends the run with 69 and the command never runs; nothing is left in
// its group. Exec has killed and reaped the gate before it returns, so both are read once.
func TestFailClosedWhenTheRecordFails(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	s := spec(t, "sh", "-c", `echo x > "$1"`, "sh", marker)
	var gotPgid int
	var gotStart uint64
	var atGate bool
	s.Started = func(pid, pgid int, start uint64) error {
		gotPgid, gotStart = pgid, start
		atGate = isGate(pid)
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
	if !atGate {
		t.Error("the started process was already the command when the record failed")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command ran although it was not recorded")
	}
	if gotPgid <= 0 {
		t.Fatalf("pgid %d", gotPgid)
	}
	left := groupSurvivors(gotPgid, gotStart)
	for _, p := range left {
		if p > 0 {
			_ = syscall.Kill(p, syscall.SIGKILL) // exact pids
		}
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
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Exec(context.Background(), spec(t, "sh", "-c", "true"))
	if err != nil || r.Code != exit.Unavailable || r.StartErr == nil {
		t.Fatalf("%+v %v, want code 69", r, err)
	}
	msg := r.StartErr.Error()
	if !strings.Contains(msg, "the exec gate") || !strings.Contains(msg, missing) || strings.Contains(msg, sh) {
		t.Errorf("StartErr %q should name the exec gate and %s, not the command %s", msg, missing, sh)
	}
	if got, want := originOf(r), "bitc"; got != want {
		t.Errorf("origin %q, want %q", got, want)
	}
}

// A context that is done before the gate says hello ends the run as a cancel would. The gate is
// held before its hello, and the context is cancelled once Exec has started it, so Exec is
// certainly waiting for a hello that has not come. Without the ctx branch Exec waits for it
// forever: this test then hangs, and go test's -timeout reports it (no deterministic failure is
// possible for a missing branch that only ever blocks).
func TestCancelBeforeTheHelloEndsTheRun(t *testing.T) {
	h := newHold(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	afterGateStart = func(int) { cancel() }
	t.Cleanup(func() { afterGateStart = nil })
	marker := filepath.Join(t.TempDir(), "ran")
	s := spec(t, "sh", "-c", `echo x > "$1"`, "sh", marker)
	s.Env = h.env(execgate.HoldHello)
	started := false
	s.Started = func(int, int, uint64) error { started = true; return nil }
	r, err := Exec(ctx, s)
	if err != nil || r.Code != 143 || r.Interrupted != syscall.SIGTERM || r.StartErr != nil {
		t.Errorf("%+v %v, want 143 by SIGTERM", r, err)
	}
	if started {
		t.Error("Started was called for a gate that never said hello")
	}
	if _, err := os.Stat(marker); err == nil { // the gate is reaped when Exec returns: final
		t.Error("the command ran although the run was cancelled before the hello")
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

// fdInfo is what the descriptor tests need to tell one open file from another.
type fdInfo struct {
	mode uint64 // st_mode & S_IFMT (uint64: st_mode is 16 bits on darwin, 32 on Linux)
	acc  int    // the F_GETFL access mode: O_RDONLY, O_WRONLY or O_RDWR
}

// listFds lists the open descriptors the directory dir names, without the one the listing itself
// opened (it is closed before the descriptors are probed).
func listFds(t *testing.T, dir string) map[int]fdInfo {
	t.Helper()
	d, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]fdInfo{}
	for _, n := range names {
		k, err := strconv.Atoi(n)
		if err != nil {
			continue
		}
		var st unix.Stat_t
		if unix.Fstat(k, &st) != nil { // the listing's own descriptor, closed above
			continue
		}
		fl, err := unix.FcntlInt(uintptr(k), unix.F_GETFL, 0)
		if err != nil {
			continue
		}
		out[k] = fdInfo{mode: uint64(st.Mode) & unix.S_IFMT, acc: fl & unix.O_ACCMODE}
	}
	return out
}

// TestStartGateAddsOnlyItsOwnFds: startGate leaves exactly two descriptors open in the parent while
// the gate waits for its hello:
// the release end of the gate pipe and the read end of the status pipe. The child's ends and every
// dup of an inherited descriptor are closed by then; a copy left open (for one, of the status
// pipe's write end) would keep the status pipe from ever ending, and a quick command would hang
// Exec. The gate is held before its hello, so the status reader is certainly still running.
func TestStartGateAddsOnlyItsOwnFds(t *testing.T) {
	h := newHold(t)
	if r, w, err := os.Pipe(); err != nil { // the first pipe starts the runtime's poller, which opens descriptors of its own
		t.Fatal(err)
	} else {
		r.Close()
		w.Close()
	}
	s := spec(t, "sh", "-c", "true")
	s.Env = h.env(execgate.HoldHello)
	lookup := exec.Command(s.Argv[0], s.Argv[1:]...)
	before := openFds(t)
	gt, err := startGate(lookup, s, true)
	if err != nil {
		t.Fatal(err)
	}
	after := openFds(t)
	release := int(gt.release.Fd())
	defer func() { // the gate, by its exact pid; never waits for the status reader, which a leak would keep
		_ = gt.cmd.Process.Kill()
		_ = gt.release.Close()
		_ = gt.cmd.Wait()
	}()
	// Go's os/exec keeps a pidfd (not a pipe) for the process it started on Linux: it is the Cmd's
	// own handle, closed by Wait, and is set apart from the pipes.
	added := map[int]fdInfo{}
	var handles []int
	for k, v := range after {
		if _, ok := before[k]; ok {
			continue
		}
		if v.mode != unix.S_IFIFO {
			handles = append(handles, k)
			continue
		}
		added[k] = v
	}
	if len(handles) > 1 {
		t.Errorf("startGate left %d new descriptors that are not pipes, want at most the process handle: %v", len(handles), handles)
	}
	if len(added) != 2 {
		t.Fatalf("startGate left %d new pipe descriptors open, want 2 (the release end and the status read end): %v", len(added), added)
	}
	if got, ok := added[release]; !ok || got.mode != unix.S_IFIFO || got.acc != unix.O_WRONLY {
		t.Errorf("the gate's release end (fd %d) is not among the new descriptors as a write-only pipe: %v", release, added)
	}
	for k, v := range added {
		if k != release && (v.mode != unix.S_IFIFO || v.acc != unix.O_RDONLY) {
			t.Errorf("the other new descriptor %d is %+v, want the status pipe's read end (a read-only pipe)", k, v)
		}
	}
}

// A run whose status pipe never ended would hang: a command that exits at once returns. A hang is
// reported by go test's -timeout; TestStartGateAddsOnlyItsOwnFds catches the usual cause, a copy of
// the status pipe's write end left open in the parent, without one.
func TestExecOfAQuickCommandReturns(t *testing.T) {
	r, err := Exec(context.Background(), spec(t, "true"))
	if err != nil || r.Code != 0 {
		t.Errorf("%+v %v", r, err)
	}
}

// T9: a signal received before the exec is forwarded only after it. The gate is held after its
// release, before the exec, until Exec's loop has taken the signal off its channel (onSignal),
// so the signal is certainly queued before the exec. onForward reports parent state only. Without
// the forward hold the forward goes out at once, to a gate that has not exec'd, and onForward sees
// execDone false.
func TestForwardsWaitForTheExec(t *testing.T) {
	h := newHold(t)
	var sigsSeen []syscall.Signal
	var execDone []bool
	onForward = func(sig syscall.Signal, done bool) {
		sigsSeen = append(sigsSeen, sig)
		execDone = append(execDone, done)
	}
	onSignal = func(syscall.Signal) { h.release() }
	t.Cleanup(func() { onForward, onSignal = nil, nil })
	s := spec(t, "sleep", "30")
	s.Env = h.env(execgate.HoldExec)
	s.Started = func(pid, pgid int, start uint64) error {
		return syscall.Kill(os.Getpid(), syscall.SIGTERM) // the loop takes it before the gate may exec
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
