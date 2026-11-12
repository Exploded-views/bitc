// Package status implements status, sweep and doctor (spec 3.3).
package status

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/admit"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/redact"
	"github.com/Exploded-views/bitc/internal/state"
)

type Holder struct {
	Job      string         `json:"job"`
	Slot     int            `json:"slot"`
	Pid      int            `json:"pid"`
	Cwd      string         `json:"cwd"`
	Argv     []string       `json:"argv"`
	Provider string         `json:"provider"`
	Nested   bool           `json:"nested"`
	Since    string         `json:"since"`
	Units    map[string]int `json:"units,omitempty"`  // units held per group
	Legacy   bool           `json:"legacy,omitempty"` // a legacy shell-pool caller holds <legacy_dir>/locks/<pool>.<slot>.lock
	// Unknown: the slot is in use as far as its ticket or lock file shows, but this host cannot
	// read lock holders (no /proc/locks, #24), so it is not known whether anyone holds it.
	Unknown bool `json:"unknown,omitempty"`
}

type Waiter struct {
	Job      string         `json:"job"`
	Class    string         `json:"class"` // high | normal
	Pid      int            `json:"pid"`
	Since    string         `json:"since"`
	WaitedS  int64          `json:"waited_s"`        // whole seconds from Since to the report's time
	Units    map[string]int `json:"units,omitempty"` // units asked for per group
	Blocking string         `json:"group_wait,omitempty"`
	Unknown  bool           `json:"unknown,omitempty"` // as Holder.Unknown: whether its owner is alive is not known
}

type Pool struct {
	Name        string   `json:"name"`
	Slots       int      `json:"slots"`
	Wait        string   `json:"wait,omitempty"`          // the pool's configured wait, in the duration form of spec 5.2
	OldestWaitS *int64   `json:"oldest_wait_s,omitempty"` // the longest waiter's waited_s; present only with waiters
	Holders     []Holder `json:"holders"`
	Waiters     []Waiter `json:"waiters"`
	LastStarved string   `json:"last_starved,omitempty"`
}

type Group struct {
	Name  string `json:"name"`
	Units int    `json:"units"`
	Held  []int  `json:"held"`
	// Unknown lists the units whose holder cannot be read (no /proc/locks, #24).
	Unknown []int `json:"unknown,omitempty"`
}

type Memory struct {
	AvailableMiB int64  `json:"available_mib"`
	Readable     bool   `json:"readable"`
	Source       string `json:"source"`
	FloorMiB     int64  `json:"floor_mib"`
}

// Report is `status --json` (docs/status.schema.json).
type Report struct {
	Version string   `json:"version"`
	Time    string   `json:"time"`
	Pools   []Pool   `json:"pools"`
	Groups  []Group  `json:"groups"`
	Memory  Memory   `json:"memory"`
	Legacy  []string `json:"legacy,omitempty"`
	Remote  any      `json:"remote,omitempty"`
}

// RemoteSection fills Report.Remote (set by the remote status code).
var RemoteSection func(e *config.Effective) any

// Redactor is the set the run path builds from the loaded config: every credential value.
func Redactor(e *config.Effective) *redact.Set {
	red := redact.New()
	for _, s := range e.Secrets {
		red.Add(s)
	}
	return red
}

func redactAll(red *redact.Set, in []string) []string {
	out := make([]string, len(in))
	for i, w := range in {
		out[i] = red.String(w)
	}
	return out
}

func iso(ns int64) string { return time.Unix(0, ns).UTC().Format(time.RFC3339) }

// ageS is the whole seconds from ns to now, measured between the same second-truncated times the
// report prints, so the age and the "since" beside it agree. Never negative.
func ageS(now time.Time, ns int64) int64 {
	d := now.Unix() - time.Unix(0, ns).Unix()
	if d < 0 {
		return 0
	}
	return d
}

// ageText is s whole seconds in the duration form of spec 5.2 ("1h22m").
func ageText(s int64) string { return config.FormatDuration(time.Duration(s) * time.Second) }

