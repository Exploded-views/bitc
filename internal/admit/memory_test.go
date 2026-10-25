package admit

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/proc"
)

func fakeRoot(t *testing.T, availKB int64, cgroup map[string]string) string {
	t.Helper()
	root := t.TempDir()
	write := func(p, s string) {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o700)
		_ = os.WriteFile(filepath.Join(root, p), []byte(s), 0o600)
	}
	if availKB >= 0 {
		write("proc/meminfo", "MemTotal:       32000000 kB\nMemFree:  1 kB\nMemAvailable:   "+itoa64(availKB)+" kB\n")
	}
	if cgroup != nil {
		write("proc/self/cgroup", "0::/user.slice/agent.scope\n")
		for k, v := range cgroup {
			write("sys/fs/cgroup/"+k, v)
		}
	}
	return root
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func gate(root string, floor int64, warn *bytes.Buffer) *MemoryGate {
	return &MemoryGate{ProcfsRoot: root, Floor: floor, Window: time.Minute,
		ReservationsPath: filepath.Join(root, "res.json"), Warn: warn}
}

func TestFloorExactlyAdmits(t *testing.T) {
	var w bytes.Buffer
	g := gate(fakeRoot(t, 6<<20, nil), 6<<30, &w) // 6 GiB in kB vs a 6 GiB floor
	if ok, reason, err := g.Check(time.Now()); !ok || err != nil {
		t.Fatal(reason, err)
	}
	g = gate(fakeRoot(t, 4<<20, nil), 6<<30, &w)
	ok, reason, _ := g.Check(time.Now())
	if ok || reason != "memory: 4096 MiB available, floor 6144 MiB" {
		t.Fatal(ok, reason)
	}
}

func TestReservationsCountWithinWindow(t *testing.T) {
	var w bytes.Buffer
	root := fakeRoot(t, 10<<20, nil)
	g := gate(root, 6<<30, &w)
	now := time.Now()
	if ok, _, _ := g.Check(now); !ok {
		t.Fatal("10 GiB free refused")
	}
	_ = g.Reserve(now, "t1", 5<<30)
	if ok, reason, _ := g.Check(now.Add(time.Second)); ok || reason != "memory: 5120 MiB available, floor 6144 MiB" {
		t.Fatal(ok, reason)
	}
	if ok, _, _ := g.Check(now.Add(2 * time.Minute)); !ok {
		t.Fatal("an expired reservation still counted")
	}
	_ = g.Reserve(now, "t2", 5<<30)
	_ = g.Drop("t2")
	if ok, _, _ := g.Check(now.Add(time.Second)); ok {
		t.Fatal("t1 must still count")
	}
}

func TestCgroupHeadroomIsTighter(t *testing.T) {
	var w bytes.Buffer
	root := fakeRoot(t, 30<<20, map[string]string{
		"user.slice/memory.max":                 "max\n",
		"user.slice/memory.current":             "1000\n",
		"user.slice/agent.scope/memory.max":     itoa64(8<<30) + "\n",
		"user.slice/agent.scope/memory.current": itoa64(4<<30) + "\n",
	})
	ok, reason, _ := gate(root, 6<<30, &w).Check(time.Now())
	if ok || reason != "memory: 4096 MiB available, floor 6144 MiB" {
		t.Fatal(ok, reason)
	}
}

func TestUnreadableWarnsOncePerCall(t *testing.T) {
	var w bytes.Buffer
	g := gate(fakeRoot(t, -1, nil), 6<<30, &w)
	for i := 0; i < 3; i++ {
		if ok, _, _ := g.Check(time.Now()); !ok {
			t.Fatal("unreadable meminfo must admit")
		}
	}
	if strings.Count(w.String(), "bitc: ") != 1 {
		t.Fatalf("warnings %q", w.String())
	}
}

func TestZeroFloorNeverReads(t *testing.T) {
	var w bytes.Buffer
	if ok, _, _ := gate(fakeRoot(t, -1, nil), 0, &w).Check(time.Now()); !ok || w.Len() != 0 {
		t.Fatal("a zero floor read meminfo or warned")
	}
}

// A ticket write that fails after Reserve (admitted() is the last step of the attempt) must
// not leave the reservation counting against everyone else for the whole window.
func TestAbandonDropsReservationWhenTicketWriteFails(t *testing.T) {
	heavy := &config.Pool{Name: "heavy", Slots: 1, Memory: 1 << 30}
	l, e := setup(t, map[string]*config.Pool{"heavy": heavy}, nil)
	root := fakeRoot(t, 30<<20, nil)
	e.Host.MemoryFloor, e.Host.ReservationWindow = 6<<30, time.Minute
	self, err := proc.ReadStat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	me, err := newTicket(l, "heavy", Ticket{Pid: os.Getpid(), Start: self.Start, Class: 1, State: "waiting", Token: "x"})
	if err != nil {
		t.Fatal(err)
	}
	a := &admission{r: req(l, e, heavy), pool: heavy, me: me, notes: io.Discard,
		memory: &MemoryGate{ProcfsRoot: root, Floor: 6 << 30, Window: time.Minute,
			ReservationsPath: l.Reservations(), Warn: io.Discard}}
	me.f.Close() // the next ticket write fails
	if _, _, err := a.attempt(time.Now()); err == nil {
		t.Fatalf("attempt err=%v, want a write error", err)
	}
	b, _ := os.ReadFile(l.Reservations())
	if !strings.Contains(string(b), me.name) {
		t.Fatalf("test setup: no reservation to drop: %q", b)
	}
	a.abandon(nil)
	b, _ = os.ReadFile(l.Reservations())
	if strings.Contains(string(b), me.name) {
		t.Fatalf("reservation survived the abandoned attempt: %q", b)
	}
}
