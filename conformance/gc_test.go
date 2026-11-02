package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// gcHost is a cpu group of four units and one pool whose own count is one unit; every run in
// these tests asks for its count with --units. The pool has slots to spare, so only the group
// decides who waits.
const gcHost = "groups = { cpu = { units = 4 }, mem = { units = 2 } }\n" +
	"[pools.work]\nslots = 8\ngroups = { cpu = 1 }\n" +
	"[pools.capped]\nslots = 8\ngroups = { cpu = 1 }\ngroups_max = { cpu = 2 }\n" +
	"[pools.both]\nslots = 8\ngroups = { cpu = 1, mem = 1 }\n" +
	"[pools.plain]\nslots = 8\n"

// waitsOnGroup reports whether pool has a waiting ticket that has recorded group g as its blocker.
func waitsOnGroup(h *harness.H, pool, g string) bool {
	for _, name := range tickets(h, pool) {
		b, err := os.ReadFile(filepath.Join(h.State, "pools", pool, "tickets", name))
		if err != nil {
			continue
		}
		var t struct {
			State     string   `json:"state"`
			GroupWait []string `json:"group_wait"`
		}
		if json.Unmarshal(b, &t) == nil && t.State == "waiting" {
			for _, w := range t.GroupWait {
				if w == g {
					return true
				}
			}
		}
	}
	return false
}

// gcStatus is the part of `status --json` these tests read.
type gcStatus struct {
	Pools []struct {
		Name    string
		Holders []struct {
			Job   string
			Units map[string]int
		}
		Waiters []struct {
			Job   string
			Units map[string]int
		}
	}
}

func readStatus(t *testing.T, h *harness.H) gcStatus {
	t.Helper()
	r := h.Run("--json", "status")
	if r.Code != 0 {
		t.Fatalf("status: %+v", r)
	}
	var s gcStatus
	if err := json.Unmarshal([]byte(r.Stdout), &s); err != nil {
		t.Fatalf("status --json: %v\n%s", err, r.Stdout)
	}
	return s
}

func readUnits(t *testing.T, path string) map[string]int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Outcome string
		Units   map[string]int
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, b)
	}
	return res.Units
}

// A gate that took three of four units leaves one for a light run; a heavy run that needs three
// waits until the gate is done. The light run arrives before the heavy one queues: once a heavy
// run is waiting, a later light run waits behind it (GC6).
func TestGC1_GateLeavesOneUnitForALightRun(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(gcHost)
	gate := h.Hold(nil, "--units", "cpu=3", "work")
	if r := h.Run("run", "work", "--", "true"); r.Code != 0 { // one unit, the pool's own count
		t.Fatalf("a light run was held back by a gate that left a unit free: %+v", r)
	}
	heavy := h.Start("run", "--units", "cpu=3", "work", "--", "true")
	h.Until(func() bool { return waitsOnGroup(h, "work", "cpu") }, 20*time.Second)
	if heavy.Exited() {
		t.Fatal("a heavy run that needs three units ran beside a gate that holds three of four")
	}
	if r := gate.Release(); r.Code != 0 {
		t.Fatalf("gate: %+v", r)
	}
	if r := heavy.Wait(); r.Code != 0 {
		t.Fatalf("the heavy run did not run once the gate ended: %+v", r)
	}
}

// FIFO by ticket time: a heavy run that arrives after a waiting gate queues behind it, even when
// the units that free up would fit the heavy run first.
func TestGC2_ALaterHeavyQueuesBehindAWaitingGate(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(gcHost)
	a := h.Hold(nil, "--units", "cpu=2", "work")
	b := h.Hold(nil, "--units", "cpu=2", "work")
	gate := h.Start("run", "--units", "cpu=3", "work", "--", "true")
	h.Until(func() bool { return waitsOnGroup(h, "work", "cpu") }, 20*time.Second)
	if r := a.Release(); r.Code != 0 { // two units are free now: enough for a heavy run, not for the gate
		t.Fatalf("a: %+v", r)
	}
	if gate.Exited() {
		t.Fatal("the gate ran with two units free")
	}
	// the later heavy run would fit, but the gate is older: it must starve rather than overtake
	r := h.Run("run", "--units", "cpu=2", "--wait", "0s", "work", "--", "true")
	if r.Code != 75 || !strings.Contains(r.Stderr, "group cpu busy") {
		t.Fatalf("a later heavy run overtook the waiting gate: %+v", r)
	}
	if gate.Exited() {
		t.Fatal("the gate ran before the second heavy run ended")
	}
	if r := b.Release(); r.Code != 0 {
		t.Fatalf("b: %+v", r)
	}
	if r := gate.Wait(); r.Code != 0 {
		t.Fatalf("the gate did not run once all four units were free: %+v", r)
	}
}

