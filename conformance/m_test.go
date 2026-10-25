package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/proc"
)

const groupHost = "groups = { bigmem = { units = 1 } }\n" +
	"[pools.heavy]\ngroups = { bigmem = 1 }\nwait = \"1s\"\n" +
	"[pools.coverage]\ngroups = { bigmem = 1 }\nwait = \"1s\"\n" +
	"[pools.e2e]\ngroups = { bigmem = 1 }\nwait = \"1s\"\n" +
	"[pools.big_build]\ngroups = { bigmem = 1 }\nwait = \"1s\"\n" +
	"[pools.lint]\nwait = \"1s\"\n"

func TestM1_GroupKeepsConsumersApart(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(groupHost)
	hold := h.Hold(nil, "heavy")
	if r := h.Run("run", "coverage", "--", "echo", "ran"); r.Code != 75 || r.Stdout != "" {
		t.Fatalf("coverage ran next to heavy: %+v", r)
	}
	if r := h.Run("run", "lint", "--", "true"); r.Code != 0 {
		t.Fatalf("a non-consumer was held back: %+v", r)
	}
	hold.Release()
	if r := h.Run("run", "coverage", "--", "true"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	h.HostConfig(strings.Replace(groupHost, "units = 1", "units = 2", 1))
	a := h.Hold(nil, "heavy")
	if r := h.Run("run", "coverage", "--", "true"); r.Code != 0 {
		t.Fatalf("units = 2 did not let two run: %+v", r)
	}
	a.Release()
}

func TestM2_NoHoldAndWait(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(groupHost)
	hold := h.Hold(nil, "heavy")
	w := h.Start("run", "coverage", "--", "true")
	time.Sleep(700 * time.Millisecond)
	if lockHeld(filepath.Join(h.State, "pools", "coverage", "slot.1.lock")) {
		t.Fatal("a waiter holds its slot while the group is busy")
	}
	if r := w.Wait(); r.Code != 75 || r.Stderr != "STARVED coverage (group bigmem busy)\n" {
		t.Fatalf("%+v", r)
	}
	hold.Release()
}

func TestM3_GroupMembershipUsesCanonicalNames(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(groupHost)
	hold := h.Hold(nil, "heavy")
	for _, spelling := range []string{"big-build", "big_build"} {
		if r := h.Run("run", spelling, "--", "true"); r.Code != 75 || !strings.Contains(r.Stderr, "group bigmem busy") {
			t.Errorf("%s: %+v", spelling, r)
		}
	}
	hold.Release()
}

func TestM6_NestedCallSkipsParentsGroup(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(groupHost)
	for _, wrap := range []string{
		`"$1" run coverage -- echo nested-ok`,
		`sh -c '"$1" run coverage -- echo nested-ok' _ "$1"`,
		`sh -c 'sh -c "\"\$1\" run coverage -- echo nested-ok" _ "$1"' _ "$1"`,
		`( "$1" run coverage -- echo nested-ok ) & wait`,
		// depth 2: the grandchild inherits what the grandparent holds, whatever the middle level is
		`"$1" run coverage -- "$1" run e2e -- echo nested-ok`,
		`"$1" run lint -- "$1" run e2e -- echo nested-ok`,
		`"$1" run heavy -- "$1" run e2e -- echo nested-ok`,
		`"$1" run heavy -- "$1" run coverage -- "$1" run e2e -- echo nested-ok`,
	} {
		r := h.Run("run", "heavy", "--", "sh", "-c", wrap, "_", harness.Bin)
		if r.Code != 0 || !strings.Contains(r.Stdout, "nested-ok") {
			t.Errorf("%s: %+v", wrap, r)
		}
	}
}

func TestM7_ForgedClaimsNotBelieved(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(groupHost)
	hold := h.Hold(nil, "heavy")
	real := hold.Env["BITC_HOLD"]
	parts := strings.Split(real, ":")
	for _, claim := range []string{
		"x", "heavy:0-0000000000000000001-00000000.json:" + parts[2],
		"heavy:" + parts[1] + ":wrongtoken",
		"heavy:../heavy/tickets/" + parts[1] + ":" + parts[2],
		real, // sibling: the right claim from a process that is not a descendant of the holder
	} {
		r := h.RunWith([]string{"BITC_HOLD=" + claim}, "run", "coverage", "--", "echo", "ran")
		if r.Code != 75 || r.Stdout != "" {
			t.Errorf("claim %q was believed: %+v", claim, r)
		}
	}
	hold.Release()
	// stale: the claim of a finished hold
	r := h.RunWith([]string{"BITC_HOLD=" + real}, "run", "coverage", "--", "true")
	if r.Code != 0 {
		t.Fatalf("%+v", r)
	}
}

// With a two-unit group, a hold that took unit 2 (unit 1 is held by another consumer) must let
// its own nested call of a third consumer run: the claim names the ticket, not a unit.
func TestM6_NestedCallRunsWhenParentTookUnitTwo(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(groupHost)
	hold := h.Hold(nil, "heavy") // takes unit 1
	h.HostConfig(strings.Replace(groupHost, "units = 1", "units = 2", 1))
	r := h.Run("run", "coverage", "--", "sh", "-c", `"$1" run big-build -- echo inner`, "_", harness.Bin)
	if r.Code != 0 || !strings.Contains(r.Stdout, "inner") || strings.Contains(r.Stderr, "STARVED") {
		t.Errorf("the nested call of the unit-2 hold did not run: %+v", r)
	}
	// and a plain third consumer, outside any hold, is still kept out while both units are taken
	h.HostConfig(groupHost) // back to one unit, still held by heavy
	if r := h.Run("run", "big-build", "--", "true"); r.Code != 75 {
		t.Errorf("a third consumer ran next to the hold: %+v", r)
	}
	hold.Release()
}

// ticketTmpl is a snapshot of the real hold's ticket (taken while it exists).
type ticketTmpl struct {
	m     map[string]any
	token string
}

func snapshotTicket(t *testing.T, h *harness.H, hold *harness.Holder) ticketTmpl {
	t.Helper()
	parts := strings.Split(hold.Env["BITC_HOLD"], ":")
	b, err := os.ReadFile(filepath.Join(h.State, "pools", "heavy", "tickets", parts[1]))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return ticketTmpl{m: m, token: parts[2]}
}

// ticketCopy writes a copy of the snapshot under a new name in the heavy tickets dir with fields
// replaced, and returns the BITC_HOLD claim naming it. When held is true the test process
// flocks the file for the life of the test.
func ticketCopy(t *testing.T, h *harness.H, tm ticketTmpl, name string, patch map[string]any, held bool) string {
	t.Helper()
	m := map[string]any{}
	for k, v := range tm.m {
		m[k] = v
	}
	m["pgid"], m["leader"] = 0, 0 // a copy must never let prune kill the real hold's command
	for k, v := range patch {
		m[k] = v
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.State, "pools", "heavy", "tickets", name)
	f, err := lock.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Rewrite(out); err != nil {
		t.Fatal(err)
	}
	if held {
		if ok, err := f.TryLock(); !ok || err != nil {
			t.Fatalf("lock %s: %v %v", path, ok, err)
		}
		t.Cleanup(func() { f.Close() })
	} else {
		f.Close()
	}
	return "heavy:" + name + ":" + tm.token
}

// A held ticket whose pid is not an integer is not believed; the same ticket with the test
// process's real pid and start is (the control), so the construction itself is sound.
func TestM7_MalformedHolderPidNotBelieved(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(groupHost)
	hold := h.Hold(nil, "heavy")
	tm := snapshotTicket(t, h, hold)
	st, err := proc.ReadStat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	good := map[string]any{"pid": os.Getpid(), "start": st.Start, "state": "running"}
	claim := ticketCopy(t, h, tm, "0-0000000000000000003-00000003.json", good, true)
	if r := h.RunWith([]string{"BITC_HOLD=" + claim}, "run", "coverage", "--", "echo", "ran"); r.Code != 0 {
		t.Fatalf("control: a well-formed held claim from an ancestor was not believed: %+v", r)
	}
	for i, pid := range []any{"abc", "$PPID*1", "1+PPID-1", 1.5, []int{1}} {
		bad := map[string]any{"pid": pid, "start": st.Start, "state": "running"}
		name := []string{"0-0000000000000000004-00000004.json", "0-0000000000000000005-00000005.json", "0-0000000000000000006-00000006.json",
			"0-0000000000000000007-00000007.json", "0-0000000000000000008-00000008.json"}[i]
		claim := ticketCopy(t, h, tm, name, bad, true)
		r := h.RunWith([]string{"BITC_HOLD=" + claim}, "run", "coverage", "--", "echo", "ran")
		if r.Code != 75 || r.Stdout != "" {
			t.Errorf("pid %v was believed: %+v", pid, r)
		}
	}
	hold.Release()
}

// A claim whose ticket file was left on disk with no one holding it (its owner was SIGKILLed
// before it unlinked it, or the file was copied) is not believed, and the probe that found it
// unheld gave its flock back: later callers are admitted and prune removes the file.
func TestM7_LeftOverUnheldTicketNotBelieved(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(groupHost)
	hold := h.Hold(nil, "heavy")
	tm := snapshotTicket(t, h, hold)
	st, err := proc.ReadStat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	forged := map[string]any{"pid": os.Getpid(), "start": st.Start, "state": "running"}
	name := "0-0000000000000000009-00000009.json"
	path := filepath.Join(h.State, "pools", "heavy", "tickets", name)
	claim := ticketCopy(t, h, tm, name, forged, false)
	r := h.RunWith([]string{"BITC_HOLD=" + claim}, "run", "coverage", "--", "echo", "ran")
	if r.Code != 75 || r.Stdout != "" {
		t.Fatalf("an unheld ticket's claim was believed: %+v", r)
	}
	hold.Release()
	claim = ticketCopy(t, h, tm, name, forged, false)
	if r := h.RunWith([]string{"BITC_HOLD=" + claim}, "run", "coverage", "--", "echo", "ran"); r.Code != 0 || r.Stdout != "ran\n" {
		t.Fatalf("the caller did not take its own unit: %+v", r)
	}
	if r := h.Run("run", "coverage", "--", "true"); r.Code != 0 {
		t.Fatalf("a later caller was blocked: %+v", r)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the unheld ticket was not pruned: its flock was not given back")
	}
}
