package status

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/jobs"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/proc"
	"github.com/Exploded-views/bitc/internal/proctest"
	"github.com/Exploded-views/bitc/internal/state"
)

func TestRotateKeepsThreeInOrder(t *testing.T) {
	l, _ := state.Open(filepath.Join(t.TempDir(), "s"))
	e := &config.Effective{Host: config.Host{HistoryMax: 10, Retention: time.Hour}}
	for i := 1; i <= 5; i++ {
		_ = os.WriteFile(l.History(), []byte("round "+strconv.Itoa(i)+strings.Repeat("x", 20)+"\n"), 0o600)
		var out bytes.Buffer
		if err := Sweep(e, l, false, &out); err != nil {
			t.Fatal(err)
		}
	}
	for s, round := range map[string]int{".1": 5, ".2": 4, ".3": 3} {
		b, err := os.ReadFile(l.History() + s)
		if err != nil || !strings.HasPrefix(string(b), "round "+strconv.Itoa(round)+"x") {
			t.Fatalf("history%s = %q, %v; want round %d", s, b, err, round)
		}
	}
	if _, err := os.Stat(l.History()); err == nil {
		t.Fatal("the current history was not rotated away")
	}
	if _, err := os.Stat(l.History() + ".4"); err == nil {
		t.Fatal("kept more than three")
	}
}

// A supervisor that exited unreaped (a zombie: this test, its parent, has not waited) is dead.
// Sweep marks its record lost and ended; the next sweep past retention removes it.
func TestSweepMarksAZombieSupervisorLostThenAgesItOut(t *testing.T) {
	l, _ := state.Open(filepath.Join(t.TempDir(), "s"))
	e := &config.Effective{Host: config.Host{HistoryMax: 1 << 20, Retention: time.Nanosecond}}
	z := exec.Command("true")
	if err := z.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = z.Wait() })
	ps, err := proc.ReadStat(z.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !proctest.Gone(z.Process.Pid); {
		if time.Now().After(deadline) {
			t.Fatal("child never exited")
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec := jobs.Open(l, "abcd1234", "j1")
	gen, err := rec.Bump()
	if err != nil {
		t.Fatal(err)
	}
	_ = rec.WriteStatus(jobs.Status{State: "running", Gen: gen, Pid: z.Process.Pid, Start: ps.Start, Log: "x"})
	var out bytes.Buffer
	_ = Sweep(e, l, false, &out)
	if !strings.Contains(out.String(), "swept job abcd1234/j1 (supervisor died; marked lost)") {
		t.Fatalf("a zombie supervisor was not marked lost:\n%s", out.String())
	}
	st, err := rec.ReadStatus(gen)
	if err != nil || st.State != "ended" || st.EndedAt == "" {
		t.Fatalf("lost record not ended: %+v %v", st, err)
	}
	out.Reset()
	_ = Sweep(e, l, false, &out)
	if _, err := os.Stat(filepath.Join(rec.Dir, "gen")); err == nil || !strings.Contains(out.String(), "older than retention") {
		t.Fatalf("lost record did not age out:\n%s", out.String())
	}
}

// The flock probe is a private file: a busy .doctor.lock (or a second doctor) cannot fail it.
func TestDoctorFlockProbeIsPrivate(t *testing.T) {
	l, _ := state.Open(filepath.Join(t.TempDir(), "s"))
	busy, err := lock.Open(filepath.Join(l.Root, ".doctor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if ok, _ := busy.TryLock(); !ok {
		t.Fatal("cannot lock")
	}
	var out bytes.Buffer
	// an accounting dir of its own: doctor looks for remote leftovers, never in this account's
	Doctor(&config.Effective{Test: &config.Test{AccountingDir: t.TempDir()}}, l, &out)
	if !strings.Contains(out.String(), "ok flock:") {
		t.Fatalf("%s", out.String())
	}
	if left, _ := filepath.Glob(filepath.Join(l.Root, ".doctor-*")); len(left) != 0 {
		t.Fatalf("probe left behind: %v", left)
	}
}

func agedRecord(t *testing.T) (state.Layout, *config.Effective, jobs.Record, int) {
	t.Helper()
	l, _ := state.Open(filepath.Join(t.TempDir(), "s"))
	e := &config.Effective{Host: config.Host{HistoryMax: 1 << 20, Retention: time.Nanosecond}}
	rec := jobs.Open(l, "abcd1234", "j1")
	gen, err := rec.Bump()
	if err != nil {
		t.Fatal(err)
	}
	_ = rec.WriteStatus(jobs.Status{State: "ended", Gen: gen, EndedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)})
	_ = rec.WriteResult(gen, map[string]any{"outcome": "success"})
	return l, e, rec, gen
}

// Retention removal keeps record.lock (same inode) so a concurrent Bump or Lock still contends.
func TestSweepRetentionKeepsRecordLockInode(t *testing.T) {
	l, e, rec, _ := agedRecord(t)
	before, err := os.Stat(filepath.Join(rec.Dir, "record.lock"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_ = Sweep(e, l, false, &out)
	if _, err := os.Stat(filepath.Join(rec.Dir, "gen")); err == nil {
		t.Fatalf("aged record was not emptied:\n%s", out.String())
	}
	after, err := os.Stat(filepath.Join(rec.Dir, "record.lock"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("record.lock was deleted or replaced: %v", err)
	}
}

// A re-run (Bump) that lands between sweep's read and its removal must not lose the live record.
func TestSweepRetentionSkipsRecordBumpedMeanwhile(t *testing.T) {
	l, e, rec, _ := agedRecord(t)
	beforeRetentionLock = func() {
		gen, err := rec.Bump()
		if err != nil {
			t.Error(err)
			return
		}
		_ = rec.WriteStatus(jobs.Status{State: "running", Gen: gen, Pid: os.Getpid(), Log: "x"})
	}
	t.Cleanup(func() { beforeRetentionLock = nil })
	var out bytes.Buffer
	_ = Sweep(e, l, false, &out)
	if g := rec.Gen(); g != 2 {
		t.Fatalf("live record was swept: gen %d", g)
	}
	if st, err := rec.ReadStatus(2); err != nil || st.State != "running" {
		t.Fatalf("live status lost: %+v %v", st, err)
	}
}