// Usage errors (64) name the group and the limit; a groups_max that is not valid is a config
// error (78) naming the key.
func TestGC3_UnitsLimitsAndGroupsMax(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(gcHost)
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"--units", "cpu=5", "work"}, []string{"cpu", "4"}},
		{[]string{"--units", "cpu=0", "work"}, []string{"cpu"}},
		{[]string{"--units", "cpu=two", "work"}, []string{"cpu=two"}},
		{[]string{"--units", "cpu", "work"}, []string{"--units"}},
		{[]string{"--units", "other=1", "work"}, []string{"other"}},                 // not a group
		{[]string{"--units", "mem=1", "work"}, []string{"mem", "work"}},             // a group, but not one work lists
		{[]string{"--units", "cpu=3", "capped"}, []string{"cpu", "2"}},              // above groups_max
		{[]string{"--units", "cpu=1", "--units", "cpu=2", "work"}, []string{"cpu"}}, // twice
		{[]string{"--units", "cpu=1", "plain"}, []string{"cpu", "plain"}},
	} {
		args := append(append([]string{"run"}, c.args...), "--", "echo", "ran")
		r := h.Run(args...)
		if r.Code != 64 || r.Stdout != "" || strings.Contains(r.Stdout, "ran") {
			t.Errorf("%q: got %+v, want 64 and nothing run", c.args, r)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(r.Stderr, w) {
				t.Errorf("%q: stderr %q lacks %q", c.args, r.Stderr, w)
			}
		}
	}
	for _, ok := range [][]string{
		{"--units", "cpu=4", "work"}, {"--units", "cpu=2", "capped"}, {"--units", "cpu=1", "capped"},
		{"--units", "cpu=2", "--units", "mem=2", "both"},
	} {
		if r := h.Run(append(append([]string{"run"}, ok...), "--", "true")...); r.Code != 0 {
			t.Errorf("%q: %+v", ok, r)
		}
	}
	for body, key := range map[string]string{
		"[pools.work]\ngroups = { cpu = 1 }\ngroups_max = { mem = 1 }\n":   "pools.work.groups_max", // work does not list mem
		"[pools.work]\ngroups = { cpu = 1 }\ngroups_max = { other = 1 }\n": "pools.work.groups_max", // not a group
		"[pools.work]\ngroups = { cpu = 1 }\ngroups_max = { cpu = 5 }\n":   "pools.work.groups_max.cpu",
		"[pools.work]\ngroups = { cpu = 1 }\ngroups_max = { cpu = 0 }\n":   "pools.work.groups_max.cpu",
		"[pools.work]\ngroups_max = { cpu = 2 }\n":                         "pools.work.groups_max",
	} {
		h.HostConfig("groups = { cpu = { units = 4 }, mem = { units = 2 } }\n" + body)
		if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, key) {
			t.Errorf("%q: got %+v, want 78 naming %s", body, r, key)
		}
		if r := h.Run("run", "work", "--", "echo", "ran"); r.Code != 78 || r.Stdout != "" {
			t.Errorf("%q: run: %+v", body, r)
		}
	}
}

