package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/lock"
)

// t6Poll keeps the polling waiters quick on a platform without a wake-up (macOS). The window the
// test needs does not come from the poll: W is stopped (t6Stop), whatever the platform's waker.
var t6Poll = []string{"BITC_POLL=1s"}

// t6Log runs `echo name >> log`.
const t6Log = `echo "$1" >> "$2"`

// t6Gated waits for the gate file $3 before it logs, so a slot taken by it stays taken.
const t6Gated = `while [ ! -e "$3" ]; do sleep 0.02; done; echo "$1" >> "$2"`

// T6 is a characterization guard: it passes on the code that introduced it. It pins first-in,
// first-out admission when newcomers arrive just after a release.
func TestT6_NewcomerAfterReleaseDoesNotJumpTheQueue(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"60s\"\n")
	hold := h.Hold(t6Poll, "lint")
	log := filepath.Join(h.Root, "t6-order")
	w := h.StartWith(t6Poll, "run", "lint", "--", "sh", "-c", t6Log, "_", "W", log)
	t6Stop(t, h, w, 2)
	hold.Release()
	ps := []*harness.Proc{w}
	for i, name := range []string{"N1", "N2", "N3"} {
		n := h.StartWith(t6Poll, "run", "--wait", "30s", "lint", "--", "sh", "-c", t6Log, "_", name, log)
		ps = append(ps, n)
		h.Until(func() bool { return len(tickets(h, "lint")) >= i+2 || n.Exited() }, 10*time.Second)
	}
	t6AssertWindow(t, h, log)
	t6Cont(w)
	for _, p := range ps {
		if r := p.Wait(); r.Code != 0 {
			t.Fatalf("%+v", r)
		}
	}
	if got := strings.Join(lines(t, log), ","); got != "W,N1,N2,N3" {
		t.Fatalf("order %s, want W,N1,N2,N3", got)
	}
}

func TestT6_OneSlotImpatientNewcomersStarveWhileAWaiterIsQueued(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"60s\"\n")
	hold := h.Hold(t6Poll, "lint")
	log, gate := filepath.Join(h.Root, "t6-strict"), filepath.Join(h.Root, "t6-gate")
	w := h.StartWith(t6Poll, "run", "lint", "--", "sh", "-c", t6Gated, "_", "W", log, gate)
	t6Stop(t, h, w, 2)
	hold.Release()
	for _, name := range []string{"N1", "N2", "N3"} {
		if r := h.RunWith(t6Poll, "run", "--wait", "0s", "lint", "--", "sh", "-c", t6Log, "_", name, log); r.Code != 75 || !strings.Contains(r.Stderr, "(queue;") {
			t.Errorf("%s: want STARVED (75) for the queue, got %+v", name, r)
		}
	}
	t6AssertAdmitFree(t, h)
	t6Cont(w)
	t6Open(t, gate)
	if r := w.Wait(); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	if got := strings.Join(lines(t, log), ","); got != "W" {
		t.Fatalf("ran %s, want only W", got)
	}
}

func TestT6_TwoSlotsFreedSlotGoesToTheWaiter(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nslots = 2\nwait = \"60s\"\n")
	h1 := h.Hold(t6Poll, "lint")
	h2 := h.Hold(t6Poll, "lint")
	log, gate := filepath.Join(h.Root, "t6-two"), filepath.Join(h.Root, "t6-two-gate")
	w := h.StartWith(t6Poll, "run", "lint", "--", "sh", "-c", t6Gated, "_", "W", log, gate)
	t6Stop(t, h, w, 3)
	h1.Release()
	for _, name := range []string{"N1", "N2", "N3"} {
		if r := h.RunWith(t6Poll, "run", "--wait", "0s", "lint", "--", "sh", "-c", t6Log, "_", name, log); r.Code != 75 || !strings.Contains(r.Stderr, "(queue;") {
			t.Errorf("%s: want STARVED (75) for the queue, got %+v", name, r)
		}
	}
	t6AssertAdmitFree(t, h)
	t6Cont(w)
	t6Open(t, gate)
	if r := w.Wait(); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	h2.Release()
	if got := strings.Join(lines(t, log), ","); got != "W" {
		t.Fatalf("ran %s, want only W", got)
	}
}

