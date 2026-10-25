package liveprobe

import (
	"fmt"
	"io"
	"sort"
	"time"
)

// PlanGroup is the probes that share one Sprite (or, for Name "", the ones that need none).
type PlanGroup struct {
	Name   string
	Probes []Probe
}

// Worst is the declared worst case of the group: its probes plus, for a group with a Sprite, the
// create, policies and delete.
func (g PlanGroup) Worst(c Config) time.Duration {
	var d time.Duration
	for _, p := range g.Probes {
		if p.Skip == nil || p.Skip(c) == "" {
			d += p.Worst
		}
	}
	if g.Name != gNone && d > 0 {
		d += GroupOverhead
	}
	return d
}

// Plan is the groups in the order they run.
type Plan struct{ Groups []PlanGroup }

// DefaultOrder is the plan's probe order when --probes is absent: the phase-1 items, then the
// warm-reuse items, each by item number.
func DefaultOrder(all []Probe) []int {
	var first, second []int
	for _, p := range all {
		if p.Warm {
			second = append(second, p.ID)
		} else {
			first = append(first, p.ID)
		}
	}
	sort.Ints(first)
	sort.Ints(second)
	return append(first, second...)
}

// BuildPlan picks the selected probes (nil = all, default order) and groups them: a group runs at
// the position of its first probe, its probes in Order. An unknown item number is an error.
func BuildPlan(all []Probe, sel []int) (Plan, error) {
	byID := map[int]Probe{}
	for _, p := range all {
		byID[p.ID] = p
	}
	if sel == nil {
		sel = DefaultOrder(all)
	}
	var pl Plan
	idx := map[string]int{}
	for _, id := range sel {
		p, has := byID[id]
		if !has {
			return Plan{}, fmt.Errorf("no probe for item %d", id)
		}
		if p.Group == gNone {
			pl.Groups = append(pl.Groups, PlanGroup{Probes: []Probe{p}})
			continue
		}
		i, seen := idx[p.Group]
		if !seen {
			idx[p.Group] = len(pl.Groups)
			pl.Groups = append(pl.Groups, PlanGroup{Name: p.Group})
			i = len(pl.Groups) - 1
		}
		pl.Groups[i].Probes = append(pl.Groups[i].Probes, p)
	}
	for i := range pl.Groups {
		g := &pl.Groups[i]
		sort.SliceStable(g.Probes, func(a, b int) bool { return g.Probes[a].Order < g.Probes[b].Order })
	}
	return pl, nil
}

// Worst is the plan's declared worst case.
func (pl Plan) Worst(c Config) time.Duration {
	var d time.Duration
	for _, g := range pl.Groups {
		d += g.Worst(c)
	}
	return d
}

// USD prices a duration of Sprite time.
func USD(d time.Duration, pricePerHour float64) float64 { return d.Seconds() * pricePerHour / 3600 }

// Print writes the plan, the caps and the worst case of each group and probe. It is all the
// --dry-run does.
func (pl Plan) Print(w io.Writer, c Config) {
	fmt.Fprintf(w, "bitc-sprites-probe plan (dry run: no request is made)\n")
	fmt.Fprintf(w, "org %s, run %s, Sprites named %s%s-<n>, at most %d live at a time\n", c.Org, c.RunID, NamePrefix, c.RunID, MaxLive)
	fmt.Fprintf(w, "caps: $%.2f (%.4f USD/h), %d minutes\n\n", c.MaxUSD, c.PricePerHour, c.MaxMinutes)
	n := 0
	for _, g := range pl.Groups {
		if g.Name == gNone || g.Worst(c) == 0 {
			for _, p := range g.Probes {
				extra := ""
				if p.Skip != nil {
					if why := p.Skip(c); why != "" {
						extra = " [skipped: " + why + "]"
					}
				}
				fmt.Fprintf(w, "no Sprite: item %d %s (worst %s)%s\n", p.ID, p.Title, p.Worst, extra)
			}
			continue
		}
		n++
		fmt.Fprintf(w, "Sprite %d (group %s), worst %s = $%.4f:\n", n, g.Name, g.Worst(c), USD(g.Worst(c), c.PricePerHour))
		for _, p := range g.Probes {
			extra := ""
			if p.Skip != nil {
				if why := p.Skip(c); why != "" {
					extra = " [skipped: " + why + "]"
				}
			}
			fmt.Fprintf(w, "  item %d %s (worst %s)%s\n", p.ID, p.Title, p.Worst, extra)
		}
		fmt.Fprintf(w, "  + create, policies and delete %s\n", GroupOverhead)
	}
	fmt.Fprintf(w, "\ntotal declared worst case: %s = $%.4f of the $%.2f cap\n", pl.Worst(c), USD(pl.Worst(c), c.PricePerHour), c.MaxUSD)
}
