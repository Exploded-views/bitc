// Package rails is the shared remote safety rails (spec 8.3).
package rails

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/proc"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/remote"
	"github.com/Exploded-views/bitc/internal/run"
	"github.com/Exploded-views/bitc/internal/state"
)

func init() {
	remote.LedgerToday = LedgerToday
	remote.BreakerState = Paused
	remote.Rails = Check
	remote.PruneReservations = PruneReservationsIn
	remote.OrphanNames = OrphanNames
	remote.DropOrphan = DropOrphan
	remote.LiveReservations = LiveReservations
	remote.OnLaunch = func(j *remote.Job) { _ = Ledger{Dir: j.RailsDir}.Launch(j.Spec.Name, j.Estimate) }
	remote.OnBegin = func(j *remote.Job) { _ = Ledger{Dir: j.RailsDir}.Begin(j.Spec.Name) }
	remote.OnEnd = func(j *remote.Job, outcome string) {
		l := Ledger{Dir: j.RailsDir}
		if j.Created.IsZero() { // nothing was created (a capacity fallback, a failed create): no cost
			_ = l.End(j.Spec.Name, 0, j.Estimate, 0, outcome, false)
			return
		}
		minutes := int(time.Since(j.Created).Minutes()) + 1 // billing starts at the create; rounded up
		span := int(j.Spec.Expiry.Sub(j.Created).Minutes()) + 1
		cost := j.Estimate
		if minutes < span {
			cost = config.Micros(int64(j.Estimate) * int64(minutes) / int64(span))
		}
		_ = l.End(j.Spec.Name, minutes, j.Estimate, cost, outcome, true)
	}
	remote.OnFailure = func(j *remote.Job) { _ = RecordFailure(j.RailsDir) }
	remote.OnUnavailable = func(j *remote.Job, why string) { // RailsDir may be unset: compute it
		acct, err := state.AccountDir(j.P.Eff)
		if err != nil {
			return
		}
		dir := filepath.Join(acct, "remote", j.Type)
		if state.MkdirPrivate(dir) == nil {
			_ = Pause(dir, why, j.Core.Breaker.Window)
		}
	}
}

type reservation struct {
	Pid      int    `json:"pid"`
	Start    uint64 `json:"start"`
	Name     string `json:"name"`
	Estimate int64  `json:"estimate"` // micro-dollars
	Expiry   int64  `json:"expiry"`   // unix
	// Orphan: the attempt's resource was discarded before attach and not confirmed gone (spec
	// 8.3). It is tied to no process: it counts in the cap and max_live while its name is
	// unlisted, is dropped when a teardown confirms the resource gone or at Expiry, and never
	// shields a listed resource from the sweep or the next cap check.
	Orphan bool `json:"orphan,omitempty"`
}