// Status and result.json (and the history lines) name the units a run holds or waits for.
func TestGC4_StatusAndResultNameTheUnits(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(gcHost)
	res1, res2, res3 := filepath.Join(h.Root, "r1.json"), filepath.Join(h.Root, "r2.json"), filepath.Join(h.Root, "r3.json")
	gate := h.Hold(nil, "--units", "cpu=3", "--name", "gate", "--result", res1, "work")
	w := h.Start("run", "--units", "cpu=2", "--name", "next", "--result", res2, "work", "--", "true")
	h.Until(func() bool { return waitsOnGroup(h, "work", "cpu") }, 20*time.Second)
	s := readStatus(t, h)
	var held, waiting map[string]int
	for _, p := range s.Pools {
		for _, x := range p.Holders {
			if x.Job == "gate" {
				held = x.Units
			}
		}
		for _, x := range p.Waiters {
			if x.Job == "next" {
				waiting = x.Units
			}
		}
	}
	if !reflect.DeepEqual(held, map[string]int{"cpu": 3}) || !reflect.DeepEqual(waiting, map[string]int{"cpu": 2}) {
		t.Errorf("status --json: holder units %v, waiter units %v; want cpu 3 and cpu 2", held, waiting)
	}
	if r := h.Run("status"); !strings.Contains(r.Stdout, "units cpu=3") || !strings.Contains(r.Stdout, "units cpu=2") {
		t.Errorf("status text does not name the units:\n%s", r.Stdout)
	}
	if r := gate.Release(); r.Code != 0 {
		t.Fatalf("gate: %+v", r)
	}
	if r := w.Wait(); r.Code != 0 {
		t.Fatalf("next: %+v", r)
	}
	if got := readUnits(t, res1); !reflect.DeepEqual(got, map[string]int{"cpu": 3}) {
		t.Errorf("result.json of the gate: units %v, want cpu 3", got)
	}
	if got := readUnits(t, res2); !reflect.DeepEqual(got, map[string]int{"cpu": 2}) {
		t.Errorf("result.json of the next run: units %v, want cpu 2", got)
	}
	// without the flag the pool's own count is what is held
	if r := h.Run("run", "--result", res3, "work", "--", "true"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	if got := readUnits(t, res3); !reflect.DeepEqual(got, map[string]int{"cpu": 1}) {
		t.Errorf("result.json without --units: units %v, want cpu 1", got)
	}
	// the history start and end lines carry them too
	b, err := os.ReadFile(filepath.Join(h.State, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e struct {
			Event, Job string
			Units      map[string]int
		}
		if json.Unmarshal([]byte(l), &e) == nil && e.Job == "gate" && (e.Event == "start" || e.Event == "end") {
			seen++
			if !reflect.DeepEqual(e.Units, map[string]int{"cpu": 3}) {
				t.Errorf("history %s line of the gate: units %v, want cpu 3\n%s", e.Event, e.Units, l)
			}
		}
	}
	if seen != 2 {
		t.Errorf("saw %d gate history lines, want start and end", seen)
	}
}

// A heavy run that is already running is never preempted by a gate that arrives later: it
// finishes with its own result, then the gate runs.
func TestGC5_ARunningHeavyIsNeverPreempted(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(gcHost)
	heavy := h.Hold(nil, "--units", "cpu=2", "work")
	gate := h.Start("run", "--units", "cpu=4", "work", "--", "true")
	h.Until(func() bool { return waitsOnGroup(h, "work", "cpu") }, 20*time.Second)
	if heavy.Proc.Exited() {
		t.Fatal("the running heavy run was stopped when a gate arrived")
	}
	if gate.Exited() {
		t.Fatal("the gate ran beside a heavy run that holds two of four units")
	}
	if r := heavy.Release(); r.Code != 0 { // its own code: it was never signalled
		t.Fatalf("the heavy run did not finish on its own: %+v", r)
	}
	if r := gate.Wait(); r.Code != 0 {
		t.Fatalf("the gate did not run after the heavy run: %+v", r)
	}
	var order []string
	for _, l := range lines(t, filepath.Join(h.State, "history.jsonl")) {
		var e struct{ Event string }
		if json.Unmarshal([]byte(l), &e) == nil {
			order = append(order, e.Event)
		}
	}
	if want := []string{"start", "end", "start", "end"}; !reflect.DeepEqual(order, want) {
		t.Errorf("history events %v, want %v (the gate starts only after the heavy run ended)", order, want)
	}
}

// With a gate running and a heavy run already waiting, a later light run waits behind the heavy
// one although a unit is free: group units are strictly first in first out (spec 6.2).
func TestGC6_ALightRunQueuesBehindAWaitingHeavy(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(gcHost)
	gate := h.Hold(nil, "--units", "cpu=3", "work")
	heavy := h.Start("run", "--units", "cpu=3", "work", "--", "true")
	h.Until(func() bool { return waitsOnGroup(h, "work", "cpu") }, 20*time.Second)
	r := h.Run("run", "--wait", "0s", "work", "--", "echo", "ran")
	if r.Code != 75 || !strings.Contains(r.Stderr, "group cpu busy") || r.Stdout != "" {
		t.Fatalf("a light run overtook the waiting heavy run: %+v", r)
	}
	if r := gate.Release(); r.Code != 0 {
		t.Fatalf("gate: %+v", r)
	}
	if r := heavy.Wait(); r.Code != 0 {
		t.Fatalf("heavy: %+v", r)
	}
}

// A bad --units is one stderr line (no usage text), 64; it names the form, or the limit 1..N.
func TestGC7_BadUnitsIsOneLine(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(gcHost)
	for v, want := range map[string]string{"cpu=0": "1..4", "cpu=x": "<group>=<k>", "cpu": "<group>=<k>"} {
		r := h.Run("run", "--units", v, "work", "--", "true")
		if r.Code != 64 || r.Stdout != "" || strings.Count(strings.TrimSuffix(r.Stderr, "\n"), "\n") != 0 ||
			!strings.HasSuffix(r.Stderr, "\n") || !strings.Contains(r.Stderr, want) || strings.Contains(r.Stderr, "usage:") {
			t.Errorf("--units %s: got %+v, want one line, 64, naming %s", v, r, want)
		}
	}
}

// --units survives --detach whatever the flag order: the supervisor's job holds the units.
func TestGC8_DetachedRunCarriesUnits(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(gcHost)
	r := h.Run("run", "--units", "cpu=3", "--detach", "--name", "dj", "work", "--", "true")
	m := startedRE.FindStringSubmatch(r.Stdout)
	if r.Code != 0 || m == nil {
		t.Fatalf("%+v", r)
	}
	if r := h.Run("wait", "dj"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	files, _ := filepath.Glob(filepath.Join(h.State, "jobs", "*", "dj", "result.json"))
	if len(files) != 1 {
		t.Fatalf("result.json files: %v", files)
	}
	if got := readUnits(t, files[0]); !reflect.DeepEqual(got, map[string]int{"cpu": 3}) {
		t.Errorf("detached result.json: units %v, want cpu 3", got)
	}
}

// A run that starves says what it asked for and what expired, in the STARVED line, in
// result.json's reason and in the history line: units wanted of the group's size, and the wait
// as configured; a slot starvation says "slots busy" and the wait.
func TestGC9_StarvedLineNamesUnitsAndWait(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(gcHost + "[pools.solo]\nslots = 1\n")
	gate := h.Hold(nil, "--units", "cpu=3", "work")
	res := filepath.Join(h.Root, "starved.json")
	const group = "group cpu busy: wanted 2 of 4 units; waited 1s"
	r := h.Run("run", "--units", "cpu=2", "--wait", "1s", "--name", "sg", "--result", res, "work", "--", "true")
	if r.Code != 75 || r.Stderr != "STARVED work ("+group+")\n" {
		t.Fatalf("%+v", r)
	}
	b, err := os.ReadFile(res)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Outcome, Reason string }
	if err := json.Unmarshal(b, &out); err != nil || out.Outcome != "starved" || out.Reason != group {
		t.Fatalf("result.json: %s (%v)", b, err)
	}
	found := false
	for _, l := range lines(t, filepath.Join(h.State, "history.jsonl")) {
		var e struct{ Event, Job, Reason string }
		if json.Unmarshal([]byte(l), &e) == nil && e.Event == "starved" && e.Job == "sg" {
			found = e.Reason == group
		}
	}
	if !found {
		t.Errorf("no history starved line with reason %q", group)
	}
	gate.Release()
	solo := h.Hold(nil, "solo")
	if r := h.Run("run", "--wait", "1s", "solo", "--", "true"); r.Code != 75 || r.Stderr != "STARVED solo (slots busy; waited 1s)\n" {
		t.Fatalf("%+v", r)
	}
	solo.Release()
}

// A run that finds a one-slot pool held and another run already waiting ahead of it starves on
// the queue: the line, and result.json's reason, say "queue" and the wait.
func TestGC10_StarvedInTheQueueNamesTheWait(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.solo]\nslots = 1\n")
	hold := h.Hold(nil, "solo")
	ahead := h.Start("run", "solo", "--", "true")
	h.Until(func() bool { return len(tickets(h, "solo")) == 2 }, 20*time.Second)
	res := filepath.Join(h.Root, "queue.json")
	const reason = "queue; waited 0s"
	r := h.Run("run", "--wait", "0s", "--result", res, "solo", "--", "true")
	if r.Code != 75 || r.Stderr != "STARVED solo ("+reason+")\n" {
		t.Fatalf("%+v", r)
	}
	var out struct{ Outcome, Reason string }
	if err := json.Unmarshal(must(os.ReadFile(res)), &out); err != nil || out.Outcome != "starved" || out.Reason != reason {
		t.Fatalf("result.json: %+v (%v)", out, err)
	}
	hold.Release()
	if r := ahead.Wait(); r.Code != 0 {
		t.Fatalf("the run ahead: %+v", r)
	}
}
