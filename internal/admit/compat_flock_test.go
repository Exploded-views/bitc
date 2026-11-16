package admit

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/proc"
)

type flockRig struct {
	fl config.Flock
	r  func(pool *config.Pool) Request
}

// flockSetup: pools heavy (2 slots) and bench (every unit of group cpu), both named in
// [compat.flock], with every file under t.TempDir().
func flockSetup(t *testing.T) (flockRig, *config.Pool, *config.Pool) {
	t.Helper()
	dir := t.TempDir()
	fl := config.Flock{Lock: filepath.Join(dir, "heavy.lock"), Gate: filepath.Join(dir, "heavy.gate"), Slot: filepath.Join(dir, "heavy.slot{n}")}
	heavy := &config.Pool{Name: "heavy", Slots: 2, Groups: map[string]int{"cpu": 1}}
	bench := &config.Pool{Name: "bench", Slots: 1, Groups: map[string]int{"cpu": 2}}
	l, e := setup(t, map[string]*config.Pool{"heavy": heavy, "bench": bench}, map[string]config.Group{"cpu": {Name: "cpu", Units: 2}})
	benchFl := fl
	benchFl.Exclusive = true
	e.Compat = &config.Compat{LegacyDir: filepath.Join(dir, "legacy"), Flock: map[string]config.Flock{"heavy": fl, "bench": benchFl}}
	return flockRig{fl: fl, r: func(p *config.Pool) Request { return req(l, e, p) }}, heavy, bench
}

// shellTake takes path the way flock(1) does (shared or exclusive) and keeps it until cleanup.
func shellTake(t *testing.T, path string, shared bool) {
	t.Helper()
	f, err := lock.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	var ok bool
	if shared {
		ok, err = f.TryLockShared()
	} else {
		ok, err = f.TryLock()
	}
	if err != nil || !ok {
		t.Fatalf("shell take of %s: %v %v", path, ok, err)
	}
}

// busy reports whether path cannot be taken exclusively now.
func busy(t *testing.T, path string) bool {
	t.Helper()
	f, err := lock.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ok, err := f.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		_ = f.Unlock()
	}
	return !ok
}

func TestFlockNormalRunTakesLockSharedAndSlotFile(t *testing.T) {
	rig, heavy, _ := flockSetup(t)
	shellTake(t, FlockSlotPath(rig.fl, 1), false) // a shell run holder of slot 1
	h, err := Acquire(context.Background(), rig.r(heavy))
	if err != nil || h.Slot != 2 {
		t.Fatal(h, err)
	}
	if !busy(t, rig.fl.Lock) || !busy(t, FlockSlotPath(rig.fl, 2)) {
		t.Fatal("a bitc run must hold the lock and its slot file")
	}
	if busy(t, rig.fl.Gate) {
		t.Fatal("a normal run only passes through the gate")
	}
	shellTake(t, rig.fl.Lock, true) // shared with shell run holders
	h.Release()
}