// Check runs breaker, launch lock, cap and max_live (spec 8.3) and records a reservation; the
// release func drops the reservation (at the job's ledger end) and is idempotent.
func Check(ctx context.Context, j *remote.Job) (func(), error) {
	acct, err := state.AccountDir(j.P.Eff)
	if err != nil {
		return nil, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	dir := filepath.Join(acct, "remote", j.Type)
	if err := state.MkdirPrivate(dir); err != nil {
		return nil, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	if why, paused := Paused(dir, j.Core.Breaker, time.Now()); paused {
		return nil, &provider.ErrTransient{Why: j.Type + " is paused: " + why}
	}
	ll, err := lock.Open(filepath.Join(dir, "launch.lock"))
	if err != nil {
		return nil, &provider.ErrTransient{Why: err.Error()}
	}
	if err := ll.LockWait(ctx, j.P.Eff.Scale(j.Core.LaunchWait)); err != nil {
		ll.Close()
		return nil, &provider.ErrTransient{Why: "the launch lock is busy"}
	}
	unlockLegacy := legacyLaunchLock(ctx, j)                       // [compat]: after our own, within launch_wait
	defer func() { unlockLegacy(); _ = ll.Unlock(); ll.Close() }() // released before the job runs
	secret, err := state.InstallSecret(acct)
	if err != nil {
		return nil, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	resvPath := filepath.Join(dir, "reservations.json")
	resv := pruneReservations(readReservations(resvPath), time.Now())
	est, err := j.Prov.Estimate(ctx, j.Spec)
	if err != nil {
		return nil, railError(err)
	}
	worst := j.Core.StartTimeout + longestTimeout(j.P.Eff) + 15*time.Minute
	in := capInput{core: j.Core, prov: j.Prov, ownPrefix: j.Core.NamePrefix + "-", worstSpan: worst,
		highest: highestPrice(j), ledger: LedgerToday(dir, time.Now()), resv: resv, secret: secret,
		thisJob: est, now: time.Now(), note: func(s string) { j.Note("%s", s) },
		debug: func(s string) { run.Debugf(j.P, j.Stderr, "%s: %s", j.Type, s) }}
	if j.P.Eff.Compat != nil && j.P.Eff.Compat.NamePrefix != "" {
		in.compatPfx = j.P.Eff.Compat.NamePrefix
	}
	if err := checkCap(ctx, &in); err != nil {
		return nil, err
	}
	resv = dropOrphans(resv, in.tornOrphans) // torn down by the cap check just now
	j.Estimate, j.RailsDir = est, dir
	self, _ := proc.ReadStat(os.Getpid())
	me := reservation{Pid: os.Getpid(), Start: self.Start, Name: j.Spec.Name, Estimate: int64(est), Expiry: j.Spec.Expiry.Unix()}
	if err := writeReservations(resvPath, append(resv, me)); err != nil {
		return nil, &provider.ErrTransient{Why: err.Error()}
	}
	dropLegacy := legacyReservation(j, est)
	done := false
	j.Orphan = func() { // keep the reservation as an orphan (spec 8.3); the release that follows is a no-op
		if done {
			return
		}
		done = true
		dropLegacy()
		_ = withLock(dir, func() error {
			rs := readReservations(resvPath)
			for i := range rs {
				if rs[i].Name == me.Name {
					rs[i].Orphan = true
				}
			}
			return writeReservations(resvPath, rs)
		})
	}
	return func() {
		if done {
			return
		}
		done = true
		dropLegacy()
		_ = withLock(dir, func() error {
			var kept []reservation
			for _, r := range readReservations(resvPath) {
				if r.Name != me.Name {
					kept = append(kept, r)
				}
			}
			return writeReservations(resvPath, kept)
		})
	}, nil
}

func readReservations(p string) []reservation {
	var rs []reservation
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &rs)
	}
	return rs
}

func writeReservations(p string, rs []reservation) error {
	b, _ := json.Marshal(rs)
	return state.WriteFileAtomic(p, b)
}

// pruneReservations drops expired reservations and those whose process is gone (a dead pid, a
// zombie, or a pid reused by a process with another start time).
func pruneReservations(rs []reservation, now time.Time) []reservation {
	var out []reservation
	for _, r := range rs {
		if now.Unix() > r.Expiry {
			continue
		}
		if r.Orphan {
			out = append(out, r)
			continue
		}
		if st, err := proc.ReadStat(r.Pid); err != nil || st.State == 'Z' || (r.Start != 0 && st.Start != r.Start) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func withLock(dir string, fn func() error) error { return withLockFor(dir, 30*time.Second, fn) }

func withLockFor(dir string, wait time.Duration, fn func() error) error {
	l, err := lock.Open(filepath.Join(dir, "launch.lock"))
	if err != nil {
		return err
	}
	defer l.Close()
	if err := l.LockWait(context.Background(), wait); err != nil {
		return err
	}
	defer l.Unlock()
	return fn()
}

func longestTimeout(e *config.Effective) time.Duration {
	var m time.Duration
	for _, p := range e.Pools {
		if p.RemoteTimeout > m {
			m = p.RemoteTimeout
		}
	}
	return m
}

// highestPrice asks the provider for its most expensive size through Estimate on a 1h job.
func highestPrice(j *remote.Job) config.Micros {
	probe := *j.Spec
	probe.SizeKey = "\x00unknown" // unknown size keys get the highest price (spec 8.7)
	probe.Expiry = time.Now().Add(time.Hour)
	m, _ := j.Prov.Estimate(context.Background(), &probe)
	return m
}

// PruneReservationsIn drops expired reservations and those whose process is gone from
// dir/reservations.json under the launch lock (bounded wait) and returns the dropped names.
// With dry the file is left alone.
func PruneReservationsIn(dir string, dry bool) []string {
	path := filepath.Join(dir, "reservations.json")
	var dropped []string
	run := func() error {
		all := readReservations(path)
		kept := pruneReservations(all, time.Now())
		if len(kept) == len(all) {
			return nil
		}
		live := map[string]bool{}
		for _, r := range kept {
			live[r.Name] = true
		}
		for _, r := range all {
			if !live[r.Name] {
				dropped = append(dropped, r.Name)
			}
		}
		if dry {
			return nil
		}
		return writeReservations(path, kept)
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	if err := withLockFor(dir, 5*time.Second, run); err != nil {
		return nil
	}
	return dropped
}

func dropOrphans(rs []reservation, names []string) []reservation {
	if len(names) == 0 {
		return rs
	}
	var out []reservation
	for _, r := range rs {
		if !r.Orphan || !slices.Contains(names, r.Name) {
			out = append(out, r)
		}
	}
	return out
}

// OrphanNames lists the names whose only reservation in dir/reservations.json is an orphan.
func OrphanNames(dir string) map[string]bool {
	out := map[string]bool{}
	live := map[string]bool{}
	for _, r := range readReservations(filepath.Join(dir, "reservations.json")) {
		if r.Orphan {
			out[r.Name] = true
		} else {
			live[r.Name] = true
		}
	}
	for n := range live {
		delete(out, n)
	}
	return out
}

// DropOrphan removes the orphan reservation for name (its resource was confirmed gone).
func DropOrphan(dir, name string) {
	path := filepath.Join(dir, "reservations.json")
	_ = withLockFor(dir, 5*time.Second, func() error {
		return writeReservations(path, dropOrphans(readReservations(path), []string{name}))
	})
}

// LiveReservations lists the names with a non-orphan reservation in dir/reservations.json.
func LiveReservations(dir string) map[string]bool {
	out := map[string]bool{}
	for _, r := range readReservations(filepath.Join(dir, "reservations.json")) {
		if !r.Orphan {
			out[r.Name] = true
		}
	}
	return out
}
