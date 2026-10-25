// Package status implements status, sweep and doctor (spec 3.3).
package status

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/admit"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/redact"
	"github.com/Exploded-views/bitc/internal/state"
)

type Holder struct {
	Job      string   `json:"job"`
	Slot     int      `json:"slot"`
	Pid      int      `json:"pid"`
	Cwd      string   `json:"cwd"`
	Argv     []string `json:"argv"`
	Provider string   `json:"provider"`
	Nested   bool     `json:"nested"`
	Since    string   `json:"since"`
}

type Waiter struct {
	Job      string `json:"job"`
	Class    string `json:"class"` // high | normal
	Pid      int    `json:"pid"`
	Since    string `json:"since"`
	Blocking string `json:"group_wait,omitempty"`
}

type Pool struct {
	Name        string   `json:"name"`
	Slots       int      `json:"slots"`
	Holders     []Holder `json:"holders"`
	Waiters     []Waiter `json:"waiters"`
	LastStarved string   `json:"last_starved,omitempty"`
}

type Group struct {
	Name  string `json:"name"`
	Units int    `json:"units"`
	Held  []int  `json:"held"`
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

// Snapshot reads tickets, locks and memory without taking any lock: holders come from
// /proc/locks (lock.HeldRO), so a concurrent `--wait 0s` caller or a legacy `flock -n` never
// sees a lock made busy by status (read-only, best effort).
func Snapshot(e *config.Effective, l state.Layout, only []string) Report {
	r := Report{Version: "1", Time: time.Now().UTC().Format(time.RFC3339), Pools: []Pool{}, Groups: []Group{}}
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
		}
		for _, t := range admit.ReadTickets(l, n) {
			if !lock.HeldRO(l.Tickets(n) + "/" + t.Name) {
				continue // dead owner: sweep prunes it
			}
			if t.T.State == "running" {
				p.Holders = append(p.Holders, Holder{Job: red.String(t.T.Job), Slot: t.T.Slot, Pid: t.T.Pid, Cwd: red.String(t.T.Cwd),
					Argv: redactAll(red, t.T.Argv), Provider: "local", Nested: t.T.Nested, Since: iso(t.T.HoldStarted)})
			} else {
				class := "normal"
				if t.T.Class == 0 {
					class = "high"
				}
				p.Waiters = append(p.Waiters, Waiter{Job: red.String(t.T.Job), Class: class, Pid: t.T.Pid, Since: iso(t.T.WaitStarted),
					Blocking: red.String(strings.Join(t.T.GroupWait, ","))})
			}
		}
		r.Pools = append(r.Pools, p)
	}
	for _, g := range sortedGroups(e) {
		grp := Group{Name: g.Name, Units: g.Units, Held: []int{}}
		for j := 1; j <= g.Units; j++ {
			if lock.HeldRO(l.GroupUnitLock(g.Name, j)) {
				grp.Held = append(grp.Held, j)
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
		fmt.Fprintf(w, "%s: %d/%d slots held, %d waiting\n", p.Name, len(p.Holders), p.Slots, len(p.Waiters))
		for _, h := range p.Holders {
			nested := ""
			if h.Nested {
				nested = " nested"
			}
			fmt.Fprintf(w, "  slot %d  %s  pid %d%s  since %s  %s\n", h.Slot, h.Job, h.Pid, nested, h.Since, strings.Join(h.Argv, " "))
		}
		for _, q := range p.Waiters {
			fmt.Fprintf(w, "  waiting  %s  %s  pid %d  since %s\n", q.Job, q.Class, q.Pid, q.Since)
		}
		if p.LastStarved != "" {
			fmt.Fprintf(w, "  last STARVED: %s\n", p.LastStarved)
		}
	}
	for _, g := range r.Groups {
		fmt.Fprintf(w, "group %s: %d/%d units held\n", g.Name, len(g.Held), g.Units)
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