func TestFlockBusyLegacyLockNamesItAndHoldsNothing(t *testing.T) {
	for _, c := range []struct {
		name   string
		bench  bool
		file   func(config.Flock) string
		shared bool
	}{
		{"bare exclusive on the lock", false, func(f config.Flock) string { return f.Lock }, false},
		{"shell bench holds the gate", false, func(f config.Flock) string { return f.Gate }, false},
		{"shell run holder blocks a bench", true, func(f config.Flock) string { return f.Lock }, true},
	} {
		rig, heavy, bench := flockSetup(t)
		pool, path := heavy, c.file(rig.fl)
		if c.bench {
			pool = bench
		}
		shellTake(t, path, c.shared)
		_, err := Acquire(context.Background(), rig.r(pool))
		if err == nil || !strings.Contains(err.Error(), "legacy pool: "+path+" busy") {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}

func TestFlockExclusivePoolTakesGateAndLockNotSlotFiles(t *testing.T) {
	rig, _, bench := flockSetup(t)
	h, err := Acquire(context.Background(), rig.r(bench))
	if err != nil {
		t.Fatal(err)
	}
	if !busy(t, rig.fl.Gate) || !busy(t, rig.fl.Lock) {
		t.Fatal("an exclusive pool holds the gate and the lock")
	}
	if busy(t, FlockSlotPath(rig.fl, 1)) {
		t.Fatal("an exclusive pool takes no slot file")
	}
	h.Release()
	if busy(t, rig.fl.Gate) || busy(t, rig.fl.Lock) {
		t.Fatal("release left the gate or the lock held")
	}
}

func TestFlockRetriesAfterAFailedAttemptHoldNothing(t *testing.T) {
	rig, heavy, _ := flockSetup(t)
	shellTake(t, FlockSlotPath(rig.fl, 1), false)
	shellTake(t, FlockSlotPath(rig.fl, 2), false)
	if _, err := Acquire(context.Background(), rig.r(heavy)); err == nil || !strings.Contains(err.Error(), "legacy pool: "+FlockSlotPath(rig.fl, 1)+" busy") {
		t.Fatal("admitted with every slot file held, or the reason does not name the slot file:", err)
	}
	if busy(t, rig.fl.Lock) || busy(t, rig.fl.Gate) {
		t.Fatal("a failed attempt kept the lock or the gate")
	}
}

// TestFlockExclusiveIsTheKeyNotTheUnits: the mode comes from [compat.flock.<pool>].exclusive, not
// from how many units of a group the pool asks for.
func TestFlockExclusiveIsTheKeyNotTheUnits(t *testing.T) {
	rig, _, bench := flockSetup(t)
	small := *bench
	small.Groups = map[string]int{"cpu": 1} // fewer units than the group has
	h, err := Acquire(context.Background(), rig.r(&small))
	if err != nil {
		t.Fatal(err)
	}
	if !busy(t, rig.fl.Gate) || !busy(t, rig.fl.Lock) || busy(t, FlockSlotPath(rig.fl, 1)) {
		t.Fatal("exclusive = true holds gate and lock whatever the units, and no slot file")
	}
	h.Release()
}

func TestFlockDefaultIsSharedEvenWhenAPoolTakesAWholeGroup(t *testing.T) {
	rig, _, bench := flockSetup(t)
	l, e := setup(t, map[string]*config.Pool{"bench": bench}, map[string]config.Group{"cpu": {Name: "cpu", Units: 2}})
	e.Compat = &config.Compat{LegacyDir: filepath.Join(t.TempDir(), "legacy"), Flock: map[string]config.Flock{"bench": rig.fl}} // exclusive unset
	h, err := Acquire(context.Background(), req(l, e, bench))
	if err != nil {
		t.Fatal(err)
	}
	if busy(t, rig.fl.Gate) || !busy(t, FlockSlotPath(rig.fl, 1)) {
		t.Fatal("without exclusive = true the pool is a normal one: slot file, gate only passed")
	}
	shellTake(t, rig.fl.Lock, true) // lock shared
	h.Release()
}

// A waiting exclusive pool that fails on the flock lock keeps its group claim, so newer bitc
// consumers of its group do not overtake it (spec 6 FIFO), and it is admitted once they drain.
func TestFlockWaitingExclusivePoolKeepsItsFIFOClaim(t *testing.T) {
	rig, heavy, bench := flockSetup(t)
	h1, err := Acquire(context.Background(), rig.r(heavy)) // lock shared
	if err != nil {
		t.Fatal(err)
	}
	attempt := func(pool *config.Pool) (*admission, bool, string) {
		self, err := proc.ReadStat(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		r := rig.r(pool)
		a := &admission{r: r, pool: pool, notes: io.Discard, compat: NewCompat(r.Eff.Compat, r.Layout)}
		me, err := newTicket(r.Layout, pool.Name, Ticket{Pid: os.Getpid(), Start: self.Start, Argv: r.Argv, Cwd: "/w", Job: "j",
			Class: 1, Units: pool.Groups, State: "waiting", Token: randHex(8)})
		if err != nil {
			t.Fatal(err)
		}
		a.me = me
		t.Cleanup(a.me.drop)
		ok, reason, err := a.attempt(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return a, ok, reason
	}
	b, ok, reason := attempt(bench)
	if ok || !strings.Contains(reason, "legacy pool: "+rig.fl.Lock+" busy") {
		t.Fatalf("bench admitted beside a shared holder: %v %q", ok, reason)
	}
	if got := b.me.t.GroupWait; len(got) != 1 || got[0] != "cpu" {
		t.Fatalf("a waiting exclusive pool must claim its group, group_wait=%v", got)
	}
	if _, ok, reason := attempt(heavy); ok || reason != "group cpu busy" {
		t.Fatalf("a newer heavy overtook the waiting bench: %v %q", ok, reason)
	}
	h1.Release()
	if got, _, err := b.attempt(time.Now()); err != nil || !got {
		t.Fatal("bench not admitted once the heavies drained", err)
	}
	b.releaseHeld()
}
