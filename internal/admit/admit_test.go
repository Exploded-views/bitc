package admit

import (
	"context"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/proc"
	"github.com/Exploded-views/bitc/internal/proctest"
	"github.com/Exploded-views/bitc/internal/state"
)

func setup(t *testing.T, pools map[string]*config.Pool, groups map[string]config.Group) (state.Layout, *config.Effective) {
	t.Helper()
	l, err := state.Open(t.TempDir() + "/state")
	if err != nil {
		t.Fatal(err)
	}
	e := &config.Effective{Pools: pools, Groups: groups, Cwd: "/w", LogicalCwd: "/w",
		Host: config.Host{Poll: 20 * time.Millisecond}}
	return l, e
}

func req(l state.Layout, e *config.Effective, p *config.Pool) Request {
	return Request{Layout: l, Eff: e, Pool: p, Argv: []string{"x"}, Job: "j", Stderr: io.Discard}
}

func TestOneSlotThenStarved(t *testing.T) {
	lint := &config.Pool{Name: "lint", Slots: 1, Wait: 200 * time.Millisecond}
	l, e := setup(t, map[string]*config.Pool{"lint": lint}, nil)
	h1, err := Acquire(context.Background(), req(l, e, lint))
	if err != nil || h1.Slot != 1 {
		t.Fatal(h1, err)
	}
	t0 := time.Now()
	_, err = Acquire(context.Background(), req(l, e, lint))
	if exit.Code(err) != 75 || err.Error() != "STARVED lint" {
		t.Fatalf("%v", err)
	}
	if d := time.Since(t0); d < 200*time.Millisecond { // starved no sooner than the wait
		t.Fatalf("waited %v", d)
	}
	h1.Release()
	h2, err := Acquire(context.Background(), req(l, e, lint))
	if err != nil || h2.Slot != 1 {
		t.Fatal(err)
	}
	h2.Release()
}

func TestTwoSlotsLowestFirst(t *testing.T) {
	db := &config.Pool{Name: "db", Slots: 2, Wait: 0}
	l, e := setup(t, map[string]*config.Pool{"db": db}, nil)
	a, _ := Acquire(context.Background(), req(l, e, db))
	b, _ := Acquire(context.Background(), req(l, e, db))
	if a.Slot != 1 || b.Slot != 2 {
		t.Fatal(a.Slot, b.Slot)
	}
	a.Release()
	c, _ := Acquire(context.Background(), req(l, e, db))
	if c.Slot != 1 {
		t.Fatal(c.Slot)
	}
	b.Release()
	c.Release()
}

func TestGroupBusyReleasesSlot(t *testing.T) {
	groups := map[string]config.Group{"bigmem": {Name: "bigmem", Units: 1}}
	heavy := &config.Pool{Name: "heavy", Slots: 1, Groups: map[string]int{"bigmem": 1}}
	cov := &config.Pool{Name: "coverage", Slots: 1, Wait: 100 * time.Millisecond, Groups: map[string]int{"bigmem": 1}}
	l, e := setup(t, map[string]*config.Pool{"heavy": heavy, "coverage": cov}, groups)
	h, err := Acquire(context.Background(), req(l, e, heavy))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Acquire(context.Background(), req(l, e, cov))
	if err == nil || err.Error() != "STARVED coverage (group bigmem busy)" {
		t.Fatalf("%v", err)
	}
	h.Release()
}

func TestWaitZeroIsOneAttempt(t *testing.T) {
	lint := &config.Pool{Name: "lint", Slots: 1}
	l, e := setup(t, map[string]*config.Pool{"lint": lint}, nil)
	h, _ := Acquire(context.Background(), req(l, e, lint))
	t0 := time.Now()
	if _, err := Acquire(context.Background(), req(l, e, lint)); exit.Code(err) != 75 || time.Since(t0) > 500*time.Millisecond {
		t.Fatal(err, time.Since(t0))
	}
	h.Release()
}

func TestContextCancelRemovesTicket(t *testing.T) {
	lint := &config.Pool{Name: "lint", Slots: 1, Wait: time.Minute}
	l, e := setup(t, map[string]*config.Pool{"lint": lint}, nil)
	h, _ := Acquire(context.Background(), req(l, e, lint))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, req(l, e, lint)); err == nil {
		t.Fatal("no error")
	}
	if n := len(ReadTickets(l, "lint")); n != 1 {
		t.Fatalf("%d tickets left, want 1 (the holder's)", n)
	}
	h.Release()
}

func TestReapWithoutAGroupKillsOnlyTheCommandTree(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60 & wait") // the "command": a shell and its child
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sib := exec.Command("sleep", "60") // same process group, started after the command
	if err := sib.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sib.Process.Kill(); _ = sib.Wait() }()
	var kids []int
	deadline := time.Now().Add(2 * time.Second)
	for len(kids) == 0 && time.Now().Before(deadline) {
		for _, pid := range proc.Pids() {
			if st, err := proc.ReadStat(pid); err == nil && st.PPid == cmd.Process.Pid {
				kids = append(kids, pid)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(kids) == 0 {
		t.Fatal("the shell never forked")
	}
	st, err := proc.ReadStat(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	reap(Ticket{Leader: cmd.Process.Pid, LeaderStart: st.Start}) // pgid 0
	_ = cmd.Wait()
	for _, k := range kids {
		gone := false
		for i := 0; i < 100 && !gone; i++ {
			gone = proctest.Gone(k)
			time.Sleep(20 * time.Millisecond)
		}
		if !gone {
			t.Fatalf("descendant %d survived", k)
		}
	}
	if proctest.Gone(sib.Process.Pid) {
		t.Fatal("a process outside the command's tree was killed")
	}
}
