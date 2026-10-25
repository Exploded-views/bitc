package conformance

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// orderScript logs its start, holds until the gate file $3 exists, then logs its end.
const orderScript = `echo "start $1" >> "$2"; while [ ! -e "$3" ]; do sleep 0.02; done; echo "end $1" >> "$2"`

func TestS1_OneSlotNeverOverlapsTwoSlotsDo(t *testing.T) {
	for _, c := range []struct {
		slots   int
		overlap bool
	}{{1, false}, {2, true}} {
		h := harness.New(t)
		h.HostConfig("[pools.lint]\nslots = " + strconv.Itoa(c.slots) + "\nwait = \"30s\"\n")
		log := filepath.Join(h.Root, "order")
		gate := filepath.Join(h.Root, "go")
		a := h.Start("run", "lint", "--", "sh", "-c", orderScript, "_", "a", log, gate)
		h.Until(func() bool { return len(lines(t, log)) > 0 }, 10*time.Second)
		b := h.Start("run", "lint", "--", "sh", "-c", orderScript, "_", "b", log, gate)
		// a keeps its slot until the gate opens: synchronise on files, never on a sleep
		if c.overlap {
			h.Until(func() bool { return slices.Contains(lines(t, log), "start b") }, 20*time.Second)
		} else {
			h.Until(func() bool { return len(tickets(h, "lint")) == 2 }, 20*time.Second) // b is queued
			time.Sleep(time.Second)                                                      // a wrong gate would let b start here
		}
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		a.Wait()
		b.Wait()
		got := lines(t, log)
		overlapped := got[1] == "start b"
		if overlapped != c.overlap {
			t.Errorf("slots=%d order %v", c.slots, got)
		}
	}
}

func TestS2_SlotReleasedWhateverTheCode(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"0s\"\n")
	for _, script := range []string{"exit 7", "kill -KILL $$", "true"} {
		r := h.Run("run", "lint", "--", "sh", "-c", script)
		if r.Code == 75 {
			t.Fatalf("%q left the slot held: %q", script, r.Stderr)
		}
	}
}

func TestS4_WaiterLeaksNoFds(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"5s\"\n")
	hold := h.Hold(nil, "lint")
	w := h.Start("run", "lint", "--", "true")
	fds := func() int {
		ents, _ := os.ReadDir(filepath.Join("/proc", strconv.Itoa(w.Cmd.Process.Pid), "fd"))
		return len(ents)
	}
	time.Sleep(500 * time.Millisecond)
	n1 := fds()
	time.Sleep(1500 * time.Millisecond)
	// an attempt in flight holds admit.lock, a slot lock and prune fds for a moment (+3);
	// a leak at a 100 ms poll adds about 15 in 1.5 s
	if n2 := fds(); n2 > n1+3 {
		t.Fatalf("fds grew %d -> %d", n1, n2)
	}
	hold.Release()
	w.Wait()
}

// A waiter on a busy shared group takes and gives back its pool slot on every attempt (the M2
// path: slot.1 free, the group busy), and leaks no fd doing it.
func TestS4_GroupWaiterLeaksNoFds(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(strings.Replace(groupHost, `[pools.coverage]
groups = { bigmem = 1 }
wait = "1s"`, `[pools.coverage]
groups = { bigmem = 1 }
wait = "5s"`, 1))
	hold := h.Hold(nil, "heavy")
	w := h.Start("run", "coverage", "--", "true")
	fds := func() int {
		ents, _ := os.ReadDir(filepath.Join("/proc", strconv.Itoa(w.Cmd.Process.Pid), "fd"))
		return len(ents)
	}
	time.Sleep(500 * time.Millisecond)
	n1 := fds()
	time.Sleep(1500 * time.Millisecond)
	if n2 := fds(); n2 > n1+3 {
		t.Fatalf("fds grew %d -> %d", n1, n2)
	}
	if w.Exited() {
		t.Fatal("the waiter ended before the group was released")
	}
	hold.Release()
	if r := w.Wait(); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestS5_WaiterGetsSlotOnRelease(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"20s\"\n")
	hold := h.Hold(nil, "lint")
	w := h.Start("run", "lint", "--", "echo", "ran")
	time.Sleep(300 * time.Millisecond)
	if w.Exited() {
		t.Fatal("waiter ran while the slot was held")
	}
	hold.Release()
	if r := w.Wait(); r.Code != 0 || r.Stdout != "ran\n" {
		t.Fatalf("%+v", r)
	}
}

func TestS6_LeftoverLockFilesDoNotBlock(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"0s\"\n")
	h.Write(filepath.Join(h.State, "pools", "lint", "slot.1.lock"), "12345\n", 0o600)
	h.Write(filepath.Join(h.State, "pools", "lint", "tickets", "1-0000000000000000001-deadbeef.json"),
		`{"pid":1,"start":1,"state":"running","token":"x"}`, 0o600)
	if err := os.Chmod(h.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if r := h.Run("run", "lint", "--", "true"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	if len(tickets(h, "lint")) != 0 {
		t.Fatal("dead ticket not pruned")
	}
}

func TestT1_StarvedAfterWait(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"1s\"\n")
	hold := h.Hold(nil, "lint")
	t0 := time.Now()
	r := h.Run("run", "lint", "--", "echo", "ran")
	if r.Code != 75 || r.Stderr != "STARVED lint\n" || r.Stdout != "" {
		t.Fatalf("%+v", r)
	}
	if d := time.Since(t0); d < time.Second || d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
	hold.Release()
}

func TestT3_PollDefaultEnvOverrideAndDeadline(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"2s\"\n")
	if out := h.RunWith([]string{"!BITC_POLL"}, "config", "explain", "host.poll").Stdout; !strings.Contains(out, "host.poll = 5s") {
		t.Fatal(out)
	}
	hold := h.Hold(nil, "lint")
	t0 := time.Now()
	r := h.RunWith([]string{"BITC_POLL=100ms"}, "run", "lint", "--", "true")
	if r.Code != 75 || time.Since(t0) < 2*time.Second || time.Since(t0) > 4*time.Second {
		t.Fatalf("%+v after %v", r, time.Since(t0))
	}
	hold.Release()
}

func TestT4_WaitZeroIsOneAttempt(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"0s\"\n")
	hold := h.Hold(nil, "lint")
	t0 := time.Now()
	if r := h.Run("run", "lint", "--", "true"); r.Code != 75 || time.Since(t0) > time.Second {
		t.Fatalf("%+v %v", r, time.Since(t0))
	}
	hold.Release()
}

func TestT5_FIFOWithinClass(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"30s\"\n")
	hold := h.Hold(nil, "lint")
	log := filepath.Join(h.Root, "fifo")
	var ps []*harness.Proc
	for i, name := range []string{"a", "b", "c"} {
		ps = append(ps, h.Start("run", "lint", "--", "sh", "-c", `echo "$1" >> "$2"`, "_", name, log))
		h.Until(func() bool { return len(tickets(h, "lint")) == i+2 }, 10*time.Second)
	}
	hold.Release()
	for _, p := range ps {
		p.Wait()
	}
	if got := strings.Join(lines(t, log), ","); got != "a,b,c" {
		t.Fatalf("order %s", got)
	}
}

func TestP6_PoolsAreIndependent(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n[pools.db]\nwait = \"0s\"\n")
	hold := h.Hold(nil, "lint")
	if r := h.Run("run", "db", "--", "true"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	hold.Release()
}
func lockHeld(path string) bool {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
