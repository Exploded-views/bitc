package conformance

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// flockHold is the stand-in for the shell callers of a one-lock-file pool (testdata/flockhold),
// built by TestMain: macOS has no flock(1), and one helper makes both OSes meet the same holder.
var flockHold string

// fcRig is a host with a pool "heavy" (two slots) and an exclusive pool "bench" (exclusive = true),
// both named in [compat.flock] over one lock file, one gate and slot files.
type fcRig struct {
	h                 *harness.H
	lock, gate, slotT string
}

func (r *fcRig) slot(n int) string { return strings.ReplaceAll(r.slotT, "{n}", strconv.Itoa(n)) }

func fcHost(t *testing.T) *fcRig {
	t.Helper()
	h := harness.New(t)
	dir := filepath.Join(h.Root, "shell")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r := &fcRig{h: h, lock: filepath.Join(dir, "heavy.lock"), gate: filepath.Join(dir, "heavy.gate"),
		slotT: filepath.Join(dir, "heavy.slot{n}")}
	h.HostConfig(r.body("lock = "+harness.Q(r.lock)+"\ngate = "+harness.Q(r.gate)+"\nslot = "+harness.Q(r.slotT)+"\n", "", ""))
	return r
}

// body is a host file body: pools heavy and bench, [compat] and [compat.flock.<pool>] tables.
func (r *fcRig) body(heavyKeys, benchKeys, extra string) string {
	if benchKeys == "" {
		benchKeys = heavyKeys
	}
	benchKeys += "exclusive = true\n"
	return "groups = { cpu = { units = 2 } }\n" +
		"[pools.heavy]\nslots = 2\nwait = \"1h\"\n" +
		"[pools.bench]\nslots = 1\nwait = \"1h\"\ngroups = { cpu = 2 }\n" +
		"[compat]\nlegacy_dir = " + harness.Q(filepath.Join(r.h.Root, "legacy")) + "\n" +
		"[compat.flock.heavy]\n" + heavyKeys + "[compat.flock.bench]\n" + benchKeys + extra
}

// shellHold starts a live shell-shaped holder taking specs (s=PATH, x=PATH) in order, as flock(1)
// callers do, and returns once it holds them all. The returned func ends it.
func shellHold(t *testing.T, h *harness.H, specs ...string) (release func()) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(flockHold, append([]string{"hold", ready}, specs...)...)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	var once sync.Once
	release = func() {
		once.Do(func() { _ = in.Close(); <-exited })
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		release()
	})
	h.Until(func() bool {
		if _, err := os.Stat(ready); err == nil {
			return true
		}
		select {
		case <-exited:
			t.Fatal("the shell holder ended before it held its locks")
		default:
		}
		return false
	}, 20*time.Second)
	return release
}

// free reports whether every spec can be taken without blocking now (flock -n).
func free(t *testing.T, specs ...string) bool {
	t.Helper()
	err := exec.Command(flockHold, append([]string{"try"}, specs...)...).Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return false
	default:
		t.Fatalf("flockhold try %v: %v", specs, err)
		return false
	}
}