// t6Stop waits until W's ticket (the nth) is waiting, then stops W's bitc. A stopped waiter keeps
// its ticket lock, so it stays the oldest live waiter while it cannot attempt: the window in which a
// newcomer would jump the queue, on every platform and whatever the poll or the waker. The cleanup
// continues W, so a failing test never leaves a stopped process.
func t6Stop(t *testing.T, h *harness.H, w *harness.Proc, nth int) {
	t.Helper()
	h.Until(func() bool {
		ts := tickets(h, "lint")
		if len(ts) != nth {
			return false
		}
		b, err := os.ReadFile(filepath.Join(h.State, "pools", "lint", "tickets", ts[nth-1]))
		return err == nil && strings.Contains(strings.ReplaceAll(string(b), " ", ""), `"state":"waiting"`)
	}, 10*time.Second)
	t.Cleanup(func() { t6Cont(w) })
	// W writes its waiting ticket before its first attempt, which holds admit.lock. A stop inside
	// that attempt would leave the lock held by a stopped process, so check after the stop and, if
	// it is held, continue W and try again.
	h.Until(func() bool {
		w.Signal(syscall.SIGSTOP)
		// SIGSTOP is asynchronous: read the lock only once W has really stopped, or it could
		// still finish a locked attempt after the read says free.
		h.Until(func() bool { return t6IsStopped(w.Cmd.Process.Pid) }, 10*time.Second)
		if t6AdmitLockHeld(h) {
			t6Cont(w)
			return false
		}
		return true
	}, 10*time.Second)
}

// t6IsStopped reports whether the process is stopped. On Linux every thread must be in state T
// (each /proc/<pid>/task/<tid>/stat: the main thread's stat alone can say T while another thread
// is still finishing a flock); on darwin the whole process stops, and `ps -o stat=` says T.
func t6IsStopped(pid int) bool {
	if runtime.GOOS != "linux" {
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		return err == nil && t6StateIsT(strings.TrimSpace(string(out)))
	}
	stats, err := filepath.Glob("/proc/" + strconv.Itoa(pid) + "/task/*/stat")
	if err != nil || len(stats) == 0 {
		return false
	}
	for _, f := range stats {
		b, err := os.ReadFile(f)
		if err != nil {
			return false
		}
		// "tid (comm) S ...": comm may hold spaces and parentheses, so cut at the last ")".
		i := strings.LastIndex(string(b), ") ")
		if i < 0 || !t6StateIsT(string(b)[i+2:]) {
			return false
		}
	}
	return true
}

func t6StateIsT(st string) bool { return strings.HasPrefix(st, "T") || strings.HasPrefix(st, "t") }

// t6AdmitLockHeld reports whether any process holds the host's admit.lock. It is the one place
// the test reads that lock.
func t6AdmitLockHeld(h *harness.H) bool {
	return lock.Inspect(filepath.Join(h.State, "admit.lock")) == lock.StateHeld
}

// t6AssertAdmitFree checks that the stopped W does not hold admit.lock.
func t6AssertAdmitFree(t *testing.T, h *harness.H) {
	t.Helper()
	if t6AdmitLockHeld(h) {
		t.Error("admit.lock is held while W is stopped")
	}
}

func t6Cont(w *harness.Proc) { w.Signal(syscall.SIGCONT) }

func t6Open(t *testing.T, gate string) {
	t.Helper()
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// t6AssertWindow checks that the window the test depends on is real: W's ticket is still waiting
// and nothing has run, so a newcomer was refused (or queued) because of W, not because W was
// already admitted.
func t6AssertWindow(t *testing.T, h *harness.H, log string) {
	t.Helper()
	if got := lines(t, log); len(got) != 0 {
		t.Errorf("W was admitted before the newcomers arrived: log %v", got)
	}
	ts := tickets(h, "lint")
	if len(ts) == 0 {
		t.Fatal("no tickets")
	}
	b := must(os.ReadFile(filepath.Join(h.State, "pools", "lint", "tickets", ts[0])))
	if !strings.Contains(strings.ReplaceAll(string(b), " ", ""), `"state":"waiting"`) {
		t.Errorf("the oldest ticket is not waiting: %s", b)
	}
}
