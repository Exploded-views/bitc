package conformance

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/proctest"
)

// Review Focus 1.
func TestAD1_TwentyWaitersFIFO(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"60s\"\n")
	hold := h.Hold(nil, "lint")
	log := filepath.Join(h.Root, "order")
	var ps []*harness.Proc
	for i := 0; i < 20; i++ {
		ps = append(ps, h.Start("run", "lint", "--", "sh", "-c", `echo "$1" >> "$2"`, "_", strconv.Itoa(i), log))
		h.Until(func() bool { return len(tickets(h, "lint")) == i+2 }, 10*time.Second)
	}
	hold.Release()
	for _, p := range ps {
		if r := p.Wait(); r.Code != 0 {
			t.Fatalf("%+v", r)
		}
	}
	got := lines(t, log)
	for i, l := range got {
		if l != strconv.Itoa(i) {
			t.Fatalf("order %v", got)
		}
	}
	if len(got) != 20 || len(tickets(h, "lint")) != 0 {
		t.Fatalf("ran %d, tickets left %v", len(got), tickets(h, "lint"))
	}
}

// Review Focus 2.
func TestAD2_SigkilledHolderAndWaiterLeaveNoStuckPool(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"60s\"\n")
	pidFile := filepath.Join(h.Root, "child.pid")
	holder := h.Start("run", "lint", "--", "sh", "-c", `echo $$ > "$1"; sleep 60 & sleep 60`, "_", pidFile)
	h.Until(func() bool { _, err := os.Stat(pidFile); return err == nil }, 10*time.Second)
	waiter := h.Start("run", "lint", "--", "true")
	h.Until(func() bool { return len(tickets(h, "lint")) == 2 }, 10*time.Second)
	_ = syscall.Kill(waiter.Cmd.Process.Pid, syscall.SIGKILL)
	_ = syscall.Kill(holder.Cmd.Process.Pid, syscall.SIGKILL)
	holder.Wait()
	waiter.Wait()
	t0 := time.Now()
	r := h.Run("run", "--wait", "5s", "lint", "--", "true")
	if r.Code != 0 || time.Since(t0) > 3*time.Second {
		t.Fatalf("%+v after %v", r, time.Since(t0))
	}
	if strings.Count(r.Stderr, "bitc: reaped orphaned lint job") != 1 {
		t.Fatalf("stderr %q", r.Stderr)
	}
	child := h.Atoi(string(must(os.ReadFile(pidFile))))
	h.Until(func() bool { return proctest.Gone(child) }, 5*time.Second)
	if len(tickets(h, "lint")) != 0 {
		t.Fatalf("tickets left %v", tickets(h, "lint"))
	}
}

func TestAD3_SamePoolNestedChildOneAtATime(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"1s\"\n")
	ready, gate := filepath.Join(h.Root, "a-ready"), filepath.Join(h.Root, "a-gate")
	// Files, not sleeps: nested child a signals it is in and holds until the gate opens; b
	// starts only after a is in, and the gate opens only after b has starved.
	script := `"$1" run lint -- sh -c ': > "$1"; while [ ! -e "$2" ]; do sleep 0.02; done; echo a' _ "$2" "$3" &
i=0; while [ ! -e "$2" ] && [ $i -lt 1000 ]; do sleep 0.02; i=$((i+1)); done
"$1" run lint -- echo b
: > "$3"; wait`
	r := h.Run("run", "lint", "--", "sh", "-c", script, "_", harness.Bin, ready, gate)
	if !strings.Contains(r.Stderr, "STARVED lint (nested child busy)") || !strings.Contains(r.Stdout, "a") ||
		strings.Contains(r.Stdout, "b") {
		t.Fatalf("%+v", r)
	}
}

func TestAD4_InotifyWakesBeforeThePoll(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[host]\npoll = \"5s\"\n[pools.lint]\nwait = \"30s\"\n")
	hold := h.Hold([]string{"!BITC_POLL"}, "lint")
	w := h.StartWith([]string{"!BITC_POLL"}, "run", "lint", "--", "true")
	time.Sleep(500 * time.Millisecond)
	t0 := time.Now()
	hold.Release()
	w.Wait()
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("waiter woke after %v (poll is 5s)", d)
	}
}

func TestAD5_CommandInheritsNoLockFds(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("groups = { g = { units = 1 } }\n[pools.lint]\ngroups = { g = 1 }\n")
	r := h.Run("run", "lint", "--", "sh", "-c", "ls -l /proc/self/fd")
	if r.Code != 0 || strings.Contains(r.Stdout, ".lock") || strings.Contains(r.Stdout, "tickets") {
		t.Fatalf("%+v", r)
	}
}
