package status

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/admit"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/state"
)

// noProcLocks points the lock view at a file that is not there, as on a host without procfs.
func noProcLocks(t *testing.T) {
	t.Helper()
	t.Cleanup(lock.SetProcLocksPath(filepath.Join(t.TempDir(), "no-proc-locks")))
}

func writeTicket(t *testing.T, l state.Layout, pool, name, body string) string {
	t.Helper()
	dir := l.Tickets(pool)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const runningTicket = `{"pid":42,"job":"build","class":1,"state":"running","slot":1,"argv":["make"],"cwd":"/w","hold_started":1,"wait_started":1}`
const ticketName = "1-0000000000000000001-aabbccdd.json"

// #24: a slot whose holder cannot be read is unknown: not free, not counted held, never reclaimable.
func TestSnapshotShowsAnUnreadableSlotAsUnknown(t *testing.T) {
	noProcLocks(t)
	l, _ := state.Open(filepath.Join(t.TempDir(), "s"))
	writeTicket(t, l, "lint", ticketName, runningTicket)
	e := &config.Effective{Pools: map[string]*config.Pool{"lint": {Name: "lint", Slots: 2}}}
	r := Snapshot(e, l, nil)
	p := r.Pools[0]
	if len(p.Holders) != 1 || !p.Holders[0].Unknown || p.Holders[0].Slot != 1 {
		t.Fatalf("holders = %+v", p.Holders)
	}
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"unknown":true`) {
		t.Fatalf("json lacks the unknown flag: %s", b)
	}
	var out bytes.Buffer
	r.Text(&out)
	if !strings.Contains(out.String(), "lint: 0/2 slots held, 1 unknown, 0 waiting") || !strings.Contains(out.String(), "slot 1  unknown") {
		t.Fatalf("text:\n%s", out.String())
	}
}

func TestSnapshotShowsAnUnreadableLegacySlotAndGroupUnitAsUnknown(t *testing.T) {
	noProcLocks(t)
	l, _ := state.Open(filepath.Join(t.TempDir(), "s"))
	c := &config.Compat{LegacyDir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(c.LegacyDir, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := lock.Open(admit.LegacySlotLock(c, "lint", 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	e := &config.Effective{Pools: map[string]*config.Pool{"lint": {Name: "lint", Slots: 1}}, Compat: c}
	r := Snapshot(e, l, nil)
	h := r.Pools[0].Holders
	if len(h) != 1 || !h[0].Legacy || !h[0].Unknown {
		t.Fatalf("holders = %+v", h)
	}
	var out bytes.Buffer
	r.Text(&out)
	if !strings.Contains(out.String(), "slot 1  legacy holder unknown") {
		t.Fatalf("text:\n%s", out.String())
	}
	if len(r.Legacy) != 1 || !strings.Contains(r.Legacy[0], "unknown") {
		t.Fatalf("legacy list = %v", r.Legacy)
	}
}

func TestDoctorWarnsOnceThatProcLocksIsUnreadable(t *testing.T) {
	noProcLocks(t)
	l, _ := state.Open(filepath.Join(t.TempDir(), "s"))
	var out bytes.Buffer
	Doctor(&config.Effective{Test: &config.Test{AccountingDir: t.TempDir()}}, l, &out)
	if n := strings.Count(out.String(), "warn locks: /proc/locks unreadable"); n != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", n, out.String())
	}
}

func TestSweepDryRunNeverCountsAnUnknownSlotReclaimable(t *testing.T) {
	noProcLocks(t)
	l, _ := state.Open(filepath.Join(t.TempDir(), "s"))
	writeTicket(t, l, "lint", ticketName, runningTicket)
	e := &config.Effective{}
	var out bytes.Buffer
	if err := Sweep(e, l, true, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "swept ticket") {
		t.Fatalf("an unknown slot was called reclaimable:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "kept ticket lint/"+ticketName+" (holder unknown: /proc/locks unreadable)") {
		t.Fatalf("no unknown note:\n%s", out.String())
	}
}
