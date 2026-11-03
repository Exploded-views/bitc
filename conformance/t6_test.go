package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// t6Poll is slow on purpose: the queued waiter W sleeps through most of it, so a newcomer that
// arrives right after a release has a real window in which to jump the queue.
var t6Poll = []string{"BITC_POLL=5s"}

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
	h.Until(func() bool { return len(tickets(h, "lint")) == 2 }, 10*time.Second)
	hold.Release()
	ps := []*harness.Proc{w}
	for i, name := range []string{"N1", "N2", "N3"} {
		n := h.StartWith(t6Poll, "run", "lint", "--", "sh", "-c", t6Log, "_", name, log)
		ps = append(ps, n)
		h.Until(func() bool { return len(tickets(h, "lint")) >= i+2 || n.Exited() }, 10*time.Second)
	}
	t6AssertWindow(t, h, log)
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
	h.Until(func() bool { return len(tickets(h, "lint")) == 2 }, 10*time.Second)
	hold.Release()
	for _, name := range []string{"N1", "N2", "N3"} {
		if r := h.RunWith(t6Poll, "run", "--wait", "0s", "lint", "--", "sh", "-c", t6Log, "_", name, log); r.Code != 75 || !strings.Contains(r.Stderr, "(queue;") {
			t.Errorf("%s: want STARVED (75) for the queue, got %+v", name, r)
		}
	}
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
	h.Until(func() bool { return len(tickets(h, "lint")) == 3 }, 10*time.Second)
	h1.Release()
	for _, name := range []string{"N1", "N2", "N3"} {
		if r := h.RunWith(t6Poll, "run", "--wait", "0s", "lint", "--", "sh", "-c", t6Log, "_", name, log); r.Code != 75 || !strings.Contains(r.Stderr, "(queue;") {
			t.Errorf("%s: want STARVED (75) for the queue, got %+v", name, r)
		}
	}
	t6Open(t, gate)
	if r := w.Wait(); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	h2.Release()
	if got := strings.Join(lines(t, log), ","); got != "W" {
		t.Fatalf("ran %s, want only W", got)
	}
}

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
