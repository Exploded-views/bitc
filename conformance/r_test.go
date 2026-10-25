package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// prioHost writes a config with lint (1 slot) whose high entries are frags, and makes dirs.
func prioHost(h *harness.H, frags ...string) {
	q := make([]string, len(frags))
	for i, f := range frags {
		q[i] = harness.Q(f)
	}
	h.HostConfig("priority = { high_cwd_contains = [" + strings.Join(q, ", ") + "] }\n[pools.lint]\nwait = \"30s\"\n[pools.db]\nwait = \"30s\"\n")
}

func startIn(h *harness.H, dir string, args ...string) *harness.Proc {
	old := h.Cwd
	h.Cwd = filepath.Join(h.Root, dir)
	_ = os.MkdirAll(h.Cwd, 0o755)
	defer func() { h.Cwd = old }()
	return h.StartWith([]string{"PWD=" + h.Cwd}, args...)
}

func highTickets(h *harness.H, pool string) int {
	n := 0
	for _, name := range tickets(h, pool) {
		if strings.HasPrefix(name, "0-") {
			n++
		}
	}
	return n
}

func TestR1_LiteralSubstringNeverGlob(t *testing.T) {
	for _, c := range []struct {
		frag, dir string
		high      bool
	}{
		{"wt-prio-x", "wt-prio-x/sub", true}, {"prio-x", "wt-prio-x/sub", true},
		{"wt-prio-?", "wt-prio-x/sub", false}, {"wt-*-x", "wt-prio-x/sub", false},
		{"wt-prio-[x]", "wt-prio-x/sub", false}, {`wt-prio\x`, "wt-prio-x/sub", false},
		{"wt[1]", "wt[1]/sub", true}, {`wt\x`, `wt\x`, true}, // glob and backslash spelled literally
	} {
		h := harness.New(t)
		prioHost(h, c.frag)
		h.Cwd = filepath.Join(h.Root, c.dir)
		_ = os.MkdirAll(h.Cwd, 0o755)
		hd := h.Hold([]string{"PWD=" + h.Cwd}, "lint")
		if got := highTickets(h, "lint") == 1; got != c.high {
			t.Errorf("fragment %q in %s: high=%v, want %v", c.frag, c.dir, got, c.high)
		}
		hd.Release()
	}
}

func TestR2_HighTicketWhileWaitingAndRunningThenGone(t *testing.T) {
	h := harness.New(t)
	prioHost(h, "urgent")
	hold := h.Hold(nil, "lint")
	p := startIn(h, "urgent-1", "run", "lint", "--", "sh", "-c", "exit 3")
	h.Until(func() bool { return highTickets(h, "lint") == 1 }, 10*time.Second)
	hold.Release()
	if r := p.Wait(); r.Code != 3 {
		t.Fatalf("%+v", r)
	}
	if highTickets(h, "lint") != 0 {
		t.Fatal("high ticket left after a failing command")
	}
}

func TestR3_NormalWaitsBehindHighHighsDoNotBlock(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("priority = { high_cwd_contains = [\"urgent\"] }\n[pools.lint]\nslots = 2\nwait = \"1s\"\n")
	old := h.Cwd
	h.Cwd = filepath.Join(h.Root, "urgent-a")
	_ = os.MkdirAll(h.Cwd, 0o755)
	high := h.Hold([]string{"PWD=" + h.Cwd}, "lint")
	r := h.RunWith([]string{"PWD=" + h.Cwd}, "run", "lint", "--", "true") // another high: slot 2 free
	h.Cwd = old
	if r.Code != 0 {
		t.Fatalf("high blocked by high: %+v", r)
	}
	r = h.Run("run", "lint", "--", "true") // normal: slot 2 free, but a high ticket is live
	if r.Code != 75 || r.Stderr != "STARVED lint (priority lane)\n" {
		t.Fatalf("%+v", r)
	}
	high.Release()
}

func TestR4_DeadTicketsPrunedPriorityPerPool(t *testing.T) {
	h := harness.New(t)
	prioHost(h, "urgent")
	h.Write(filepath.Join(h.State, "pools", "lint", "tickets", "0-0000000000000000001-0badf00d.json"),
		`{"pid":1,"start":1,"class":0,"state":"waiting","token":"x"}`, 0o600)
	_ = os.Chmod(h.State, 0o700)
	if r := h.Run("run", "--wait", "0s", "lint", "--", "true"); r.Code != 0 {
		t.Fatalf("dead high ticket blocked: %+v", r)
	}
	high := func() *harness.Holder {
		old := h.Cwd
		defer func() { h.Cwd = old }()
		h.Cwd = filepath.Join(h.Root, "urgent-b")
		_ = os.MkdirAll(h.Cwd, 0o755)
		return h.Hold([]string{"PWD=" + h.Cwd}, "db")
	}()
	if r := h.Run("run", "--wait", "0s", "lint", "--", "true"); r.Code != 0 {
		t.Fatalf("a high ticket of db blocked lint: %+v", r)
	}
	high.Release()
}

func TestR5_OutsideFragmentsQueuesBehindHigh(t *testing.T) {
	h := harness.New(t)
	prioHost(h, "urgent")
	hold := h.Hold(nil, "lint")
	hp := startIn(h, "urgent-c", "run", "lint", "--", "true")
	h.Until(func() bool { return highTickets(h, "lint") == 1 }, 10*time.Second)
	np := h.Start("run", "lint", "--", "true")
	time.Sleep(300 * time.Millisecond)
	for _, n := range tickets(h, "lint") {
		if !strings.HasPrefix(n, "0-") && !strings.HasPrefix(n, "1-") {
			t.Fatalf("bad ticket name %s", n)
		}
	}
	if len(tickets(h, "lint")) != 3 || highTickets(h, "lint") != 1 {
		t.Fatalf("tickets %v", tickets(h, "lint"))
	}
	hold.Release()
	hp.Wait()
	np.Wait()
}

func TestR6_PriorityIsPerPoolNotPerGroup(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("priority = { high_cwd_contains = [\"urgent\"] }\ngroups = { bigmem = { units = 2 } }\n" +
		"[pools.heavy]\ngroups = { bigmem = 1 }\nwait = \"30s\"\n[pools.coverage]\ngroups = { bigmem = 1 }\nwait = \"1s\"\n")
	heavyHold := h.Hold(nil, "heavy")                          // heavy's only slot and one bigmem unit, kept to the end
	hp := startIn(h, "urgent-d", "run", "heavy", "--", "true") // high heavy waits for heavy's slot
	h.Until(func() bool { return highTickets(h, "heavy") == 1 }, 10*time.Second)
	// coverage shares bigmem with heavy, but heavy's high ticket is heavy's alone: coverage
	// takes the free unit at once, while that ticket still waits
	if r := h.Run("run", "coverage", "--", "true"); r.Code != 0 || strings.Contains(r.Stderr, "priority lane") {
		t.Fatalf("coverage waited behind heavy's high ticket: %+v", r)
	}
	if highTickets(h, "heavy") != 1 || hp.Exited() {
		t.Fatal("the high heavy ticket is no longer waiting")
	}
	heavyHold.Release()
	if r := hp.Wait(); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
}
