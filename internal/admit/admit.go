package admit

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/proc"
)

const admitLockWait = 30 * time.Second

type admission struct {
	r           Request
	pool        *config.Pool
	me          *own
	parent      *TicketFile // verified BITC_HOLD, or nil
	samePool    bool
	held        []*lock.File // locks taken by the current or successful attempt
	units       map[string][]int
	slot        int
	depth       int
	compat      CompatLocks
	legacyGroup string   // group of a verified legacy claim ([compat])
	inherited   []string // groups held by ancestors (verified parent chain, legacy claim)
	releases    []func() // legacy locks taken with the bitc ones ([compat])
	memory      *MemoryGate
	notes       io.Writer
}

// Acquire queues for a slot of r.Pool and returns the hold, or exit.Starved at the deadline.
func Acquire(ctx context.Context, r Request) (*Hold, error) {
	now := r.Now
	if now == nil {
		now = time.Now
	}
	self, err := proc.ReadStat(os.Getpid())
	if err != nil {
		return nil, exit.Unavailablef("cannot read /proc/self/stat: %v", err)
	}
	a := &admission{r: r, pool: r.Pool, notes: r.Stderr, compat: NewCompat(r.Eff.Compat, r.Layout)}
	if a.notes == nil {
		a.notes = io.Discard
	}
	if r.CallerHold != "" {
		if p, ok := VerifyHold(r.Layout, r.CallerHold); ok {
			a.parent = p
			a.samePool = p.Pool == r.Pool.Name
			a.depth = p.T.Depth + 1
			for g, js := range p.T.Groups {
				if len(js) > 0 {
					a.inherited = append(a.inherited, g)
				}
			}
			a.inherited = append(a.inherited, p.T.Inherited...)
		}
	}
	if a.compat != nil {
		if g, ok := a.compat.VerifiedClaim(r.CallerEnv); ok {
			a.legacyGroup = g
			a.inherited = append(a.inherited, g)
		}
	}
	sort.Strings(a.inherited)
	a.inherited = slices.Compact(a.inherited)
	if r.Pool.Memory > 0 && a.parent == nil && a.legacyGroup == "" {
		var procfs string
		if r.Eff.Test != nil {
			procfs = r.Eff.Test.ProcfsRoot
		}
		a.memory = &MemoryGate{ProcfsRoot: procfs, Floor: int64(r.Eff.Host.MemoryFloor),
			Window: r.Eff.Host.ReservationWindow, ReservationsPath: r.Layout.Reservations(), Warn: a.notes}
	}
	class := 1
	if IsHigh(r.Eff) {
		class = 0
	}
	token := randHex(16)
	me, err := newTicket(r.Layout, r.Pool.Name, Ticket{Pid: os.Getpid(), Start: self.Start, Argv: r.Argv,
		Cwd: r.Eff.Cwd, Job: r.Job, Class: class, Nested: a.samePool, Depth: a.depth, Inherited: a.inherited, State: "waiting", Token: token})
	if err != nil {
		return nil, exit.Unavailablef("cannot create a ticket: %v", err)
	}
	a.me = me
	var removeMirror func()
	if class == 0 && a.compat != nil {
		removeMirror, _ = a.compat.MirrorHigh(r.Pool.Name, os.Getpid())
	}
	waker := newWaker(a.watchDirs())
	defer waker.close()
	start := now()
	deadline := start.Add(r.Eff.Scale(r.Pool.Wait))
	poll := r.Eff.Scale(r.Eff.Host.Poll)
	for {
		ok, reason, err := a.attempt(now())
		if err != nil {
			a.abandon(removeMirror)
			return nil, exit.Unavailablef("admission: %v", err)
		}
		if ok {
			return a.hold(start, now(), removeMirror), nil
		}
		if !now().Before(deadline) {
			a.abandon(removeMirror)
			return nil, exit.Starved(r.Pool.Name, reason)
		}
		if ctx.Err() != nil {
			a.abandon(removeMirror)
			return nil, ctx.Err()
		}
		wait := deadline.Sub(now())
		if wait > poll {
			wait = poll
		}
		waker.sleep(ctx, wait)
	}
}

// consumers are the pools that list group g (canonical names).
func (a *admission) consumers(g string) []string {
	var out []string
	for name, p := range a.r.Eff.Pools {
		if p.Groups[g] > 0 {
			out = append(out, name)
		}
	}
	if a.pool.AdHoc && a.pool.Groups[g] > 0 {
		out = append(out, a.pool.Name)
	}
	sort.Strings(out)
	return out
}