func TestFC1_ShellRunHolderMakesSlotOneBusyBitcTakesSlotTwo(t *testing.T) {
	r := fcHost(t)
	// the real run script takes the gate for an instant and lets it go before it takes the lock
	shellHold(t, r.h, "s="+r.lock, "x="+r.slot(1))
	hold := r.h.Hold(nil, "heavy")
	if got := hold.Env["BITC_SLOT"]; got != "heavy.2" {
		t.Fatalf("BITC_SLOT = %q, want heavy.2 (slot 1 is held by the shell caller)", got)
	}
	if free(t, "x="+r.slot(2)) {
		t.Fatal("a bitc hold of slot 2 does not take the shell slot file 2")
	}
	if r := hold.Release(); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestFC2_BareExclusiveFlockBlocksEveryBitcSlot(t *testing.T) {
	r := fcHost(t)
	release := shellHold(t, r.h, "x="+r.lock) // flock -x heavy.lock cmd
	want := "STARVED heavy (legacy pool: " + r.lock + " busy)\n"
	if res := r.h.Run("run", "--wait", "0s", "heavy", "--", "true"); res.Code != 75 || res.Stderr != want {
		t.Fatalf("%+v, want 75 %q", res, want)
	}
	// With a wait, bitc queues and runs once the caller is gone.
	p := r.h.Start("run", "heavy", "--", "echo", "ran")
	r.h.Until(func() bool { return len(tickets(r.h, "heavy")) > 0 || p.Exited() }, 20*time.Second)
	if p.Exited() {
		t.Fatalf("bitc ran past a bare exclusive flock: %+v", p.Wait())
	}
	release()
	if res := p.Wait(); res.Code != 0 || !strings.Contains(res.Stdout, "ran") {
		t.Fatalf("%+v", res)
	}
}

func TestFC3_BenchHoldersExcludeEachOtherBothWays(t *testing.T) {
	r := fcHost(t)
	// bitc bench holder: a shell run holder (gate shared, lock shared) cannot start.
	bench := r.h.Hold(nil, "bench")
	if free(t, "s="+r.gate) || free(t, "s="+r.lock) {
		t.Fatal("a bitc exclusive hold does not block a shell run holder")
	}
	if res := bench.Release(); res.Code != 0 {
		t.Fatalf("%+v", res)
	}
	if !free(t, "s="+r.gate, "s="+r.lock) {
		t.Fatal("the bitc exclusive hold kept its locks after it ended")
	}
	// shell bench holder (gate and lock exclusive): bitc waits out both its pools.
	release := shellHold(t, r.h, "x="+r.gate, "x="+r.lock)
	for _, pool := range []string{"heavy", "bench"} {
		want := "STARVED " + pool + " (legacy pool: " + r.gate + " busy)\n"
		if res := r.h.Run("run", "--wait", "0s", pool, "--", "true"); res.Code != 75 || res.Stderr != want {
			t.Fatalf("%s: %+v, want 75 %q", pool, res, want)
		}
	}
	release()
	if res := r.h.Run("run", "--wait", "0s", "bench", "--", "true"); res.Code != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestFC4_BitcHolderMakesNonBlockingExclusiveFlockFail(t *testing.T) {
	r := fcHost(t)
	hold := r.h.Hold(nil, "heavy")
	if free(t, "x="+r.lock) {
		t.Fatal("flock -n heavy.lock succeeded beside a bitc holder")
	}
	if !free(t, "s="+r.lock) {
		t.Fatal("a bitc run holder must share the lock with shell run holders")
	}
	if free(t, "x="+r.slot(1)) {
		t.Fatal("a bitc hold does not take the shell slot file")
	}
	if res := hold.Release(); res.Code != 0 {
		t.Fatalf("%+v", res)
	}
	if !free(t, "x="+r.gate, "x="+r.lock, "x="+r.slot(1), "x="+r.slot(2)) {
		t.Fatal("a finished bitc hold left a gate, lock or slot file held")
	}
}

func TestFC5_BadCompatFlockConfigIs78(t *testing.T) {
	r := fcHost(t)
	l, g, s := "lock = "+harness.Q(r.lock)+"\n", "gate = "+harness.Q(r.gate)+"\n", "slot = "+harness.Q(r.slotT)+"\n"
	cases := []struct{ name, heavy, extra, want string }{
		{"no lock", g + s, "", "compat.flock.heavy.lock is required"},
		{"no gate", l + s, "", "compat.flock.heavy.gate is required"},
		{"no slot", l + g, "", "compat.flock.heavy.slot is required"},
		{"slot without {n}", l + g + "slot = " + harness.Q(filepath.Join(r.h.Root, "heavy.slot")) + "\n", "", "compat.flock.heavy.slot must contain {n}"},
		{"relative lock", "lock = \"heavy.lock\"\n" + g + s, "", "compat.flock.heavy.lock must be an absolute path"},
		{"relative gate", l + "gate = \"heavy.gate\"\n" + s, "", "compat.flock.heavy.gate must be an absolute path"},
		{"relative slot", l + g + "slot = \"heavy.slot{n}\"\n", "", "compat.flock.heavy.slot must be an absolute path"},
		{"lock is the gate", l + "gate = " + harness.Q(r.lock) + "\n" + s, "", "compat.flock.heavy: lock and gate must be different files"},
		{"undeclared pool", l + g + s, "[compat.flock.nosuch]\n" + l + g + s, "compat.flock.nosuch names no declared pool"},
	}
	for _, c := range cases {
		r.h.HostConfig(r.body(c.heavy, l+g+s, c.extra))
		if res := r.h.Run("config", "check"); res.Code != 78 || !strings.Contains(res.Stderr, c.want) {
			t.Errorf("%s: %+v, want 78 naming %q", c.name, res, c.want)
		}
	}
	r.h.HostConfig(r.body(l+g+s, "", ""))
	if res := r.h.Run("config", "check"); res.Code != 0 {
		t.Fatalf("a good [compat.flock] was refused: %+v", res)
	}
}
