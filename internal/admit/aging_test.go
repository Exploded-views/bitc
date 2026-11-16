package admit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/state"
)

func tf(class int, ts time.Time) TicketFile {
	return TicketFile{Name: fmt.Sprintf("%d-%019d-0badf00d.json", class, ts.UnixNano()),
		T: Ticket{Class: class, State: "waiting", WaitStarted: ts.UnixNano()}}
}

var t0 = time.Unix(1_800_000_000, 0)

func TestAgedNeedsClassNormalAndEnoughAge(t *testing.T) {
	after := 10 * time.Minute
	for _, c := range []struct {
		name string
		tk   TicketFile
		now  time.Time
		aft  time.Duration
		want bool
	}{
		{"normal at the threshold", tf(1, t0), t0.Add(after), after, true},
		{"normal just under", tf(1, t0), t0.Add(after - time.Nanosecond), after, false},
		{"normal, aging off", tf(1, t0), t0.Add(24 * time.Hour), 0, false},
		{"high is never 'aged'", tf(0, t0), t0.Add(24 * time.Hour), after, false},
		{"clock stepped back: negative age", tf(1, t0), t0.Add(-time.Hour), after, false},
		{"name without a parsable time", TicketFile{Name: "1-xxxxxxxxxxxxxxxxxxx-0badf00d.json", T: Ticket{Class: 1}}, t0, after, false},
		{"name too short", TicketFile{Name: "1-", T: Ticket{Class: 1}}, t0, after, false},
	} {
		if got := Aged(c.tk, c.now, c.aft); got != c.want {
			t.Errorf("%s: Aged = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAheadOrder(t *testing.T) {
	after := 10 * time.Minute
	now := t0.Add(time.Hour)
	oldN, newN := tf(1, t0), tf(1, t0.Add(55*time.Minute))
	oldH, newH := tf(0, t0.Add(time.Minute)), tf(0, t0.Add(58*time.Minute))
	for _, c := range []struct {
		name string
		a, b TicketFile
		aft  time.Duration
		want bool
	}{
		{"aged normal before a later high", oldN, newH, after, true},
		{"later high not before the aged normal", newH, oldN, after, false},
		{"earlier high before an aged normal", oldH, tf(1, t0.Add(2*time.Minute)), after, true},
		{"non-aged normal after a high that arrived later", newN, newH, after, false},
		{"high before a non-aged normal", newH, newN, after, true},
		{"aging off: later high still before the old normal", newH, oldN, 0, true},
		{"aging off: old normal not before a later high", oldN, newH, 0, false},
		{"FIFO among aged normals", oldN, tf(1, t0.Add(time.Minute)), after, true},
		{"FIFO among highs", oldH, newH, after, true},
		{"FIFO among normals", oldN, newN, after, true},
	} {
		if got := Ahead(c.a, c.b, now, c.aft); got != c.want {
			t.Errorf("%s: Ahead = %v, want %v", c.name, got, c.want)
		}
	}
}

// plant writes a live ticket (flocked by the test) with a chosen class, time and state.
func plant(t *testing.T, l state.Layout, pool string, class int, ts time.Time, st string) {
	t.Helper()
	k := tf(class, ts)
	k.T.State, k.T.Token = st, "planted"
	if st == "running" {
		k.T.Slot = 1
	}
	b, _ := json.Marshal(k.T)
	f, err := lock.Open(l.Tickets(pool) + "/" + k.Name)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.TryLock(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := f.Rewrite(b); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
}

// acquireAt makes one normal-lane attempt whose clock runs ahead of real time by skew, so the
// ticket it creates is skew old at its first attempt (no sleeping).
func acquireAt(t *testing.T, slots int, ageAfter, skew time.Duration, plants func(state.Layout)) error {
	t.Helper()
	lint := &config.Pool{Name: "lint", Slots: slots, Wait: 0}
	l, e := setup(t, map[string]*config.Pool{"lint": lint}, nil)
	e.Priority.AgeAfter = ageAfter
	plants(l)
	r := req(l, e, lint)
	r.Now = func() time.Time { return time.Now().Add(skew) }
	h, err := Acquire(context.Background(), r)
	if err == nil {
		h.Release()
	}
	return err
}

func TestAgedNormalIsNotOvertakenByALaterHigh(t *testing.T) {
	later := func(l state.Layout) { plant(t, l, "lint", 0, time.Now().Add(time.Hour), "waiting") }
	if err := acquireAt(t, 1, 10*time.Minute, 11*time.Minute, later); err != nil {
		t.Fatalf("aged normal behind a later high: %v", err)
	}
	if err := acquireAt(t, 1, 0, 11*time.Minute, later); exit.Code(err) != 75 {
		t.Fatalf(`age_after "0" keeps the old order: %v`, err)
	}
}

func TestNonAgedNormalYieldsToHigh(t *testing.T) {
	later := func(l state.Layout) { plant(t, l, "lint", 0, time.Now().Add(time.Hour), "waiting") }
	if err := acquireAt(t, 1, 10*time.Minute, 9*time.Minute, later); exit.Code(err) != 75 {
		t.Fatalf("a normal run under age_after must wait behind a high one: %v", err)
	}
}

func TestAgedNormalWaitsForAnEarlierHigh(t *testing.T) {
	earlier := func(l state.Layout) { plant(t, l, "lint", 0, time.Now().Add(-time.Hour), "waiting") }
	if err := acquireAt(t, 1, 10*time.Minute, 11*time.Minute, earlier); exit.Code(err) != 75 {
		t.Fatalf("aged normal must not jump an earlier high: %v", err)
	}
}

// The earlier waiter is a normal ticket that is itself aged, and a high one is later than both:
// with aging the two normal tickets go first, so the newcomer is held by the queue; without it
// the high ticket holds it by the priority lane.
func TestAgedNormalWaitsForAnEarlierAgedNormal(t *testing.T) {
	plants := func(l state.Layout) {
		plant(t, l, "lint", 1, time.Now().Add(-time.Hour), "waiting")
		plant(t, l, "lint", 0, time.Now().Add(time.Hour), "waiting")
	}
	err := acquireAt(t, 1, 10*time.Minute, 11*time.Minute, plants)
	if exit.Code(err) != 75 || !strings.HasPrefix(err.Error(), "STARVED lint (queue") {
		t.Fatalf("FIFO among aged runs: %v", err)
	}
	err = acquireAt(t, 1, 0, 11*time.Minute, plants)
	if exit.Code(err) != 75 || err.Error() != "STARVED lint (priority lane)" {
		t.Fatalf("aging off: the high ticket holds it: %v", err)
	}
}

func TestAgedNormalIgnoresARunningHigh(t *testing.T) {
	running := func(l state.Layout) { plant(t, l, "lint", 0, time.Now().Add(-time.Minute), "running") }
	if err := acquireAt(t, 2, 10*time.Minute, 11*time.Minute, running); err != nil {
		t.Fatalf("aged normal must take the free slot beside a running high: %v", err)
	}
	if err := acquireAt(t, 2, 10*time.Minute, 9*time.Minute, running); exit.Code(err) != 75 {
		t.Fatalf("a non-aged normal still yields to a running high: %v", err)
	}
}