func (a *admission) related() []string {
	set := map[string]bool{a.pool.Name: true}
	for g := range a.pool.Groups {
		for _, p := range a.consumers(g) {
			set[p] = true
		}
	}
	var out []string
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (a *admission) watchDirs() []string {
	var dirs []string
	for _, p := range a.related() {
		_ = os.MkdirAll(a.r.Layout.Tickets(p), 0o700)
		dirs = append(dirs, a.r.Layout.Tickets(p))
	}
	return dirs
}

// attempt runs one admission attempt under admit.lock (spec 6.2 step 2).
func (a *admission) attempt(now time.Time) (bool, string, error) {
	al, err := lock.Open(a.r.Layout.AdmitLock())
	if err != nil {
		return false, "", err
	}
	defer al.Close()
	if err := al.LockWait(context.Background(), admitLockWait); err != nil {
		return false, "", err
	}
	defer al.Unlock()

	for _, p := range a.related() { // prune
		prune(a.r.Layout, p, func(note, rp, job string) {
			fmt.Fprintln(a.notes, note)
			if a.r.OnReap != nil {
				a.r.OnReap(rp, job)
			}
		}, func(t Ticket) {
			if t.Class == 0 && a.compat != nil {
				a.compat.RemoveMirror(p, t.Pid)
			}
		})
	}
	tickets := ReadTickets(a.r.Layout, a.pool.Name)

	fail := func(reason string, groupWait []string) (bool, string, error) {
		a.releaseHeld()
		a.me.t.GroupWait = groupWait
		return false, reason, a.me.write()
	}

	// priority: a normal ticket waits while the pool has a live high ticket (per pool)
	if a.me.t.Class == 1 && !a.samePool {
		for _, t := range tickets {
			if t.Name != a.me.name && t.T.Class == 0 {
				return fail("priority lane", nil)
			}
		}
		if a.compat != nil && a.compat.LegacyHigh(a.pool.Name) {
			return fail("priority lane", nil)
		}
	}

	if a.samePool { // one nested child per hold at a time (spec 6.6)
		nl, err := lock.Open(a.r.Layout.NestLock(a.pool.Name, a.parent.T.Slot, a.depth))
		if err != nil {
			return false, "", err
		}
		if ok, _ := nl.TryLock(); !ok {
			nl.Close()
			return fail("nested child busy", nil)
		}
		a.held = append(a.held, nl)
		a.slot = a.parent.T.Slot
		return a.admitted(now)
	}

	// queue: eligible iff our (class, time) position among waiters < free slots
	running, pos := 0, 0
	for _, t := range tickets {
		switch {
		case t.T.Nested:
		case t.T.State == "running":
			running++
		case t.Name < a.me.name:
			pos++
		}
	}
	free := a.pool.Slots - running
	if pos >= free {
		if pos > 0 {
			return fail("queue", nil)
		}
		return fail("", nil)
	}

	// slot: lowest free slot.<i>.lock (with [compat], slot i also needs the legacy slot i lock)
	legacyBusy := ""
	for i := 1; i <= a.pool.Slots && a.slot == 0; i++ {
		sl, err := lock.Open(a.r.Layout.SlotLock(a.pool.Name, i))
		if err != nil {
			return false, "", err
		}
		if ok, _ := sl.TryLock(); !ok {
			sl.Close()
			continue
		}
		if a.compat != nil {
			rel, ok, err := a.compat.TrySlot(a.pool.Name, i)
			if err != nil || !ok {
				_ = sl.Unlock()
				sl.Close()
				if err != nil {
					return false, "", err
				}
				if legacyBusy == "" {
					legacyBusy = fmt.Sprintf("%s.%d", a.pool.Name, i)
				}
				continue
			}
			a.releases = append(a.releases, rel)
		}
		a.held = append(a.held, sl)
		a.slot = i
	}
	if a.slot == 0 {
		if legacyBusy != "" {
			return fail("legacy pool: "+legacyBusy+" busy", nil)
		}
		return fail("", nil)
	}

	// groups: busy if an older live ticket of any consumer lists g in group_wait; else take
	// units free unit.<j>.lock all or none
	a.units = map[string][]int{}
	names := make([]string, 0, len(a.pool.Groups))
	for g := range a.pool.Groups {
		names = append(names, g)
	}
	sort.Strings(names)
	for _, g := range names {
		if contains(a.inherited, g) {
			continue // an ancestor holds this group: a verified BITC_HOLD chain (spec 6.6) or legacy claim (spec 12)
		}
		for _, p := range a.consumers(g) {
			for _, t := range ReadTickets(a.r.Layout, p) {
				if t.T.WaitStarted < a.me.t.WaitStarted && contains(t.T.GroupWait, g) {
					return fail("group "+g+" busy", []string{g})
				}
			}
		}
		need := a.pool.Groups[g]
		var got []int
		legacyUnit := ""
		for j := 1; j <= a.r.Eff.Groups[g].Units && len(got) < need; j++ {
			ul, err := lock.Open(a.r.Layout.GroupUnitLock(g, j))
			if err != nil {
				return false, "", err
			}
			if ok, _ := ul.TryLock(); !ok {
				ul.Close()
				continue
			}
			if a.compat != nil {
				rel, ok, err := a.compat.TryUnit(g, j, os.Getpid())
				if err != nil || !ok {
					_ = ul.Unlock()
					ul.Close()
					if err != nil {
						return false, "", err
					}
					legacyUnit = fmt.Sprintf("%s.%d", g, j)
					continue
				}
				a.releases = append(a.releases, rel)
			}
			a.held = append(a.held, ul)
			got = append(got, j)
		}
		if len(got) < need {
			if legacyUnit != "" {
				return fail("legacy pool: "+legacyUnit+" busy", nil)
			}
			return fail("group "+g+" busy", []string{g})
		}
		a.units[g] = got
	}

	// memory (pools with memory, outside a verified nested hold)
	if a.memory != nil {
		ok, reason, err := a.memory.Check(now)
		if err != nil {
			return false, "", err
		}
		if !ok {
			return fail(reason, nil)
		}
		if err := a.memory.Reserve(now, a.me.name, int64(a.pool.Memory)); err != nil {
			return false, "", err
		}
	}
	return a.admitted(now)
}

func (a *admission) admitted(now time.Time) (bool, string, error) {
	a.me.t.State, a.me.t.Slot, a.me.t.Groups, a.me.t.GroupWait = "running", a.slot, a.units, nil
	a.me.t.HoldStarted = now.UnixNano()
	return true, "", a.me.write()
}

func (a *admission) releaseHeld() {
	for _, rel := range a.releases {
		rel()
	}
	a.releases = nil
	for _, l := range a.held {
		_ = l.Unlock()
		l.Close()
	}
	a.held, a.slot, a.units = nil, 0, nil
}

func (a *admission) abandon(removeMirror func()) {
	a.releaseHeld()
	if a.memory != nil { // a failure after Reserve (the ticket write) must not leave it counting
		al, err := lock.Open(a.r.Layout.AdmitLock())
		if err == nil {
			_ = al.LockWait(context.Background(), admitLockWait)
		}
		_ = a.memory.Drop(a.me.name)
		if al != nil {
			_ = al.Unlock()
			al.Close()
		}
	}
	if removeMirror != nil {
		removeMirror()
	}
	a.me.drop()
}

func (a *admission) hold(start, now time.Time, removeMirror func()) *Hold {
	h := &Hold{Pool: a.pool.Name, Slot: a.slot, Ticket: a.me.name, Token: a.me.t.Token, Nested: a.parent != nil,
		Depth: a.depth, Units: a.units, WaitMS: now.Sub(start).Milliseconds(), Started: now}
	if a.compat != nil {
		if kv := a.compat.HoldEnv(a.units); kv != "" {
			h.ExtraEnv = []string{kv}
		}
	}
	h.setProcess = func(pid, pgid int, leaderStart uint64) error {
		al, err := lock.Open(a.r.Layout.AdmitLock())
		if err != nil {
			return err
		}
		defer al.Close()
		if err := al.LockWait(context.Background(), admitLockWait); err != nil {
			return err
		}
		defer al.Unlock()
		a.me.t.Leader, a.me.t.Pgid, a.me.t.LeaderStart = pid, pgid, leaderStart
		return a.me.write()
	}
	h.release = func() {
		al, err := lock.Open(a.r.Layout.AdmitLock())
		if err == nil {
			_ = al.LockWait(context.Background(), admitLockWait)
		}
		a.releaseHeld() // unlock slot, units, nest and compat locks first
		if a.memory != nil {
			_ = a.memory.Drop(a.me.name)
		}
		if removeMirror != nil {
			removeMirror()
		}
		a.me.drop() // then unlink the ticket and release its flock
		if al != nil {
			_ = al.Unlock()
			al.Close()
		}
	}
	return h
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