// Snapshot reads tickets, locks and memory without taking any lock: holders come from
// /proc/locks on Linux and fcntl(F_GETLK) on darwin (lock.Inspect; neither takes a lock), so a
// concurrent `--wait 0s` caller or a legacy `flock -n` never sees a lock made busy by status
// (read-only, best effort). On a Linux without a readable /proc/locks nothing is locked either:
// the slot or unit shows as unknown (lock.Inspect, #24).
func Snapshot(e *config.Effective, l state.Layout, only []string) Report {
	now := time.Now().UTC().Truncate(time.Second) // the report's own time: every age is measured to it
	r := Report{Version: "1", Time: now.Format(time.RFC3339), Pools: []Pool{}, Groups: []Group{}}
	names := only
	if len(names) == 0 {
		for n := range e.Pools {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	red := Redactor(e)
	last := lastStarved(l)
	for _, n := range names {
		n = config.CanonName(n)
		p := Pool{Name: n, Holders: []Holder{}, Waiters: []Waiter{}, LastStarved: red.String(last[n])}
		if cfg := e.Pools[n]; cfg != nil {
			p.Slots = cfg.Slots
			p.Wait = config.FormatDuration(cfg.Wait)
		}
		for _, t := range admit.ReadTickets(l, n) {
			st := lock.Inspect(l.Tickets(n) + "/" + t.Name)
			if st == lock.StateFree {
				continue // dead owner: sweep prunes it
			}
			unknown := st == lock.StateUnknown
			if t.T.State == "running" {
				p.Holders = append(p.Holders, Holder{Job: red.String(t.T.Job), Slot: t.T.Slot, Pid: t.T.Pid, Cwd: red.String(t.T.Cwd),
					Argv: redactAll(red, t.T.Argv), Provider: "local", Nested: t.T.Nested, Since: iso(t.T.HoldStarted), Units: t.T.Units, Unknown: unknown})
			} else {
				class := "normal"
				if t.T.Class == 0 {
					class = "high"
				}
				p.Waiters = append(p.Waiters, Waiter{Job: red.String(t.T.Job), Class: class, Pid: t.T.Pid, Since: iso(t.T.WaitStarted), WaitedS: ageS(now, t.T.WaitStarted), Units: t.T.Units,
					Blocking: red.String(strings.Join(t.T.GroupWait, ",")), Unknown: unknown})
			}
		}
		for i, q := range p.Waiters {
			if i == 0 || q.WaitedS > *p.OldestWaitS {
				v := q.WaitedS
				p.OldestWaitS = &v
			}
		}
		p.Holders = append(p.Holders, legacyHolders(e.Compat, n, p)...)
		r.Pools = append(r.Pools, p)
	}
	for _, g := range sortedGroups(e) {
		grp := Group{Name: g.Name, Units: g.Units, Held: []int{}}
		for j := 1; j <= g.Units; j++ {
			switch lock.Inspect(l.GroupUnitLock(g.Name, j)) {
			case lock.StateHeld:
				grp.Held = append(grp.Held, j)
			case lock.StateUnknown:
				grp.Unknown = append(grp.Unknown, j)
			}
		}
		r.Groups = append(r.Groups, grp)
	}
	root := ""
	if e.Test != nil {
		root = e.Test.ProcfsRoot
	}
	avail, ok, src := admit.ReadMemory(root)
	r.Memory = Memory{AvailableMiB: avail >> 20, Readable: ok, Source: src, FloorMiB: int64(e.Host.MemoryFloor) >> 20}
	r.Legacy = redactAll(red, admit.LegacyHolders(e.Compat))
	if RemoteSection != nil {
		r.Remote = RemoteSection(e)
	}
	return r
}

// legacyHolders attributes each held <legacy_dir>/locks/<pool>.<n>.lock (the lock admission
// takes with bitc slot n) to the pool's slot n, unless a bitc ticket already holds that slot.
// Observe only (lock.Inspect); the pid is the one the legacy caller wrote into the file, if any.
func legacyHolders(c *config.Compat, pool string, p Pool) []Holder {
	if c == nil {
		return nil
	}
	taken := map[int]bool{}
	for _, h := range p.Holders {
		taken[h.Slot] = true
	}
	var out []Holder
	for n := 1; n <= p.Slots; n++ {
		path := admit.LegacySlotLock(c, pool, n)
		st := lock.Inspect(path)
		if taken[n] || st == lock.StateFree {
			continue
		}
		pid := 0
		if st == lock.StateUnknown {
			// the file's pid may be stale and nothing says the lock is held: name no pid
			out = append(out, Holder{Job: "legacy", Slot: n, Argv: []string{}, Provider: "local", Legacy: true, Unknown: true})
			continue
		}
		if b, err := os.ReadFile(path); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		out = append(out, Holder{Job: "legacy", Slot: n, Pid: pid, Argv: []string{}, Provider: "local", Legacy: true})
	}
	return out
}

func sortedGroups(e *config.Effective) []config.Group {
	var out []config.Group
	for _, g := range e.Groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// lastStarved maps pool -> "<reason> at <ts>" from the newest starved history events.
func lastStarved(l state.Layout) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(l.History())
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		var e state.Event
		if json.Unmarshal([]byte(line), &e) == nil && e.Event == "starved" {
			reason := e.Reason
			if reason == "" {
				reason = "all slots busy"
			}
			out[e.Pool] = reason + " at " + e.TS
		}
	}
	return out
}

// Text prints the report for people.
func (r Report) Text(w io.Writer) {
	for _, p := range r.Pools {
		held, unk := 0, 0
		for _, h := range p.Holders {
			if h.Unknown {
				unk++
			} else {
				held++
			}
		}
		unkNote := ""
		if unk > 0 {
			unkNote = fmt.Sprintf(", %d unknown", unk)
		}
		ages := ""
		if p.OldestWaitS != nil {
			ages = fmt.Sprintf(" (wait %s, oldest %s)", p.Wait, ageText(*p.OldestWaitS))
		}
		fmt.Fprintf(w, "%s: %d/%d slots held%s, %d waiting%s\n", p.Name, held, p.Slots, unkNote, len(p.Waiters), ages)
		for _, h := range p.Holders {
			if h.Legacy && h.Unknown {
				fmt.Fprintf(w, "  slot %d  legacy holder unknown\n", h.Slot)
				continue
			}
			if h.Unknown {
				fmt.Fprintf(w, "  slot %d  unknown  %s  pid %d%s  since %s  %s\n", h.Slot, h.Job, h.Pid, unitsText(h.Units), h.Since, strings.Join(h.Argv, " "))
				continue
			}
			if h.Legacy {
				pid := ""
				if h.Pid > 0 {
					pid = fmt.Sprintf("  pid %d", h.Pid)
				}
				fmt.Fprintf(w, "  slot %d  legacy holder%s\n", h.Slot, pid)
				continue
			}
			nested := ""
			if h.Nested {
				nested = " nested"
			}
			fmt.Fprintf(w, "  slot %d  %s  pid %d%s%s  since %s  %s\n", h.Slot, h.Job, h.Pid, nested, unitsText(h.Units), h.Since, strings.Join(h.Argv, " "))
		}
		for _, q := range p.Waiters {
			unk := ""
			if q.Unknown {
				unk = "  unknown"
			}
			fmt.Fprintf(w, "  waiting  %s  %s  pid %d%s  since %s (%s)%s\n", q.Job, q.Class, q.Pid, unitsText(q.Units), q.Since, ageText(q.WaitedS), unk)
		}
		if p.LastStarved != "" {
			fmt.Fprintf(w, "  last STARVED: %s\n", p.LastStarved)
		}
	}
	for _, g := range r.Groups {
		unk := ""
		if len(g.Unknown) > 0 {
			unk = fmt.Sprintf(", %d unknown", len(g.Unknown))
		}
		fmt.Fprintf(w, "group %s: %d/%d units held%s\n", g.Name, len(g.Held), g.Units, unk)
	}
	if r.Memory.Readable {
		fmt.Fprintf(w, "memory: %d MiB available (%s), floor %d MiB\n", r.Memory.AvailableMiB, r.Memory.Source, r.Memory.FloorMiB)
	} else {
		fmt.Fprintf(w, "memory: unreadable (%s)\n", r.Memory.Source)
	}
	for _, x := range r.Legacy {
		fmt.Fprintf(w, "legacy: %s\n", x)
	}
}

// unitsText is "  units cpu=3 mem=1" (groups sorted), or "" when none.
func unitsText(u map[string]int) string {
	if len(u) == 0 {
		return ""
	}
	names := make([]string, 0, len(u))
	for g := range u {
		names = append(names, g)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, g := range names {
		parts[i] = g + "=" + strconv.Itoa(u[g])
	}
	return "  units " + strings.Join(parts, " ")
}
