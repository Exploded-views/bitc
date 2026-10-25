package liveprobe

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func ids(g PlanGroup) []int {
	var out []int
	for _, p := range g.Probes {
		out = append(out, p.ID)
	}
	return out
}

func TestDefaultOrderPutsPhaseOneFirst(t *testing.T) {
	order := DefaultOrder(AllProbes())
	if len(order) != 25 {
		t.Fatalf("%d probes in the default order, want all 25: %v", len(order), order)
	}
	seenWarm := false
	byID := map[int]Probe{}
	for _, p := range AllProbes() {
		byID[p.ID] = p
	}
	for _, id := range order {
		if byID[id].Warm {
			seenWarm = true
		} else if seenWarm {
			t.Fatalf("phase-1 item %d comes after a warm-reuse item: %v", id, order)
		}
	}
	for _, id := range []int{2, 3, 5, 6, 8, 10, 11, 12, 13, 15, 16, 22, 23, 24, 25} {
		if byID[id].Warm {
			t.Errorf("item %d is a phase-1 item", id)
		}
	}
}

func TestBuildPlanGroupsTheReadOnlyProbesInOneSprite(t *testing.T) {
	pl, err := BuildPlan(AllProbes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var ro PlanGroup
	n := 0
	for _, g := range pl.Groups {
		if g.Name == gRO {
			ro = g
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d read-only groups, want 1", n)
	}
	want := map[int]bool{24: true, 15: true, 21: true, 23: true, 20: true, 4: true, 14: true}
	for _, id := range ids(ro) {
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("the read-only group lacks %v: %v", want, ids(ro))
	}
	if got := ids(ro); got[0] != 24 || got[len(got)-1] != 4 {
		t.Fatalf("24 must run first and the privileges policy (4) last: %v", got)
	}
}

func TestBuildPlanHonoursSelectionAndRefusesUnknown(t *testing.T) {
	pl, err := BuildPlan(AllProbes(), []int{25, 24, 13, 15})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, g := range pl.Groups {
		order = append(order, g.Name)
	}
	if strings.Join(order, ",") != "ret,ro," {
		t.Fatalf("groups %v", order)
	}
	if _, err := BuildPlan(AllProbes(), []int{99}); err == nil {
		t.Fatal("an unknown item was accepted")
	}
}

func TestDefaultPlanWorstCaseIsUnderTheCap(t *testing.T) {
	c := Config{MaxUSD: DefaultMaxUSD, PricePerHour: DefaultPricePerHour, DNSCanary: "canary.example.org"}
	pl, _ := BuildPlan(AllProbes(), nil)
	worst := USD(pl.Worst(c), c.PricePerHour)
	if worst <= 0.05 || worst >= DefaultMaxUSD {
		t.Fatalf("the default plan's declared worst case is $%.4f, want between $0.05 and $%.2f", worst, DefaultMaxUSD)
	}
	if USD(pl.Worst(c), c.PricePerHour) <= USD(pl.Worst(Config{PricePerHour: c.PricePerHour}), c.PricePerHour) {
		t.Fatal("probe 22 adds a Sprite only when a canary domain is given")
	}
}

func TestPlanPrintShowsCapsAndEveryProbe(t *testing.T) {
	c := Config{Org: "sandbox-org", RunID: "r1", MaxUSD: 0.90, PricePerHour: 0.7571, MaxMinutes: 75}
	pl, _ := BuildPlan(AllProbes(), nil)
	var b bytes.Buffer
	pl.Print(&b, c)
	out := b.String()
	for _, want := range []string{"no request is made", "bitcproof-s0-r1-<n>", "$0.90", "75 minutes", "item 24 ", "item 25 ", "item 22 ", "skipped: assume-worst: no --dns-canary-domain", "total declared worst case"} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan lacks %q:\n%s", want, out)
		}
	}
}

func TestEveryProbeHasARunAndAWorstCase(t *testing.T) {
	seen := map[int]bool{}
	for _, p := range AllProbes() {
		if p.Run == nil || p.Worst <= 0 || p.Title == "" || seen[p.ID] || p.ID < 1 || p.ID > 25 {
			t.Errorf("probe %d is malformed", p.ID)
		}
		seen[p.ID] = true
		if p.Group != gNone {
			if _, has := Groups[p.Group]; !has {
				t.Errorf("probe %d names the unknown group %q", p.ID, p.Group)
			}
		}
	}
	if len(seen) != 25 {
		t.Fatalf("%d probes, want 25", len(seen))
	}
	_ = context.Background
}
